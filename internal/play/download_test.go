package play

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"ysun.co/miruro/internal/upstream"
)

// sampleSegment synthesizes one transport stream segment, with audio unless the test
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
		t.Skipf("cannot synthesize a segment: %v: %s", err, out)
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
		upstream.Stream{URL: base + "/media.m3u8", Kind: upstream.HLS},
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

// a fragmented mp4 stream names an init segment every segment depends on, which
// whole-file caching cannot reproduce, so the download falls back to ffmpeg
// through the proxy and still lands whole, the init segment relayed untouched
// and the fragments passed by the decoy strip, which looks for transport
// stream packets alone
func TestDownloadFallsBackForFragmentedMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	src := t.TempDir()
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "libx264", "-preset", "ultrafast",
		"-g", "10", "-keyint_min", "10", "-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,n_forced*1)", "-c:a", "aac",
		"-f", "hls", "-hls_time", "1", "-hls_list_size", "0",
		"-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(src, "seg%d.m4s"),
		filepath.Join(src, "media.m3u8"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot synthesize a fragmented mp4 stream: %v: %s", err, out)
	}
	cdn := httptest.NewServer(http.FileServer(http.Dir(src)))
	defer cdn.Close()
	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	dir, cache := t.TempDir(), filepath.Join(t.TempDir(), "cache")
	tl := px.Tally()
	s := tl.Stream(upstream.Stream{URL: cdn.URL + "/media.m3u8", Kind: upstream.HLS})
	if _, err := Download(context.Background(), http.DefaultClient, s, nil, dir, "Show - E1", cache, nil); err != nil {
		t.Fatalf("download: %v", err)
	}
	dest := filepath.Join(dir, "Show - E1.mp4")
	if !streams(t, dest, "v") || !streams(t, dest, "a") {
		t.Error("the episode lacks picture or sound")
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("a stream the cache refuses left a cache directory: %v", err)
	}
	if tl.Served() == 0 || tl.Refused() != 0 {
		t.Errorf("served %d refused %d, want the fragments relayed and none refused", tl.Served(), tl.Refused())
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
		upstream.Stream{URL: base + "/master.m3u8", Kind: upstream.HLS},
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
			w.Write(mp4Bytes)
		case strings.HasSuffix(r.URL.Path, "good.vtt"):
			w.Write([]byte("WEBVTT\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir, name := t.TempDir(), "Show - E1"
	subs := []upstream.Subtitle{
		{File: srv.URL + "/good.vtt", Label: "English", Lang: "en"},
		{File: srv.URL + "/gone.vtt", Label: "Spanish", Lang: "es"},
	}
	missed, err := Download(context.Background(), http.DefaultClient,
		upstream.Stream{URL: srv.URL + "/video.mp4", Kind: upstream.MP4},
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

// hop's older encodes name WebVTT files .srt, and the api calls them srt too,
// so what the file holds decides its extension, and a real SubRip file keeps
// its own
func TestDownloadNamesASidecarByContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "video.mp4"):
			w.Write(mp4Bytes)
		case strings.HasSuffix(r.URL.Path, "vtt.srt"):
			w.Write([]byte("\xef\xbb\xbfWEBVTT\n\n00:00:00.000 --> 00:00:01.000\nhi\n"))
		case strings.HasSuffix(r.URL.Path, "real.srt"):
			w.Write([]byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n"))
		}
	}))
	defer srv.Close()

	dir, name := t.TempDir(), "Show - E1"
	subs := []upstream.Subtitle{
		{File: srv.URL + "/1_en.vtt.srt", Label: "English", Lang: "en"},
		{File: srv.URL + "/1_pt.real.srt", Label: "Portugues", Lang: "pt"},
	}
	if _, err := Download(context.Background(), http.DefaultClient,
		upstream.Stream{URL: srv.URL + "/video.mp4", Kind: upstream.MP4}, subs, dir, name, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{name + ".en.vtt", name + ".pt.srt"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s is missing: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, name+".en.srt")); !os.IsNotExist(err) {
		t.Errorf("the WebVTT file also stayed under .srt: %v", err)
	}
}

// mp4Bytes opens with the file type box every mp4 leads with and carries
// nothing a player could show, which is all the container check reads
var mp4Bytes = []byte("\x00\x00\x00\x18ftypisom" + "not really an mp4, but bytes on disk")

// a host answering a page where the video should be used to be saved as the
// episode and skipped as finished on every rerun
func TestDownloadRefusesAnMP4ThatIsNotOne(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>not found</body></html>")
	}))
	defer srv.Close()

	dir, name := t.TempDir(), "Show - E1"
	_, err := Download(context.Background(), http.DefaultClient,
		upstream.Stream{URL: srv.URL + "/video.mp4", Kind: upstream.MP4}, nil, dir, name, "", nil)
	if err == nil || !strings.Contains(err.Error(), "not an mp4") {
		t.Fatalf("err = %v, want the page refused", err)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".mp4")); !os.IsNotExist(err) {
		t.Error("the page was kept as the episode")
	}
	// the refusal is retried like a dropped connection, since that is what it
	// can also be
	if n := hits.Load(); n != attempts {
		t.Errorf("fetched %d times, want %d", n, attempts)
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
		upstream.Stream{URL: "http://unused/video.mp4", Kind: upstream.MP4},
		[]upstream.Subtitle{{File: "http://unused/sub.vtt", Label: "English"}},
		dir, name, "", func(d, tot int64, _ float64) { done, total = d, tot })
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

