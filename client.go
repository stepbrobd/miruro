// Package miruro is the data layer, an AniList-backed client for miruro.tv.
// It owns the search, episode, and source resolution against the secure pipe,
// including the browser header set, the HTTP/2 transport, and deobfuscation.
package miruro

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// UserAgent is the one browser identity shared by the pipe, the quality
	// probe, and the stream proxy, so a CDN sees a single client across a
	// playlist and its segments
	UserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

	// maxPipeBody caps the decoded pipe response against a decompression bomb
	// the largest real payload, One Piece, decodes to about 8.7 MB
	maxPipeBody = 64 << 20
	// maxPipeRaw caps the wire body feeding decode, sized so anything that
	// decodes within maxPipeBody fits despite the base64 over gzip expansion
	maxPipeRaw = 96 << 20
)

var (
	// ErrBlocked is fatal
	// the WAF rejected the request
	ErrBlocked = errors.New("cloudflare blocked request")
	// ErrUpstream is recoverable and drives provider fallback
	ErrUpstream = errors.New("miruro upstream unreachable")
)

// mirrors are the domains that front one miruro backend, which answers every
// one of them with the same bytes
// www.miruro.com publishes this list and is not itself a pipe host
// the order leads with .ru on MiruroAPI's report that it carries the softest
// Cloudflare rules, which is unverified here
var mirrors = []string{
	"https://www.miruro.ru",
	"https://www.miruro.to",
	"https://www.miruro.bz",
	"https://www.miruro.tv",
}

// obfKey is VITE_PIPE_OBF_KEY, applied only when x-obfuscated is 2
var obfKey = []byte{
	0x71, 0x95, 0x10, 0x34, 0xf8, 0xfb, 0xcf, 0x53,
	0xd8, 0x9d, 0xb5, 0x2c, 0xeb, 0x3d, 0xc2, 0x2c,
}

type Client struct {
	// Bases are the mirror origins tried in order
	Bases []string
	HTTP  *http.Client

	mu sync.Mutex
	// base indexes the mirror that answered last, so one failure does not cost
	// every later request the same walk
	base int

	// the capability table is fetched at most once per client
	// cfgMu is only ever taken before mu
	cfgMu   sync.Mutex
	cfg     Capabilities
	cfgErr  error
	cfgDone bool
}

// Name is the backend name, what a config entry and a log line call it
func (c *Client) Name() string { return "miruro" }

func New() *Client {
	// the cloned default transport keeps HTTP/2 via ALPN, which passes the WAF
	// ResponseHeaderTimeout excludes the body read, so Timeout backstops an
	// upstream that answers and then stalls mid-body
	// the largest episodes payload, One Piece at 13278 rows, reads in about 1.2s,
	// so this bound cannot cut a real response short
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &Client{
		Bases: slices.Clone(mirrors),
		HTTP:  &http.Client{Transport: tr, Timeout: 2 * time.Minute},
	}
}

type envelope struct {
	Path   string            `json:"path"`
	Method string            `json:"method"`
	Query  map[string]string `json:"query"`
	Body   any               `json:"body"`
}

// pipe runs an obfuscated secure-pipe GET and returns the decoded JSON body.
// It walks the mirrors from the one that answered last, and only a failure a
// different mirror could answer moves it along
func (c *Client) pipe(ctx context.Context, path string, query map[string]string) ([]byte, error) {
	if query == nil {
		query = map[string]string{}
	}
	env, err := json.Marshal(envelope{Path: path, Method: http.MethodGet, Query: query})
	if err != nil {
		return nil, err
	}
	e := base64.RawURLEncoding.EncodeToString(env)

	if len(c.Bases) == 0 {
		return nil, fmt.Errorf("%w: no mirror configured", ErrUpstream)
	}

	start := c.current()
	blocked := false
	var last error
	for i := range c.Bases {
		idx := (start + i) % len(c.Bases)
		body, v, err := c.attempt(ctx, c.Bases[idx], path, e)
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
		if errors.Is(err, ErrBlocked) {
			blocked = true
		}
		last = err
	}
	// one mirror answering with a WAF rejection is enough to call the session
	// blocked, even when a later mirror failed to connect at all, since reporting
	// that as recoverable sends the fallback loop back into the block
	if blocked {
		return nil, ErrBlocked
	}
	return nil, last
}

// verdict is what pipe does with one mirror's outcome
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

// attempt runs the pipe against one mirror and reports what pipe should do
// next.
// A transport failure and a WAF rejection are what another mirror could answer
// every mirror fronts the same backend, so walking them all on a backend status
// would multiply the requests a provider outage already costs
func (c *Client) attempt(ctx context.Context, base, path, e string) ([]byte, verdict, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/secure/pipe?e="+e, nil)
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
		// keep a cancelled context as a context error so callers can match it
		// otherwise the fallback loop treats Ctrl-C as a recoverable failure
		if ctx.Err() != nil {
			return nil, aborted, ctx.Err()
		}
		return nil, reached(connected.Load()), fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPipeRaw+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, aborted, ctx.Err()
		}
		// the headers already arrived, so this host answered
		return nil, refused, err
	}
	if len(body) > maxPipeRaw {
		return nil, refused, fmt.Errorf("pipe response exceeds %d bytes", maxPipeRaw)
	}

	isHTML := strings.Contains(resp.Header.Get("content-type"), "text/html")
	switch {
	case resp.StatusCode == http.StatusForbidden && isHTML:
		return nil, unreachable, ErrBlocked
	case resp.StatusCode >= 400:
		return nil, refused, fmt.Errorf("%w: status %d", ErrUpstream, resp.StatusCode)
	case isHTML:
		return nil, refused, ErrUpstream
	}

	if obf := resp.Header.Get("x-obfuscated"); obf != "" {
		if body, err = decode(body, obf); err != nil {
			return nil, refused, err
		}
	}

	// a resource that fails answers 200 with an error object rather than a status
	// every resource does this, so the check belongs here rather than per caller
	var fail struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &fail) == nil && fail.Error != "" {
		return nil, refused, fmt.Errorf("%w: %s: %s", ErrUpstream, path, fail.Error)
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

// decode reverses base64url, then the optional xor, then gzip
func decode(body []byte, obf string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(string(bytes.TrimRight(body, "=")))
	if err != nil {
		return nil, err
	}
	if obf == "2" {
		for i := range raw {
			raw[i] ^= obfKey[i%len(obfKey)]
		}
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxPipeBody+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxPipeBody {
		return nil, fmt.Errorf("pipe response exceeds %d bytes", maxPipeBody)
	}
	return out, nil
}

func newGet(ctx context.Context, url, referer string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	return req, nil
}

// setHeaders writes the browser header set the WAF expects
// the origin follows the mirror in use, so a rule comparing it against the host
// sees what a browser on that domain would send
func setHeaders(h http.Header, base string) {
	h.Set("User-Agent", UserAgent)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", "en-US,en;q=0.5")
	h.Set("Referer", base+"/")
	h.Set("Origin", base)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
}
