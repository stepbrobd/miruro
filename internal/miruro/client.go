// Package miruro is the miruro.tv backend, an aggregator fronting several
// provider sites behind one obfuscated api.
// It owns the search, episode, and source resolution against that api,
// including the browser header set, the HTTP/2 transport, and deobfuscation.
package miruro

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"ysun.co/miruro/internal/upstream"
)

// maxBody caps a response, both as it arrives and once decoded, against an
// endless body and a decompression bomb
// the largest real payload, One Piece's 1184 episodes, is 266 KB on the wire and
// about 1 MB decoded
const maxBody = 64 << 20

// maxTokens bounds the json tokens a catalog body may hold
// json.Unmarshal builds a whole row out of {}, so a body inside maxBody can
// still build tens of millions of rows, gigabytes of them, and bounding the
// tokens bounds what any body builds whatever its shape
// a real episode row is a few dozen tokens, so even the ten thousand rows an
// episode list may answer stay well under it
const maxTokens = 1 << 21

// mirrors are the domains that front one miruro backend, which answers every
// one of them with the same bytes, in the order www.miruro.com publishes them
// www.miruro.com is not itself an api host, and in 2026-09 it marked .ru
// deprecated in favor of the new .cx, so .ru is left out even while it answers
var mirrors = []string{
	"https://www.miruro.to",
	"https://www.miruro.bz",
	"https://www.miruro.cx",
	"https://www.miruro.tv",
}

// catalogKey is what the catalog api xors its gzip bodies with, the bytes of a
// string fixed in the site's bundle
var catalogKey = []byte("miruro/catalog")

type Client struct {
	// Bases are the mirror origins tried in order
	Bases []string
	HTTP  *http.Client
	// Timeout bounds one request against one mirror, headers and body together,
	// zero leaving it unbounded
	// the transport's header timeout stops counting once the headers arrive, so
	// without this an upstream that answers and then stalls mid-body hangs the
	// run with no output
	Timeout time.Duration

	// pace spaces the requests this client sends and holds them through a 429,
	// the zero pacer spacing none of them
	pace upstream.Pacer

	mu sync.Mutex
	// base indexes the mirror that answered last, so one failure does not cost
	// every later request the same walk
	base int

	// codes is the provider table, fetched at most once per client
	// codesMu is only ever taken before mu
	codesMu sync.Mutex
	codes   map[string]string

	// preloaded is what Preload kept, the records carrying each AniList id it
	// was asked about
	preloadMu sync.Mutex
	preloaded map[int][]title
}

// Name is the backend name, what a config entry and a log line call it
func (c *Client) Name() string { return "miruro" }

func New() *Client {
	// the cloned default transport keeps HTTP/2 via ALPN, which passes the WAF
	// the largest payload, One Piece's episode list, reads in about 0.25s, so
	// the two minute bound cannot cut a real response short
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &Client{
		Bases:   slices.Clone(mirrors),
		HTTP:    &http.Client{Transport: tr},
		Timeout: 2 * time.Minute,
		pace:    upstream.Pacer{Burst: paceBurst, Every: paceEvery},
	}
}

// get runs a GET for ref, a path and query under a mirror, and returns the
// decoded JSON body
// it walks the mirrors from the one that answered last, and only a failure a
// different mirror could answer moves it along
func (c *Client) get(ctx context.Context, ref string) ([]byte, error) {
	if len(c.Bases) == 0 {
		return nil, fmt.Errorf("%w: no mirror configured", upstream.ErrUnreachable)
	}

	start := c.current()
	blocked := false
	var last error
	for i := range c.Bases {
		idx := (start + i) % len(c.Bases)
		body, v, err := c.sitOut(ctx, c.Bases[idx], ref)
		switch v {
		case served:
			c.prefer(idx)
			return body, nil
		case refused:
			// this host reached the backend, so it is the one to start from next
			// time even though the backend said no
			c.prefer(idx)
			return nil, err
		case aborted:
			return nil, err
		}
		if errors.Is(err, upstream.ErrBlocked) {
			blocked = true
		}
		last = err
	}
	// one mirror answering with a WAF rejection is enough to call the session
	// blocked, even when a later mirror failed to connect at all, since reporting
	// that as recoverable sends the fallback loop back into the block
	if blocked {
		return nil, upstream.ErrBlocked
	}
	return nil, last
}

