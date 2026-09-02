package play

import (
	"errors"
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
	out, err := p.rewrite([]byte(body), "https://ref/", u, 0)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func filtered(t *testing.T, body string, height int) []byte {
	t.Helper()
	out, err := filterMaster([]byte(body), height)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// a playlist the scanner cannot take apart must not reach the player with its
// upstream urls intact
func TestRewriteRefusesAnUnscannablePlaylist(t *testing.T) {
	body := "#EXTM3U\n#EXTINF:4.0,\n" + strings.Repeat("a", 9<<20) + "\n"
	u, _ := url.Parse("https://cdn.example/x.m3u8")
	if _, err := fakeProxy().rewrite([]byte(body), "", u, 0); !errors.Is(err, errPlaylist) {
		t.Errorf("err = %v, want %v", err, errPlaylist)
	}
	master := "#EXTM3U\n#EXT-X-STREAM-INF:RESOLUTION=1280x720\n" + strings.Repeat("a", 9<<20) + "\n"
	if _, err := filterMaster([]byte(master), 720); !errors.Is(err, errPlaylist) {
		t.Errorf("filterMaster err = %v, want %v", err, errPlaylist)
	}
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

// a declared key makes segments ciphertext, which the sync scan must leave alone
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

// a non-http key URI is decoded by the player itself, so it survives rewriting
// untouched instead of becoming a proxy URL that only 502s
func TestRewriteSkipsDataURI(t *testing.T) {
	media := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"data:text/plain;base64,AAAA\"\n#EXTINF:1,\nseg0.ts\n"
	out := rewritten(fakeProxy(), media, "https://cdn.example/s/media.m3u8")
	if !strings.Contains(out, `URI="data:text/plain;base64,AAAA"`) {
		t.Errorf("data: key URI should pass through untouched:\n%s", out)
	}
}

// a stream restricted to one height must lose the other variants and nothing
// else, or the player would still negotiate its own pick from the full master
func TestFilterMaster(t *testing.T) {
	master := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"ja\",URI=\"audio/index.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=1920x1080,AUDIO=\"a\"\n" +
		"1080/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=500,RESOLUTION=1280x720,AUDIO=\"a\"\n" +
		"720/index.m3u8\n"

	out := string(filtered(t, master, 720))
	if strings.Contains(out, "1080/index.m3u8") || strings.Contains(out, "RESOLUTION=1920x1080") {
		t.Errorf("the other variant survived:\n%s", out)
	}
	if !strings.Contains(out, "720/index.m3u8") {
		t.Errorf("the wanted variant was dropped:\n%s", out)
	}
	if !strings.Contains(out, "#EXT-X-MEDIA:TYPE=AUDIO") {
		t.Errorf("the audio rendition was dropped:\n%s", out)
	}

	// a height nothing carries keeps the master whole, since an empty master
	// plays nothing at all
	if got := string(filtered(t, master, 480)); got != master {
		t.Errorf("an absent height changed the master:\n%s", got)
	}

	// a media playlist has no variants to restrict
	media := "#EXTM3U\n#EXTINF:1,\nseg0.ts\n#EXT-X-ENDLIST\n"
	if got := string(filtered(t, media, 720)); got != media {
		t.Errorf("a media playlist was rewritten:\n%s", got)
	}
}

// the restriction rides the proxied stream URL, so the height set on a Stream
// has to survive the payload round trip into the rewrite
func TestRewriteRestrictsToStreamHeight(t *testing.T) {
	master := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=1920x1080\n" +
		"1080/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=500,RESOLUTION=1280x720\n" +
		"720/index.m3u8\n"
	u, err := url.Parse("https://cdn.example/stream/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	rewrote, err := fakeProxy().rewrite([]byte(master), "https://ref/", u, 1080)
	if err != nil {
		t.Fatal(err)
	}
	out := string(rewrote)
	if strings.Contains(out, "RESOLUTION=1280x720") {
		t.Errorf("the restricted variant survived:\n%s", out)
	}
	if n := strings.Count(out, "http://127.0.0.1:9999/tok/"); n != 1 {
		t.Errorf("want 1 proxied variant, got %d:\n%s", n, out)
	}
}

// EXT-X-MEDIA names a media playlist whatever the uri looks like, so an
// extensionless rendition still has to be rewritten as one
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
