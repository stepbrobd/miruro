package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ysun.co/miruro/internal/play"
	"ysun.co/miruro/internal/upstream"
)

// the refusal budget is spent at exactly its count, so a stream refused that
// many times and quiet after is abandoned then rather than at the grace
func TestAbandonStalledAtExactlyTheBudget(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer cdn.Close()
	px, err := play.StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	tl := px.Tally()
	u := tl.Stream(upstream.Stream{URL: cdn.URL + "/ep.mp4", Kind: upstream.MP4}).URL
	for range 3 {
		if resp, err := http.Get(u); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	if n := tl.Refused(); n != 3 {
		t.Fatalf("refused %d, want the three bodies asked for", n)
	}

	wd := watchdog{grace: time.Hour, budget: 3, check: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pctx, stop := context.WithCancel(ctx)
	defer stop()
	if !<-wd.abandonStalled(pctx, tl, "Mp4", stop) {
		t.Error("a stream refused its whole budget was not abandoned before the grace")
	}
}
