package play

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ysun.co/miruro/internal/upstream"
)

// walk fetches a proxied playlist and everything it names, renditions and
// playlists all the way down, the way a player starts a stream
func walk(t *testing.T, u string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(body), "#EXTM3U") {
		return
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if _, uri, ok := strings.Cut(line, `URI="`); ok {
			line, _, _ = strings.Cut(uri, `"`)
		}
		if strings.HasPrefix(line, "http") {
			walk(t, line)
		}
	}
}

// a segment the CDN answers with no body at all showed the player nothing, so
// it counts as refused rather than served
func TestTallyTakesAnEmptySegmentForRefused(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media.m3u8" {
			io.WriteString(w, "#EXTM3U\n#EXTINF:1,\ns0.ts\n#EXT-X-ENDLIST\n")
		}
	}))
	defer cdn.Close()
	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	tl := px.Tally()
	walk(t, tl.Stream(upstream.Stream{URL: cdn.URL + "/media.m3u8", Kind: upstream.HLS}).URL)
	if tl.Served() != 0 || tl.Refused() != 1 {
		t.Errorf("served %d refused %d, want the empty segment refused", tl.Served(), tl.Refused())
	}
}

// segmentBody is eight aligned transport stream packets, a body the proxy
// passes as a segment
var segmentBody = bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 8)

// serveHLS serves a master and what it names from bodies keyed by path, and 404s
// anything else
func serveHLS(t *testing.T, bodies map[string]string) (*Proxy, string) {
	t.Helper()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch body, ok := bodies[r.URL.Path]; {
		case ok:
			io.WriteString(w, body)
		case strings.HasSuffix(r.URL.Path, ".ts"):
			w.Write(segmentBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cdn.Close)
	px, err := StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { px.Close() })
	return px, cdn.URL + "/master.m3u8"
}

// a stream carrying its sound inside the video names no audio rendition, so
// however it plays it is never said to play without sound
func TestTallyNeverCallsAMuxedStreamSilent(t *testing.T) {
	px, master := serveHLS(t, map[string]string{
		"/master.m3u8": "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nvideo.m3u8\n",
		"/video.m3u8":  "#EXTM3U\n#EXTINF:1,\nv0.ts\n#EXT-X-ENDLIST\n",
	})
	tl := px.Tally()
	walk(t, tl.Stream(upstream.Stream{URL: master, Kind: upstream.HLS}).URL)
	if tl.Served() != 1 || tl.Silent() {
		t.Errorf("served %d silent %v, want the muxed stream playing and not silent", tl.Served(), tl.Silent())
	}
}

// a subtitle rendition is no sound, so its playlist relayed with its cues
// refused leaves a stream playing its muxed sound not silent
func TestTallyTakesNoSubtitleRenditionForSound(t *testing.T) {
	px, master := serveHLS(t, map[string]string{
		"/master.m3u8": "#EXTM3U\n" + `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="s",NAME="en",URI="subs.m3u8"` + "\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=1,SUBTITLES=\"s\"\nvideo.m3u8\n",
		"/video.m3u8": "#EXTM3U\n#EXTINF:1,\nv0.ts\n#EXT-X-ENDLIST\n",
		"/subs.m3u8":  "#EXTM3U\n#EXTINF:1,\ns0.vtt\n#EXT-X-ENDLIST\n",
	})
	tl := px.Tally()
	walk(t, tl.Stream(upstream.Stream{URL: master, Kind: upstream.HLS}).URL)
	if tl.Served() != 1 || tl.Silent() {
		t.Errorf("served %d silent %v, want the picture playing and a subtitle cue not taken for sound", tl.Served(), tl.Silent())
	}
}
