package miruro

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestParseHeight(t *testing.T) {
	cases := map[string]int{
		"1080p": 1080, "720": 720, " 480p ": 480,
		"": 0, "best": 0, "worst": 0, "p": 0, "-5": 0,
	}
	for in, want := range cases {
		if got := parseHeight(in); got != want {
			t.Errorf("parseHeight(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestPickQuality(t *testing.T) {
	streams := []Stream{
		{URL: "360", Quality: "360p"},
		{URL: "1080", Quality: "1080p"},
		{URL: "720", Quality: "720p"},
		{URL: "1440", Quality: "1440p"},
		{URL: "raw", Quality: ""}, // unlabelled, ignored
	}
	check := func(q, wantURL string, wantOK bool) {
		t.Helper()
		s, ok := pickQuality(streams, q)
		if ok != wantOK || (ok && s.URL != wantURL) {
			t.Errorf("pickQuality(%q) = (%q, %v), want (%q, %v)", q, s.URL, ok, wantURL, wantOK)
		}
	}
	check("", "1440", true)     // default is best
	check("best", "1440", true) // tallest
	check("worst", "360", true) // shortest
	check("720p", "720", true)  // exact
	check("720", "720", true)   // exact, no suffix
	check("144p", "", false)    // 144 must not match 1440
	check("2160p", "", false)   // absent height

	if _, ok := pickQuality([]Stream{{Quality: ""}, {Quality: "auto"}}, "720p"); ok {
		t.Error("pickQuality matched on streams with no usable height label")
	}
}

// failTransport fails the test on any network use
// a labelled request must be satisfied without touching the wire
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected network call to %s", r.URL)
	return nil, errors.New("network use forbidden")
}

// top is the stream a caller plays, the head of Rank
func top(t *testing.T, c *Client, r *Result, quality string) Stream {
	t.Helper()
	ranked := c.Rank(context.Background(), r, quality)
	if len(ranked) == 0 {
		t.Fatal("Rank returned nothing playable")
	}
	return ranked[0]
}

func TestRankHead(t *testing.T) {
	ctx := context.Background()

	t.Run("labelled hls match needs no network", func(t *testing.T) {
		c := &Client{HTTP: &http.Client{Transport: failTransport{t}}}
		r := &Result{Streams: []Stream{
			{URL: "u1080", Kind: HLS, Quality: "1080p"},
			{URL: "u720", Kind: HLS, Quality: "720p"},
		}}
		s := top(t, c, r, "720p")
		if s.URL != "u720" {
			t.Errorf("selected %q, want u720", s.URL)
		}
	})

	t.Run("unlabelled hls expands the master", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "#EXTM3U\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1920x1080\nindex-1080.m3u8\n"+
				"#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1280x720\nindex-720.m3u8\n")
		}))
		defer srv.Close()

		c := &Client{HTTP: srv.Client()}
		r := &Result{Streams: []Stream{{URL: srv.URL + "/master.m3u8", Kind: HLS}}}
		s := top(t, c, r, "720p")
		if want := srv.URL + "/index-720.m3u8"; s.URL != want {
			t.Errorf("selected %q, want %q", s.URL, want)
		}
		if s.Quality != "720p" {
			t.Errorf("quality = %q, want 720p", s.Quality)
		}
	})

	t.Run("failed master expansion falls back to the first hls", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()

		c := &Client{HTTP: srv.Client()}
		first := Stream{URL: srv.URL + "/master.m3u8", Kind: HLS}
		r := &Result{Streams: []Stream{first, {URL: srv.URL + "/other.m3u8", Kind: HLS}}}
		s := top(t, c, r, "720p")
		if s.URL != first.URL {
			t.Errorf("selected %q, want the first hls %q", s.URL, first.URL)
		}
	})

	t.Run("mp4 only picks by label then falls back", func(t *testing.T) {
		c := &Client{HTTP: &http.Client{Transport: failTransport{t}}}
		r := &Result{Streams: []Stream{
			{URL: "m480", Kind: MP4, Quality: "480p"},
			{URL: "m720", Kind: MP4, Quality: "720p"},
		}}
		s := top(t, c, r, "720p")
		if s.URL != "m720" {
			t.Errorf("selected %q, want m720", s.URL)
		}
		s = top(t, c, r, "2160p")
		if s.URL != "m480" {
			t.Errorf("selected %q, want the first mp4 m480", s.URL)
		}
	})

	t.Run("empty result ranks nothing", func(t *testing.T) {
		c := &Client{HTTP: &http.Client{Transport: failTransport{t}}}
		if got := c.Rank(ctx, &Result{}, "best"); len(got) != 0 {
			t.Errorf("Rank over an empty result = %v, want nothing playable", got)
		}
	})
}

