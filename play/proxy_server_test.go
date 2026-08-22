package play

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ysun.co/miruro"
)

// mpv is the real consumer of the proxy and a decoy-disguised segment is the
// case that broke, so drive the actual binary through the whole chain
// the segment is served with ServeContent, which answers a Range with 206, the
// exact condition that previously slipped past the decoy strip
func TestProxyServesDisguisedSegmentToMPV(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("mpv not installed")
	}
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
	raw, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	disguised := append([]byte("\x89PNG\r\n\x1a\n"+strings.Repeat("D", 244)), raw...)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".ts") {
			http.ServeContent(w, r, "seg.ts", time.Time{}, bytes.NewReader(disguised))
			return
		}
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n%s/seg.ts\n#EXT-X-ENDLIST\n", base)
	})

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	play := exec.CommandContext(ctx, "mpv", "--no-config", "--vo=null", "--ao=null",
		"--frames=1", "--msg-level=all=error",
		px.URL(miruro.Stream{URL: base + "/media.m3u8", Kind: miruro.HLS}))
	if out, err := play.CombinedOutput(); err != nil {
		t.Fatalf("mpv could not play the proxied stream: %v: %s", err, out)
	}
}

func TestProxyServesNormalizedHLS(t *testing.T) {
	mux2 := http.NewServeMux()
	upstream := httptest.NewServer(mux2)
	defer upstream.Close()
	up := upstream.URL
	mux2.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://ref/" {
			t.Errorf("referer not forwarded upstream: %q", r.Header.Get("Referer"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "media.m3u8"):
			fmt.Fprintf(w, "#EXTM3U\n#EXTINF:1.0,\n%s/seg0.ts\n#EXT-X-ENDLIST\n", up)
		case strings.HasSuffix(r.URL.Path, "seg0.ts"):
			w.Write(append([]byte("\x89PNG-decoy-bytes"), tsBlob(12)...))
		}
	})

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	playlistURL := px.URL(miruro.Stream{URL: up + "/media.m3u8", Kind: miruro.HLS, Referer: "https://ref/"})
	body := httpGetString(t, playlistURL)
	if strings.Contains(body, up) {
		t.Errorf("segment URL not rewritten to proxy:\n%s", body)
	}

	segURL := firstProxiedLine(t, body, px.base)
	seg := httpGetBytes(t, segURL)
	if len(seg) == 0 || seg[0] != 0x47 {
		t.Fatalf("proxied segment not normalized to TS sync")
	}
}

// a playlist served through a redirect resolves its relative children against
// the final URL rather than the one first requested
func TestProxyRewritesAgainstRedirectedURL(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a/pl.m3u8":
			http.Redirect(w, r, "/b/pl.m3u8", http.StatusFound)
		case "/b/pl.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\nseg0.ts\n#EXT-X-ENDLIST\n")
		}
	}))
	defer upstream.Close()

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	body := httpGetString(t, px.URL(miruro.Stream{URL: upstream.URL + "/a/pl.m3u8", Kind: miruro.HLS}))
	seg := firstProxiedLine(t, body, px.base)
	u, err := url.Parse(seg)
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := px.decode(u.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(tgt.URL, "/b/seg0.ts") {
		t.Errorf("child resolved against the pre-redirect base: %s", tgt.URL)
	}
}

// a player may probe a segment with a Range header
// the proxy must still fetch the whole segment and strip the decoy rather than
// relay a partial that keeps the image prefix
func TestProxyNormalizesSegmentDespiteClientRange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			// a cdn honouring the range would drop the leading framing
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Write(append([]byte("\x89PNG-decoy-bytes"), tsBlob(12)...))
	}))
	defer upstream.Close()

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	req, _ := http.NewRequest(http.MethodGet, px.proxied(upstream.URL+"/seg0.ts", "", segment), nil)
	req.Header.Set("Range", "bytes=0-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if len(got) == 0 || got[0] != 0x47 {
		t.Fatalf("segment not normalized to TS sync, first byte %#x", got)
	}
}

// a buffered kind reads the body whole, so an upstream that answers and then
// stalls mid-body must trip the proxy deadline rather than wedge the player
// or a download worker
func TestProxyBoundsStalledSegment(t *testing.T) {
	stall := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-stall
	}))
	// the handler blocks until stall closes, and Close waits on the handler,
	// so this defer has to run first
	defer upstream.Close()
	defer close(stall)

	// the proxy is built by hand so the short deadline is set before it serves
	px := &Proxy{hc: &http.Client{}, token: "tok", timeout: 100 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(px.handle))
	defer srv.Close()
	px.base = srv.URL + "/tok"

	resp, err := http.Get(px.proxied(upstream.URL+"/seg0.ts", "", segment))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("stalled upstream answered %d, want 502", resp.StatusCode)
	}
}

