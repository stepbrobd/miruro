package play

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ysun.co/miruro/internal/upstream"
)

// realPause is the production backoff, captured before TestMain replaces it
var realPause = pause

// the suite drives the retry and segment paths on purpose, so it must not sleep
// through the real backoff or the pacing between segment requests
// TestRetrySchedule and the pacer tests in upstream cover what is skipped here
func TestMain(m *testing.M) {
	pause = func(int, error) time.Duration { return 0 }
	paced.Every = 0
	os.Exit(m.Run())
}

// a 429 waits out the window the CDN counts over, where the doubling backoff
// alone would spend every attempt inside it
func TestRetrySchedule(t *testing.T) {
	for _, tc := range []struct {
		n    int
		err  error
		want time.Duration
	}{
		{1, status(http.StatusBadGateway), time.Second},
		{2, status(http.StatusBadGateway), 2 * time.Second},
		{1, fmt.Errorf("segment 3: %w", status(http.StatusTooManyRequests)), throttled},
		{2, status(http.StatusTooManyRequests), throttled},
	} {
		if got := realPause(tc.n, tc.err); got != tc.want {
			t.Errorf("pause(%d, %v) = %v, want %v", tc.n, tc.err, got, tc.want)
		}
	}
}

func TestTransient(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{status(http.StatusBadGateway), true},
		{fmt.Errorf("segment 3: %w", status(http.StatusServiceUnavailable)), true},
		{status(http.StatusTooManyRequests), true},
		{status(http.StatusRequestTimeout), true},
		{status(http.StatusNotFound), false},
		{status(http.StatusForbidden), false},
		{context.Canceled, false},
		// a timeout is an upstream being slow, and a run that ran out of time is
		// told apart by retry asking its own context
		{context.DeadlineExceeded, true},
		{fmt.Errorf("playlist: %w of 16 bytes", errTooLarge), false},
		{&fs.PathError{Op: "create", Path: "/x", Err: errors.New("read-only")}, false},
		{errors.New("unexpected EOF"), true},
		{errors.New("segment is not a transport stream"), true},
	}
	for _, c := range cases {
		if got := transient(c.err); got != c.want {
			t.Errorf("transient(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestRetryStopsEarly(t *testing.T) {
	calls := 0
	err := retry(context.Background(), func() error {
		calls++
		return status(http.StatusNotFound)
	})
	if calls != 1 {
		t.Errorf("a permanent status was tried %d times, want 1", calls)
	}
	if !errors.Is(err, status(http.StatusNotFound)) {
		t.Errorf("err = %v, want the 404 through", err)
	}

	calls = 0
	if err := retry(context.Background(), func() error {
		calls++
		return status(http.StatusBadGateway)
	}); err == nil {
		t.Error("a run out of attempts must report the last failure")
	}
	if calls != attempts {
		t.Errorf("a transient status was tried %d times, want %d", calls, attempts)
	}
}

func TestRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retry(ctx, func() error {
		calls++
		cancel()
		return status(http.StatusBadGateway)
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("a canceled retry ran %d attempts, want 1", calls)
	}
}

func TestRetryHonorsADeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	calls := 0
	err := retry(ctx, func() error {
		calls++
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if calls != 1 {
		t.Errorf("a run out of time ran %d attempts, want 1", calls)
	}
}

// net/http reports a header timeout as a deadline, which read as a run out of
// time, so one slow answer failed the episode without a second attempt
func TestGrabRetriesAHeaderTimeout(t *testing.T) {
	var hits atomic.Int64
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Write([]byte("episode bytes"))
	}))
	defer srv.Close()

	hc := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}}
	dest := filepath.Join(t.TempDir(), "ep.mp4")
	if err := grab(context.Background(), hc, srv.URL+"/video.mp4", dest, 0, nil, nil); err != nil {
		t.Fatalf("a download that answered on its second attempt failed: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("server saw %d requests, want 2", n)
	}
}

// a provider that 502s once and then answers used to lose the whole episode
func TestGrabRetriesTransientStatus(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
			return
		}
		w.Write([]byte("episode bytes"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "ep.mp4")
	if err := grab(context.Background(), srv.Client(), srv.URL+"/video.mp4", dest, 0, nil, nil); err != nil {
		t.Fatalf("a recoverable download failed: %v", err)
	}
	if body, err := os.ReadFile(dest); err != nil || string(body) != "episode bytes" {
		t.Errorf("dest holds %q (%v), want the whole body", body, err)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("a retried download left its part file behind")
	}
}

// the proxy answers a body over its cap with a 502, which otherwise means a
// transport failure worth another attempt, so through the proxy errTooLarge
// never fired and a segment past the cap was fetched three times over
func TestOverCapRefusalThroughTheProxyIsFinal(t *testing.T) {
	var hits atomic.Int64
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasSuffix(r.URL.Path, ".vtt") {
			w.Header().Set("Content-Length", fmt.Sprint(maxOpaqueBody+1))
			w.WriteHeader(http.StatusOK)
			return
		}
		io.CopyN(w, zeros{}, maxPlaylistBody+1)
	}))
	defer cdn.Close()
	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	// a segment past its cap is a quarter of a GB through a proxy that buffers
	// it, so the segment reader is handed the refusal the proxy answers it with
	segments := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		refuse(w, target{URL: r.URL.String(), Kind: segment}, http.StatusBadGateway, fmt.Errorf("upstream %w", errTooLarge))
	}))
	defer segments.Close()

	ctx, dir := context.Background(), t.TempDir()
	for _, tc := range []struct {
		name  string
		fetch func() error
	}{
		{"playlist", func() error {
			_, err := fetchText(ctx, px.Client(), px.URL(upstream.Stream{URL: cdn.URL + "/media.m3u8", Kind: upstream.HLS}))
			return err
		}},
		{"sidecar", func() error {
			return grab(ctx, px.Client(), px.Opaque(cdn.URL+"/en.vtt", ""), filepath.Join(dir, "en.vtt"), maxOpaqueBody, nil, nil)
		}},
		{"segment", func() error {
			_, err := fetchSegment(ctx, http.DefaultClient, segments.URL+"/seg.ts", filepath.Join(dir, segName(0)), true, false)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits.Store(0)
			if err := tc.fetch(); !errors.Is(err, errTooLarge) {
				t.Errorf("err = %v, want %v", err, errTooLarge)
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("fetched %d times, want once", n)
			}
		})
	}
}

// a body cut short is what a dropped connection looks like, and the next attempt
// usually completes
func TestFetchSegmentRetriesShortBody(t *testing.T) {
	whole := tsBlob(4)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Length", fmt.Sprint(len(whole)))
		if hits == 1 {
			w.Write(whole[:len(whole)/2])
			return
		}
		w.Write(whole)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "00000.ts")
	n, err := fetchSegment(context.Background(), srv.Client(), srv.URL+"/seg.ts", dest, true, false)
	if err != nil {
		t.Fatalf("a short segment was not retried: %v", err)
	}
	if n != int64(len(whole)) {
		t.Errorf("wrote %d bytes, want %d", n, len(whole))
	}
	if hits != 2 {
		t.Errorf("server saw %d requests, want 2", hits)
	}
}
