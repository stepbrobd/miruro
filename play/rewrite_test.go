package play

import (
	"net/url"
	"strings"
	"testing"
)

func fakeProxy() *Proxy {
	return &Proxy{base: "http://127.0.0.1:9999/tok"}
}

func rewritten(p *Proxy, body, base string) string {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	return string(p.rewrite([]byte(body), "https://ref/", u))
}

func TestRewriteMasterPlaylist(t *testing.T) {
	master := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\n" +
		"360p/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2800000,RESOLUTION=1280x720\n" +
		"https://cdn.example/720p/index.m3u8\n"
	out := rewritten(fakeProxy(), master, "https://cdn.example/stream/master.m3u8")

	if strings.Contains(out, "360p/index.m3u8\n") || strings.Contains(out, "https://cdn.example/720p") {
		t.Errorf("variant urls were not rewritten:\n%s", out)
	}
	if n := strings.Count(out, "http://127.0.0.1:9999/tok/"); n != 2 {
		t.Errorf("want 2 proxied variants, got %d:\n%s", n, out)
	}
	if !strings.HasPrefix(out, "#EXTM3U") {
		t.Error("header line dropped")
	}
}

func TestRewriteMediaPlaylist(t *testing.T) {
	media := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:9.9,\nseg0.ts\n#EXTINF:9.9,\nseg1.ts\n#EXT-X-ENDLIST\n"
	out := rewritten(fakeProxy(), media, "https://cdn.example/stream/media.m3u8")

	if strings.Contains(out, "\nseg0.ts\n") || strings.Contains(out, "\nseg1.ts\n") {
		t.Errorf("segment urls were not rewritten:\n%s", out)
	}
	if n := strings.Count(out, "http://127.0.0.1:9999/tok/"); n != 2 {
		t.Errorf("want 2 proxied segments, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "#EXT-X-ENDLIST") {
		t.Error("tags dropped")
	}
}

func TestRewriteKeyURI(t *testing.T) {
	media := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXTINF:9.9,\nseg0.ts\n"
	out := rewritten(fakeProxy(), media, "https://cdn.example/stream/media.m3u8")

	if strings.Contains(out, "URI=\"key.bin\"") {
		t.Errorf("key uri was not rewritten:\n%s", out)
	}
	if !strings.Contains(out, "URI=\"http://127.0.0.1:9999/tok/") {
		t.Errorf("rewritten key uri missing proxy prefix:\n%s", out)
	}
}

// a declared key makes segments ciphertext, which must not be sync scanned.
func TestRewriteMarksEncryptedSegments(t *testing.T) {
	plain := []byte("#EXTM3U\n#EXTINF:9.9,\nseg0.ts\n")
	if got := childKind(plain); got != segment {
		t.Errorf("plain playlist child kind = %q, want %q", got, segment)
	}

	sealed := []byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:9.9,\nseg0.ts\n")
	if got := childKind(sealed); got != cipher {
		t.Errorf("encrypted playlist child kind = %q, want %q", got, cipher)
	}

	none := []byte("#EXTM3U\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:9.9,\nseg0.ts\n")
	if got := childKind(none); got != segment {
		t.Errorf("METHOD=NONE child kind = %q, want %q", got, segment)
	}
}

// A non-http(s) key URI is data the player decodes itself, so it must survive
// rewriting untouched rather than become a proxy URL that only 502s.
func TestRewriteSkipsDataURI(t *testing.T) {
	media := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"data:text/plain;base64,AAAA\"\n#EXTINF:1,\nseg0.ts\n"
	out := rewritten(fakeProxy(), media, "https://cdn.example/s/media.m3u8")
	if !strings.Contains(out, `URI="data:text/plain;base64,AAAA"`) {
		t.Errorf("data: key URI should pass through untouched:\n%s", out)
	}
}

// EXT-X-MEDIA names a media playlist whatever the uri looks like, so an
// extensionless rendition still has to be rewritten as one.
func TestRewriteExtensionlessRendition(t *testing.T) {
	master := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"English\",URI=\"audio/eng/index?t=1\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000\n" +
		"360p/index.m3u8\n"
	out := rewritten(fakeProxy(), master, "https://cdn.example/stream/master.m3u8")

	if !strings.Contains(out, "URI=\"http://127.0.0.1:9999/tok/") {
		t.Errorf("rendition uri was not rewritten:\n%s", out)
	}
	if !strings.Contains(out, ".m3u8\"") {
		t.Errorf("rendition uri was not marked as a playlist:\n%s", out)
	}
}
