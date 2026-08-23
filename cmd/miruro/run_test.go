package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	"time"

	"github.com/charmbracelet/log"

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

// Available orders providers by preference, where ally leads bonk, so ally
// probes first when no pin reorders them
func twoProviderCatalog() *miruro.Catalog {
	return &miruro.Catalog{
		Providers: map[string]miruro.Provider{
			"ally": {Code: "ally", Sub: []miruro.Episode{{ID: "ally-1", Number: 1}}},
			"bonk": {Code: "bonk", Sub: []miruro.Episode{{ID: "bonk-1", Number: 1}}},
		},
	}
}

// deadCDN serves an episode body except under prefix, which 404s, so a test can
// spell one dead host among live ones
func deadCDN(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "episode bytes")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newSaver wires a saver against srv with a proxy and a download directory, and
// returns that directory
func newSaver(t *testing.T, srv *httptest.Server, cat *miruro.Catalog) (saver, string) {
	t.Helper()
	px, err := play.StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { px.Close() })
	dir := t.TempDir()
	return saver{
		client:   &miruro.Client{Bases: []string{srv.URL}, HTTP: srv.Client()},
		px:       px,
		hc:       http.DefaultClient,
		cat:      cat,
		title:    "Show",
		category: miruro.Sub,
		cfg:      config{Quality: "best", DownloadDir: dir},
	}, dir
}

