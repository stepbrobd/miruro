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
	"sync"
	"sync/atomic"
	"time"

	"ysun.co/miruro/internal/upstream"
)

// maxBody caps a response, both as it arrives and once decoded, against an
// endless body and a decompression bomb
// the largest real payload, One Piece's 1184 episodes, is 266 KB on the wire and
// about 1 MB decoded
const maxBody = 64 << 20

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

	mu sync.Mutex
	// base indexes the mirror that answered last, so one failure does not cost
	// every later request the same walk
	base int

	// codes is the provider table, fetched at most once per client
	// codesMu is only ever taken before mu
	codesMu sync.Mutex
	codes   map[string]string
}

// Name is the backend name, what a config entry and a log line call it
func (c *Client) Name() string { return "miruro" }

func New() *Client {
	// the cloned default transport keeps HTTP/2 via ALPN, which passes the WAF
	// ResponseHeaderTimeout excludes the body read, so Timeout backstops an
	// upstream that answers and then stalls mid-body
	// the largest payload, One Piece's episode list, reads in about 0.25s, so
	// this bound cannot cut a real response short
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &Client{
		Bases: slices.Clone(mirrors),
		HTTP:  &http.Client{Transport: tr, Timeout: 2 * time.Minute},
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
		body, v, err := c.attempt(ctx, c.Bases[idx], ref)
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

// attempt runs the request against one mirror and reports what get should do
// next
// a transport failure and a WAF rejection are what another mirror could answer
// every mirror fronts the same backend, so walking them all on a backend status
// would multiply the requests a provider outage already costs
func (c *Client) attempt(ctx context.Context, base, ref string) ([]byte, verdict, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+ref, nil)
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
		return nil, refused, fmt.Errorf("%w: miruro %s: %s", upstream.ErrUnreachable, req.URL.Path, refusal(resp.StatusCode, kind, body))
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
	for _, s := range []string{p.Title, p.Detail} {
		if s != "" {
			out += ": " + s
		}
	}
	return out
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
