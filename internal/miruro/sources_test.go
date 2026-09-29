package miruro

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/upstream"
)

// config names four providers the way the live table does, one of them hidden
// and one an embed child
const config = `{"streaming":{
	"anikoto":{"label":"bee","visible":true,"parent":null,"relationship":null},
	"kickassanime":{"label":"hop","visible":true,"parent":null,"relationship":null},
	"animepahe":{"label":"kiwi","visible":true,"parent":null,"relationship":null},
	"allmanga":{"label":"ally","visible":false,"parent":null,"relationship":null},
	"kwik":{"label":"telli","visible":true,"parent":"animepahe","relationship":"embed"}
},"providerOrder":["anikoto","kickassanime","animepahe"]}`

// play is episode 1 as the live play resource shaped it on 2026-09-29
// kiwi's sub server is embed only, hop serves ssub and dub, bee serves ssub and
// dub from a server asking for an origin its referer names, and the hidden
// ally and an unnamed provider both answer
const play = `{"episode_number":1,"requested_track":null,"tracks":[
	{"track":"sub","providers":[
		{"provider":"animepahe","subtitles":[],"thumbnails":null,"downloads":[],"servers":[
			{"server":"animepahe","headers":{"Referer":"https://kwik.cx/"},"streams":[],"embed":{"url":"https://kwik.cx/e/x"}}]},
		{"provider":"allmanga","subtitles":[],"servers":[
			{"server":"Mp4","headers":{},"streams":[{"url":"https://cdn.example/ally.mp4","format":"mp4","quality":null}]}]}]},
	{"track":"ssub","providers":[
		{"provider":"kickassanime","subtitles":[
			{"language":"en","label":"English","file":"https://subst.example/en.vtt","format":"vtt","default":true},
			{"language":"pt","label":"Portugues","file":"https:///subbl.example/1_pt.srt","format":"srt","default":false},
			{"language":"en","label":"sprites","file":"https://subst.example/thumbs.jpg","format":"jpg","default":false}],
		 "servers":[{"server":"Vid","headers":{"Referer":"https://krussdomi.com/"},"streams":[
			{"url":"https://hls.example/master.m3u8","format":"hls","quality":null},
			{"url":"https://hls.example/master.mpd","format":"dash","quality":null},
			{"url":"","format":"hls","quality":null}]}]},
		{"provider":"anikoto","subtitles":[],"servers":[
			{"server":"HD-1","headers":{"Origin":"https://megaplay.buzz","Referer":"https://megaplay.buzz/"},"streams":[
				{"url":"https://fetch.example/master.m3u8","format":"hls","quality":"1080p"}]},
			{"server":"HD-2","headers":{"Referer":"https://megaplay.buzz/","Cookie":"x"},"streams":[
				{"url":"https://cookie.example/master.m3u8","format":"hls","quality":null}]}]},
		{"provider":"mystery","subtitles":[],"servers":[
			{"server":"X","headers":{},"streams":[{"url":"https://x.example/x.m3u8","format":"hls"}]}]}]},
	{"track":"dub","providers":[
		{"provider":"kickassanime","subtitles":[],"servers":[
			{"server":"Vid","headers":{"Referer":"https://krussdomi.com/"},"streams":[{"url":"https://hls.example/dub.m3u8","format":"hls"}]}]},
		{"provider":"animepahe","subtitles":[],"servers":[
			{"server":"animepahe","headers":{"Referer":"https://kwik.cx/"},"streams":[{"url":"https://vault.example/uwu.m3u8","format":"hls","quality":"720p"}]}]}]},
	{"track":"raw","providers":[
		{"provider":"animepahe","subtitles":[],"servers":[
			{"server":"animepahe","headers":{},"streams":[{"url":"https://vault.example/raw.m3u8","format":"hls"}]}]}]}]}`

const playPath = "/api/v1/anime/o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm/episodes/1/play"

