// Package pipe is the sole HTTP boundary to the miruro secure pipe.
// It owns the browser header set, the HTTP/2 transport, and the response
// deobfuscation. Callers see decoded JSON bytes and two sentinel errors.
package pipe

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

const endpoint = "https://www.miruro.tv"

const userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

var (
	// ErrBlocked is fatal, the WAF rejected the request
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
	return &Client{
		Base: endpoint,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

type envelope struct {
	Path   string            `json:"path"`
	Method string            `json:"method"`
	Query  map[string]string `json:"query"`
	Body   any               `json:"body"`
}

// Get runs an obfuscated secure-pipe GET and returns the decoded JSON body.
func (c *Client) Get(ctx context.Context, path string, query map[string]string) ([]byte, error) {
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
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrBlocked
	}
	if resp.StatusCode >= 500 || resp.StatusCode == 444 {
		return nil, fmt.Errorf("%w: status %d", ErrUpstream, resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("content-type"), "text/html") {
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
	return io.ReadAll(zr)
}

func setHeaders(h http.Header) {
	h.Set("User-Agent", userAgent)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", "en-US,en;q=0.5")
	h.Set("Referer", "https://www.miruro.tv/")
	h.Set("Origin", "https://www.miruro.tv")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
}
