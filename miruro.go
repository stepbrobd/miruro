// Package miruro is the vocabulary every backend speaks: the title, episode,
// stream, and subtitle records, the Backend interface an upstream implements,
// the merge of several backends into one catalog, and the heuristics that
// pick a stream and a subtitle track out of what they answer.
// The backends themselves live under backend, one package per upstream.
package miruro

import (
	"context"
	"errors"
	"net/http"
)

// UserAgent is the one browser identity shared by every backend, the quality
// probe, and the stream proxy, so a CDN sees a single client across a
// playlist and its segments
const UserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"

var (
	// ErrBlocked is fatal for the backend that raised it
	// its firewall rejected this client, and every further request would meet
	// the same rejection
	ErrBlocked = errors.New("cloudflare blocked request")
	// ErrUpstream is recoverable and drives provider fallback
	ErrUpstream = errors.New("upstream unreachable")
)

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
