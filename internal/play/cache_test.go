package play

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// zeros streams an endless zero body for over-cap tests
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { return len(p), nil }

// hlsFixture synthesizes a short multi-segment stream and serves it
// counting requests is what lets a test prove a resumed run refetched only what
// it was missing
type hlsFixture struct {
	srv  *httptest.Server
	dir  string
	mu   sync.Mutex
	hits map[string]int
}

func (f *hlsFixture) hit(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[name]++
}

func (f *hlsFixture) counts() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.hits))
	maps.Copy(out, f.hits)
	return out
}

func newHLSFixture(t *testing.T) *hlsFixture {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	// segments can only split on a keyframe, so force one per second or the
	// fixture collapses to a single segment and proves nothing about resume
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=3",
		"-c:v", "libx264", "-preset", "ultrafast",
		"-g", "10", "-keyint_min", "10", "-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,n_forced*1)",
		"-f", "hls", "-hls_time", "1", "-hls_list_size", "0",
		"-hls_segment_filename", filepath.Join(dir, "seg%d.ts"),
		filepath.Join(dir, "media.m3u8"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot synthesize an hls stream: %v: %s", err, out)
	}

	f := &hlsFixture{dir: dir, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hit(filepath.Base(r.URL.Path))
		http.ServeFile(w, r, filepath.Join(dir, filepath.Base(r.URL.Path)))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *hlsFixture) url() string { return f.srv.URL + "/media.m3u8" }

// newAudioHLSFixture synthesizes a stream whose sound comes in a rendition of
// its own, video and audio segments apart under one master, the shape hop
// serves
func newAudioHLSFixture(t *testing.T) *hlsFixture {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	gen := exec.Command("ffmpeg", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=10:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-preset", "ultrafast",
		"-g", "10", "-keyint_min", "10", "-sc_threshold", "0",
		"-force_key_frames", "expr:gte(t,n_forced*1)",
		"-c:a", "aac",
		"-f", "hls", "-hls_time", "1", "-hls_list_size", "0",
		"-var_stream_map", "v:0,agroup:aud a:0,agroup:aud,default:yes",
		"-master_pl_name", "master.m3u8",
		"-hls_segment_filename", filepath.Join(dir, "seg_%v_%d.ts"),
		filepath.Join(dir, "media_%v.m3u8"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot synthesize an hls stream with an audio rendition: %v: %s", err, out)
	}
	f := &hlsFixture{dir: dir, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hit(filepath.Base(r.URL.Path))
		http.ServeFile(w, r, filepath.Join(dir, filepath.Base(r.URL.Path)))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// fetched counts the names starting with prefix requested since before
func (f *hlsFixture) fetched(prefix string, before map[string]int) int {
	n := 0
	for name, c := range f.counts() {
		if strings.HasPrefix(name, prefix) && c > before[name] {
			n++
		}
	}
	return n
}

// a title starting with a dash under the relative default directory reads as
// an ffmpeg option, and every hls download of it used to fail at the remux
func TestRunFFmpegNamesTheOutputAbsolutely(t *testing.T) {
	f := newHLSFixture(t)
	t.Chdir(t.TempDir())
	if err := runFFmpeg(context.Background(), "-Show - E1.mp4", nil, "-i", f.url()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat("-Show - E1.mp4"); err != nil || fi.Size() == 0 {
		t.Errorf("episode not saved: %v", err)
	}
}

// a cached download must leave a playable file and no cache behind
func TestCachedHLSRemovesItsCache(t *testing.T) {
	f := newHLSFixture(t)
	out := t.TempDir()
	cache := filepath.Join(out, "cache")
	dest := filepath.Join(out, "show.mp4")

	if err := cachedHLS(context.Background(), http.DefaultClient, f.url(), dest, cache, nil); err != nil {
		t.Fatalf("cachedHLS: %v", err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("the segment cache outlived a finished download: %v", err)
	}
	if fi, err := os.Stat(dest); err != nil || fi.Size() == 0 {
		t.Fatalf("no usable output: %v", err)
	}
	if out, err := exec.Command("ffmpeg", "-v", "error", "-i", dest, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("output is not playable: %v: %s", err, out)
	}
}

// ffmpeg resolves the local playlist's segments against the playlist's own
// directory, so a relative cache dir used to remux dir/dir/00000.ts and the
// failure wiped the fully fetched cache
func TestCachedHLSAcceptsARelativeDir(t *testing.T) {
	f := newHLSFixture(t)
	t.Chdir(t.TempDir())

	if err := cachedHLS(context.Background(), http.DefaultClient, f.url(), "show.mp4", "cache", nil); err != nil {
		t.Fatalf("cachedHLS: %v", err)
	}
	if fi, err := os.Stat("show.mp4"); err != nil || fi.Size() == 0 {
		t.Fatalf("no usable output: %v", err)
	}
}

// an interrupted run must refetch only the segments it still lacks
func TestCachedHLSResumesFromPartialCache(t *testing.T) {
	f := newHLSFixture(t)
	out := t.TempDir()
	cache := filepath.Join(out, "cache")
	dest := filepath.Join(out, "show.mp4")

	pl, err := resolvePlaylist(context.Background(), http.DefaultClient, f.url())
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.segAt) < 2 {
		t.Skipf("fixture produced %d segments, need at least 2", len(pl.segAt))
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(cache, pl); err != nil {
		t.Fatal(err)
	}
	// leave every segment but the first already cached
	if err := fetchSegments(context.Background(), http.DefaultClient, pl, cache, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cache, segName(0))); err != nil {
		t.Fatal(err)
	}

	before := f.counts()
	if err := cachedHLS(context.Background(), http.DefaultClient, f.url(), dest, cache, nil); err != nil {
		t.Fatalf("resumed download: %v", err)
	}

	var refetched int
	for name, n := range f.counts() {
		if strings.HasSuffix(name, ".ts") && n > before[name] {
			refetched++
		}
	}
	if refetched != 1 {
		t.Errorf("resume refetched %d segments, want exactly the missing one", refetched)
	}
	if out, err := exec.Command("ffmpeg", "-v", "error", "-i", dest, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("resumed output is not playable: %v: %s", err, out)
	}
}

// a cache describing other content must not be spliced into this download
func TestReconcileWipesMismatchedCache(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, segName(0))
	if err := os.WriteFile(stale, []byte("stale segment"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(dir, &mediaPlaylist{segAt: []int{0, 1}, durations: []float64{10, 10}}); err != nil {
		t.Fatal(err)
	}
	// a differently segmented playlist for the same key means a re-encode
	if err := reconcile(dir, &mediaPlaylist{segAt: []int{0}, durations: []float64{10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale segment survived a playlist that no longer matches")
	}
}

// a cache holding the sound apart from the picture is another shape of the
// episode, so a playlist that starts naming a rendition does not resume on
// what the other shape cached
func TestReconcileWipesACacheOfAnotherShape(t *testing.T) {
	dir := t.TempDir()
	video := func() *mediaPlaylist { return &mediaPlaylist{segAt: []int{0, 1}, durations: []float64{10, 10}} }
	if err := reconcile(dir, video()); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, segName(0))
	if err := os.WriteFile(stale, []byte("segment with its own sound"), 0o644); err != nil {
		t.Fatal(err)
	}
	apart := video()
	apart.audio = &mediaPlaylist{segAt: []int{0, 1}, durations: []float64{10, 10}, prefix: "a"}
	if err := reconcile(dir, apart); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a segment cached without a rendition survived a playlist that names one")
	}
}

func TestReconcileKeepsMatchingCache(t *testing.T) {
	dir := t.TempDir()
	pl := &mediaPlaylist{segAt: []int{0, 1}, durations: []float64{10.010, 9.5}}
	if err := reconcile(dir, pl); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(dir, segName(0))
	if err := os.WriteFile(kept, []byte("good segment"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(dir, pl); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("an unchanged playlist discarded its cache: %v", err)
	}
}

// a byterange playlist addresses slices of one resource, so whole-segment
// caching cannot reproduce it and the caller must fall back
func TestResolvePlaylistRejectsByterange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-BYTERANGE:1000@0\n#EXTINF:1.0,\nseg.ts\n#EXT-X-ENDLIST\n")
	}))
	defer srv.Close()

	_, err := resolvePlaylist(context.Background(), http.DefaultClient, srv.URL+"/media.m3u8")
	if !errors.Is(err, errNoCache) {
		t.Errorf("want errNoCache, got %v", err)
	}
}

// a master naming no bandwidth is still a master, and losing the resumable
// path over a missing label would be losing it for no reason
func TestBestVariantTakesAnUnlabeledVariant(t *testing.T) {
	master := "#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1280x720\n720/index.m3u8\n"
	got, err := bestVariant([]byte(master), "https://cdn.example/master.m3u8")
	if err != nil || got.uri != "https://cdn.example/720/index.m3u8" || got.audio != "" {
		t.Errorf("bestVariant = %+v, %v", got, err)
	}
	if _, err := bestVariant([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n"), "https://cdn.example/m.m3u8"); err == nil {
		t.Error("a master naming no variant resolved")
	}
}

func TestResolvePlaylistFollowsHighestBandwidth(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	base := srv.URL
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "master.m3u8"):
			fmt.Fprintf(w, "#EXTM3U\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=800000\n%[1]s/low.m3u8\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=5000000\n%[1]s/high.m3u8\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=2000000\n%[1]s/mid.m3u8\n", base)
		default:
			fmt.Fprintf(w, "#EXTM3U\n#EXTINF:1.0,\n%s\n#EXT-X-ENDLIST\n",
				strings.TrimSuffix(filepath.Base(r.URL.Path), ".m3u8")+".ts")
		}
	})
	defer srv.Close()

	pl, err := resolvePlaylist(context.Background(), http.DefaultClient, base+"/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.segAt) != 1 {
		t.Fatalf("want 1 segment, got %d", len(pl.segAt))
	}
	if got := pl.lines[pl.segAt[0]]; !strings.HasSuffix(got, "/high.ts") {
		t.Errorf("followed the wrong variant: %s", got)
	}
}