// verdict is what get does with one mirror's outcome
type verdict int

const (
	// served means the mirror returned a decoded body
	served verdict = iota
	// refused means the mirror reached the backend and the backend said no
	// the host works, so the walk stops and the index stays on it
	refused
	// unreachable means only this mirror failed, so the walk continues
	unreachable
	// aborted means the request never left, so there is nothing to walk to
	aborted
)

// sitOut runs attempt against one mirror, and sits out a 429 whose wait is no
// longer than maxHold, holding every other request of this client with it
// cloudflare counts each mirror apart, so moving on would only shift the load
// the 429 asked to shed onto the next one
// a 429 therefore ends the walk like any other answer from the backend
func (c *Client) sitOut(ctx context.Context, base, ref string) ([]byte, verdict, error) {
	for n := 0; ; n++ {
		body, v, err := c.attempt(ctx, base, ref)
		var l limited
		if !errors.As(err, &l) || time.Duration(l) > maxHold {
			return body, v, err
		}
		// the hold stands even when this request gives up, so the requests
		// behind it do not walk into the same limit
		c.pace.Hold(time.Duration(l))
		if n == holds {
			return body, v, err
		}
	}
}

// attempt runs the request against one mirror and reports what get should do
// next
// a transport failure and a WAF rejection are what another mirror could answer
// every mirror fronts the same backend, so walking them all on a backend status
// would multiply the requests a provider outage already costs
func (c *Client) attempt(ctx context.Context, base, ref string) ([]byte, verdict, error) {
	// the turn is waited for before the deadline starts, so time spent queued
	// behind other requests is never taken from this one
	if err := c.pace.Wait(ctx); err != nil {
		return nil, aborted, err
	}
	// the request runs under its own deadline, and every check below asks the
	// caller's context, so a run the user stopped aborts while a request that
	// ran out of time is the backend's failure
	rctx := ctx
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, base+ref, nil)
	if err != nil {
		return nil, aborted, err
	}
	setHeaders(req.Header, base)

	// a host that handed over a connection is reachable, so anything that goes
	// wrong after that is the backend's and the other mirrors will reproduce it
	// without this a backend that accepts and then goes quiet costs the whole
	// response header timeout once per mirror instead of once
	var connected atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	}))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// keep a canceled context as a context error so callers can match it
		// otherwise the fallback loop treats Ctrl-C as a recoverable failure
		if ctx.Err() != nil {
			return nil, aborted, ctx.Err()
		}
		return nil, reached(connected.Load()), fmt.Errorf("%w: %v", upstream.ErrUnreachable, err)
	}
	defer resp.Body.Close()

	// every failure past this point came from a host that answered, so it is
	// the backend's, and ErrUnreachable keeps the typed surface whole while the
	// cause stays matchable beside it
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, aborted, ctx.Err()
		}
		return nil, refused, fmt.Errorf("%w: miruro %s: %w", upstream.ErrUnreachable, req.URL.Path, err)
	}
	if len(body) > maxBody {
		return nil, refused, fmt.Errorf("%w: miruro %s exceeds %d bytes", upstream.ErrUnreachable, req.URL.Path, maxBody)
	}

	kind := mediaType(resp.Header)
	isHTML := kind == "text/html"
	switch {
	case resp.StatusCode == http.StatusForbidden && isHTML:
		return nil, unreachable, upstream.ErrBlocked
	case resp.StatusCode >= 400:
		err := fmt.Errorf("%w: miruro %s: %s", upstream.ErrUnreachable, req.URL.Path, refusal(resp.StatusCode, kind, body))
		if d, ok := retryAfter(resp.Header, time.Now()); ok && resp.StatusCode == http.StatusTooManyRequests {
			err = fmt.Errorf("%w: %w", err, limited(d))
		}
		return nil, refused, err
	case isHTML:
		return nil, refused, fmt.Errorf("%w: miruro %s answered html", upstream.ErrUnreachable, req.URL.Path)
	}

	// the catalog masks what it serves as an octet stream and serves the rest,
	// the provider table among them, as plain json
	if kind == "application/octet-stream" {
		if body, err = unmask(body); err != nil {
			return nil, refused, fmt.Errorf("%w: miruro %s: %w", upstream.ErrUnreachable, req.URL.Path, err)
		}
	}
	return body, served, nil
}

