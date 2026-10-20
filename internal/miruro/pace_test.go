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

// the site's request helper waits 100ms before its second attempt and doubles
// that to at most 400ms, and a retry-after of 5s or less in either form stands
// in for the backoff, while one naming a longer wait or none that parses ends
// the retries
func TestRetryWait(t *testing.T) {
	c := New()
	now := time.Date(2026, 10, 9, 14, 48, 50, 0, time.UTC)
	cases := []struct {
		n     int
		value string
		want  time.Duration
		ok    bool
	}{
		{1, "", 100 * time.Millisecond, true},
		{2, "", 200 * time.Millisecond, true},
		{3, "", 400 * time.Millisecond, true},
		{4, "", 400 * time.Millisecond, true},
		{1, "5", 5 * time.Second, true},
		{2, "0", 0, true},
		{1, "Fri, 09 Oct 2026 14:48:55 GMT", 5 * time.Second, true},
		{1, "6", 0, false},
		{1, "Fri, 09 Oct 2026 14:48:56 GMT", 0, false},
		{1, "2147483648", 0, false},
		{1, "soon", 0, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.value != "" {
			h.Set("Retry-After", tc.value)
		}
		got, ok := c.retryWait(tc.n, h, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("retryWait(%d, %q) = %s, %v, want %s, %v", tc.n, tc.value, got, ok, tc.want, tc.ok)
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

// unavailable answers the way cloudflare does when the backend behind it is
// down, naming the wait in retry-after when after is set
func unavailable(status int, after string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if after != "" {
			w.Header().Set("Retry-After", after)
		}
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(status)
		io.WriteString(w, "<!doctype html>")
	}
}

// failsOnce answers the first request with fail and every later one with a body
func failsOnce(fail http.HandlerFunc) http.HandlerFunc {
	var n atomic.Int64
	return func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			fail(w, r)
			return
		}
		io.WriteString(w, `{"ok":true}`)
	}
}

// the site's request helper tries a 502, a 503 and a network error again, three
// attempts in all, on every route but play, so a short outage costs this client
// a lookup or an episode list no sooner than it costs a browser
func TestOutage(t *testing.T) {
	ctx := context.Background()

	t.Run("a 502 is tried again and goes through", func(t *testing.T) {
		srv := mirror(t, failsOnce(unavailable(http.StatusBadGateway, "")))
		if _, err := client(srv).get(ctx, "/api/v1/anime"); err != nil {
			t.Fatal(err)
		}
		if got := srv.hits.Load(); got != 2 {
			t.Errorf("mirror served %d requests, want the 502 and the retry", got)
		}
	})

	t.Run("a 503 that persists fails after three attempts", func(t *testing.T) {
		srv := mirror(t, unavailable(http.StatusServiceUnavailable, ""))
		_, err := client(srv).get(ctx, "/api/v1/anime")
		if !errors.Is(err, upstream.ErrUnreachable) || !strings.HasSuffix(err.Error(), "status 503") {
			t.Errorf("err = %v, want the last 503", err)
		}
		if got := srv.hits.Load(); got != 3 {
			t.Errorf("mirror served %d requests, want the three the site sends", got)
		}
	})

	t.Run("every attempt waits its turn", func(t *testing.T) {
		srv := mirror(t, unavailable(http.StatusServiceUnavailable, ""))
		every := 50 * time.Millisecond
		c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client(), pace: upstream.Pacer{Burst: 1, Every: every}}
		start := time.Now()
		c.get(ctx, "/api/v1/anime")
		if took := time.Since(start); took < 2*every {
			t.Errorf("three attempts took %v, want each retry an interval after the last", took)
		}
	})

	// a host that handed over a connection is the one that answered, and the
	// site tries a network error again
	t.Run("a body that dies is tried again", func(t *testing.T) {
		srv := mirror(t, failsOnce(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1024")
			io.WriteString(w, "{\"data\":")
		}))
		if _, err := client(srv).get(ctx, "/api/v1/anime"); err != nil {
			t.Fatal(err)
		}
		if got := srv.hits.Load(); got != 2 {
			t.Errorf("mirror served %d requests, want the dead body and the retry", got)
		}
	})

	t.Run("a play request is not tried again", func(t *testing.T) {
		for path, want := range map[string]int64{
			playPath: 1,
			"/api/v1/anime/o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm/episodes": 3,
		} {
			srv := mirror(t, unavailable(http.StatusBadGateway, ""))
			if _, err := client(srv).get(ctx, path); !errors.Is(err, upstream.ErrUnreachable) {
				t.Errorf("%s: err = %v, want upstream.ErrUnreachable", path, err)
			}
			if got := srv.hits.Load(); got != want {
				t.Errorf("%s: mirror served %d requests, want %d", path, got, want)
			}
		}
	})

	// the backoff is an hour, so only the retry-after can let the retry through
	// before the deadline
	t.Run("a short retry-after stands in for the backoff", func(t *testing.T) {
		for _, after := range []string{"0", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)} {
			srv := mirror(t, failsOnce(unavailable(http.StatusServiceUnavailable, after)))
			c := client(srv)
			c.backoff = time.Hour
			short, cancel := context.WithTimeout(ctx, 10*time.Second)
			if _, err := c.get(short, "/api/v1/anime"); err != nil {
				t.Errorf("retry-after %q: %v", after, err)
			}
			cancel()
		}
	})

	for _, after := range []string{"6", "2147483648"} {
		t.Run("a retry-after of "+after+" ends the retries", func(t *testing.T) {
			srv := mirror(t, unavailable(http.StatusServiceUnavailable, after))
			_, err := client(srv).get(ctx, "/api/v1/anime")
			if !errors.Is(err, upstream.ErrUnreachable) || !strings.HasSuffix(err.Error(), "status 503") {
				t.Errorf("err = %v, want the 503", err)
			}
			if got := srv.hits.Load(); got != 1 {
				t.Errorf("mirror served %d requests, want only the first", got)
			}
		})
	}

	// a browser's fetch runs out of no time of its own, and trying a timeout
	// again would triple the wait the GotConn hook keeps to one
	t.Run("a timeout is not tried again", func(t *testing.T) {
		stall := make(chan struct{})
		defer close(stall)
		headers := mirror(t, func(w http.ResponseWriter, r *http.Request) { <-stall })
		body := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1024")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-stall
		})
		hc := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond}}
		for name, srv := range map[string]*counter{"headers": headers, "body": body} {
			c := &Client{Bases: []string{srv.URL}, HTTP: hc, Timeout: 200 * time.Millisecond}
			if _, err := c.get(ctx, "/api/v1/anime"); !errors.Is(err, upstream.ErrUnreachable) {
				t.Errorf("stalled %s: err = %v, want upstream.ErrUnreachable", name, err)
			}
			if got := srv.hits.Load(); got != 1 {
				t.Errorf("stalled %s: mirror served %d requests, want only the first", name, got)
			}
		}
	})

	// the backoff is an hour, so the run ends while the retry waits for it
	t.Run("a canceled run is not tried again", func(t *testing.T) {
		srv := mirror(t, unavailable(http.StatusServiceUnavailable, ""))
		c := client(srv)
		c.backoff = time.Hour
		canceled, cancel := context.WithCancel(ctx)
		defer cancel()
		time.AfterFunc(100*time.Millisecond, cancel)
		done := make(chan error, 1)
		go func() { _, err := c.get(canceled, "/api/v1/anime"); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want %v", err, context.Canceled)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the retry outlived its run")
		}
		if got := srv.hits.Load(); got != 1 {
			t.Errorf("mirror served %d requests, want only the first", got)
		}
	})
}
