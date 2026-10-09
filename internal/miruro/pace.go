package miruro

import (
	"net/http"
	"strconv"
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
