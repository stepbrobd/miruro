package play

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

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
