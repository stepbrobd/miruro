package play

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/upstream"
)

// a sidecar renamed by what it holds, or one needing no rename, is no reason
// to say it was kept under its upstream name
func TestDownloadSaysNothingOfASidecarItNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/video.mp4":
			w.Write(mp4Bytes)
		case "/1_en.srt":
			w.Write([]byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nhi\n"))
		case "/1_pt.srt":
			w.Write([]byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n"))
		}
	}))
	defer srv.Close()

	var said bytes.Buffer
	log.SetOutput(&said)
	defer log.SetOutput(os.Stderr)
	subs := []upstream.Subtitle{
		{File: srv.URL + "/1_en.srt", Label: "English", Lang: "en"},
		{File: srv.URL + "/1_pt.srt", Label: "Portugues", Lang: "pt"},
	}
	if _, err := Download(context.Background(), http.DefaultClient,
		upstream.Stream{URL: srv.URL + "/video.mp4", Kind: upstream.MP4}, subs, t.TempDir(), "Show - E1", "", nil); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(said.Bytes(), []byte("kept under its upstream name")) {
		t.Errorf("a sidecar named without trouble was warned about:\n%s", said.String())
	}
}

// a chunked body of any size was saved as a sidecar, an endless one until the
// disk filled, and the cap guards against a hostile upstream, so a body past
// it is not fetched again
// through the proxy the body reaches the reader one byte past the cap, which
// is what tells it from one cut short and worth another attempt
func TestSidecarsRefusesAnOversizedBody(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.CopyN(w, zeros{}, maxOpaqueBody+1)
	}))
	defer srv.Close()
	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	dir := t.TempDir()
	subs := px.Subtitles([]upstream.Subtitle{{File: srv.URL + "/en.vtt", Lang: "en"}}, "")
	missed, err := Sidecars(context.Background(), px.Client(), subs, dir, "Show - E1")
	if err != nil || missed != 1 {
		t.Fatalf("Sidecars = %d, %v, want the one sidecar missed", missed, err)
	}
	for _, name := range []string{"Show - E1.en.vtt", "Show - E1.en.vtt.part"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was left on disk", name)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("fetched %d times, want once", n)
	}
}

// a sidecar body that stalls used to hold its download worker for good
func TestSidecarsGivesUpOnAStalledBody(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "WEBVTT\n")
		w.(http.Flusher).Flush()
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(stall)

	defer func(was time.Duration) { sidecarTimeout = was }(sidecarTimeout)
	sidecarTimeout = 100 * time.Millisecond

	done := make(chan struct{})
	var (
		missed int
		err    error
	)
	go func() {
		defer close(done)
		missed, err = Sidecars(context.Background(), http.DefaultClient,
			[]upstream.Subtitle{{File: srv.URL + "/en.vtt", Lang: "en"}}, t.TempDir(), "Show - E1")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled sidecar held the download")
	}
	// a sidecar out of time is a missing one, not a canceled run
	if err != nil || missed != 1 {
		t.Errorf("Sidecars = %d, %v, want the one sidecar missed", missed, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("the sidecar's own deadline was reported as the run's")
	}
}
