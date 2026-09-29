package play

import (
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
