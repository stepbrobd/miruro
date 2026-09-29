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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/upstream"
)

// counted addresses a url through the proxy the way a stream's playlist does,
// counted against tl
func counted(tl *Tally, rawURL, referer string, k kind) string {
	return tl.px.encode(target{URL: rawURL, Referer: referer, Kind: k, Tally: tl.id})
}

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
		t.Skipf("cannot synthesize a segment: %v: %s", err, out)
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

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	play := exec.CommandContext(ctx, "mpv", "--no-config", "--vo=null", "--ao=null",
		"--frames=1", "--msg-level=all=error",
		px.URL(upstream.Stream{URL: base + "/media.m3u8", Kind: upstream.HLS}))
	if out, err := play.CombinedOutput(); err != nil {
		t.Fatalf("mpv could not play the proxied stream: %v: %s", err, out)
	}
}

func TestProxyServesNormalizedHLS(t *testing.T) {
	mux2 := http.NewServeMux()
	origin := httptest.NewServer(mux2)
	defer origin.Close()
	up := origin.URL
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

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	playlistURL := px.URL(upstream.Stream{URL: up + "/media.m3u8", Kind: upstream.HLS, Referer: "https://ref/"})
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
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a/pl.m3u8":
			http.Redirect(w, r, "/b/pl.m3u8", http.StatusFound)
		case "/b/pl.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\nseg0.ts\n#EXT-X-ENDLIST\n")
		}
	}))
	defer origin.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	body := httpGetString(t, px.URL(upstream.Stream{URL: origin.URL + "/a/pl.m3u8", Kind: upstream.HLS}))
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
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			// a cdn honoring the range would drop the leading framing
			w.WriteHeader(http.StatusPartialContent)
		}
		w.Write(append([]byte("\x89PNG-decoy-bytes"), tsBlob(12)...))
	}))
	defer origin.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	req, _ := http.NewRequest(http.MethodGet, px.proxied(origin.URL+"/seg0.ts", "", segment), nil)
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
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-stall
	}))
	// the handler blocks until stall closes, and Close waits on the handler,
	// so this defer has to run first
	defer origin.Close()
	defer close(stall)

	// the proxy is built by hand so the short deadline is set before it serves
	px := &Proxy{hc: &http.Client{}, token: "tok", timeout: 100 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(px.handle))
	defer srv.Close()
	px.base = srv.URL + "/tok"

	resp, err := http.Get(px.proxied(origin.URL+"/seg0.ts", "", segment))
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
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.CopyN(w, zeros{}, maxPlaylistBody+1)
	}))
	defer origin.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	resp, err := http.Get(px.URL(upstream.Stream{URL: origin.URL + "/media.m3u8", Kind: upstream.HLS}))
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
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.bin", time.Time{}, bytes.NewReader(full))
	}))
	defer origin.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	req, _ := http.NewRequest(http.MethodGet, px.Opaque(origin.URL+"/x.bin", ""), nil)
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

// an upstream that answers and then drops the connection mid-body must not
// reach the player as a body that ended cleanly, since a download would rename
// that into place as a finished episode
func TestProxyDropsARelayTheUpstreamCutShort(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Write(bytes.Repeat([]byte("v"), 5000))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer cdn.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	tl := px.Tally()

	resp, err := http.Get(tl.URL(upstream.Stream{URL: cdn.URL + "/ep.mp4", Kind: upstream.MP4}))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("relayed %d bytes as a whole body", len(got))
	}
	if tl.Served() != 1 {
		t.Errorf("served = %d, want the body counted as it started", tl.Served())
	}
}

// a buffered body is announced with its length, so a reader that compares what
// arrived against what was announced can do so through the proxy
func TestProxyAnnouncesTheLengthOfABufferedBody(t *testing.T) {
	seg := bytes.Repeat(tsPacket188(), 40)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Write(seg)
	}))
	defer cdn.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	resp, err := http.Get(px.proxied(cdn.URL+"/0.ts", "", segment))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ContentLength != int64(len(seg)) {
		t.Errorf("content length = %d, want %d", resp.ContentLength, len(seg))
	}
}