// reached maps whether a connection was obtained to who owns the failure
func reached(connected bool) verdict {
	if connected {
		return refused
	}
	return unreachable
}

// mediaType is the response's content type without its parameters, empty when
// it names none that parses
func mediaType(h http.Header) string {
	kind, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return kind
}

// refusal names why the backend refused, the status plus the reason a problem
// document gives
// the catalog api answers a request it cannot serve with an rfc 9457 problem,
// while a route it does not have falls through to the site's html shell, which
// carries nothing worth reading
func refusal(status int, kind string, body []byte) string {
	out := fmt.Sprintf("status %d", status)
	if kind != "application/problem+json" {
		return out
	}
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &p) != nil {
		return out
	}
	for _, s := range []string{plain(p.Title), plain(p.Detail)} {
		if s != "" {
			out += ": " + s
		}
	}
	return out
}

// plain is catalog text with its control characters dropped, C0, DEL and C1
// a title or a label ends up in a view, and a view keeps the escape sequences
// in its rows since huh styles them with some, so one arriving in the text
// reaches the terminal whole, an OSC 52 clipboard write or an OSC 0 title
// change, and a newline throws off the row count the views redraw by
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func (c *Client) current() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base >= len(c.Bases) {
		return 0
	}
	return c.base
}

func (c *Client) prefer(i int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.base = i
}

// unmask reverses the catalog api's obfuscation, a repeating xor over gzip
// the body is xored in place, since nothing reads the masked bytes afterwards
func unmask(body []byte) ([]byte, error) {
	for i := range body {
		body[i] ^= catalogKey[i%len(catalogKey)]
	}
	return inflate(body)
}

// inflate gunzips raw, refusing anything that decodes past maxBody
func inflate(raw []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxBody {
		return nil, fmt.Errorf("miruro response exceeds %d bytes", maxBody)
	}
	return out, nil
}

// decode unmarshals a catalog body into v once counting its tokens has shown
// that building it stays small
func decode(body []byte, v any) error {
	if _, err := records(body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// decodePage is decode for a page, whose data array holds no more rows than its
// request asked for, since the catalog answers no more than that and a longer
// one is rows that never need building
func decodePage(body []byte, v any, rows int) error {
	n, err := records(body)
	if err != nil {
		return err
	}
	if n > rows {
		return fmt.Errorf("miruro answered %d records to a page of %d", n, rows)
	}
	return json.Unmarshal(body, v)
}

// records counts the rows of body's top-level data array without building any,
// and refuses a body of more than maxTokens tokens
// a body that is not json is left to json.Unmarshal to report, which checks all
// of it before it builds anything
func records(body []byte) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	// a number stays as written, so none fails here that Unmarshal would skip
	dec.UseNumber()
	var (
		n, depth int
		// object is whether the body is an object, and key whether its next
		// token is one of its keys
		object, key bool
		// name is the key of the top-level value being read, and data whether
		// that value is the data array
		name string
		data bool
	)
	for range maxTokens + 1 {
		t, err := dec.Token()
		if err != nil {
			return n, nil
		}
		if s, ok := t.(string); ok && depth == 1 && key {
			name, key = s, false
			continue
		}
		if data && depth == 2 && t != json.Delim(']') {
			n++
		}
		switch t {
		case json.Delim('{'), json.Delim('['):
			if depth == 0 {
				object = t == json.Delim('{')
			}
			if depth == 1 && object && name == "data" && t == json.Delim('[') {
				data = true
			}
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
			if depth == 1 {
				data = false
			}
		}
		// back at the top of the object a value has ended, so a key comes next
		key = depth == 1 && object
	}
	return 0, fmt.Errorf("miruro response holds more than %d json tokens", maxTokens)
}

// setHeaders writes the browser header set the WAF expects
// the origin follows the mirror in use, so a rule comparing it against the host
// sees what a browser on that domain would send
func setHeaders(h http.Header, base string) {
	h.Set("User-Agent", upstream.UserAgent)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", "en-US,en;q=0.5")
	h.Set("Referer", base+"/")
	h.Set("Origin", base)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
}
