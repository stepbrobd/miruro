package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"ysun.co/miruro"
	"ysun.co/miruro/play"
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
		res, served, err := autoResolve(ctx, client, twoProviderCatalog(), 1, miruro.Sub, "bonk", nil)
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
		_, _, err := autoResolve(ctx, client, twoProviderCatalog(), 1, miruro.Sub, "bonk", nil)
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
		_, served, err := autoResolve(ctx, client, twoProviderCatalog(), 1, miruro.Sub, "", nil)
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
		_, _, err := autoResolve(ctx, client, twoProviderCatalog(), 9, miruro.Sub, "", nil)
		if err == nil || !strings.Contains(err.Error(), "no provider has episode 9") {
			t.Fatalf("err = %v, want the no-source error", err)
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("probed %d providers for an absent episode, want 0", n)
		}
	})
}

func TestMediaLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    miruro.Media
		want string
	}{
		{"format and count", miruro.Media{English: "T", Format: "TV", Episodes: 12}, "T (TV, 12 eps)"},
		{"mapped format", miruro.Media{English: "T", Format: "TV_SHORT", Episodes: 3}, "T (TV Short, 3 eps)"},
		{"movie without count", miruro.Media{English: "T", Format: "MOVIE"}, "T (Movie)"},
		{"unknown format passes through", miruro.Media{English: "T", Format: "WEIRD"}, "T (WEIRD)"},
		{"bare title", miruro.Media{English: "T"}, "T"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mediaLabel(tc.m); got != tc.want {
				t.Errorf("mediaLabel = %q, want %q", got, tc.want)
			}
		})
	}
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

func TestParseEpisodes(t *testing.T) {
	numbers := []float64{1, 2, 2.5, 3, 10}
	for _, tc := range []struct {
		name    string
		spec    string
		want    []float64
		wantErr bool
	}{
		{"single", "2", []float64{2}, false},
		{"fractional", "2.5", []float64{2.5}, false},
		{"range", "2-3", []float64{2, 2.5, 3}, false},
		{"range with spaces", " 1 - 2 ", []float64{1, 2}, false},
		{"range clamps to available", "2-20", []float64{2, 2.5, 3, 10}, false},
		{"empty range", "4-9", nil, true},
		{"absent single", "7", nil, true},
		{"negative", "-5", nil, true},
		{"garbage", "abc", nil, true},
		{"bad range bound", "a-3", nil, true},
		{"trailing dash", "3-", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEpisodes(tc.spec, numbers)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseEpisodes(%q) error = %v, wantErr %v", tc.spec, err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("parseEpisodes(%q) = %v, want %v", tc.spec, got, tc.want)
			}
		})
	}
}

func TestControls(t *testing.T) {
	numbers := []float64{1, 2, 3}
	for _, tc := range []struct {
		name string
		ep   float64
		want []string
	}{
		{"first has no previous", 1, []string{"next", "replay", "select", "change provider", "quit"}},
		{"middle has both", 2, []string{"next", "replay", "previous", "select", "change provider", "quit"}},
		{"last has no next", 3, []string{"replay", "previous", "select", "change provider", "quit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := controls(numbers, tc.ep); !slices.Equal(got, tc.want) {
				t.Errorf("controls(ep %v) = %v, want %v", tc.ep, got, tc.want)
			}
		})
	}
}

func TestApply(t *testing.T) {
	numbers := []float64{1, 2, 3}
	for _, tc := range []struct {
		action   string
		want     step
		wantQuit bool
	}{
		{"next", step{ep: 3}, false},
		{"previous", step{ep: 1}, false},
		{"replay", step{ep: 2}, false},
		{"select", step{reselect: true}, false},
		{"change provider", step{ep: 2, reprovide: true}, false},
		{"quit", step{}, true},
	} {
		t.Run(tc.action, func(t *testing.T) {
			got, quit := apply(tc.action, numbers, 2)
			if got != tc.want || quit != tc.wantQuit {
				t.Errorf("apply(%q) = (%+v, %v), want (%+v, %v)", tc.action, got, quit, tc.want, tc.wantQuit)
			}
		})
	}
}