// the local playlist must keep every tag so EXT-X-MEDIA-SEQUENCE still lines up
// with the segments, which is what AES-128 derives its IV from
func TestLocalizePreservesTagsAndRewritesKey(t *testing.T) {
	pl := &mediaPlaylist{
		lines: []string{
			"#EXTM3U",
			"#EXT-X-MEDIA-SEQUENCE:7",
			`#EXT-X-KEY:METHOD=AES-128,URI="https://cdn/mon.key"`,
			"#EXTINF:10.0,",
			"https://cdn/a.ts",
			"#EXT-X-ENDLIST",
		},
		segAt:     []int{4},
		durations: []float64{10},
		keyAt:     2,
		keyURI:    "https://cdn/mon.key",
	}
	got := pl.localize("/cache", "/cache/key.bin")

	for _, want := range []string{"#EXT-X-MEDIA-SEQUENCE:7", "#EXTINF:10.0,", "#EXT-X-ENDLIST", "METHOD=AES-128"} {
		if !strings.Contains(got, want) {
			t.Errorf("localized playlist dropped %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "https://cdn/") {
		t.Errorf("localized playlist still points upstream:\n%s", got)
	}
	if !strings.Contains(got, filepath.Join("/cache", segName(0))) {
		t.Errorf("segment not rewritten to its cached file:\n%s", got)
	}
	if !strings.Contains(got, `URI="/cache/key.bin"`) {
		t.Errorf("key not rewritten to its cached copy:\n%s", got)
	}
}

// every playlist shape this package cannot reproduce must fall back rather than
// produce a file that is quietly missing audio, an init segment or a key
func TestResolvePlaylistRefusesUnreproducibleShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"a video rendition group", "#EXTM3U\n" +
			`#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID="cam",NAME="angle",URI="angle.m3u8"` + "\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,VIDEO=\"cam\"\nvideo.m3u8\n"},
		{"an audio group the master never describes", "#EXTM3U\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=800000,AUDIO=\"aud\"\nvideo.m3u8\n"},
		{"init segment", "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1.0,\nseg.m4s\n#EXT-X-ENDLIST\n"},
		{"rotating keys", "#EXTM3U\n" +
			"#EXT-X-KEY:METHOD=AES-128,URI=\"https://cdn/k0.key\"\n#EXTINF:1.0,\na.ts\n" +
			"#EXT-X-KEY:METHOD=AES-128,URI=\"https://cdn/k1.key\"\n#EXTINF:1.0,\nb.ts\n#EXT-X-ENDLIST\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			if _, err := resolvePlaylist(context.Background(), http.DefaultClient, srv.URL+"/media.m3u8"); !errors.Is(err, errNoCache) {
				t.Errorf("want errNoCache, got %v", err)
			}
		})
	}
}