// tsPacket188 is one transport stream packet, a sync byte and padding
func tsPacket188() []byte {
	p := make([]byte, tsPacket)
	p[0] = 0x47
	return p
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
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") == "" {
			http.Error(w, "referer required", http.StatusForbidden)
			return
		}
		io.WriteString(w, "WEBVTT\n")
	}))
	defer origin.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	subs := px.Subtitles([]upstream.Subtitle{
		{File: origin.URL + "/track.srt", Label: "Portugues (Brasil)", Lang: "pt-BR"},
		{File: origin.URL + "/track", Lang: "en"},
		{File: origin.URL + "/../track.vtt", Label: "../../escape"},
		// sun's shape, a label naming its own extension over a file named .jpeg
		{File: origin.URL + "/track.jpeg", Label: "English.VTT", Lang: "en"},
	}, origin.URL+"/")

	want := []string{"Portugues (Brasil).srt", "en.vtt", "-..-escape.vtt", "English.vtt"}
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
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gone.ts":
			http.Error(w, "gone", http.StatusNotFound)
		case "/denied.ts":
			http.Error(w, "denied", http.StatusForbidden)
		default:
			http.Error(w, "upstream unreachable", http.StatusBadGateway)
		}
	}))
	defer origin.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	for path, want := range map[string]int{
		"/gone.ts":   http.StatusNotFound,
		"/denied.ts": http.StatusForbidden,
		"/dead.ts":   http.StatusBadGateway,
	} {
		resp, err := http.Get(px.proxied(origin.URL+path, "", segment))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s answered %d, want %d", path, resp.StatusCode, want)
		}
	}
}

// the body a refusal answers with reaches mpv or ffmpeg, which show it to
// nobody, so --verbose is where the cause has to surface
func TestProxyLogsARefusalUnderVerbose(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer origin.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	var buf bytes.Buffer
	level := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.DebugLevel)
	defer func() {
		log.SetOutput(os.Stderr)
		log.SetLevel(level)
	}()

	resp, err := http.Get(px.proxied(origin.URL+"/gone.ts", "", segment))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	said := buf.String()
	for _, want := range []string{"relay refused", "status=404", "host=" + strings.TrimPrefix(origin.URL, "http://")} {
		if !strings.Contains(said, want) {
			t.Errorf("the log does not carry %q:\n%s", want, said)
		}
	}
}

// a target the relay cannot address never reached an upstream, so answering it
// as a gateway failure would have the downloader retry what cannot succeed
func TestProxyRefusesAnUnaddressableTarget(t *testing.T) {
	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	for _, raw := range []string{"https:///subbl.example/1_en.srt", "file:///etc/passwd", "https://cdn.example/%zz"} {
		resp, err := http.Get(px.Opaque(raw, ""))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s answered %d, want %d", raw, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

// a payload names whatever a provider's playlist named, so one pointing at this
// machine or its network is forbidden rather than fetched, and forbidden rather
// than failed so the downloader does not retry it
// the playlist case matters most, since the children it names are rewritten
// into payloads without passing anything but the rewriter
func TestProxyRefusesAPrivateTarget(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the relay reached a loopback server at %s", r.URL.Path)
	}))
	defer local.Close()

	px, err := StartProxy(context.Background(), upstream.Public())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	tl := px.Tally()

	for _, u := range []string{
		counted(tl, local.URL+"/seg.ts", "", segment),
		tl.URL(upstream.Stream{URL: local.URL + "/master.m3u8", Kind: upstream.HLS}),
		px.Opaque(local.URL+"/en.vtt", ""),
	} {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s answered %d, want %d", u, resp.StatusCode, http.StatusForbidden)
		}
	}
	if tl.Refused() != 1 {
		t.Errorf("refused = %d, want the one picture body counted", tl.Refused())
	}
}

// Served has to say whether the player got picture, so a playlist, an aes key,
// and a subtitle sidecar must not raise it
func TestProxyServedCountsPictureOnly(t *testing.T) {
	seg := tsBlob(12)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1.0,\n%s\n#EXT-X-ENDLIST\n", "seg0.ts")
		case strings.HasSuffix(r.URL.Path, ".ts"):
			w.Write(seg)
		default:
			io.WriteString(w, "WEBVTT\n")
		}
	}))
	defer origin.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	tl := px.Tally()

	body := httpGetString(t, tl.URL(upstream.Stream{URL: origin.URL + "/media.m3u8", Kind: upstream.HLS}))
	if tl.Served() != 0 {
		t.Errorf("a playlist raised Served to %d", tl.Served())
	}

	httpGetBytes(t, px.Opaque(origin.URL+"/key.bin", ""))
	httpGetString(t, px.Subtitles([]upstream.Subtitle{{File: origin.URL + "/en.vtt", Lang: "en"}}, "")[0].File)
	if tl.Served() != 0 {
		t.Errorf("a key or a sidecar raised Served to %d", tl.Served())
	}

	httpGetBytes(t, firstProxiedLine(t, body, px.base))
	if tl.Served() != 1 {
		t.Errorf("a segment left Served at %d, want 1", tl.Served())
	}

	httpGetBytes(t, tl.URL(upstream.Stream{URL: origin.URL + "/video.mp4", Kind: upstream.MP4}))
	if tl.Served() != 2 {
		t.Errorf("an mp4 body left Served at %d, want 2", tl.Served())
	}
}