func TestListing(t *testing.T) {
	srv, asked := api(t, map[string]string{"/api/config": config, playPath: play})
	c := client(srv)

	l, err := c.Listing(context.Background(), "o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm", 1)
	if err != nil {
		t.Fatal(err)
	}
	// kiwi's sub is an embed, ally is hidden, and mystery is nobody the table
	// names, so none of those is listed under sub
	if got := slices.Sorted(maps.Keys(l.Providers)); !slices.Equal(got, []string{"bee", "hop", "kiwi"}) {
		t.Fatalf("providers = %v, want bee, hop and kiwi", got)
	}
	caps := upstream.Capabilities{"bee": {Soft: true}, "hop": {Soft: true}}
	if !reflect.DeepEqual(l.Caps, caps) {
		t.Errorf("caps = %+v, want %+v", l.Caps, caps)
	}

	hop := l.Providers["hop"]
	if hop.Backend != c || hop.Code != "hop" {
		t.Errorf("hop = %+v, want it pointing back at the client", hop)
	}
	id := "o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm/1/kickassanime"
	if len(hop.Sub) != 1 || hop.Sub[0].ID != id || len(hop.Dub) != 1 || hop.Dub[0].ID != id {
		t.Errorf("hop = %+v, want the episode under sub and dub keyed to kickassanime", hop)
	}
	if kiwi := l.Providers["kiwi"]; len(kiwi.Sub) != 0 || len(kiwi.Dub) != 1 {
		t.Errorf("kiwi = %+v, want the dub only", kiwi)
	}

	// Sources answers from the same resource, on the track it is asked for
	res, err := c.Sources(context.Background(), id, "hop", upstream.Ssub)
	if err != nil {
		t.Fatal(err)
	}
	streams := []upstream.Stream{{URL: "https://hls.example/master.m3u8", Kind: upstream.HLS, Referer: "https://krussdomi.com/", Server: "Vid"}}
	if !reflect.DeepEqual(res.Streams, streams) {
		t.Errorf("streams = %+v, want %+v", res.Streams, streams)
	}
	subs := []upstream.Subtitle{
		{File: "https://subst.example/en.vtt", Label: "English", Lang: "en", Default: true},
		{File: "https://subbl.example/1_pt.srt", Label: "Portugues", Lang: "pt"},
	}
	if !reflect.DeepEqual(res.Subtitles, subs) {
		t.Errorf("subtitles = %+v, want %+v", res.Subtitles, subs)
	}

	res, err = c.Sources(context.Background(), "o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm/1/anikoto", "bee", upstream.Ssub)
	if err != nil {
		t.Fatal(err)
	}
	// the server asking for a cookie is one the proxy cannot reach
	streams = []upstream.Stream{{URL: "https://fetch.example/master.m3u8", Kind: upstream.HLS, Quality: "1080p", Referer: "https://megaplay.buzz/", Server: "HD-1"}}
	if !reflect.DeepEqual(res.Streams, streams) {
		t.Errorf("streams = %+v, want %+v", res.Streams, streams)
	}

	// a rendition the provider does not serve is no stream rather than an empty one
	if _, err := c.Sources(context.Background(), id, "hop", upstream.Sub); !errors.Is(err, upstream.ErrNoStream) {
		t.Errorf("err = %v, want %v", err, upstream.ErrNoStream)
	}

	// the table is read once, and every resolution asks the play resource again
	want := []string{"/api/config", playPath, playPath, playPath, playPath}
	if got := asked(); !slices.Equal(got, want) {
		t.Errorf("asked %v, want %v", got, want)
	}
}

func TestListingRefusesAFractionalEpisode(t *testing.T) {
	srv, asked := api(t, map[string]string{"/api/config": config})
	if _, err := client(srv).Listing(context.Background(), "t", 1.5); err == nil {
		t.Error("a fractional episode was listed")
	}
	if got := asked(); len(got) != 0 {
		t.Errorf("asked %v for an episode no request can address", got)
	}
}

func TestParseKey(t *testing.T) {
	key := episodeKey{title: "o2Eqmv4w0JYgQJWQFi9CDDdH-PgffNpm", number: 12, provider: "kickassanime"}
	if got, err := parseKey(key.String()); err != nil || got != key {
		t.Errorf("parseKey(%q) = %+v, %v, want the key back", key.String(), got, err)
	}
	for _, bad := range []string{"", "t/1", "t/x/p", "t/0/p", "ally-1"} {
		if _, err := parseKey(bad); err == nil {
			t.Errorf("parseKey(%q) accepted", bad)
		}
	}
}