// an audio rendition is cached by the same rules as the video, so one this
// package cannot reproduce sends the whole episode to ffmpeg rather than
// leaving a file with picture and no sound
func TestResolvePlaylistRefusesAnAudioRenditionItCannotCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n"+
				`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="jp",DEFAULT=YES,URI="audio.m3u8"`+"\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=800000,AUDIO=\"aud\"\nvideo.m3u8\n")
		case "/audio.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1.0,\na.m4s\n#EXT-X-ENDLIST\n")
		default:
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1.0,\nv.ts\n#EXT-X-ENDLIST\n")
		}
	}))
	defer srv.Close()
	if _, err := resolvePlaylist(context.Background(), http.DefaultClient, srv.URL+"/master.m3u8"); !errors.Is(err, errNoCache) {
		t.Errorf("want errNoCache, got %v", err)
	}
}

// the rendition a download keeps is the one a player would pick, and hop lists
// eight languages in one group with the original marked default, twice over
func TestBestVariantFollowsTheAudioAPlayerWould(t *testing.T) {
	const base = "https://cdn.example/master.m3u8"
	var hop strings.Builder
	hop.WriteString("#EXTM3U\n")
	for range 2 {
		for _, lang := range []string{"hin", "eng", "jpn", "spa"} {
			def := ""
			if lang == "jpn" {
				def = "DEFAULT=YES,"
			}
			hop.WriteString(fmt.Sprintf(`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="stereo",NAME="%[1]s, stereo",%[2]sLANGUAGE="%[1]s",URI="%[1]s.m3u8"`+"\n", lang, def))
		}
	}
	hop.WriteString(`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",DEFAULT=YES,URI="subs.m3u8"` + "\n" +
		"#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=9000000,BANDWIDTH=1000000,AUDIO=\"stereo\",SUBTITLES=\"subs\"\nlow.m3u8\n" +
		"#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=100,BANDWIDTH=5000000,AUDIO=\"stereo\",SUBTITLES=\"subs\"\nhigh.m3u8\n")

	for _, tc := range []struct {
		name, master string
		want         variant
	}{
		{"the default of the variant's group, by bandwidth rather than its average", hop.String(),
			variant{uri: "https://cdn.example/high.m3u8", audio: "https://cdn.example/jpn.m3u8"}},
		{"one marked for automatic selection without a default", "#EXTM3U\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="x",URI="x.m3u8"` + "\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="y",AUTOSELECT=YES,URI="y.m3u8"` + "\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n",
			variant{uri: "https://cdn.example/v.m3u8", audio: "https://cdn.example/y.m3u8"}},
		{"the first of the group whatever another group marks", "#EXTM3U\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="other",NAME="z",DEFAULT=YES,URI="z.m3u8"` + "\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="x",URI="x.m3u8"` + "\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n",
			variant{uri: "https://cdn.example/v.m3u8", audio: "https://cdn.example/x.m3u8"}},
		{"none where the rendition is the variant's own sound", "#EXTM3U\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="main",DEFAULT=YES` + "\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n",
			variant{uri: "https://cdn.example/v.m3u8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bestVariant([]byte(tc.master), base)
			if err != nil || got != tc.want {
				t.Errorf("bestVariant = %+v, %v, want %+v", got, err, tc.want)
			}
		})
	}
}

