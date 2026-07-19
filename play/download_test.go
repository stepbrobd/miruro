package play

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ysun.co/miruro"
)

// ffmpeg chooses its muxer from the output file extension, and the download
// writes to a .part file, so the format has to be named explicitly
// only driving the real binary catches a muxer refusal, no unit assertion does
func TestDownloadHLSWritesPlayableMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	seg := filepath.Join(t.TempDir(), "seg.ts")
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-f", "mpegts", seg)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot synthesise a segment: %v: %s", err, out)
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".ts") {
			http.ServeFile(w, r, seg)
			return
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n%s/seg.ts\n#EXT-X-ENDLIST\n", srv.URL)
	}))
	defer srv.Close()

	dir, name := t.TempDir(), "Show - E1"
	if _, err := Download(context.Background(), http.DefaultClient,
		miruro.Stream{URL: srv.URL + "/media.m3u8", Kind: miruro.HLS},
		nil, dir, name, nil); err != nil {
		t.Fatalf("download: %v", err)
	}

	dest := filepath.Join(dir, name+".mp4")
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("no output file: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("output file is empty")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Error("the .part file was left behind")
	}
	if out, err := exec.Command("ffmpeg", "-v", "error", "-i", dest, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("output is not a playable mp4: %v: %s", err, out)
	}
}

func TestSafeNameStaysOneComponent(t *testing.T) {
	for _, in := range []string{
		"../../../home/ysun/.bashrc",
		"..",
		"a/b\\c",
		"Fate/stay night - E1",
		"Re:ZERO - E3",
		"\x00\x01evil",
	} {
		got := safeName(in)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("safeName(%q) = %q still holds a separator", in, got)
		}
		if dir := filepath.Dir(filepath.Join("/dl", got)); dir != "/dl" {
			t.Errorf("safeName(%q) = %q escapes the dir (parent %q)", in, got, dir)
		}
	}
}

func TestSafeNameDefaultsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "  ..  ", "."} {
		if got := safeName(in); got != "untitled" {
			t.Errorf("safeName(%q) = %q, want untitled", in, got)
		}
	}
}

func TestSafeNameKeepsPlainTitles(t *testing.T) {
	if got := safeName("Frieren - E5"); got != "Frieren - E5" {
		t.Errorf("safeName mangled a plain title: %q", got)
	}
}

// a sidecar that 404s must not discard a video already on disk, but the loss
// has to be counted so the caller can report it
func TestDownloadCountsMissingSidecars(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "video.mp4"):
			w.Write([]byte("not really an mp4, but bytes on disk"))
		case strings.HasSuffix(r.URL.Path, "good.vtt"):
			w.Write([]byte("WEBVTT\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir, name := t.TempDir(), "Show - E1"
	subs := []miruro.Subtitle{
		{File: srv.URL + "/good.vtt", Label: "English"},
		{File: srv.URL + "/gone.vtt", Label: "Spanish"},
	}
	missed, err := Download(context.Background(), http.DefaultClient,
		miruro.Stream{URL: srv.URL + "/video.mp4", Kind: miruro.MP4},
		subs, dir, name, nil)
	if err != nil {
		t.Fatalf("a missing sidecar must not fail the download: %v", err)
	}
	if missed != 1 {
		t.Errorf("missed = %d, want 1", missed)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".mp4")); err != nil {
		t.Errorf("video did not survive the sidecar failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".0.English.vtt")); err != nil {
		t.Errorf("the sidecar that resolved was not written: %v", err)
	}
}