// savedEpisode asserts the episode landed whole under dir
func savedEpisode(t *testing.T, dir string) {
	t.Helper()
	if body, err := os.ReadFile(filepath.Join(dir, "Show - E1.mp4")); err != nil || string(body) != "episode bytes" {
		t.Errorf("saved %q (%v), want the whole episode", body, err)
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

		client := &miruro.Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
		res, src, err := autoResolve(ctx, client, twoProviderCatalog(), 1, miruro.Sub, Pin{Code: "bonk"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if src.Code != "ally" {
			t.Errorf("served = %q, want ally", src.Code)
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

		client := &miruro.Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
		_, _, err := autoResolve(ctx, client, twoProviderCatalog(), 1, miruro.Sub, Pin{Code: "bonk"}, nil, nil)
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

		client := &miruro.Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
		_, src, err := autoResolve(ctx, client, twoProviderCatalog(), 1, miruro.Sub, Pin{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if src.Code != "bonk" {
			t.Errorf("served = %q, want bonk", src.Code)
		}
	})

	t.Run("no provider has the episode", func(t *testing.T) {
		var hits atomic.Int64
		srv := sourcesServer(t, map[string]http.HandlerFunc{}, &hits)
		defer srv.Close()

		client := &miruro.Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
		_, _, err := autoResolve(ctx, client, twoProviderCatalog(), 9, miruro.Sub, Pin{}, nil, nil)
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

	client := &miruro.Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
	_, src, err := autoResolve(context.Background(), client, twoProviderCatalog(), 1, miruro.Sub, Pin{}, map[string]bool{"ally": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if src.Code != "bonk" {
		t.Errorf("served = %q, want bonk", src.Code)
	}

	_, _, err = autoResolve(context.Background(), client, twoProviderCatalog(), 1, miruro.Sub, Pin{}, map[string]bool{"ally": true, "bonk": true}, nil)
	if err == nil || !strings.Contains(err.Error(), "no source resolved") {
		t.Fatalf("err = %v, want the no-source error once every provider is spent", err)
	}
}

// a provider that resolves and then dies mid-download used to lose the episode
func TestSaveFallsBackToAnotherProvider(t *testing.T) {
	cdn := deadCDN(t, "/dead")
	srv := sourcesServer(t, map[string]http.HandlerFunc{
		"ally": serveJSON(`{"streams":[{"url":"` + cdn.URL + `/dead.mp4","type":"mp4"}]}`),
		"bonk": serveJSON(`{"streams":[{"url":"` + cdn.URL + `/live.mp4","type":"mp4"}]}`),
	}, nil)
	defer srv.Close()

	sv, dir := newSaver(t, srv, twoProviderCatalog())
	if _, err := sv.save(context.Background(), 1, nil); err != nil {
		t.Fatalf("the fallback provider did not save the episode: %v", err)
	}
	savedEpisode(t, dir)
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

	sv, _ := newSaver(t, srv, twoProviderCatalog())
	_, err := sv.save(context.Background(), 1, nil)
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
	cdn := deadCDN(t, "/hd1")

	// ally leads alphabetically, so the pin is what puts bonk in front
	srv := sourcesServer(t, map[string]http.HandlerFunc{
		"bonk": serveJSON(`{"streams":[
			{"url":"` + cdn.URL + `/hd1.mp4","type":"mp4"},
			{"url":"` + cdn.URL + `/hd2.mp4","type":"mp4"}]}`),
	}, nil)
	defer srv.Close()

	cat := &miruro.Catalog{Providers: map[string]miruro.Provider{
		"bonk": {Code: "bonk", Sub: []miruro.Episode{{ID: "bonk-1", Number: 1}}},
	}}
	sv, dir := newSaver(t, srv, cat)
	if _, err := sv.save(context.Background(), 1, nil); err != nil {
		t.Fatalf("the second stream did not save the episode: %v", err)
	}
	savedEpisode(t, dir)
}

// fakePlay stands in for the player, fetching what it was handed the way a real
// one does, so the proxy sees exactly what playback would have made it see
func fakePlay(t *testing.T, px *play.Proxy, tried *[]string) func(context.Context, miruro.Stream) error {
	return func(ctx context.Context, s miruro.Stream) error {
		*tried = append(*tried, s.Server)
		resp, err := http.Get(px.Stream(s).URL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("player exit 2: %d", resp.StatusCode)
		}
		return nil
	}
}

func TestPlayStreams(t *testing.T) {
	ctx := context.Background()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dead") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "picture")
	}))
	defer cdn.Close()

	px, err := play.StartProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	dead := miruro.Stream{URL: cdn.URL + "/dead.mp4", Kind: miruro.MP4, Server: "HD-1"}
	live := miruro.Stream{URL: cdn.URL + "/live.mp4", Kind: miruro.MP4, Server: "HD-2"}

	t.Run("a stream that never started falls through", func(t *testing.T) {
		var tried []string
		if err := playStreams(ctx, px, []miruro.Stream{dead, live}, fakePlay(t, px, &tried)); err != nil {
			t.Fatalf("the live stream did not play: %v", err)
		}
		if !slices.Equal(tried, []string{"HD-1", "HD-2"}) {
			t.Errorf("tried %v, want both servers in order", tried)
		}
	})

	// quitting the player two seconds in is not a dead stream, and restarting on
	// another server would fight the user
	t.Run("a stream that played is not retried", func(t *testing.T) {
		var tried []string
		quit := errors.New("player exit 4")
		err := playStreams(ctx, px, []miruro.Stream{live, dead}, func(ctx context.Context, s miruro.Stream) error {
			fakePlay(t, px, &tried)(ctx, s)
			return quit
		})
		if !errors.Is(err, quit) {
			t.Errorf("err = %v, want the player's own failure", err)
		}
		if !slices.Equal(tried, []string{"HD-2"}) {
			t.Errorf("tried %v, want only the stream that played", tried)
		}
	})

	t.Run("every stream dead reports the last failure", func(t *testing.T) {
		var tried []string
		err := playStreams(ctx, px, []miruro.Stream{dead, dead}, fakePlay(t, px, &tried))
		if err == nil {
			t.Fatal("nothing played, playback must fail")
		}
		if len(tried) != 2 {
			t.Errorf("tried %d streams, want 2", len(tried))
		}
	})
}

func TestDeadStream(t *testing.T) {
	fail := errors.New("player exit 2")
	cases := []struct {
		err           error
		before, after int
		want          bool
	}{
		{fail, 0, 0, true},  // exited with an error having got no picture
		{fail, 3, 7, false}, // played, then failed, so the user or the CDN quit
		{nil, 0, 0, false},  // a clean exit is never retried
		{nil, 0, 9, false},  //
		{fail, 2, 2, true},  // a later episode starts the count above zero
	}
	for _, c := range cases {
		if got := deadStream(c.err, c.before, c.after); got != c.want {
			t.Errorf("deadStream(%v, %d, %d) = %v, want %v", c.err, c.before, c.after, got, c.want)
		}
	}
}

// a catalog that names nothing must still render a bare number
func TestEpisodeLabel(t *testing.T) {
	label := episodeLabel(map[float64]miruro.Episode{
		1:   {Number: 1, Title: "Rebirth"},
		2:   {Number: 2, Title: "Confrontation", Filler: true},
		3:   {Number: 3},
		4.5: {Number: 4.5, Filler: true},
	})
	for _, tc := range []struct {
		ep   float64
		want string
	}{
		{1, "1  Rebirth"},
		{2, "2  Confrontation  (filler)"},
		{3, "3"},
		{4.5, "4.5  (filler)"},
		{9, "9"},
	} {
		if got := label(tc.ep); got != tc.want {
			t.Errorf("label(%v) = %q, want %q", tc.ep, got, tc.want)
		}
	}
}

// pipeQuery decodes the query the client packed into the pipe envelope
func pipeQuery(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("e"))
	if err != nil {
		t.Fatalf("undecodable envelope: %v", err)
	}
	var env struct {
		Query map[string]string `json:"query"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not json: %v", err)
	}
	return env.Query
}

// the two sub renditions are separate category values on the wire, and asking a
// provider for the one it does not carry answers 444, so the variant has to
// reach Sources rather than stay a client-side attach decision
func TestAutoResolveAsksForTheDeclaredRendition(t *testing.T) {
	caps := miruro.Capabilities{
		"kiwi": {Hard: true},
		"bee":  {Soft: true},
		"bonk": {Hard: true, Soft: true},
	}
	cat := &miruro.Catalog{Providers: map[string]miruro.Provider{
		"kiwi": {Code: "kiwi", Sub: []miruro.Episode{{ID: "kiwi-1", Number: 1}}},
		"bee":  {Code: "bee", Sub: []miruro.Episode{{ID: "bee-1", Number: 1}}},
		"bonk": {Code: "bonk", Sub: []miruro.Episode{{ID: "bonk-1", Number: 1}}},
	}}

	for _, tc := range []struct {
		name     string
		pin      Pin
		category miruro.Category
		wantCat  string
		attach   bool
	}{
		{"a hardsub provider is asked for sub", Pin{"kiwi", Hard}, miruro.Sub, "sub", false},
		{"a softsub provider is asked for ssub", Pin{"bee", Soft}, miruro.Sub, "ssub", true},
		{"bonk soft is asked for ssub", Pin{"bonk", Soft}, miruro.Sub, "ssub", true},
		{"bonk hard is asked for sub", Pin{"bonk", Hard}, miruro.Sub, "sub", false},
		// a pin contradicting the table would ask for a rendition that 444s, so
		// the declared one wins over what was typed
		{"a soft pin on a hardsub provider is corrected", Pin{"kiwi", Soft}, miruro.Sub, "sub", false},
		{"a hard pin on a softsub provider is corrected", Pin{"bee", Hard}, miruro.Sub, "ssub", true},
		// the variant names a sub rendition, so a dub run ignores it rather than
		// suppressing one provider's tracks and no other's
		{"dub is never rewritten and keeps its tracks", Pin{"bonk", Hard}, miruro.Dub, "dub", true},
		{"dub with a soft pin keeps them too", Pin{"bonk", Soft}, miruro.Dub, "dub", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked map[string]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = pipeQuery(t, r)
				serveJSON(hlsPayload)(w, r)
			}))
			defer srv.Close()

			catalog := cat
			if tc.category == miruro.Dub {
				catalog = &miruro.Catalog{Providers: map[string]miruro.Provider{
					"bonk": {Code: "bonk", Dub: []miruro.Episode{{ID: "bonk-d1", Number: 1}}},
				}}
			}
			client := &miruro.Client{Bases: []string{srv.URL}, HTTP: srv.Client()}
			_, src, err := autoResolve(context.Background(), client, catalog, 1, tc.category, tc.pin, nil, caps)
			if err != nil {
				t.Fatal(err)
			}
			if asked["category"] != tc.wantCat {
				t.Errorf("sources category = %q, want %q", asked["category"], tc.wantCat)
			}
			if string(src.Category) != tc.wantCat {
				t.Errorf("source category = %q, want %q", src.Category, tc.wantCat)
			}
			if src.Attach != tc.attach {
				t.Errorf("attach = %v, want %v", src.Attach, tc.attach)
			}
			if src.Code != tc.pin.Code {
				t.Errorf("served = %q, want %q", src.Code, tc.pin.Code)
			}
		})
	}
}

// a provider whose every stream is refused is dead for that episode however
// many it listed, and the download path has always moved off one
// ally on "Grow Up Show" is the live case, two mp4 hosts answering 401 and 403
// beside three embeds, with a healthy pewe one step down the preference list
func TestPlaybackFallsBackToTheNextProvider(t *testing.T) {
	ctx := context.Background()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/refused") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "picture")
	}))
	defer cdn.Close()

	px, err := play.StartProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	srv := sourcesServer(t, map[string]http.HandlerFunc{
		"ally": serveJSON(`{"streams":[
			{"url":"` + cdn.URL + `/refused-a.mp4","type":"mp4","server":"Yt-mp4"},
			{"url":"` + cdn.URL + `/refused-b.mp4","type":"mp4","server":"Mp4"}]}`),
		"pewe": serveJSON(`{"streams":[{"url":"` + cdn.URL + `/live.m3u8","type":"mp4","server":"AniDBApp"}]}`),
	}, nil)
	defer srv.Close()

	cat := &miruro.Catalog{Providers: map[string]miruro.Provider{
		"ally": {Code: "ally", Sub: []miruro.Episode{{ID: "ally-8", Number: 8}}},
		"pewe": {Code: "pewe", Sub: []miruro.Episode{{ID: "pewe-8", Number: 8}}},
	}}

	var tried []string
	stage := playback{
		client:   &miruro.Client{Bases: []string{srv.URL}, HTTP: srv.Client()},
		px:       px,
		cat:      cat,
		category: miruro.Sub,
		cfg:      config{Quality: "best"},
		pin:      Pin{Code: "ally", Variant: Hard},
		title:    "Grow Up Show",
		ep:       8,
		launch: func(ctx context.Context, s miruro.Stream, _ []miruro.Subtitle) error {
			tried = append(tried, s.Server)
			return fakePlay(t, px, new([]string))(ctx, s)
		},
	}

	// ally is the pin, so it is what resolve would have handed over
	res, err := stage.client.Sources(ctx, "ally-8", "ally", miruro.Sub)
	if err != nil {
		t.Fatal(err)
	}
	said := captureLog(t)
	if err := stage.run(ctx, res, offer{Pin: stage.pin, declared: true}.source(miruro.Sub)); err != nil {
		t.Fatalf("pewe should have played after ally refused everything: %v", err)
	}
	if want := []string{"Yt-mp4", "Mp4", "AniDBApp"}; !slices.Equal(tried, want) {
		t.Errorf("tried %v, want both dead ally streams then pewe", tried)
	}
	// the walk has to say where it went, and the menu picks the log up from here
	told := said.String()
	for _, want := range []string{
		`playing title="Grow Up Show" ep=8 provider=ally server=Yt-mp4`,
		`stream did not play, trying the next server=Yt-mp4`,
		`nothing played, trying the next provider provider=ally next=pewe`,
		`playing title="Grow Up Show" ep=8 provider=pewe server=AniDBApp`,
	} {
		if !strings.Contains(told, want) {
			t.Errorf("the log does not carry %q:\n%s", want, told)
		}
	}
	// the last stream of a provider is covered by the provider's own record, and
	// nothing must promise a next attempt that never happens
	if strings.Contains(told, "server=Mp4") {
		t.Errorf("the last stream of a provider claimed another was coming:\n%s", told)
	}
}

// captureLog points the log at a buffer for the length of a test, at a level
// that keeps everything the playback writes
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	level := log.GetLevel()
	log.SetOutput(&b)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetLevel(level)
	})
	return &b
}

// a stream that produced picture and then failed is the user's to deal with,
// so the walk must not restart the episode somewhere else under them
func TestPlaybackKeepsAProviderThatPlayed(t *testing.T) {
	ctx := context.Background()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "picture")
	}))
	defer cdn.Close()

	px, err := play.StartProxy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	srv := sourcesServer(t, map[string]http.HandlerFunc{
		"ally": serveJSON(`{"streams":[{"url":"` + cdn.URL + `/live.mp4","type":"mp4","server":"Yt-mp4"}]}`),
		"pewe": serveJSON(`{"streams":[{"url":"` + cdn.URL + `/other.mp4","type":"mp4","server":"AniDBApp"}]}`),
	}, nil)
	defer srv.Close()

	quit := errors.New("player exit 4")
	var tried []string
	stage := playback{
		client: &miruro.Client{Bases: []string{srv.URL}, HTTP: srv.Client()},
		px:     px,
		cat: &miruro.Catalog{Providers: map[string]miruro.Provider{
			"ally": {Code: "ally", Sub: []miruro.Episode{{ID: "ally-8", Number: 8}}},
			// a second provider the walk could reach, so the guard has something
			// to prevent rather than nothing to do
			"pewe": {Code: "pewe", Sub: []miruro.Episode{{ID: "pewe-8", Number: 8}}},
		}},
		category: miruro.Sub,
		cfg:      config{Quality: "best"},
		pin:      Pin{Code: "ally", Variant: Hard},
		ep:       8,
		launch: func(ctx context.Context, s miruro.Stream, _ []miruro.Subtitle) error {
			tried = append(tried, s.Server)
			fakePlay(t, px, new([]string))(ctx, s)
			return quit
		},
	}

	res, err := stage.client.Sources(ctx, "ally-8", "ally", miruro.Sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.run(ctx, res, offer{Pin: stage.pin, declared: true}.source(miruro.Sub)); !errors.Is(err, quit) {
		t.Errorf("err = %v, want the player's own failure", err)
	}
	if !slices.Equal(tried, []string{"Yt-mp4"}) {
		t.Errorf("tried %v, want only the stream that played", tried)
	}
}

// ffmpeg's hls demuxer skips a segment it cannot fetch and asks for the next,
// so a stream whose CDN refuses every one runs forever without a frame
// bonk on Tensura S3 episode 19 did exactly that on 2026-08-23, its segments
// served from an ad CDN answering 403, and mpv was still running four minutes
// later having shown nothing
func TestAbandonStalled(t *testing.T) {
	restore := func(g time.Duration, b int, c time.Duration) func() {
		return func() { startGrace, refusalBudget, refusalCheck = g, b, c }
	}(startGrace, refusalBudget, refusalCheck)
	defer restore()
	startGrace, refusalBudget, refusalCheck = 5*time.Second, 3, 10*time.Millisecond

	px, err := play.StartProxy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	// skipping mimics the demuxer, fetching forever and never giving up
	skipping := func(ctx context.Context, s miruro.Stream) error {
		for {
			if ctx.Err() != nil {
				return errors.New("signal: killed")
			}
			resp, err := http.Get(px.Stream(s).URL)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}

	t.Run("a stream that is only refused is abandoned", func(t *testing.T) {
		cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer cdn.Close()

		done := make(chan error, 1)
		go func() {
			done <- playStreams(context.Background(), px,
				[]miruro.Stream{{URL: cdn.URL + "/seg.mp4", Kind: miruro.MP4, Server: "HD-1"}}, skipping)
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a stream that showed nothing must not report success")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the player was never stopped, which is the hang this guards")
		}
	})

	// bee played after two refusals on 2026-08-23, so a refusal is not by itself
	// a reason to give up on a stream
	t.Run("a stream that plays survives its refusals", func(t *testing.T) {
		var refused atomic.Int64
		cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if refused.Load() <= int64(refusalBudget) {
				refused.Add(1)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			io.WriteString(w, "picture")
		}))
		defer cdn.Close()

		ctx, cancel := context.WithCancel(context.Background())
		played := make(chan error, 1)
		go func() {
			played <- playStreams(ctx, px,
				[]miruro.Stream{{URL: cdn.URL + "/seg.mp4", Kind: miruro.MP4, Server: "HD-1"}},
				func(pctx context.Context, s miruro.Stream) error {
					// fetch until picture lands, then sit there the way a player
					// does
					// a refused fetch answers with the upstream status as its
					// body, so only the content itself counts as having played
					for pctx.Err() == nil {
						resp, err := http.Get(px.Stream(s).URL)
						if err != nil {
							continue
						}
						body, _ := io.ReadAll(resp.Body)
						resp.Body.Close()
						if resp.StatusCode == http.StatusOK && string(body) == "picture" {
							break
						}
					}
					<-pctx.Done()
					return errors.New("signal: killed")
				})
		}()

		select {
		case <-played:
			t.Fatal("a stream that relayed picture was abandoned")
		case <-time.After(2 * time.Second):
			// still running well past the refusals it spent getting there
		}
		if got := int(refused.Load()); got <= refusalBudget {
			t.Errorf("the stream was refused %d times, want more than the budget of %d", got, refusalBudget)
		}
		// the watcher reads the tunables this test rewrote, so it has to be done
		// before the restore runs
		cancel()
		<-played
	})
}
