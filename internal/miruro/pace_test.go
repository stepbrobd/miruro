package miruro

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ysun.co/miruro/internal/upstream"
)

// cloudflare lets about 30 requests through in ten seconds, and the most a
// burst and a rate can put in one of its windows has to stay under that
func TestNewPacesUnderTheLimit(t *testing.T) {
	c := New()
	if c.pace.Every <= 0 {
		t.Fatal("the api client sends its requests unpaced")
	}
	if worst := c.pace.Burst + int(10*time.Second/c.pace.Every); worst > 24 {
		t.Errorf("a ten second window can hold %d requests, want at most 24", worst)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 9, 14, 48, 50, 0, time.UTC)
	cases := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"10", 10 * time.Second, true},
		{"0", 0, true},
		{"Fri, 09 Oct 2026 14:49:00 GMT", 10 * time.Second, true},
		{"Fri, 09 Oct 2026 14:48:00 GMT", 0, true},
		{"", 0, false},
		{"-1", 0, false},
		{"+5", 0, false},
		{"2147483648", 0, false},
		{"soon", 0, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.value != "" {
			h.Set("Retry-After", tc.value)
		}
		got, ok := retryAfter(h, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("retryAfter(%q) = %s, %v, want %s, %v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

// rateLimit answers the way cloudflare's rate limit does, naming the wait in
// retry-after when after is set
func rateLimit(after string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if after != "" {
			w.Header().Set("Retry-After", after)
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "error code: 1015")
	}
}

// a 429 is sat out on the mirror that sent it for the wait it named, at most
// holds times, and one naming a long wait or none fails at once
func TestRateLimit(t *testing.T) {
	ctx := context.Background()

	t.Run("a short wait is sat out and the request goes through", func(t *testing.T) {
		var n atomic.Int64
		srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) == 1 {
				rateLimit("0")(w, r)
				return
			}
			io.WriteString(w, `{"ok":true}`)
		})
		if _, err := client(srv).get(ctx, "/api/v1/anime"); err != nil {
			t.Fatal(err)
		}
		if got := srv.hits.Load(); got != 2 {
			t.Errorf("mirror served %d requests, want the 429 and the retry", got)
		}
	})

	t.Run("a limit that persists fails after its holds", func(t *testing.T) {
		srv := mirror(t, rateLimit("0"))
		_, err := client(srv).get(ctx, "/api/v1/anime")
		if !errors.Is(err, upstream.ErrUnreachable) || !strings.Contains(err.Error(), "status 429: rate limited, retry after 0s") {
			t.Errorf("err = %v, want a recoverable 429 naming its wait", err)
		}
		if got := srv.hits.Load(); got != holds+1 {
			t.Errorf("mirror served %d requests, want the first and %d holds", got, holds)
		}
	})

	// the request that gives up is not the only one the limit refuses, so the
	// wait it was told holds the rest of the client
	t.Run("the last 429 still holds the requests behind it", func(t *testing.T) {
		var n atomic.Int64
		srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			after := "0"
			if n.Add(1) > holds {
				after = "1"
			}
			rateLimit(after)(w, r)
		})
		c := client(srv)
		if _, err := c.get(ctx, "/api/v1/anime"); err == nil {
			t.Fatal("a limit that persists went through")
		}
		start := time.Now()
		short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if err := c.pace.Wait(short); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the next turn came after %v with %v, want it held for the second named", time.Since(start), err)
		}
	})

	for _, tc := range []struct{ name, after string }{
		{"a 429 naming no wait fails at once", ""},
		{"a 429 naming a wait past maxHold fails at once", "60"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mirror(t, rateLimit(tc.after))
			_, err := client(srv).get(ctx, "/api/v1/anime")
			if !errors.Is(err, upstream.ErrUnreachable) || !strings.Contains(err.Error(), "status 429") {
				t.Errorf("err = %v, want a recoverable 429", err)
			}
			if got := srv.hits.Load(); got != 1 {
				t.Errorf("mirror served %d requests, want only the first", got)
			}
		})
	}

	// cloudflare counts each mirror apart, so the next one would take the load
	// the 429 asked to shed
	t.Run("the walk does not move on a 429", func(t *testing.T) {
		a, b := mirror(t, rateLimit("")), mirror(t, serves(`{"ok":true}`))
		c := &Client{Bases: []string{a.URL, b.URL}, HTTP: a.Client()}
		if _, err := c.get(ctx, "/api/v1/anime"); !errors.Is(err, upstream.ErrUnreachable) {
			t.Fatalf("err = %v, want upstream.ErrUnreachable", err)
		}
		if got := b.hits.Load(); got != 0 {
			t.Errorf("the second mirror served %d requests, want none", got)
		}
	})
}
