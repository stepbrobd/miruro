package miruro

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// the api names hardsub "sub" and softsub "ssub", so a swapped mapping would
// attach a subtitle file over a picture that already carries one
const configBody = `{"streaming":{
	"bee":{"capabilities":{"sub":false,"ssub":true},"player":"native","relationship":null},
	"kiwi":{"capabilities":{"sub":true,"ssub":false},"player":"native","relationship":null},
	"bonk":{"capabilities":{"sub":true,"ssub":true},"player":"native","relationship":null},
	"twin":{"capabilities":{"sub":true,"ssub":true},"player":"iframe","relationship":"embed"}
},"providerOrder":["bee","kiwi","bonk","twin"]}`

func TestConfig(t *testing.T) {
	ctx := context.Background()

	t.Run("capabilities are read the way the api means them", func(t *testing.T) {
		srv := mirror(t, serves(configBody))
		cfg, err := (&Client{Bases: []string{srv.URL}, HTTP: srv.Client()}).Config(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := Config{
			"bee":  {Soft: true},
			"kiwi": {Hard: true},
			"bonk": {Hard: true, Soft: true},
			"twin": {Hard: true, Soft: true, Embed: true},
		}
		for code, w := range want {
			if got := cfg[code]; got != w {
				t.Errorf("%s = %+v, want %+v", code, got, w)
			}
		}
		// a provider the episodes resource serves and this one omits, such as
		// ANIMEDUNYA, must read as undeclared rather than as incapable
		if _, ok := cfg["ANIMEDUNYA"]; ok {
			t.Error("an unnamed provider must not appear in the table")
		}
	})

	t.Run("the table is fetched once", func(t *testing.T) {
		srv := mirror(t, serves(configBody))
		c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
		for range 3 {
			if _, err := c.Config(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if got := srv.hits.Load(); got != 1 {
			t.Errorf("the resource was fetched %d times, want 1", got)
		}
	})

	t.Run("a failure is remembered rather than retried per episode", func(t *testing.T) {
		srv := mirror(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
		for range 3 {
			if _, err := c.Config(ctx); !errors.Is(err, ErrUpstream) {
				t.Fatalf("err = %v, want ErrUpstream", err)
			}
		}
		if got := srv.hits.Load(); got != 1 {
			t.Errorf("the resource was fetched %d times, want 1", got)
		}
	})

	// a run cancelled before the table arrives says nothing about the resource
	t.Run("a cancelled fetch is not remembered", func(t *testing.T) {
		srv := mirror(t, serves(configBody))
		c := &Client{Bases: []string{srv.URL}, HTTP: srv.Client()}

		dead, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := c.Config(dead); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if _, err := c.Config(ctx); err != nil {
			t.Fatalf("the next run inherited the cancellation: %v", err)
		}
	})

	t.Run("a body that is not the table is an error", func(t *testing.T) {
		srv := mirror(t, serves(`[1,2,3]`))
		if _, err := (&Client{Bases: []string{srv.URL}, HTTP: srv.Client()}).Config(ctx); err == nil {
			t.Fatal("want a parse error")
		}
	})
}