func TestOutcome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no false binary")
	}
	exit := exec.Command("false").Run()
	other := errors.New("no player")
	for _, tc := range []struct {
		name  string
		err   error
		batch bool
		want  bool
	}{
		{"clean end mid-batch advances", nil, true, true},
		{"clean end alone stays", nil, false, false},
		{"failure stays", exit, true, false},
		{"unrunnable player dismisses", other, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := outcome(tc.err, tc.batch); got != tc.want {
				t.Errorf("outcome = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// a retried episode must move past the providers it already burned, or the
// fallback loop would resolve the same dead source forever
func TestAutoResolveSkipsProvidersAlreadyTried(t *testing.T) {
	srv := sourcesServer(t, map[string]http.HandlerFunc{
		"bonk": serveJSON(hlsPayload),
	}, nil)
	defer srv.Close()

	client := &miruro.Client{Base: srv.URL, HTTP: srv.Client()}
	_, served, err := autoResolve(context.Background(), client, twoProviderCatalog(), 1, miruro.Sub, "", map[string]bool{"ally": true})
	if err != nil {
		t.Fatal(err)
	}
	if served != "bonk" {
		t.Errorf("served = %q, want bonk", served)
	}

	_, _, err = autoResolve(context.Background(), client, twoProviderCatalog(), 1, miruro.Sub, "", map[string]bool{"ally": true, "bonk": true})
	if err == nil || !strings.Contains(err.Error(), "no source resolved") {
		t.Fatalf("err = %v, want the no-source error once every provider is spent", err)
	}
}

// a provider that resolves and then dies mid-download used to lose the episode
func TestSaveFallsBackToAnotherProvider(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dead") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "episode bytes")
	}))
	defer cdn.Close()

	srv := sourcesServer(t, map[string]http.HandlerFunc{
		"ally": serveJSON(`{"streams":[{"url":"` + cdn.URL + `/dead.mp4","type":"mp4"}]}`),
		"bonk": serveJSON(`{"streams":[{"url":"` + cdn.URL + `/live.mp4","type":"mp4"}]}`),
	}, nil)
	defer srv.Close()

	ctx := context.Background()
	px, err := play.StartProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	dir := t.TempDir()
	sv := saver{
		client:   &miruro.Client{Base: srv.URL, HTTP: srv.Client()},
		px:       px,
		hc:       http.DefaultClient,
		cat:      twoProviderCatalog(),
		title:    "Show",
		category: miruro.Sub,
		cfg:      config{Quality: "best", DownloadDir: dir},
	}
	if _, err := sv.save(ctx, 1, nil); err != nil {
		t.Fatalf("the fallback provider did not save the episode: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "Show - E1.mp4"))
	if err != nil || string(body) != "episode bytes" {
		t.Errorf("saved %q (%v), want the live provider's body", body, err)
	}
}

// with every provider dead the episode fails, and the report has to name the
// download that failed rather than the resolution that ran out of providers
func TestSaveReportsTheDownloadFailure(t *testing.T) {
	cdn := httptest.NewServer(http.NotFoundHandler())
	defer cdn.Close()

	srv := sourcesServer(t, map[string]http.HandlerFunc{
		"ally": serveJSON(`{"streams":[{"url":"` + cdn.URL + `/a.mp4","type":"mp4"}]}`),
		"bonk": serveJSON(`{"streams":[{"url":"` + cdn.URL + `/b.mp4","type":"mp4"}]}`),
	}, nil)
	defer srv.Close()

	ctx := context.Background()
	px, err := play.StartProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	sv := saver{
		client:   &miruro.Client{Base: srv.URL, HTTP: srv.Client()},
		px:       px,
		hc:       http.DefaultClient,
		cat:      twoProviderCatalog(),
		title:    "Show",
		category: miruro.Sub,
		cfg:      config{Quality: "best", DownloadDir: t.TempDir()},
	}
	_, err = sv.save(ctx, 1, nil)
	if err == nil {
		t.Fatal("every provider was dead, the episode must fail")
	}
	if !strings.Contains(err.Error(), "bonk") {
		t.Errorf("err = %v, want the last provider that failed to download", err)
	}
}

// a provider that serves an episode from several hosts is not dead when the
// first of them is, so the download walks its streams before the next provider
func TestSaveFallsBackToAnotherStream(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/hd1") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "episode bytes")
	}))
	defer cdn.Close()

	// ally leads alphabetically, so the pin is what puts bonk in front
	srv := sourcesServer(t, map[string]http.HandlerFunc{
		"bonk": serveJSON(`{"streams":[
			{"url":"` + cdn.URL + `/hd1.mp4","type":"mp4"},
			{"url":"` + cdn.URL + `/hd2.mp4","type":"mp4"}]}`),
	}, nil)
	defer srv.Close()

	ctx := context.Background()
	px, err := play.StartProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	dir := t.TempDir()
	sv := saver{
		client:   &miruro.Client{Base: srv.URL, HTTP: srv.Client()},
		px:       px,
		hc:       http.DefaultClient,
		cat:      &miruro.Catalog{Providers: map[string]miruro.Provider{"bonk": {Code: "bonk", Sub: []miruro.Episode{{ID: "bonk-1", Number: 1}}}}},
		title:    "Show",
		category: miruro.Sub,
		cfg:      config{Quality: "best", DownloadDir: dir},
	}
	if _, err := sv.save(ctx, 1, nil); err != nil {
		t.Fatalf("the second stream did not save the episode: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "Show - E1.mp4")); err != nil || string(body) != "episode bytes" {
		t.Errorf("saved %q (%v), want the live stream's body", body, err)
	}
}
