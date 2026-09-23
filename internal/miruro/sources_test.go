package miruro

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"ysun.co/miruro/internal/upstream"
)

func TestSourcesKeepsOnlyDialogueTracks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"streams":[{"url":"u","type":"hls"}],"subtitles":[
			{"file":"thumbs.vtt","label":"thumbnails","kind":"thumbnails"},
			{"file":"en.vtt","label":"English","kind":"captions","language":"en","default":true},
			{"file":"pt.vtt","label":"Portugues","kind":"subtitles","language":"pt-BR"},
			{"file":"bare.vtt","label":"Bare"}]}`)
	}))
	defer srv.Close()

	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	res, err := c.Sources(context.Background(), "ep", "bonk", upstream.Sub)
	if err != nil {
		t.Fatal(err)
	}
	want := []upstream.Subtitle{
		{File: "en.vtt", Label: "English", Lang: "en", Default: true},
		{File: "pt.vtt", Label: "Portugues", Lang: "pt-BR"},
		{File: "bare.vtt", Label: "Bare"},
	}
	if !reflect.DeepEqual(res.Subtitles, want) {
		t.Errorf("subtitles = %+v, want %+v", res.Subtitles, want)
	}
}

// a container the closed set does not name is dropped where it arrives
func TestSourcesDropUnknownKinds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"streams":[{"url":"d","type":"dash"},{"url":"h","type":"hls"},{"url":"","type":""}]}`)
	}))
	defer srv.Close()
	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	res, err := c.Sources(context.Background(), "e", "p", upstream.Sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Streams) != 1 || res.Streams[0].Kind != upstream.HLS {
		t.Errorf("streams = %+v, want only the hls one", res.Streams)
	}
}

func TestAbsentActiveFlagStaysPlayable(t *testing.T) {
	srv := mirror(t, serves(`{"streams":[{"url":"u","type":"hls"},{"url":"v","type":"hls","isActive":false}]}`))
	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	res, err := c.Sources(context.Background(), "ep", "bonk", upstream.Sub)
	if err != nil {
		t.Fatal(err)
	}
	if res.Streams[0].Dead {
		t.Error("a stream with no isActive was marked dead")
	}
	if !res.Streams[1].Dead {
		t.Error("an explicit isActive false was not marked dead")
	}
}

// hop lists the subtitle files of its older encodes with a third slash after the
// scheme, which the site's player reads past and net/url reads as an empty host
func TestSourcesReadURLsTheWayABrowserDoes(t *testing.T) {
	srv := mirror(t, serves(`{"streams":[{"url":"https:///bl.example/master.m3u8","type":"hls"}],
		"subtitles":[{"file":"https:///subbl.example/1_en.srt","label":"English","language":"en"}]}`))
	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	res, err := c.Sources(context.Background(), "ep", "hop", upstream.Ssub)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Streams[0].URL, "https://bl.example/master.m3u8"; got != want {
		t.Errorf("stream url = %q, want %q", got, want)
	}
	if got, want := res.Subtitles[0].File, "https://subbl.example/1_en.srt"; got != want {
		t.Errorf("subtitle file = %q, want %q", got, want)
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