// the proxy sends a referer and the origin it belongs to, so a server asking
// for anything else is one it cannot reach
func TestReferer(t *testing.T) {
	for _, tc := range []struct {
		headers map[string]string
		want    string
		ok      bool
	}{
		{nil, "", true},
		{map[string]string{"Referer": "https://kwik.cx/"}, "https://kwik.cx/", true},
		{map[string]string{"referer": "https://kwik.cx/"}, "https://kwik.cx/", true},
		{map[string]string{"Origin": "https://ultracloud.cc", "Referer": "https://ultracloud.cc/"}, "https://ultracloud.cc/", true},
		{map[string]string{"Origin": "https://elsewhere.example", "Referer": "https://ultracloud.cc/"}, "", false},
		{map[string]string{"Referer": "https://kwik.cx/", "User-Agent": "x"}, "", false},
	} {
		got, ok := referer(tc.headers)
		if got != tc.want || ok != tc.ok {
			t.Errorf("referer(%v) = %q, %v, want %q, %v", tc.headers, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBrowserURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://cdn.example/a//b.vtt":       "https://cdn.example/a//b.vtt",
		"https:///cdn.example/a.vtt":         "https://cdn.example/a.vtt",
		"http:////cdn.example/a.vtt":         "http://cdn.example/a.vtt",
		"HTTPS:///cdn.example/a.vtt":         "HTTPS://cdn.example/a.vtt",
		"ftp:///cdn.example/a.vtt":           "ftp:///cdn.example/a.vtt",
		"/a.vtt?next=https:///cdn.example/b": "/a.vtt?next=https:///cdn.example/b",
		"https:/cdn.example/a.vtt":           "https:/cdn.example/a.vtt",
		"en.vtt":                             "en.vtt",
		"":                                   "",
	} {
		if got := browserURL(raw); got != want {
			t.Errorf("browserURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestProviderCodes(t *testing.T) {
	ctx := context.Background()

	t.Run("names providers the way the site does and only the ones it offers", func(t *testing.T) {
		srv, asked := api(t, map[string]string{"/api/config": config})
		c := client(srv)
		for range 3 {
			codes, err := c.providerCodes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"anikoto": "bee", "kickassanime": "hop", "animepahe": "kiwi"}
			if !reflect.DeepEqual(codes, want) {
				t.Fatalf("codes = %v, want %v", codes, want)
			}
		}
		if got := asked(); len(got) != 1 {
			t.Errorf("the table was fetched %d times, want once", len(got))
		}
	})

	// the site's banners announce outages and slowdowns, which is what a run
	// that turns slow should be able to show under --verbose
	t.Run("an enabled notice is said once", func(t *testing.T) {
		srv, _ := api(t, map[string]string{"/api/config": `{"streaming":{},"messages":[
			{"id":"server-overload","enabled":true,"title":"servers may be slower than usual","text":"x"},
			{"id":"discord","enabled":false,"title":"Join the Discord community"},
			{"id":"untitled","enabled":true,"title":"","text":"only a text"}]}`})
		var buf bytes.Buffer
		level := log.GetLevel()
		log.SetOutput(&buf)
		log.SetLevel(log.InfoLevel)
		defer func() {
			log.SetOutput(os.Stderr)
			log.SetLevel(level)
		}()
		c := client(srv)
		for range 2 {
			if _, err := c.providerCodes(ctx); err != nil {
				t.Fatal(err)
			}
		}
		said := buf.String()
		if strings.Count(said, "servers may be slower than usual") != 1 || !strings.Contains(said, "only a text") {
			t.Errorf("notices said:\n%s\nwant each enabled one once", said)
		}
		if strings.Contains(said, "Discord") {
			t.Errorf("a disabled notice was said:\n%s", said)
		}
	})

	t.Run("an unlabeled provider is known by its id", func(t *testing.T) {
		srv, _ := api(t, map[string]string{"/api/config": `{"streaming":{"newcomer":{"label":"","visible":true}}}`})
		codes, err := client(srv).providerCodes(ctx)
		if err != nil || codes["newcomer"] != "newcomer" {
			t.Errorf("codes = %v, %v, want the id", codes, err)
		}
	})

	t.Run("a code two providers share is refused", func(t *testing.T) {
		srv, _ := api(t, map[string]string{"/api/config": `{"streaming":{
			"a":{"label":"bee","visible":true},"b":{"label":"bee","visible":true}}}`})
		if _, err := client(srv).providerCodes(ctx); err == nil || !strings.Contains(err.Error(), "bee") {
			t.Errorf("err = %v, want the shared code named", err)
		}
	})

	// a run cannot name a provider without the table, so one failed fetch must
	// not stand for every later one
	t.Run("a failure is not remembered", func(t *testing.T) {
		srv, asked := api(t, map[string]string{})
		c := client(srv)
		for range 2 {
			if _, err := c.providerCodes(ctx); !errors.Is(err, upstream.ErrUnreachable) {
				t.Fatalf("err = %v, want %v", err, upstream.ErrUnreachable)
			}
		}
		if got := asked(); len(got) != 2 {
			t.Errorf("the table was fetched %d times, want once per call", len(got))
		}
	})
}