// a host that answers 200 with nothing is a dead stream, and counting it would
// tell the caller the player started
func TestProxyServedIgnoresAnEmptyBody(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer origin.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	tl := px.Tally()

	httpGetBytes(t, tl.URL(upstream.Stream{URL: origin.URL + "/video.mp4", Kind: upstream.MP4}))
	if tl.Served() != 0 {
		t.Errorf("an empty body left Served at %d, want 0", tl.Served())
	}
}

// an mp4 is one request that lasts the episode, so counting it when the copy
// finishes would report that nothing had played while the picture was on screen
// and hand the caller grounds to abandon a stream the user is watching
func TestProxyCountsARelayedBodyAsItStarts(t *testing.T) {
	release := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "rest")
	}))
	defer cdn.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	tl := px.Tally()

	body := make(chan struct{})
	go func() {
		defer close(body)
		resp, err := http.Get(tl.Stream(upstream.Stream{URL: cdn.URL + "/v.mp4", Kind: upstream.MP4}).URL)
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	deadline := time.After(10 * time.Second)
	for tl.Served() == 0 {
		select {
		case <-deadline:
			close(release)
			<-body
			t.Fatal("a body still streaming was never counted, which reads as nothing playing")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := tl.Refused(); got != 0 {
		t.Errorf("Refused = %d, want 0 for a body that is arriving", got)
	}
	close(release)
	<-body
}

// a media body the upstream refuses is what tells a dead stream from a slow one
func TestProxyRefused(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/gone"):
			w.WriteHeader(http.StatusForbidden)
		case strings.HasPrefix(r.URL.Path, "/empty"):
		default:
			io.WriteString(w, "#EXTM3U\n")
		}
	}))
	defer cdn.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	tl := px.Tally()

	get := func(u string) { //nolint
		resp, err := http.Get(u)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	// a playlist the upstream refuses is not picture, so it counts as neither
	get(tl.Stream(upstream.Stream{URL: cdn.URL + "/gone.m3u8", Kind: upstream.HLS}).URL)
	if tl.Refused() != 0 || tl.Served() != 0 {
		t.Errorf("a refused playlist counted as %d served and %d refused, want neither",
			tl.Served(), tl.Refused())
	}

	get(tl.Stream(upstream.Stream{URL: cdn.URL + "/gone.mp4", Kind: upstream.MP4}).URL)
	if tl.Refused() != 1 {
		t.Errorf("Refused = %d after one refused media body, want 1", tl.Refused())
	}

	// an upstream that answers 200 with nothing gave the player no picture
	get(tl.Stream(upstream.Stream{URL: cdn.URL + "/empty.mp4", Kind: upstream.MP4}).URL)
	if tl.Refused() != 2 || tl.Served() != 0 {
		t.Errorf("an empty body counted as %d served and %d refused, want 0 and 2",
			tl.Served(), tl.Refused())
	}
}

// a segment CDN that keys on the browser pair sees only what the proxy sends,
// so the origin the referer belongs to has to arrive with it
// hop answers 403 to every segment without it, which reads as a stream that
// resolves, lists its segments, and never relays one
func TestProxySendsTheOriginWithTheReferer(t *testing.T) {
	seg := bytes.Repeat(tsPacket188(), 8)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "https://provider.example" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Write(seg)
	}))
	defer cdn.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	tl := px.Tally()

	resp, err := http.Get(counted(tl, cdn.URL+"/000.ts", "https://provider.example/player", segment))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the CDN refused the fetch with status %d", resp.StatusCode)
	}
	if got, _ := io.ReadAll(resp.Body); len(got) != len(seg) {
		t.Errorf("relayed %d bytes, want %d", len(got), len(seg))
	}
	if tl.Served() != 1 {
		t.Errorf("served = %d, want the segment counted", tl.Served())
	}
}

// the token is what keeps another local process from driving the relay, since
// the proxy will fetch whatever a decoded payload names
// nothing else stands between a request and that fetch, so a path carrying the
// wrong token, or none, must never reach it
func TestProxyRefusesAWrongToken(t *testing.T) {
	var reached atomic.Int64
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Write(bytes.Repeat(tsPacket188(), 8))
	}))
	defer cdn.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	// the address a player is handed, with the token swapped for another
	good := px.proxied(cdn.URL+"/000.ts", "", segment)
	payload := good[strings.LastIndexByte(good, '/')+1:]
	base := good[:strings.Index(good, "/"+px.token)]

	for _, tc := range []struct{ name, path string }{
		{"another token", "/" + strings.Repeat("0", len(px.token)) + "/" + payload},
		{"no token", "/" + payload},
		{"an empty token", "//" + payload},
		{"a truncated token", "/" + px.token[:len(px.token)-1] + "/" + payload},
		{"the token as the payload", "/" + px.token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(base + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status %d, want the request refused", resp.StatusCode)
			}
		})
	}
	if n := reached.Load(); n != 0 {
		t.Errorf("the relay fetched upstream %d times for a request carrying no valid token", n)
	}

	// the same payload under the right token still plays, so the refusals above
	// are the token and not the path
	resp, err := http.Get(good)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the valid token was refused with status %d", resp.StatusCode)
	}
}

