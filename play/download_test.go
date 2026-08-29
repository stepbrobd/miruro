package play

import (
	"context"
	"errors"
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

// sampleSegment synthesises one transport stream segment, with audio unless the test
// needs the silent case
func sampleSegment(t *testing.T, audio bool) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	seg := filepath.Join(t.TempDir(), "seg.ts")
	args := []string{"-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=1"}
	if audio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "aac")
	}
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-f", "mpegts", seg)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("cannot synthesise a segment: %v: %s", err, out)
	}
	return seg
}

// ffmpeg chooses its muxer from the output file extension, and the download
// writes to a .part file, so the format has to be named explicitly
// only driving the real binary catches a muxer refusal, no unit assertion does
func TestDownloadHLSWritesPlayableMP4(t *testing.T) {
	seg := sampleSegment(t, true)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".ts") {
			http.ServeFile(w, r, seg)
			return
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n%s/seg.ts\n#EXT-X-ENDLIST\n", base)
	})

	dir, name := t.TempDir(), "Show - E1"
	if _, err := Download(context.Background(), http.DefaultClient,
		miruro.Stream{URL: base + "/media.m3u8", Kind: miruro.HLS},
		nil, dir, name, "", nil); err != nil {
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

// ffmpeg keeps going when a demuxed master's audio rendition refuses to serve
// and exits zero on a silent episode, so the download has to refuse the result
// itself rather than keep a file nobody can watch
func TestDownloadRefusesASilentEpisode(t *testing.T) {
	seg := sampleSegment(t, false)
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "master.m3u8"):
			fmt.Fprintf(w, "#EXTM3U\n"+
				"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"ja\",DEFAULT=YES,URI=\"%s/audio.m3u8\"\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=64x64,AUDIO=\"a\"\n%s/video.m3u8\n", base, base)
		case strings.HasSuffix(r.URL.Path, "video.m3u8"):
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n%s/seg.ts\n#EXT-X-ENDLIST\n", base)
		case strings.HasSuffix(r.URL.Path, ".ts"):
			http.ServeFile(w, r, seg)
		default:
			http.Error(w, "denied", http.StatusForbidden)
		}
	})

	dir, name := t.TempDir(), "Show - E1"
	_, err := Download(context.Background(), http.DefaultClient,
		miruro.Stream{URL: base + "/master.m3u8", Kind: miruro.HLS},
		nil, dir, name, "", nil)
	if err == nil || !strings.Contains(err.Error(), "no audio") {
		t.Fatalf("err = %v, want the silent episode refused", err)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".mp4")); !os.IsNotExist(err) {
		t.Error("the silent file was kept, so a rerun would skip the episode forever")
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
		{File: srv.URL + "/good.vtt", Label: "English", Lang: "en"},
		{File: srv.URL + "/gone.vtt", Label: "Spanish", Lang: "es"},
	}
	missed, err := Download(context.Background(), http.DefaultClient,
		miruro.Stream{URL: srv.URL + "/video.mp4", Kind: miruro.MP4},
		subs, dir, name, "", nil)
	if err != nil {
		t.Fatalf("a missing sidecar must not fail the download: %v", err)
	}
	if missed != 1 {
		t.Errorf("missed = %d, want 1", missed)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".mp4")); err != nil {
		t.Errorf("video did not survive the sidecar failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".en.vtt")); err != nil {
		t.Errorf("the sidecar that resolved was not written: %v", err)
	}
}

// noNet fails the test on any request, proving a path never touches the network
type noNet struct{ t *testing.T }

func (n noNet) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("request for %s on a path that must not touch the network", r.URL)
	return nil, errors.New("no network")
}

// an existing dest is always a complete download, so a rerun must report it
// finished and fetch nothing
func TestDownloadSkipsExistingEpisode(t *testing.T) {
	dir, name := t.TempDir(), "Show - E1"
	body := []byte("finished episode")
	if err := os.WriteFile(filepath.Join(dir, name+".mp4"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	hc := &http.Client{Transport: noNet{t}}
	var done, total int64 = -1, -1
	missed, err := Download(context.Background(), hc,
		miruro.Stream{URL: "http://unused/video.mp4", Kind: miruro.MP4},
		[]miruro.Subtitle{{File: "http://unused/sub.vtt", Label: "English"}},
		dir, name, "", func(d, tot int64) { done, total = d, tot })
	if err != nil {
		t.Fatalf("an existing episode failed the rerun: %v", err)
	}
	if missed != 0 {
		t.Errorf("missed = %d, want 0", missed)
	}
	want := int64(len(body))
	if done != want || total != want {
		t.Errorf("progress reported %d of %d, want %d of %d", done, total, want, want)
	}
}

// a sidecar named for its language is what a player auto-loads next to the
// video, and two tracks naming one language must not overwrite each other
func TestSidecarNames(t *testing.T) {
	seen := map[string]int{}
	cases := []struct {
		sub  miruro.Subtitle
		want string
	}{
		{miruro.Subtitle{File: "http://x/a.vtt", Label: "English", Lang: "en"}, ".en.vtt"},
		{miruro.Subtitle{File: "http://x/b.srt", Label: "English", Lang: "en"}, ".en.1.srt"},
		{miruro.Subtitle{File: "http://x/c.ass?token=1", Label: "Signs"}, ".Signs.ass"},
		{miruro.Subtitle{File: "http://x/d"}, ".sub.vtt"},
		{miruro.Subtitle{File: "http://x/e.exe", Lang: "../../etc"}, ".-..-etc.vtt"},
	}
	for _, c := range cases {
		if got := sidecar(c.sub, seen); got != c.want {
			t.Errorf("sidecar(%+v) = %q, want %q", c.sub, got, c.want)
		}
	}
}
