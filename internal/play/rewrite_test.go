package play

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func fakeProxy() *Proxy {
	return &Proxy{base: "http://127.0.0.1:9999/tok", token: "tok"}
}

func rewritten(p *Proxy, body, base string) string {
	u, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	out, err := p.rewrite([]byte(body), target{Referer: "https://ref/"}, u)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// a byterange playlist slices one resource, and a segment fetched whole for
// every slice would hand the player the wrong offset each time
func TestRewriteRelaysByterangeEntries(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\n#EXT-X-BYTERANGE:14852@0\nep.ts\n#EXTINF:4.0,\n#EXT-X-BYTERANGE:12345@14852\nep.ts\n#EXT-X-ENDLIST\n"
	out := rewritten(fakeProxy(), body, "https://cdn.example/ep.m3u8")
	if strings.Contains(out, ".ts\n") {
		t.Errorf("byterange entries were rewritten as segments:\n%s", out)
	}
	p := fakeProxy()
	target, err := p.decode(strings.TrimPrefix(firstLine(out, "http://"), "http://127.0.0.1:9999"))
	if err != nil {
		t.Fatal(err)
	}
	if target.Kind != media || target.URL != "https://cdn.example/ep.ts" {
		t.Errorf("entry = %+v, want a media relay of the resource", target)
	}
}

func firstLine(body, prefix string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
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
	if _, err := fakeProxy().rewrite([]byte(body), target{}, u); !errors.Is(err, errPlaylist) {
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
	rewrote, err := fakeProxy().rewrite([]byte(master), target{Referer: "https://ref/", Height: 1080}, u)
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

// hop's dub and soft sub share one master of eight languages with Japanese
// marked default, so the language the provider means has to become the
// default, or every player and download takes Japanese on a dub run
func TestRewriteDefaultsToTheMeantAudio(t *testing.T) {
	var master strings.Builder
	master.WriteString("#EXTM3U\n")
	for range 2 {
		for _, lang := range []string{"hin", "eng", "jpn"} {
			def := ""
			if lang == "jpn" {
				def = "DEFAULT=YES,"
			}
			master.WriteString(`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="stereo",NAME="` + lang + `",` + def + `LANGUAGE="` + lang + `",URI="` + lang + `.m3u8"` + "\n")
		}
	}
	master.WriteString(`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",DEFAULT=YES,LANGUAGE="eng",URI="subs.m3u8"` + "\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,AUDIO=\"stereo\",SUBTITLES=\"subs\"\nv.m3u8\n")
	u, err := url.Parse("https://cdn.example/master.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	rewrite := func(lang string) map[string][]string {
		t.Helper()
		out, err := fakeProxy().rewrite([]byte(master.String()), target{Referer: "https://ref/", Lang: lang}, u)
		if err != nil {
			t.Fatal(err)
		}
		flags := map[string][]string{}
		for line := range strings.SplitSeq(string(out), "\n") {
			if a := attributes(line); strings.HasPrefix(line, "#EXT-X-MEDIA:") {
				flags[a["TYPE"]+" "+a["LANGUAGE"]] = append(flags[a["TYPE"]+" "+a["LANGUAGE"]], a["DEFAULT"])
			}
		}
		return flags
	}

	dub := rewrite("en-US")
	for key, want := range map[string]string{"AUDIO eng": "YES", "AUDIO jpn": "NO", "AUDIO hin": "NO", "SUBTITLES eng": "YES"} {
		if got := dub[key]; len(got) == 0 || got[0] != want || got[len(got)-1] != want {
			t.Errorf("%s is DEFAULT=%v on a dub, want %s", key, got, want)
		}
	}
	// a language the master does not carry leaves its own default standing
	if got := rewrite("ko")["AUDIO jpn"]; len(got) != 2 || got[0] != "YES" {
		t.Errorf("jpn is DEFAULT=%v for an absent language, want its own YES", got)
	}
	if got := rewrite("")["AUDIO eng"]; len(got) != 2 || got[0] != "" {
		t.Errorf("eng is DEFAULT=%v with no language meant, want untouched", got)
	}
}

// setAttr replaces an enumerated attribute where the tag carries it, adds
// DEFAULT where it does not, and never invents an AUTOSELECT
func TestSetAttr(t *testing.T) {
	for _, tc := range []struct{ line, key, value, want string }{
		{`#EXT-X-MEDIA:TYPE=AUDIO,DEFAULT=YES,URI="a"`, "DEFAULT", "NO", `#EXT-X-MEDIA:TYPE=AUDIO,DEFAULT=NO,URI="a"`},
		{`#EXT-X-MEDIA:TYPE=AUDIO,URI="a"`, "DEFAULT", "YES", `#EXT-X-MEDIA:TYPE=AUDIO,URI="a",DEFAULT=YES`},
		{`#EXT-X-MEDIA:TYPE=AUDIO,AUTOSELECT=NO,URI="a"`, "AUTOSELECT", "YES", `#EXT-X-MEDIA:TYPE=AUDIO,AUTOSELECT=YES,URI="a"`},
		{`#EXT-X-MEDIA:TYPE=AUDIO,URI="a"`, "AUTOSELECT", "YES", `#EXT-X-MEDIA:TYPE=AUDIO,URI="a"`},
		{`#EXT-X-MEDIA:DEFAULT=NO,TYPE=AUDIO` + "\r", "DEFAULT", "YES", `#EXT-X-MEDIA:DEFAULT=YES,TYPE=AUDIO` + "\r"},
	} {
		if got := setAttr(tc.line, tc.key, tc.value); got != tc.want {
			t.Errorf("setAttr(%q, %s, %s) = %q, want %q", tc.line, tc.key, tc.value, got, tc.want)
		}
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

// a stream restricted to no height is every height, so a master must pass
// through whole rather than be filtered down to the variants that happen to
// carry no resolution
func TestFilterMasterKeepsEverythingWithoutAHeight(t *testing.T) {
	master := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000\nplain.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1920x1080\nhd.m3u8\n"

	for _, height := range []int{0, -1} {
		got, err := filterMaster([]byte(master), height)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != master {
			t.Errorf("height %d filtered the master instead of passing it through:\n%s", height, got)
		}
	}

	// a height the master does carry keeps only those variants
	got, err := filterMaster([]byte(master), 1080)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "plain.m3u8") || !strings.Contains(string(got), "hd.m3u8") {
		t.Errorf("height 1080 kept the wrong variants:\n%s", got)
	}
}

// a playlist written with CRLF endings names the same children, and a carriage
// return left on a url would send the proxy somewhere else entirely
func TestRewriteReadsACRLFPlaylist(t *testing.T) {
	body := "#EXTM3U\r\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\r\n#EXTINF:4.0,\r\nseg0.ts\r\n#EXT-X-ENDLIST\r\n"
	p := fakeProxy()
	out := rewritten(p, body, "https://cdn.example/a/media.m3u8")
	var urls []string
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "#EXT-X-KEY"):
			urls = append(urls, strings.SplitN(strings.SplitN(line, `URI="`, 2)[1], `"`, 2)[0])
		case strings.HasPrefix(line, "http://"):
			urls = append(urls, line)
		}
	}
	want := []string{"https://cdn.example/a/key.bin", "https://cdn.example/a/seg0.ts"}
	if len(urls) != len(want) {
		t.Fatalf("rewrote %d urls, want %d:\n%s", len(urls), len(want), out)
	}
	for i, u := range urls {
		tgt, err := p.decode(strings.TrimPrefix(u, "http://127.0.0.1:9999"))
		if err != nil {
			t.Fatal(err)
		}
		if tgt.URL != want[i] {
			t.Errorf("child %d reaches %q, want %q", i, tgt.URL, want[i])
		}
	}
	if strings.Contains(out, "\r") {
		t.Errorf("a carriage return survived the rewrite:\n%q", out)
	}
}
