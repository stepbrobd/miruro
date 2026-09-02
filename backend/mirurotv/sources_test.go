package mirurotv

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"ysun.co/miruro"
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
	res, err := c.Sources(context.Background(), "ep", "bonk", miruro.Sub)
	if err != nil {
		t.Fatal(err)
	}
	want := []miruro.Subtitle{
		{File: "en.vtt", Label: "English", Lang: "en", Default: true},
		{File: "pt.vtt", Label: "Portugues", Lang: "pt-BR"},
		{File: "bare.vtt", Label: "Bare"},
	}
	if !reflect.DeepEqual(res.Subtitles, want) {
		t.Errorf("subtitles = %+v, want %+v", res.Subtitles, want)
	}
}

func TestAbsentActiveFlagStaysPlayable(t *testing.T) {
	srv := mirror(t, serves(`{"streams":[{"url":"u","type":"hls"},{"url":"v","type":"hls","isActive":false}]}`))
	c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	res, err := c.Sources(context.Background(), "ep", "bonk", miruro.Sub)
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