// a quoted value keeps its commas, and a name is matched whole, so BANDWIDTH
// is not read out of AVERAGE-BANDWIDTH
func TestAttributes(t *testing.T) {
	got := attributes(`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="stereo",NAME="Hindi, dubbed",DEFAULT=YES,URI="a.m3u8?x=1,2"`)
	want := map[string]string{"TYPE": "AUDIO", "GROUP-ID": "stereo", "NAME": "Hindi, dubbed", "DEFAULT": "YES", "URI": "a.m3u8?x=1,2"}
	if !maps.Equal(got, want) {
		t.Errorf("attributes = %v, want %v", got, want)
	}
	if got := attributes("#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=7,BANDWIDTH=9"); got["BANDWIDTH"] != "9" {
		t.Errorf("BANDWIDTH = %q, want 9", got["BANDWIDTH"])
	}
	if got := attributes("#EXTM3U"); len(got) != 0 {
		t.Errorf("a tag with no list read as %v", got)
	}
}

// a stream whose sound comes apart downloads both into the cache and remuxes
// them into one file with picture and sound, and a resumed run refetches only
// the audio segment it lacks
func TestCachedHLSFetchesTheAudioRendition(t *testing.T) {
	f := newAudioHLSFixture(t)
	out := t.TempDir()
	cache := filepath.Join(out, "cache")
	dest := filepath.Join(out, "show.mp4")
	ctx := context.Background()
	master := f.srv.URL + "/master.m3u8"

	pl, err := resolvePlaylist(ctx, http.DefaultClient, master)
	if err != nil {
		t.Fatal(err)
	}
	if pl.audio == nil || len(pl.audio.segAt) < 2 {
		t.Fatalf("the audio rendition was not followed: %+v", pl.audio)
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(cache, pl); err != nil {
		t.Fatal(err)
	}
	if err := fetchSegments(ctx, http.DefaultClient, pl, cache, nil); err != nil {
		t.Fatal(err)
	}
	c, err := Cached(cache)
	if err != nil || c.Have != c.Want || c.Want != len(pl.segAt)+len(pl.audio.segAt) {
		t.Errorf("cache holds %+v (%v), want every video and audio segment counted", c, err)
	}
	if err := os.Remove(filepath.Join(cache, pl.audio.seg(0))); err != nil {
		t.Fatal(err)
	}

	before := f.counts()
	if err := cachedHLS(ctx, http.DefaultClient, master, dest, cache, nil); err != nil {
		t.Fatalf("resumed download: %v", err)
	}
	if n := f.fetched("seg_1_", before); n != 1 {
		t.Errorf("resume refetched %d audio segments, want exactly the missing one", n)
	}
	if n := f.fetched("seg_0_", before); n != 0 {
		t.Errorf("resume refetched %d video segments already cached", n)
	}
	if !streams(t, dest, "v") || !streams(t, dest, "a") {
		t.Error("the episode lacks picture or sound")
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Errorf("the segment cache outlived a finished download: %v", err)
	}
}

// a data key is already inline, so it needs no fetch and must survive verbatim
func TestParsePlaylistLeavesDataKeyAlone(t *testing.T) {
	body := "#EXTM3U\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="data:text/plain;base64,YWJjZA=="` + "\n" +
		"#EXTINF:1.0,\nhttps://cdn/a.ts\n#EXT-X-ENDLIST\n"
	pl, err := parsePlaylist([]byte(body), "https://cdn/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if pl.keyURI != "" {
		t.Errorf("a data key should not be fetched, got %q", pl.keyURI)
	}
	if !pl.encrypted {
		t.Error("a data key did not mark the stream encrypted")
	}
	if got := pl.localize("/cache", ""); !strings.Contains(got, "data:text/plain;base64,YWJjZA==") {
		t.Errorf("data key did not survive:\n%s", got)
	}
}

// METHOD=NONE switches encryption off, so the plaintext TS check still applies
func TestParsePlaylistMethodNoneStaysPlain(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-KEY:METHOD=NONE\n" +
		"#EXTINF:1.0,\nhttps://cdn/a.ts\n#EXT-X-ENDLIST\n"
	pl, err := parsePlaylist([]byte(body), "https://cdn/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if pl.encrypted {
		t.Error("METHOD=NONE marked the stream encrypted")
	}
}

// segments under a data key are ciphertext even though no key is fetched, so
// demanding TS sync bytes from them would fail every encrypted download
func TestFetchSegmentsAcceptsCiphertextUnderDataKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("AES ciphertext with no sync byte anywhere in sight"))
	}))
	defer srv.Close()

	body := "#EXTM3U\n" +
		`#EXT-X-KEY:METHOD=AES-128,URI="data:text/plain;base64,YWJjZA=="` + "\n" +
		"#EXTINF:1.0,\n" + srv.URL + "/a.ts\n#EXT-X-ENDLIST\n"
	pl, err := parsePlaylist([]byte(body), srv.URL+"/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := fetchSegments(context.Background(), http.DefaultClient, pl, dir, nil); err != nil {
		t.Fatalf("ciphertext segment rejected: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, segName(0))); err != nil || fi.Size() == 0 {
		t.Errorf("segment not cached: %v", err)
	}
}