// Saved is the test Download skips an episode by, so the two have to agree on
// the file an episode lands in and on an empty one not being an episode
func TestSaved(t *testing.T) {
	dir, name := t.TempDir(), "Show: One - E1"
	if Saved(dir, name) {
		t.Error("an episode never written reads as saved")
	}
	// the colon is one safeName replaces, so the check has to look for the file
	// the download wrote rather than the name it was asked for
	dest := filepath.Join(dir, "Show- One - E1.mp4")
	if err := os.WriteFile(dest, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if Saved(dir, name) {
		t.Error("an empty file reads as a saved episode")
	}
	if err := os.WriteFile(dest, []byte("finished episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Saved(dir, name) {
		t.Error("the episode on disk does not read as saved")
	}
}

// a sidecar named for its language is what a player auto-loads next to the
// video, and two tracks naming one language must not overwrite each other
func TestSidecarNames(t *testing.T) {
	seen := map[string]int{}
	cases := []struct {
		sub  upstream.Subtitle
		want string
	}{
		{upstream.Subtitle{File: "http://x/a.vtt", Label: "English", Lang: "en"}, ".en.vtt"},
		{upstream.Subtitle{File: "http://x/b.srt", Label: "English", Lang: "en"}, ".en.1.srt"},
		{upstream.Subtitle{File: "http://x/c.ass?token=1", Label: "Signs"}, ".Signs.ass"},
		{upstream.Subtitle{File: "http://x/d"}, ".sub.vtt"},
		{upstream.Subtitle{File: "http://x/e.exe", Lang: "../../etc"}, ".-..-etc.vtt"},
	}
	for _, c := range cases {
		if got := sidecar(c.sub, seen); got != c.want {
			t.Errorf("sidecar(%+v) = %q, want %q", c.sub, got, c.want)
		}
	}
}

// a dead audio rendition remuxes into a file ffmpeg exits zero on, and the only
// sign is an audio track that ends well before the picture
// the two thresholds are what separate that from the fractions of a second real
// renditions drift by, so both have to be driven
func TestAudibleCatchesAnAudioTrackThatEndsEarly(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	dir := t.TempDir()

	// make one file per case, the audio as long as the case wants it
	build := func(t *testing.T, name string, video, audio float64) string {
		t.Helper()
		dest := filepath.Join(dir, name)
		cmd := exec.Command("ffmpeg", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=64x64:rate=10:duration=%g", video),
			"-f", "lavfi", "-i", fmt.Sprintf("sine=duration=%g", audio),
			"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac",
			"-map", "0:v", "-map", "1:a", dest)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("cannot synthesize a clip: %v: %s", err, out)
		}
		return dest
	}

	for _, tc := range []struct {
		name         string
		video, audio float64
		wantRefused  bool
	}{
		// the drift a real rendition has, well inside both thresholds
		{"audio a moment short", 20, 19.6, false},
		// longer than five seconds but under a tenth of the video
		{"audio short but proportional", 120, 114, false},
		// past a tenth of the video but under the five second floor, which is
		// what keeps a short clip's ordinary drift from reading as a dead track
		{"audio short on a short clip", 20, 16, false},
		// a dead rendition: past both thresholds
		{"audio minutes short", 30, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := audible(context.Background(), build(t, tc.name+".mp4", tc.video, tc.audio))
			if tc.wantRefused && err == nil {
				t.Error("an episode whose audio dies early was accepted as whole")
			}
			if !tc.wantRefused && err != nil {
				t.Errorf("ordinary drift was refused: %v", err)
			}
		})
	}
}

// the judgment over ffprobe's report, with the rows a real file is hard to
// make: an audio stream reported with no duration is heard and not measured,
// and each threshold of the gap holds on its own
func TestSilence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		streams []probed
		quiet   bool
	}{
		{"no audio at all", []probed{{"video", "1420.0"}}, true},
		{"audio a dead rendition cut short", []probed{{"video", "1420.0"}, {"audio", "300.0"}}, true},
		{"audio with no duration beside timed video", []probed{{"video", "1420.0"}, {"audio", "N/A"}}, false},
		{"audio with an empty duration", []probed{{"video", "1420.0"}, {"audio", ""}}, false},
		{"a drift under five seconds", []probed{{"video", "1420.0"}, {"audio", "1416.0"}}, false},
		{"a gap over five seconds but under a tenth of a long video", []probed{{"video", "1420.0"}, {"audio", "1300.0"}}, false},
		{"a gap over a tenth but under five seconds of a short clip", []probed{{"video", "30.0"}, {"audio", "26.0"}}, false},
		{"a gap over both", []probed{{"video", "30.0"}, {"audio", "20.0"}}, true},
		{"the longest of several audio streams counts", []probed{{"video", "1420.0"}, {"audio", "10.0"}, {"audio", "1419.0"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := silence(tc.streams); (err != nil) != tc.quiet {
				t.Errorf("silence = %v, want quiet %v", err, tc.quiet)
			}
		})
	}
}
