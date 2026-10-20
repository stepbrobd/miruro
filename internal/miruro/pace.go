package miruro

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// the catalog sits behind a cloudflare rate limit, which on 2026-10-09 answered
// the 33rd of a run of sequential lookups sent to one mirror over 4.2 seconds
// with 429, error code 1015 and retry-after 10
// cloudflare counts over an approximated sliding window of ten seconds, which
// a burst followed by a steady rate fills to at most the burst plus ten
// seconds of the rate, 24 for these
// the burst lets the lookup, the episode list, the provider table and the play
// request of an episode start leave back to back, where the segment pacer in
// play banks none
const (
	paceBurst = 4
	paceEvery = 500 * time.Millisecond
)

const (
	// maxHold is the longest wait a 429 can name and have a request sit it out,
	// past which the 429 is the request's failure
	maxHold = 30 * time.Second
	// holds is how many 429s one request sits out before failing
	holds = 2
)

// the site's request helper, N in the shared fa6-*.js chunk on 2026-10-09,
// tries a 502, a 503 and a network error again on every route but play, and
// this client tries an outage the same way
const (
	// attempts bounds the tries one request makes through an outage, the first
	// included
	attempts = 3
	// retryBackoff is the wait before the first retry, which doubles to at most
	// four times it, though three attempts only ever wait the first two
	retryBackoff = 100 * time.Millisecond
	// maxRetryAfter is the longest wait an outage can name and still be tried
	// again
	maxRetryAfter = 5 * time.Second
)

// outage is a failure the site's request helper tries again, a 502, a 503 or a
// network error once a connection was handed over, keeping the headers of the
// answer when one came
// it reads as the error it carries, so the error a request gives up with is
// the one its last attempt returned
type outage struct {
	error
	header http.Header
}

func (o outage) Unwrap() error { return o.error }

// retryWait is the wait before retry n, counting from one, after an outage
// whose answer carried h, and whether to retry at all
// a retry-after of maxRetryAfter or less stands in for the backoff, and one
// naming a longer wait, or none that parses, ends the retries
func (c *Client) retryWait(n int, h http.Header, now time.Time) (time.Duration, bool) {
	if h.Get("Retry-After") == "" {
		return c.backoff << min(n-1, 2), true
	}
	if d, ok := retryAfter(h, now); ok && d <= maxRetryAfter {
		return d, true
	}
	return 0, false
}

// retryable reports whether the site's request helper tries the route ref
// names again, which it does for every route but an episode's play
func retryable(ref string) bool {
	path, _, _ := strings.Cut(ref, "?")
	return !strings.HasSuffix(path, "/play")
}

// sleep waits d or until ctx ends and reports the context's error either way,
// since a fired timer and a done context are both ready and select picks
// between them at random, and a canceled run must not start another attempt
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
	return ctx.Err()
}

// limited is a 429, carrying the wait its retry-after named
type limited time.Duration

func (l limited) Error() string {
	return "rate limited, retry after " + time.Duration(l).String()
}

// retryAfter reads a retry-after header in either form rfc 9110 gives it, as a
// wait from now
// a delay of 2^31 seconds or more, or a value in neither form, names no wait
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := h.Get("Retry-After")
	if s, err := strconv.ParseUint(v, 10, 31); err == nil {
		return time.Duration(s) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}