// without durations only the segment count identifies a playlist, which is too
// weak to risk reusing another episode's segments
func TestReconcileWipesWhenDurationsAreUnknown(t *testing.T) {
	dir := t.TempDir()
	pl := &mediaPlaylist{segAt: []int{0, 1}, durations: []float64{-1, -1}}
	if err := reconcile(dir, pl); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, segName(0))
	if err := os.WriteFile(stale, []byte("from another episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reconcile(dir, pl); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("an unidentifiable cache was trusted")
	}
}

// a cached error page would remux into a silently truncated episode, so a body
// that is not whole media must never be renamed into place
func TestFetchSegmentRejectsBodyThatIsNotMedia(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  []byte
		short bool
		plain bool
	}{
		{name: "error page", body: []byte("<html>Access Denied</html>"), plain: true},
		{name: "empty body", body: nil, plain: true},
		// "Gateway Timeout" opens on 0x47, and is too short to carry a packet
		{name: "short body opening on a sync byte", body: []byte("Gateway Timeout"), plain: true},
		{name: "truncated transport stream", body: tsBlob(4), short: true, plain: true},
		{name: "truncated ciphertext", body: []byte("encrypted bytes"), short: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.short {
					// announce more than is sent, as a dropped connection does
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)+500))
					w.WriteHeader(http.StatusOK)
					w.Write(tc.body)
					return
				}
				w.Write(tc.body)
			}))
			defer srv.Close()

			dest := filepath.Join(t.TempDir(), segName(0))
			if _, err := fetchSegment(context.Background(), http.DefaultClient, srv.URL+"/seg.ts", dest, tc.plain); err == nil {
				t.Error("a body that is not a whole segment was accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Error("a rejected segment was still cached")
			}
			if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
				t.Error("a rejected segment left its .part behind")
			}
		})
	}
}

// ciphertext cannot be inspected, so an encrypted segment of the right length
// has to be accepted
func TestFetchSegmentAcceptsCiphertext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("opaque encrypted payload"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), segName(0))
	n, err := fetchSegment(context.Background(), http.DefaultClient, srv.URL+"/seg.ts", dest, false)
	if err != nil {
		t.Fatalf("encrypted segment rejected: %v", err)
	}
	if n == 0 {
		t.Error("no bytes reported")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("encrypted segment not cached: %v", err)
	}
}

// a body of any other size cannot be an AES-128 key, and caching it would
// poison every later resume
func TestCacheKeyRejectsWrongSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>Access Denied</html>"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	pl := &mediaPlaylist{keyURI: srv.URL + "/mon.key"}
	if _, err := cacheKey(context.Background(), http.DefaultClient, pl, dir); err == nil {
		t.Error("a body that cannot be a key was accepted")
	}
	for _, name := range []string{"key.bin", "key.bin.part"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s written for a rejected key", name)
		}
	}
}

