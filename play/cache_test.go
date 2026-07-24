package play

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// hlsFixture synthesises a short multi-segment stream and serves it
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
		t.Skipf("cannot synthesise an hls stream: %v: %s", err, out)
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
func TestLocalisePreservesTagsAndRewritesKey(t *testing.T) {
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
	got := pl.localise("/cache", "/cache/key.bin")

	for _, want := range []string{"#EXT-X-MEDIA-SEQUENCE:7", "#EXTINF:10.0,", "#EXT-X-ENDLIST", "METHOD=AES-128"} {
		if !strings.Contains(got, want) {
			t.Errorf("localised playlist dropped %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "https://cdn/") {
		t.Errorf("localised playlist still points upstream:\n%s", got)
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
		{"separate audio rendition", "#EXTM3U\n" +
			`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",URI="audio.m3u8"` + "\n" +
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
	if got := pl.localise("/cache", ""); !strings.Contains(got, "data:text/plain;base64,YWJjZA==") {
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