// a token-signed master can redirect to another host or path, so relative
// variants must resolve against the URL the master was served from
func TestExpandMasterRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/cdn/v2/master.m3u8", http.StatusFound)
	})
	mux.HandleFunc("/cdn/v2/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1,RESOLUTION=1920x1080\nindex-1080.m3u8\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{HTTP: srv.Client()}
	variants, err := c.expandMaster(context.Background(), Stream{URL: srv.URL + "/master.m3u8", Kind: HLS})
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(variants))
	}
	if want := srv.URL + "/cdn/v2/index-1080.m3u8"; variants[0].URL != want {
		t.Errorf("variant URL = %q, want %q", variants[0].URL, want)
	}
	if variants[0].Quality != "1080p" {
		t.Errorf("variant quality = %q, want %q", variants[0].Quality, "1080p")
	}
}

// the api mirrors the html5 track kinds, so a thumbnails sprite index arrives on
// the same list as dialogue and must never reach a player as subtitles
func TestSourcesKeepsOnlyDialogueTracks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"streams":[{"url":"u","type":"hls"}],"subtitles":[
			{"file":"thumbs.vtt","label":"thumbnails","kind":"thumbnails"},
			{"file":"en.vtt","label":"English","kind":"captions","language":"en","default":true},
			{"file":"pt.vtt","label":"Portugues","kind":"subtitles","language":"pt-BR"},
			{"file":"bare.vtt","label":"Bare"}]}`)
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	res, err := c.Sources(context.Background(), "ep", "bonk", Sub)
	if err != nil {
		t.Fatal(err)
	}
	want := []Subtitle{
		{File: "en.vtt", Label: "English", Lang: "en", Default: true},
		{File: "pt.vtt", Label: "Portugues", Lang: "pt-BR"},
		{File: "bare.vtt", Label: "Bare"},
	}
	if !reflect.DeepEqual(res.Subtitles, want) {
		t.Errorf("subtitles = %+v, want %+v", res.Subtitles, want)
	}
}

func TestOrder(t *testing.T) {
	en := Subtitle{Label: "English", Lang: "en"}
	es := Subtitle{Label: "Spanish", Lang: "es", Default: true}
	pt := Subtitle{Label: "Portugues", Lang: "pt-BR"}
	subs := []Subtitle{pt, en, es}

	first := func(lang string) string {
		t.Helper()
		out := Order(subs, lang)
		if len(out) != len(subs) {
			t.Fatalf("Order(%q) returned %d tracks, want %d", lang, len(out), len(subs))
		}
		return out[0].Label
	}

	if got := first(""); got != "Spanish" {
		t.Errorf("with no preference the provider default leads, got %q", got)
	}
	if got := first("en"); got != "English" {
		t.Errorf("Order by tag = %q, want English", got)
	}
	if got := first("English"); got != "English" {
		t.Errorf("Order by label = %q, want English", got)
	}
	if got := first("pt"); got != "Portugues" {
		t.Errorf("a primary subtag must select its regional track, got %q", got)
	}
	if got := first("de"); got != "Spanish" {
		t.Errorf("an absent language falls back to the default track, got %q", got)
	}

	// the input must survive, since the caller still holds it
	if subs[0].Label != "Portugues" {
		t.Error("Order reordered the slice it was given")
	}
}

// one provider serves an episode from several hosts, and the one it lists first
// can be the dead one, so the rest have to stay reachable behind it
func TestRank(t *testing.T) {
	ctx := context.Background()
	c := &Client{HTTP: &http.Client{Transport: failTransport{t}}}

	r := &Result{Streams: []Stream{
		{URL: "hd1", Kind: HLS, Quality: "1080p"},
		{URL: "embed", Kind: Embed},
		{URL: "hd2", Kind: HLS, Quality: "720p"},
		{URL: "direct", Kind: MP4, Quality: "480p"},
	}}

	urls := func(quality string) []string {
		t.Helper()
		var out []string
		for _, s := range c.Rank(ctx, r, quality) {
			out = append(out, s.URL)
		}
		return out
	}

	if got, want := urls("best"), []string{"hd1", "hd2", "direct"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Rank(best) = %v, want %v", got, want)
	}
	// the quality pick leads and is not repeated further down
	if got, want := urls("720p"), []string{"hd2", "hd1", "direct"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Rank(720p) = %v, want %v", got, want)
	}
	if got := c.Rank(ctx, &Result{Streams: []Stream{{URL: "embed", Kind: Embed}}}, "best"); len(got) != 0 {
		t.Errorf("Rank over an embed-only result = %v, want nothing playable", got)
	}

	// the order the api lists streams in is not a promise, the flag is
	flagged := &Result{Streams: []Stream{
		{URL: "second", Kind: HLS, Server: "HD-2"},
		{URL: "first", Kind: HLS, Server: "HD-1", Default: true},
	}}
	if got := c.Rank(ctx, flagged, "best"); len(got) != 2 || got[0].URL != "first" || got[1].URL != "second" {
		t.Errorf("Rank ignored the provider's default flag: %+v", got)
	}
}