// a stale key.bin of the wrong size predates the shape check and must be
// refetched rather than trusted, while a whole one is reused without a fetch
func TestCacheKeyRefetchesStaleShortKey(t *testing.T) {
	good := []byte("0123456789abcdef")
	var (
		mu   sync.Mutex
		hits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write(good)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.bin"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	pl := &mediaPlaylist{keyURI: srv.URL + "/mon.key"}
	path, err := cacheKey(context.Background(), http.DefaultClient, pl, dir)
	if err != nil {
		t.Fatalf("cacheKey: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, good) {
		t.Errorf("stale key not overwritten, got %q, %v", got, err)
	}
	if _, err := cacheKey(context.Background(), http.DefaultClient, pl, dir); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	fetched := hits
	mu.Unlock()
	if fetched != 1 {
		t.Errorf("key fetched %d times, want 1", fetched)
	}
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Error("the .part file was left behind")
	}
}

// an endless chunked playlist body would otherwise buffer until memory runs out
func TestFetchTextRefusesOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeros{}, maxTextBody+1)
	}))
	defer srv.Close()

	// the url is the proxy's own loopback address carrying an encoded target, so
	// the cap is what the message names rather than where the body came from
	u := srv.URL + "/media.m3u8"
	_, err := fetchText(context.Background(), http.DefaultClient, u)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want an over-cap error, got %v", err)
	}
	if strings.Contains(err.Error(), u) {
		t.Errorf("the message carries the proxy url a reader cannot act on: %v", err)
	}
	// the cap guards against a hostile body, so refetching it only pays twice
	if transient(err) {
		t.Error("an over-cap body must not be retried")
	}
}

// a playlist past the segment ceiling would mint that many cache files, so it
// falls back to plain streaming instead
func TestParsePlaylistRefusesAbsurdSegmentCount(t *testing.T) {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for i := range maxSegments + 1 {
		fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts\n", i)
	}
	b.WriteString("#EXT-X-ENDLIST\n")

	_, err := parsePlaylist([]byte(b.String()), "https://cdn/media.m3u8")
	if !errors.Is(err, errNoCache) {
		t.Errorf("want errNoCache, got %v", err)
	}
}

// a playlist without EXT-X-ENDLIST is still growing, and a snapshot of it
// would remux into a finished-looking partial episode
func TestResolvePlaylistRejectsGrowingPlaylist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:1.0,\nseg0.ts\n")
	}))
	defer srv.Close()

	_, err := resolvePlaylist(context.Background(), http.DefaultClient, srv.URL+"/media.m3u8")
	if !errors.Is(err, errNoCache) {
		t.Errorf("want errNoCache, got %v", err)
	}
}

// a variant that lives in its own directory has its children resolved against
// the variant url, not the master's
func TestResolvePlaylistUsesTheVariantBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv/media.m3u8\n")
		case "/v/media.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:1.0,\nseg0.ts\n#EXT-X-ENDLIST\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	pl, err := resolvePlaylist(context.Background(), http.DefaultClient, srv.URL+"/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pl.lines[pl.segAt[0]], srv.URL+"/v/seg0.ts"; got != want {
		t.Errorf("segment = %q, want %q", got, want)
	}
}

// a cache path is a file name, so a '$' in it must survive into the playlist
// rather than read as a capture group reference
func TestLocalizeKeepsADollarInTheKeyPath(t *testing.T) {
	pl := &mediaPlaylist{
		lines:  []string{"#EXTM3U", `#EXT-X-KEY:METHOD=AES-128,URI="https://cdn/key"`, "seg0.ts"},
		segAt:  []int{2},
		keyAt:  1,
		keyURI: "https://cdn/key",
	}
	key := filepath.Join("/tmp/a$name", "key.bin")
	if got := pl.localize("/tmp/a$name", key); !strings.Contains(got, `URI="`+key+`"`) {
		t.Errorf("localize dropped part of the key path:\n%s", got)
	}
}

// two providers can segment an episode the same way at different bitrates, and
// the durations are the only thing left that tells their caches apart
// the tolerance exists because upstream prints them with limited precision, so
// it has to be asserted in both directions: a difference inside it keeps the
// cache, one outside it wipes it
func TestReconcileComparesDurationsWithinATolerance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second []float64
		kept   bool
	}{
		{"the same durations", []float64{10, 9.5}, true},
		{"inside the tolerance", []float64{10.0009, 9.4991}, true},
		{"outside the tolerance", []float64{10.002, 9.5}, false},
		{"far outside it", []float64{12, 9.5}, false},
		{"an unknown duration", []float64{-1, 9.5}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			first := &mediaPlaylist{segAt: []int{0, 1}, durations: []float64{10, 9.5}}
			if err := reconcile(dir, first); err != nil {
				t.Fatal(err)
			}
			seg := filepath.Join(dir, segName(0))
			if err := os.WriteFile(seg, []byte("segment"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := reconcile(dir, &mediaPlaylist{segAt: []int{0, 1}, durations: tc.second}); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(seg)
			if tc.kept && err != nil {
				t.Errorf("the cache was wiped for a difference the tolerance covers: %v", err)
			}
			if !tc.kept && !os.IsNotExist(err) {
				t.Error("the cache survived durations that do not match")
			}
		})
	}
}

