package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"ysun.co/miruro"
)

// sourcesServer decodes the pipe envelope and dispatches on the provider in
// its query, so each fake provider can answer differently
func sourcesServer(t *testing.T, respond map[string]http.HandlerFunc, hits *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("e"))
		if err != nil {
			t.Errorf("undecodable envelope: %v", err)
			http.Error(w, "bad envelope", http.StatusBadRequest)
			return
		}
		var env struct {
			Query map[string]string `json:"query"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Errorf("envelope is not json: %v", err)
			http.Error(w, "bad envelope", http.StatusBadRequest)
			return
		}
		h, ok := respond[env.Query["provider"]]
		if !ok {
			t.Errorf("unexpected provider %q probed", env.Query["provider"])
			http.Error(w, "unknown provider", http.StatusBadRequest)
			return
		}
		h(w, r)
	}))
}

const (
	hlsPayload   = `{"streams":[{"url":"http://cdn/master.m3u8","type":"hls","quality":"1080p"}]}`
	embedPayload = `{"streams":[{"url":"http://cdn/embed","type":"embed"}]}`
)

func serveJSON(payload string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, payload)
	}
}

func serveStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

func serveBlocked(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusForbidden)
	io.WriteString(w, "<html>blocked</html>")
}

// Available orders providers by code, so ally probes before bonk when no pin
// reorders them
func twoProviderCatalog() *miruro.Catalog {
	return &miruro.Catalog{
		Providers: map[string]miruro.Provider{
			"ally": {Code: "ally", Sub: []miruro.Episode{{ID: "ally-1", Number: 1}}},
			"bonk": {Code: "bonk", Sub: []miruro.Episode{{ID: "bonk-1", Number: 1}}},
		},
	}
}

func TestAutoResolve(t *testing.T) {
	ctx := context.Background()

	t.Run("falls back when the pinned provider errors", func(t *testing.T) {
		srv := sourcesServer(t, map[string]http.HandlerFunc{
			"bonk": serveStatus(http.StatusInternalServerError),
			"ally": serveJSON(hlsPayload),
		}, nil)
		defer srv.Close()

		client := &miruro.Client{Base: srv.URL, HTTP: srv.Client()}
		res, served, err := autoResolve(ctx, client, twoProviderCatalog(), 1, miruro.Sub, "bonk")
		if err != nil {
			t.Fatal(err)
		}
		if served != "ally" {
			t.Errorf("served = %q, want ally", served)
		}
		if !res.Playable() {
			t.Error("resolved result is not playable")
		}
	})

	t.Run("a block aborts without probing further", func(t *testing.T) {
		var hits atomic.Int64
		srv := sourcesServer(t, map[string]http.HandlerFunc{
			"bonk": serveBlocked,
			"ally": serveJSON(hlsPayload),
		}, &hits)
		defer srv.Close()

		client := &miruro.Client{Base: srv.URL, HTTP: srv.Client()}
		_, _, err := autoResolve(ctx, client, twoProviderCatalog(), 1, miruro.Sub, "bonk")
		if !errors.Is(err, miruro.ErrBlocked) {
			t.Fatalf("err = %v, want ErrBlocked", err)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("probed %d providers after the block, want 1", n)
		}
	})

	t.Run("an embed-only provider is skipped", func(t *testing.T) {
		srv := sourcesServer(t, map[string]http.HandlerFunc{
			"ally": serveJSON(embedPayload),
			"bonk": serveJSON(hlsPayload),
		}, nil)
		defer srv.Close()

		client := &miruro.Client{Base: srv.URL, HTTP: srv.Client()}
		_, served, err := autoResolve(ctx, client, twoProviderCatalog(), 1, miruro.Sub, "")
		if err != nil {
			t.Fatal(err)
		}
		if served != "bonk" {
			t.Errorf("served = %q, want bonk", served)
		}
	})

	t.Run("no provider has the episode", func(t *testing.T) {
		var hits atomic.Int64
		srv := sourcesServer(t, map[string]http.HandlerFunc{}, &hits)
		defer srv.Close()

		client := &miruro.Client{Base: srv.URL, HTTP: srv.Client()}
		_, _, err := autoResolve(ctx, client, twoProviderCatalog(), 9, miruro.Sub, "")
		if err == nil || !strings.Contains(err.Error(), "no provider has episode 9") {
			t.Fatalf("err = %v, want the no-source error", err)
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("probed %d providers for an absent episode, want 0", n)
		}
	})
}

func TestOrderPinned(t *testing.T) {
	providers := []miruro.Provider{{Code: "ally"}, {Code: "bonk"}, {Code: "cost"}}
	for _, tc := range []struct {
		name string
		pin  string
		want []string
	}{
		{"pinned code moves to the front", "bonk", []string{"bonk", "ally", "cost"}},
		{"absent code keeps the order", "zzz", []string{"ally", "bonk", "cost"}},
		{"empty pin keeps the order", "", []string{"ally", "bonk", "cost"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, p := range orderPinned(providers, tc.pin) {
				got = append(got, p.Code)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("orderPinned(%q) = %v, want %v", tc.pin, got, tc.want)
			}
		})
	}
}

// the pin's variant describes the pinned provider only, so a fallback must
// not inherit hard and lose its subtitles
func TestApplied(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pin    Pin
		served string
		want   Variant
	}{
		{"pinned hard applies", Pin{"bonk", Hard}, "bonk", Hard},
		{"fallback resets to soft", Pin{"bonk", Hard}, "ally", Soft},
		{"pinned soft stays soft", Pin{"bonk", Soft}, "bonk", Soft},
		{"empty pin is soft", Pin{}, "ally", Soft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := applied(tc.pin, tc.served); got != tc.want {
				t.Errorf("applied(%+v, %q) = %q, want %q", tc.pin, tc.served, got, tc.want)
			}
		})
	}
}

func TestFind(t *testing.T) {
	eps := []miruro.Episode{{ID: "a1", Number: 1}, {ID: "a2", Number: 2.5}}
	for _, tc := range []struct {
		name   string
		n      float64
		wantID string
	}{
		{"integer episode", 1, "a1"},
		{"fractional episode", 2.5, "a2"},
		{"absent episode", 3, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := find(eps, tc.n)
			switch {
			case tc.wantID == "" && got != nil:
				t.Errorf("find(%v) = %+v, want nil", tc.n, got)
			case tc.wantID != "" && (got == nil || got.ID != tc.wantID):
				t.Errorf("find(%v) = %+v, want id %s", tc.n, got, tc.wantID)
			}
		})
	}
}

func TestNeighbor(t *testing.T) {
	numbers := []float64{1, 2, 5}
	for _, tc := range []struct {
		name   string
		ep     float64
		dir    int
		want   float64
		wantOK bool
	}{
		{"next of the first", 1, 1, 2, true},
		{"next across a gap", 2, 1, 5, true},
		{"no next at the end", 5, 1, 0, false},
		{"previous of the middle", 2, -1, 1, true},
		{"no previous at the start", 1, -1, 0, false},
		{"absent episode has no neighbor", 3, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := neighbor(numbers, tc.ep, tc.dir)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("neighbor(%v, %d) = (%v, %v), want (%v, %v)",
					tc.ep, tc.dir, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