// a hostile or broken upstream serving an endless playlist must get a 502
// rather than buffer until memory runs out
func TestProxyRefusesOversizedPlaylist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeros{}, maxPlaylistBody+1)
	}))
	defer upstream.Close()

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	resp, err := http.Get(px.URL(miruro.Stream{URL: upstream.URL + "/media.m3u8", Kind: miruro.HLS}))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502 for an over-cap playlist, got %d", resp.StatusCode)
	}
}

func TestProxyRelaysRange(t *testing.T) {
	full := bytes.Repeat([]byte("A"), 1000)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.bin", time.Time{}, bytes.NewReader(full))
	}))
	defer upstream.Close()

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	req, _ := http.NewRequest(http.MethodGet, px.Opaque(upstream.URL+"/x.bin", ""), nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range not relayed, status %d", resp.StatusCode)
	}
	if got, _ := io.ReadAll(resp.Body); len(got) != 10 {
		t.Errorf("want 10 bytes, got %d", len(got))
	}
}

func httpGetString(t *testing.T, u string) string { return string(httpGetBytes(t, u)) }

func httpGetBytes(t *testing.T, u string) []byte {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s -> %d: %s", u, resp.StatusCode, b)
	}
	return b
}

func firstProxiedLine(t *testing.T, body, base string) string {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), base) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no proxied URL in playlist:\n%s", body)
	return ""
}

// mpv titles an external subtitle track from the last path component of its url,
// so the proxy has to carry a readable name past the base64 payload without
// losing the target
func TestProxySubtitleURLIsReadableAndRelays(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") == "" {
			http.Error(w, "referer required", http.StatusForbidden)
			return
		}
		io.WriteString(w, "WEBVTT\n")
	}))
	defer upstream.Close()

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	subs := px.Subtitles([]miruro.Subtitle{
		{File: upstream.URL + "/track.srt", Label: "Portugues (Brasil)", Lang: "pt-BR"},
		{File: upstream.URL + "/track", Lang: "en"},
		{File: upstream.URL + "/../track.vtt", Label: "../../escape"},
	}, upstream.URL+"/")

	want := []string{"Portugues (Brasil).srt", "en.vtt", "-..-escape.vtt"}
	for i, s := range subs {
		u, err := url.Parse(s.File)
		if err != nil {
			t.Fatalf("subtitle %d is not a url: %v", i, err)
		}
		if got := path.Base(u.Path); got != want[i] {
			t.Errorf("subtitle %d shows as %q, want %q", i, got, want[i])
		}
		if body := httpGetString(t, s.File); body != "WEBVTT\n" {
			t.Errorf("subtitle %d relayed %q", i, body)
		}
	}
	if subs[0].Label != "Portugues (Brasil)" || subs[0].Lang != "pt-BR" {
		t.Error("proxying dropped the track metadata the caller still needs")
	}
}

// the downloader retries on the status the proxy answers with, so flattening
// every upstream failure to 502 would make a dead url look worth retrying
func TestProxyMirrorsUpstreamStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gone.ts":
			http.Error(w, "gone", http.StatusNotFound)
		case "/denied.ts":
			http.Error(w, "denied", http.StatusForbidden)
		default:
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		}
	}))
	defer upstream.Close()

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	for path, want := range map[string]int{
		"/gone.ts":   http.StatusNotFound,
		"/denied.ts": http.StatusForbidden,
		"/dead.ts":   http.StatusBadGateway,
	} {
		resp, err := http.Get(px.proxied(upstream.URL+path, "", segment))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s answered %d, want %d", path, resp.StatusCode, want)
		}
	}
}

// Served has to say whether the player got picture, so a playlist, an aes key,
// and a subtitle sidecar must not raise it
func TestProxyServedCountsPictureOnly(t *testing.T) {
	seg := tsBlob(12)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n%s\n#EXT-X-ENDLIST\n", "seg0.ts")
		case strings.HasSuffix(r.URL.Path, ".ts"):
			w.Write(seg)
		default:
			io.WriteString(w, "WEBVTT\n")
		}
	}))
	defer upstream.Close()

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	body := httpGetString(t, px.URL(miruro.Stream{URL: upstream.URL + "/media.m3u8", Kind: miruro.HLS}))
	if px.Served() != 0 {
		t.Errorf("a playlist raised Served to %d", px.Served())
	}

	httpGetBytes(t, px.Opaque(upstream.URL+"/key.bin", ""))
	httpGetString(t, px.Subtitles([]miruro.Subtitle{{File: upstream.URL + "/en.vtt", Lang: "en"}}, "")[0].File)
	if px.Served() != 0 {
		t.Errorf("a key or a sidecar raised Served to %d", px.Served())
	}

	httpGetBytes(t, firstProxiedLine(t, body, px.base))
	if px.Served() != 1 {
		t.Errorf("a segment left Served at %d, want 1", px.Served())
	}

	httpGetBytes(t, px.URL(miruro.Stream{URL: upstream.URL + "/video.mp4", Kind: miruro.MP4}))
	if px.Served() != 2 {
		t.Errorf("an mp4 body left Served at %d, want 2", px.Served())
	}
}