// cachedHLS removes its cache directory once the episode is on disk, and
// filepath.Abs turns an empty one into the working directory
// a caller that passes no cache root would therefore delete the directory the
// run was started from, so the refusal belongs here rather than only at the
// call site
func TestCachedHLSRefusesAnEmptyCacheRoot(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "a-file-the-run-must-not-delete")
	if err := os.WriteFile(keep, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	// the working directory is what an empty root resolves to
	t.Chdir(dir)

	err := cachedHLS(context.Background(), http.DefaultClient, "http://127.0.0.1:1/x.m3u8",
		filepath.Join(dir, "out.mp4"), "", nil)
	if !errors.Is(err, errNoCache) {
		t.Errorf("err = %v, want the empty cache root refused as uncacheable", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the working directory was wiped: %v", err)
	}
}

// a segment listing no running time at all is still a known length, where one
// naming none leaves the whole unknown
func TestLengthCountsASegmentOfNoRunningTime(t *testing.T) {
	pl, err := parsePlaylist([]byte("#EXTM3U\n#EXTINF:0,\ns0.ts\n#EXTINF:2.0,\ns1.ts\n#EXT-X-ENDLIST\n"), "https://cdn.example/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if got := pl.length(); got != 2 {
		t.Errorf("length = %v, want the 2s the playlist lists", got)
	}
}

// nobody announces an hls episode's byte total, so the cache path tells how
// far it got as a share of the running time the playlist lists, which reaches
// the whole once every segment is on disk and counts what a resumed run
// already had
func TestFetchSegmentsReportsTheShareOfRunningTime(t *testing.T) {
	seg := bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(seg) }))
	defer srv.Close()

	body := "#EXTM3U\n#EXTINF:6.0,\ns0.ts\n#EXTINF:2.0,\ns1.ts\n#EXTINF:2.0,\ns2.ts\n#EXT-X-ENDLIST\n"
	pl, err := parsePlaylist([]byte(body), srv.URL+"/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// a resumed run already has the long first segment
	if err := os.WriteFile(filepath.Join(dir, segName(0)), seg, 0o644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var shares []float64
	prog := func(_, total int64, share float64) {
		mu.Lock()
		defer mu.Unlock()
		if total != 0 {
			t.Errorf("reported a byte total of %d nobody announced", total)
		}
		shares = append(shares, share)
	}
	if err := fetchSegments(context.Background(), http.DefaultClient, pl, dir, prog); err != nil {
		t.Fatal(err)
	}
	if len(shares) == 0 || shares[0] < 0.6 {
		t.Errorf("shares = %v, want the first report to count the cached segment", shares)
	}
	if last := slices.Max(shares); last != 1 {
		t.Errorf("shares = %v, want the whole running time at the end", shares)
	}

	// a segment naming no duration leaves the length unknown rather than
	// letting a partial sum run past the end
	unknown, err := parsePlaylist([]byte("#EXTM3U\ns0.ts\n#EXTINF:2.0,\ns1.ts\n#EXT-X-ENDLIST\n"), srv.URL+"/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if got := unknown.length(); got != 0 {
		t.Errorf("length = %v, want 0 when a segment names no duration", got)
	}
}

// with the sound apart, the part of the running time written is the part where
// picture and sound are both in, so a resumed run holding the whole picture and
// none of the sound has written none of it
func TestFetchSegmentsReportsTheShareBothHaveReached(t *testing.T) {
	seg := bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(seg) }))
	defer srv.Close()

	pl, err := parsePlaylist([]byte("#EXTM3U\n#EXTINF:6.0,\nv0.ts\n#EXTINF:4.0,\nv1.ts\n#EXT-X-ENDLIST\n"), srv.URL+"/video.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if pl.audio, err = parsePlaylist([]byte("#EXTM3U\n#EXTINF:5.0,\na0.ts\n#EXTINF:5.0,\na1.ts\n#EXT-X-ENDLIST\n"), srv.URL+"/audio.m3u8"); err != nil {
		t.Fatal(err)
	}
	pl.audio.prefix = "a"
	dir := t.TempDir()
	for n := range pl.segAt {
		if err := os.WriteFile(filepath.Join(dir, pl.seg(n)), seg, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var shares []float64
	prog := func(_, _ int64, share float64) {
		mu.Lock()
		defer mu.Unlock()
		shares = append(shares, share)
	}
	if err := fetchSegments(context.Background(), http.DefaultClient, pl, dir, prog); err != nil {
		t.Fatal(err)
	}
	if len(shares) == 0 || shares[0] != 0 {
		t.Errorf("shares = %v, want none of the running time before any sound", shares)
	}
	if last := slices.Max(shares); last != 1 {
		t.Errorf("shares = %v, want the whole running time at the end", shares)
	}
	for n := range pl.audio.segAt {
		if _, err := os.Stat(filepath.Join(dir, "a"+segName(n))); err != nil {
			t.Errorf("audio segment %d not cached under its prefix: %v", n, err)
		}
	}
}

// requests are spaced from when each asks, so a caller that waited elsewhere
// does not bank its turns into a burst, and a canceled caller stops waiting
func TestPacerSpacesRequests(t *testing.T) {
	p := &pacer{gap: 20 * time.Millisecond}
	ctx := context.Background()
	start := time.Now()
	for range 5 {
		if err := p.wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if took := time.Since(start); took < 80*time.Millisecond {
		t.Errorf("five turns took %v, want at least four gaps", took)
	}

	time.Sleep(100 * time.Millisecond)
	start = time.Now()
	for range 3 {
		p.wait(ctx)
	}
	if took := time.Since(start); took < 40*time.Millisecond {
		t.Errorf("three turns after an idle spell took %v, want at least two gaps", took)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	p.gap = time.Hour
	p.wait(ctx)
	if err := p.wait(canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled wait returned %v", err)
	}
}

// an audio segment that fails is named as one, since the video's index alone
// would send the reader to the wrong playlist
func TestFetchSegmentsNamesAFailedAudioSegment(t *testing.T) {
	seg := bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a0.ts" {
			http.NotFound(w, r)
			return
		}
		w.Write(seg)
	}))
	defer srv.Close()
	pl, err := parsePlaylist([]byte("#EXTM3U\n#EXTINF:1.0,\nv0.ts\n#EXT-X-ENDLIST\n"), srv.URL+"/video.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	if pl.audio, err = parsePlaylist([]byte("#EXTM3U\n#EXTINF:1.0,\na0.ts\n#EXT-X-ENDLIST\n"), srv.URL+"/audio.m3u8"); err != nil {
		t.Fatal(err)
	}
	pl.audio.prefix = "a"
	if err := fetchSegments(context.Background(), http.DefaultClient, pl, t.TempDir(), nil); err == nil || !strings.HasPrefix(err.Error(), "audio segment 0:") {
		t.Errorf("err = %v, want the audio segment named", err)
	}
}

