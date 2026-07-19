package play

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ysun.co/miruro"
)

func TestProxyServesNormalizedHLS(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://ref/" {
			t.Errorf("referer not forwarded upstream: %q", r.Header.Get("Referer"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "media.m3u8"):
			fmt.Fprintf(w, "#EXTM3U\n#EXTINF:1.0,\n%s/seg0.ts\n#EXT-X-ENDLIST\n", upstream.URL)
		case strings.HasSuffix(r.URL.Path, "seg0.ts"):
			w.Write(append([]byte("\x89PNG-decoy-bytes"), tsBlob(12)...))
		}
	}))
	defer upstream.Close()

	px, err := StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	playlistURL := px.URL(miruro.Stream{URL: upstream.URL + "/media.m3u8", Kind: miruro.HLS, Referer: "https://ref/"})
	body := httpGetString(t, playlistURL)
	if strings.Contains(body, upstream.URL) {
		t.Errorf("segment URL not rewritten to proxy:\n%s", body)
	}

	segURL := firstProxiedLine(t, body, px.base)
	seg := httpGetBytes(t, segURL)
	if len(seg) == 0 || seg[0] != 0x47 {
		t.Fatalf("proxied segment not normalized to TS sync")
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
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), base) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no proxied URL in playlist:\n%s", body)
	return ""
}