// a segment must arrive whole, since the decoy strip and any decryption line up
// against the start of the body, so a Range from the player is answered by the
// proxy's own buffered copy rather than forwarded upstream
// a relayed body such as an mp4 or a sidecar has no such constraint and must
// forward it, or seeking an episode would refetch it from the beginning
func TestProxyForwardsARangeOnlyForARelayedBody(t *testing.T) {
	var ranges sync.Map
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges.Store(r.URL.Path, r.Header.Get("Range"))
		http.ServeContent(w, r, "x", time.Time{}, bytes.NewReader(bytes.Repeat(tsPacket188(), 8)))
	}))
	defer cdn.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	for _, tc := range []struct {
		kind      kind
		path      string
		forwarded bool
	}{
		{segment, "/seg.ts", false},
		{cipher, "/enc.ts", false},
		{playlist, "/list.m3u8", false},
		{media, "/ep.mp4", true},
		{opaque, "/subs.vtt", true},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, px.proxied(cdn.URL+tc.path, "", tc.kind), nil)
			req.Header.Set("Range", "bytes=10-19")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)

			got, _ := ranges.Load(tc.path)
			sent, _ := got.(string)
			if tc.forwarded && sent == "" {
				t.Error("a relayed body did not carry the range upstream, so a seek refetches from the start")
			}
			if !tc.forwarded && sent != "" {
				t.Errorf("a buffered body carried %q upstream, so it can arrive cut and the decoy strip misaligns", sent)
			}
		})
	}
}

// Served counts media bodies, not writes, since it is what tells a stream that
// never produced picture from one the user quit
// a body the relay copies in several chunks is still one body
func TestProxyCountsARelayedBodyOnce(t *testing.T) {
	// larger than the relay's 32k copy buffer, so the body reaches the player in
	// several writes
	body := bytes.Repeat([]byte("v"), 200<<10)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Write(body)
	}))
	defer cdn.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	tl := px.Tally()

	resp, err := http.Get(tl.URL(upstream.Stream{URL: cdn.URL + "/ep.mp4", Kind: upstream.MP4}))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if len(got) != len(body) {
		t.Fatalf("relayed %d bytes, want %d", len(got), len(body))
	}
	if n := tl.Served(); n != 1 {
		t.Errorf("served = %d for one body written in %d chunks, want 1", n, len(body)/(32<<10))
	}
}

// a handler still running for a stream the player just left must not move the
// baseline of the stream that replaced it, which the process-wide counters did
// in both directions, and a payload naming a tally this proxy never issued
// counts nowhere
func TestTallyCountsItsOwnStreamOnly(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write(tsBlob(8))
	}))
	defer cdn.Close()

	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	fetch := func(u string) {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	left, next := px.Tally(), px.Tally()
	fetch(counted(left, cdn.URL+"/gone.ts", "", segment))
	fetch(counted(left, cdn.URL+"/late.ts", "", segment))
	if left.Refused() != 1 || left.Served() != 1 {
		t.Errorf("the left stream counted %d served and %d refused, want 1 and 1", left.Served(), left.Refused())
	}
	if next.Served() != 0 || next.Refused() != 0 {
		t.Errorf("the next stream counted %d served and %d refused for bodies it never asked for", next.Served(), next.Refused())
	}

	forged := px.encode(target{URL: cdn.URL + "/x.ts", Kind: segment, Tally: 999})
	fetch(forged)
	if left.Served() != 1 || next.Served() != 0 {
		t.Error("a payload naming an unknown tally moved a real one")
	}
}

// a proxied address decodes back to the upstream one it relays, so a caller can
// name the host behind an answer, and one this proxy never minted does not
func TestProxyUpstreamReadsTheRelayedAddress(t *testing.T) {
	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	s := upstream.Stream{URL: "https://cdn.example/a/master.m3u8", Kind: upstream.HLS, Referer: "https://ref.example/"}
	if got, err := px.Upstream(px.URL(s)); err != nil || got != s.URL {
		t.Errorf("Upstream = %q, %v, want %q", got, err, s.URL)
	}
	if _, err := px.Upstream("http://127.0.0.1:1/forged/eyJ1IjoiaHR0cHM6Ly94In0.m3u8"); err == nil {
		t.Error("an address under another token decoded")
	}
}