// a playlist whose running time is unknown reports none of it as written, where
// a share of an unknown whole would be infinite or no number at all, and a
// segment file left empty by a failed run is no segment and counts for nothing
func TestFetchSegmentsReportsOnlyAShareItKnows(t *testing.T) {
	seg := bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(seg) }))
	defer srv.Close()
	shares := func(body string, cached ...int) []float64 {
		t.Helper()
		pl, err := parsePlaylist([]byte(body), srv.URL+"/media.m3u8")
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		for _, n := range cached {
			if err := os.WriteFile(filepath.Join(dir, segName(n)), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		var mu sync.Mutex
		var got []float64
		prog := func(_, _ int64, share float64) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, share)
		}
		if err := fetchSegments(context.Background(), http.DefaultClient, pl, dir, prog); err != nil {
			t.Fatal(err)
		}
		return got
	}

	for _, s := range shares("#EXTM3U\ns0.ts\n#EXTINF:2.0,\ns1.ts\n#EXT-X-ENDLIST\n") {
		if s != 0 {
			t.Errorf("reported a share of %v of an unknown running time", s)
		}
	}
	got := shares("#EXTM3U\n#EXTINF:2.0,\ns0.ts\n#EXTINF:2.0,\ns1.ts\n#EXT-X-ENDLIST\n", 0)
	if len(got) == 0 || got[0] != 0 || slices.Max(got) != 1 {
		t.Errorf("shares = %v, want an empty leftover counted as nothing and the whole at the end", got)
	}
}

// one dead segment makes the remux incomplete, so the first to fail is named
// and nothing past it is started, where every later segment would be fetched
// for an episode already lost
func TestFetchSegmentsStopsAtTheFirstFailure(t *testing.T) {
	seg := bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 8)
	var fetched atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		if r.URL.Path == "/s1.ts" {
			http.NotFound(w, r)
			return
		}
		// the live segments are slow, so the failure lands while they are running
		time.Sleep(50 * time.Millisecond)
		w.Write(seg)
	}))
	defer srv.Close()

	var body strings.Builder
	body.WriteString("#EXTM3U\n")
	const n = 40
	for i := range n {
		fmt.Fprintf(&body, "#EXTINF:1.0,\ns%d.ts\n", i)
	}
	body.WriteString("#EXT-X-ENDLIST\n")
	pl, err := parsePlaylist([]byte(body.String()), srv.URL+"/media.m3u8")
	if err != nil {
		t.Fatal(err)
	}

	err = fetchSegments(context.Background(), http.DefaultClient, pl, t.TempDir(), nil)
	if err == nil || !strings.HasPrefix(err.Error(), "segment 1:") {
		t.Fatalf("err = %v, want the first failed segment named", err)
	}
	if got := fetched.Load(); got >= n {
		t.Errorf("fetched %d of %d segments after the first one failed", got, n)
	}
}
