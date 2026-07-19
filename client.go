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
	"strings"
	"time"
)

const (
	endpoint = "https://www.miruro.tv"
	// UserAgent is the one browser identity shared by the pipe, the quality
	// probe, and the stream proxy, so a CDN sees a single client across a
	// playlist and its segments
	UserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

	// maxPipeBody caps the decoded pipe response against a decompression bomb
	// the largest real payload, One Piece, decodes to about 8.7 MB
	maxPipeBody = 64 << 20
)

var (
	// ErrBlocked is fatal
	// the WAF rejected the request
	ErrBlocked = errors.New("cloudflare blocked request")
	// ErrUpstream is recoverable and drives provider fallback
	ErrUpstream = errors.New("miruro upstream unreachable")
)

// obfKey is VITE_PIPE_OBF_KEY, applied only when x-obfuscated is 2
var obfKey = []byte{
	0x71, 0x95, 0x10, 0x34, 0xf8, 0xfb, 0xcf, 0x53,
	0xd8, 0x9d, 0xb5, 0x2c, 0xeb, 0x3d, 0xc2, 0x2c,
}

type Client struct {
	Base string
	HTTP *http.Client
}

func New() *Client {
	// the cloned default transport keeps HTTP/2 via ALPN, which passes the WAF
	// ResponseHeaderTimeout excludes the body read, so Timeout backstops an
	// upstream that answers and then stalls mid-body
	// the largest episodes payload, One Piece at 13278 rows, reads in about 1.2s,
	// so this bound cannot cut a real response short
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &Client{
		Base: endpoint,
		HTTP: &http.Client{Transport: tr, Timeout: 2 * time.Minute},
	}
}

type envelope struct {
	Path   string            `json:"path"`
	Method string            `json:"method"`
	Query  map[string]string `json:"query"`
	Body   any               `json:"body"`
}

// pipe runs an obfuscated secure-pipe GET and returns the decoded JSON body.
func (c *Client) pipe(ctx context.Context, path string, query map[string]string) ([]byte, error) {
	if query == nil {
		query = map[string]string{}
	}
	env, err := json.Marshal(envelope{Path: path, Method: http.MethodGet, Query: query})
	if err != nil {
		return nil, err
	}
	e := base64.RawURLEncoding.EncodeToString(env)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/secure/pipe?e="+e, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req.Header)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// keep a cancelled context as a context error so callers can match it
		// otherwise the fallback loop treats Ctrl-C as a recoverable failure
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}

	isHTML := strings.Contains(resp.Header.Get("content-type"), "text/html")
	switch {
	case resp.StatusCode == http.StatusForbidden && isHTML:
		return nil, ErrBlocked
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("%w: status %d", ErrUpstream, resp.StatusCode)
	case isHTML:
		return nil, ErrUpstream
	}

	obf := resp.Header.Get("x-obfuscated")
	if obf == "" {
		return body, nil
	}
	return decode(body, obf)
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

func setHeaders(h http.Header) {
	h.Set("User-Agent", UserAgent)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", "en-US,en;q=0.5")
	h.Set("Referer", "https://www.miruro.tv/")
	h.Set("Origin", "https://www.miruro.tv")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
}
