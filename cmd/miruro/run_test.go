package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/play"
	"ysun.co/miruro/internal/upstream"
)

// reply is how a stub resolves one provider, whatever category it is asked for
type reply func(upstream.Category) (*upstream.Result, error)

func streams(ss ...upstream.Stream) reply {
	return func(upstream.Category) (*upstream.Result, error) { return &upstream.Result{Streams: ss}, nil }
}

func fails(err error) reply {
	return func(upstream.Category) (*upstream.Result, error) { return nil, err }
}

var (
	hls       = streams(upstream.Stream{URL: "http://cdn/master.m3u8", Kind: upstream.HLS, Quality: "1080p"})
	embedOnly = streams(upstream.Stream{URL: "http://cdn/embed", Kind: upstream.Embed})
	// down is what the client reports for a backend answering a server error
	down = fails(fmt.Errorf("%w: miruro status 500", upstream.ErrUnreachable))
)

// stub is a backend answering from memory
// it lists each provider under the sub and dub episodes named for it, with the
// renditions caps declares, or refuses the listing with listErr, and resolves
// each provider through replies, recording every listing and resolution it was
// asked for
type stub struct {
	t       *testing.T
	name    string
	sub     map[string][]float64
	dub     map[string][]float64
	caps    upstream.Capabilities
	replies map[string]reply
	listErr error

	mu    sync.Mutex
	lists int
	asked []string
}

func (s *stub) Name() string { return s.name }

func (s *stub) Episodes(context.Context, upstream.Media) (*upstream.Catalog, error) {
	return &upstream.Catalog{Refs: map[string]string{s.name: s.name}}, nil
}

func (s *stub) Listing(_ context.Context, _ string, number float64) (*upstream.Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	l := &upstream.Listing{Providers: map[string]upstream.Provider{}, Caps: upstream.Capabilities{}}
	add := func(code string, eps []float64, dub bool) {
		if !slices.Contains(eps, number) {
			return
		}
		p := l.Providers[code]
		p.Code, p.Backend = code, s
		e := []upstream.Episode{{ID: code + "-" + num(number), Number: number}}
		if dub {
			p.Dub = e
		} else {
			p.Sub = e
		}
		l.Providers[code] = p
		if c, ok := s.caps[code]; ok {
			l.Caps[code] = c
		}
	}
	for code, eps := range s.sub {
		add(code, eps, false)
	}
	for code, eps := range s.dub {
		add(code, eps, true)
	}
	return l, nil
}

func (s *stub) Sources(_ context.Context, _, provider string, cat upstream.Category) (*upstream.Result, error) {
	s.mu.Lock()
	s.asked = append(s.asked, provider+":"+string(cat))
	r, ok := s.replies[provider]
	s.mu.Unlock()
	if !ok {
		s.t.Errorf("unexpected provider %q probed", provider)
		return nil, errors.New("unknown provider")
	}
	return r(cat)
}

// probed is every resolution the stub was asked for, as provider:category
func (s *stub) probed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// listed is how many listings the stub was asked for
func (s *stub) listed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

// twoProviders lists ally and bonk on episode 1, where the preference order puts
// ally first, so ally probes first when no pin reorders them
func twoProviders(t *testing.T, replies map[string]reply) *stub {
	return &stub{t: t, name: "miruro", sub: map[string][]float64{"ally": {1}, "bonk": {1}}, replies: replies}
}

// resolver is the state one resolution needs against the given backends
// fallback is on because these exercise the walk past a provider, and the
// pinned run that refuses to walk has its own test
func resolver(category upstream.Category, backends ...upstream.Backend) *runState {
	cat := &upstream.Catalog{Refs: map[string]string{}}
	for _, b := range backends {
		cat.Refs[b.Name()] = b.Name()
	}
	return &runState{hc: http.DefaultClient, backends: backends, cat: cat, category: category, fallback: true}
}

// episodeBody opens with the file type box the download path checks for and
// carries nothing a player could show, which is all that check reads
const episodeBody = "\x00\x00\x00\x18ftypisom episode bytes"

// deadCDN serves an episode body except under prefix, which 404s, so a test can
// spell one dead host among live ones
func deadCDN(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, episodeBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newSaver wires a saver against b with a proxy, a download directory and a
// state directory of its own, and returns the download directory
func newSaver(t *testing.T, b *stub) (saver, string) {
	t.Helper()
	stateRoot(t)
	px, err := play.StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { px.Close() })
	dir := t.TempDir()
	state := resolver(upstream.Sub, b)
	state.title = "Show"
	state.cfg = config{Quality: "best", DownloadDir: dir}
	return saver{runState: state, px: px, media: http.DefaultClient}, dir
}

// savedEpisode asserts the episode landed whole under dir
func savedEpisode(t *testing.T, dir string) {
	t.Helper()
	if body, err := os.ReadFile(filepath.Join(dir, "Show - E1.mp4")); err != nil || string(body) != episodeBody {
		t.Errorf("saved %q (%v), want the whole episode", body, err)
	}
}

func TestAutoResolve(t *testing.T) {
	ctx := context.Background()

	t.Run("falls back when the pinned provider errors", func(t *testing.T) {
		b := twoProviders(t, map[string]reply{"bonk": down, "ally": hls})
		res, src, err := resolver(upstream.Sub, b).autoResolve(ctx, 1, Pin{Code: "bonk"}, nil)
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
		b := twoProviders(t, map[string]reply{"bonk": fails(upstream.ErrBlocked), "ally": hls})
		_, _, err := resolver(upstream.Sub, b).autoResolve(ctx, 1, Pin{Code: "bonk"}, nil)
		if !errors.Is(err, upstream.ErrBlocked) {
			t.Fatalf("err = %v, want ErrBlocked", err)
		}
		if n := len(b.probed()); n != 1 {
			t.Errorf("probed %d providers after the block, want 1", n)
		}
	})

	t.Run("an embed-only provider is skipped", func(t *testing.T) {
		b := twoProviders(t, map[string]reply{"ally": embedOnly, "bonk": hls})
		_, src, err := resolver(upstream.Sub, b).autoResolve(ctx, 1, Pin{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if src.Code != "bonk" {
			t.Errorf("served = %q, want bonk", src.Code)
		}
	})

	t.Run("no provider has the episode", func(t *testing.T) {
		b := twoProviders(t, nil)
		_, _, err := resolver(upstream.Sub, b).autoResolve(ctx, 9, Pin{}, nil)
		if err == nil || !strings.Contains(err.Error(), "no provider has episode 9") {
			t.Fatalf("err = %v, want the no-source error", err)
		}
		if n := len(b.probed()); n != 0 {
			t.Errorf("probed %d providers for an absent episode, want 0", n)
		}
	})
}

// the catalog pages at fifteen, so the picker ends in a row asking for the next
// page while there is one, and only then
func TestHits(t *testing.T) {
	media := []upstream.Media{{ID: 1, Romaji: "One"}, {ID: 2, Romaji: "Two"}}
	rows := hits(media, true)
	if len(rows) != 3 || !rows[2].more || rows[0].more || rows[0].media.ID != 1 {
		t.Fatalf("rows = %+v, want both titles then the more row", rows)
	}
	if got := hitLabel(rows[2]); got != "more results" {
		t.Errorf("more row reads %q", got)
	}
	if got := hitLabel(rows[0]); got != mediaLabel(media[0]) {
		t.Errorf("title row reads %q, want the media label", got)
	}
	if rows := hits(media, false); len(rows) != 2 {
		t.Errorf("rows = %+v, want no more row on the last page", rows)
	}
}

func TestMediaLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    upstream.Media
		want string
	}{
		{"format and count", upstream.Media{English: "T", Format: "TV", Episodes: 12}, "T (TV, 12 eps)"},
		{"mapped format", upstream.Media{English: "T", Format: "TV_SHORT", Episodes: 3}, "T (TV Short, 3 eps)"},
		{"movie without count", upstream.Media{English: "T", Format: "MOVIE"}, "T (Movie)"},
		{"unknown format passes through", upstream.Media{English: "T", Format: "WEIRD"}, "T (WEIRD)"},
		{"bare title", upstream.Media{English: "T"}, "T"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mediaLabel(tc.m); got != tc.want {
				t.Errorf("mediaLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFind(t *testing.T) {
	eps := []upstream.Episode{{ID: "a1", Number: 1}, {ID: "a2", Number: 2.5}}
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
		{"open start", "-2.5", []float64{1, 2, 2.5}, false},
		{"open end", "3-", []float64{3, 10}, false},
		{"open end clamps nothing", "2.5 -", []float64{2.5, 3, 10}, false},
		{"latest", "latest", []float64{10}, false},
		{"latest in any case", "Latest", []float64{10}, false},
		{"no bound at all", "-", nil, true},
		{"garbage", "abc", nil, true},
		{"bad range bound", "a-3", nil, true},
		{"bad open bound", "x-", nil, true},
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

// a range wider than the catalog is clamped and says so, while a bound left off
// asked for nothing, so it clamps nothing and says nothing
func TestParseEpisodesSaysWhenItClamps(t *testing.T) {
	numbers := []float64{1, 2, 2.5, 3, 10}
	for _, tc := range []struct {
		spec   string
		warned bool
	}{
		{"2-3", false},
		{"2-20", true},
		{"0-3", true},
		{"0-20", true},
		{"3-", false},
		{"-2.5", false},
	} {
		said := captureLog(t)
		if _, err := parseEpisodes(tc.spec, numbers); err != nil {
			t.Fatalf("parseEpisodes(%q): %v", tc.spec, err)
		}
		if got := strings.Contains(said.String(), "range clamped"); got != tc.warned {
			t.Errorf("parseEpisodes(%q) warned = %v, want %v:\n%s", tc.spec, got, tc.warned, said)
		}
	}
}

// -8 reads like a flag, so it has to reach -e as its value, which pflag does
// for a flag that takes one
func TestEpisodeFlagTakesAnOpenStart(t *testing.T) {
	t.Cleanup(func() { flagEpisode = "" })
	for _, args := range [][]string{{"-e", "-8"}, {"--episode", "-8"}, {"-e=-8"}} {
		flagEpisode = ""
		if err := root.ParseFlags(args); err != nil || flagEpisode != "-8" {
			t.Errorf("parsing %v gave %q, %v, want -8", args, flagEpisode, err)
		}
	}
}

func TestControls(t *testing.T) {
	numbers := []float64{1, 2, 3}
	for _, tc := range []struct {
		name    string
		ep      float64
		servers int
		want    []string
	}{
		{"first has no previous", 1, 1, []string{"next", "replay", "select", "change provider", "quit"}},
		{"middle has both", 2, 1, []string{"next", "replay", "previous", "select", "change provider", "quit"}},
		{"last has no next", 3, 1, []string{"replay", "previous", "select", "change provider", "quit"}},
		{"a second server offers another stream", 2, 2, []string{"next", "replay", "previous", "select", "change stream", "change provider", "quit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := controls(numbers, tc.ep, tc.servers); !slices.Equal(got, tc.want) {
				t.Errorf("controls(ep %v, %d servers) = %v, want %v", tc.ep, tc.servers, got, tc.want)
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
		{"change stream", step{ep: 2, restream: true}, false},
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

// a server listing several qualities is one host to move past, neither a dead
// stream nor an embed is one to move to, and a skip names the provider it was
// made under
func TestHosts(t *testing.T) {
	res := &upstream.Result{Streams: []upstream.Stream{
		{URL: "https://a/720.mp4", Kind: upstream.MP4, Server: "HD-1", Quality: "720p"},
		{URL: "https://a/1080.mp4", Kind: upstream.MP4, Server: "HD-1", Quality: "1080p"},
		{URL: "https://b/master.m3u8", Kind: upstream.HLS, Server: "HD-2"},
		{URL: "https://c/master.m3u8", Kind: upstream.HLS, Server: "HD-3", Dead: true},
		{URL: "https://d/e/1", Kind: upstream.Embed, Server: "Vidstream"},
	}}
	for _, tc := range []struct {
		name    string
		skipped map[feed]bool
		want    int
	}{
		{"nothing skipped", nil, 2},
		{"the server moved past", map[feed]bool{{"kiwi", "HD-1"}: true}, 1},
		{"the same server under another provider", map[feed]bool{{"hop", "HD-1"}: true}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hosts(res, "kiwi", tc.skipped); got != tc.want {
				t.Errorf("hosts = %d, want %d", got, tc.want)
			}
		})
	}
}

// change stream keeps adding the stream on screen to what is skipped, one that
// never reached the screen adds nothing, and any other step lets go of it all
func TestMoveOn(t *testing.T) {
	a, b := feed{"bonk", "HD-1"}, feed{"bonk", "HD-2"}
	skipped := moveOn(nil, step{ep: 2, restream: true}, a)
	skipped = moveOn(skipped, step{ep: 2, restream: true}, b)
	if want := map[feed]bool{a: true, b: true}; !maps.Equal(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
	if got := moveOn(skipped, step{ep: 2, restream: true}, feed{}); len(got) != 2 {
		t.Errorf("a stream never shown changed what is skipped to %v", got)
	}
	for _, next := range []step{{ep: 3}, {ep: 2}, {reselect: true}, {ep: 2, reprovide: true}} {
		if got := moveOn(map[feed]bool{a: true}, next, b); got != nil {
			t.Errorf("step %+v kept %v skipped", next, got)
		}
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

// a backend whose firewall refused the run would answer every one of its
// providers the same, so the walk skips them and reaches the other backend,
// and only when nothing else served is the refusal what comes back
func TestAutoResolveSkipsABlockedBackend(t *testing.T) {
	blocked := &stub{t: t, name: "miruro", sub: map[string][]float64{"ally": {1}, "pewe": {1}},
		replies: map[string]reply{"ally": fails(upstream.ErrBlocked), "pewe": fails(upstream.ErrBlocked)}}
	open := &stub{t: t, name: "other", sub: map[string][]float64{"other": {1}},
		replies: map[string]reply{"other": streams(upstream.Stream{URL: "other", Kind: upstream.HLS})}}
	st := resolver(upstream.Sub, blocked, open)
	res, src, err := st.autoResolve(context.Background(), 1, Pin{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if src.Code != "other" || res.Streams[0].URL != "other" {
		t.Errorf("served = %q, want other", src.Code)
	}
	if n := len(blocked.probed()); n != 1 {
		t.Errorf("blocked backend asked %d times, want once", n)
	}

	// the refusal holds for the run, so the next episode costs it no request
	if _, src, err := st.autoResolve(context.Background(), 1, Pin{Code: "ally"}, nil); err != nil || src.Code != "other" {
		t.Errorf("second resolution = %q, %v, want other again", src.Code, err)
	}
	if n := len(blocked.probed()); n != 1 {
		t.Errorf("blocked backend asked %d times across two resolutions, want once", n)
	}

	open.sub = nil
	if _, _, err := st.autoResolve(context.Background(), 1, Pin{}, nil); !errors.Is(err, upstream.ErrBlocked) {
		t.Errorf("err = %v, want %v when nothing else served", err, upstream.ErrBlocked)
	}
}

// the play request is where the miruro backend meets its firewall, so a refusal
// there takes the backend out of the run the way a refused resolution does,
// and one that merely failed costs the episode its providers and says why
func TestListingFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("a refused listing holds for the run", func(t *testing.T) {
		blocked := &stub{t: t, name: "miruro", listErr: upstream.ErrBlocked}
		open := &stub{t: t, name: "other", sub: map[string][]float64{"other": {1}}, replies: map[string]reply{"other": hls}}
		st := resolver(upstream.Sub, blocked, open)
		for range 2 {
			if _, src, err := st.autoResolve(ctx, 1, Pin{}, nil); err != nil || src.Code != "other" {
				t.Fatalf("served = %q, %v, want other", src.Code, err)
			}
		}
		if n := blocked.listed(); n != 1 {
			t.Errorf("the refusing backend was asked %d times, want once", n)
		}
		open.sub = nil
		if _, _, err := st.autoResolve(ctx, 1, Pin{}, nil); !errors.Is(err, upstream.ErrBlocked) {
			t.Errorf("err = %v, want %v when nothing else lists the episode", err, upstream.ErrBlocked)
		}
	})

	t.Run("a failed listing is what comes back when nothing else lists", func(t *testing.T) {
		dead := &stub{t: t, name: "miruro", listErr: fmt.Errorf("%w: miruro status 502", upstream.ErrUnreachable)}
		_, _, err := resolver(upstream.Sub, dead).autoResolve(ctx, 1, Pin{}, nil)
		if !errors.Is(err, upstream.ErrUnreachable) || !strings.Contains(err.Error(), "miruro: ") {
			t.Errorf("err = %v, want the failure named after its backend", err)
		}
	})
}

func TestEnabled(t *testing.T) {
	a, b := &stub{name: "miruro"}, &stub{name: "other"}
	all := upstream.Backends{a, b}
	names := func(bs upstream.Backends) string {
		var out []string
		for _, b := range bs {
			out = append(out, b.Name())
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		names []string
		want  string
	}{
		{nil, "miruro,other"},
		{[]string{"other"}, "other"},
		{[]string{"other", "miruro"}, "other,miruro"},
		{[]string{"anilist", "miruro"}, "miruro"},
		{[]string{"anilist"}, "miruro,other"},
	} {
		if got := names(enabled(all, tc.names)); got != tc.want {
			t.Errorf("enabled(%v) = %s, want %s", tc.names, got, tc.want)
		}
	}
}

// a retried episode must move past the providers it already burned, or the
// fallback loop would resolve the same dead source forever
func TestAutoResolveSkipsProvidersAlreadyTried(t *testing.T) {
	b := twoProviders(t, map[string]reply{"bonk": hls})
	_, src, err := resolver(upstream.Sub, b).autoResolve(context.Background(), 1, Pin{}, map[string]bool{"ally": true})
	if err != nil {
		t.Fatal(err)
	}
	if src.Code != "bonk" {
		t.Errorf("served = %q, want bonk", src.Code)
	}

	_, _, err = resolver(upstream.Sub, b).autoResolve(context.Background(), 1, Pin{}, map[string]bool{"ally": true, "bonk": true})
	if err == nil || !strings.Contains(err.Error(), "no provider resolved a stream") {
		t.Fatalf("err = %v, want the spent-walk error once every provider is tried", err)
	}
}

// a provider that resolves and then dies mid-download used to lose the episode
func TestSaveFallsBackToAnotherProvider(t *testing.T) {
	cdn := deadCDN(t, "/dead")
	sv, dir := newSaver(t, twoProviders(t, map[string]reply{
		"ally": streams(upstream.Stream{URL: cdn.URL + "/dead.mp4", Kind: upstream.MP4}),
		"bonk": streams(upstream.Stream{URL: cdn.URL + "/live.mp4", Kind: upstream.MP4}),
	}))
	if _, _, _, err := sv.save(context.Background(), 1, nil); err != nil {
		t.Fatalf("the fallback provider did not save the episode: %v", err)
	}
	savedEpisode(t, dir)
}

// with every provider dead the episode fails, and the report has to name the
// download that failed rather than the resolution that ran out of providers
func TestSaveReportsTheDownloadFailure(t *testing.T) {
	cdn := httptest.NewServer(http.NotFoundHandler())
	defer cdn.Close()

	sv, _ := newSaver(t, twoProviders(t, map[string]reply{
		"ally": streams(upstream.Stream{URL: cdn.URL + "/a.mp4", Kind: upstream.MP4}),
		"bonk": streams(upstream.Stream{URL: cdn.URL + "/b.mp4", Kind: upstream.MP4}),
	}))
	_, _, _, err := sv.save(context.Background(), 1, nil)
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
	sv, dir := newSaver(t, &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{
		"bonk": streams(
			upstream.Stream{URL: cdn.URL + "/hd1.mp4", Kind: upstream.MP4},
			upstream.Stream{URL: cdn.URL + "/hd2.mp4", Kind: upstream.MP4}),
	}})
	if _, _, _, err := sv.save(context.Background(), 1, nil); err != nil {
		t.Fatalf("the second stream did not save the episode: %v", err)
	}
	savedEpisode(t, dir)
}

// an episode already on disk is left out before anything is resolved for it,
// and a rerun over a finished range says so rather than drawing finished bars
func TestDownloadLeavesEpisodesOnDiskAlone(t *testing.T) {
	cdn := deadCDN(t, "/dead")
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1, 2}}, replies: map[string]reply{
		"bonk": streams(upstream.Stream{URL: cdn.URL + "/live.mp4", Kind: upstream.MP4}),
	}}
	sv, dir := newSaver(t, b)
	if err := os.WriteFile(filepath.Join(dir, "Show - E1.mp4"), []byte(episodeBody), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	out := printed(t, func() error { return sv.download(ctx, []float64{1, 2}, Pin{}) })
	if want := fmt.Sprintf("saved 1 episode to %s, 1 already there\n", dir); out != want {
		t.Errorf("printed %q, want %q", out, want)
	}
	if n := b.listed(); n != 1 {
		t.Errorf("listed %d episodes, want only the one missing", n)
	}

	out = printed(t, func() error { return sv.download(ctx, []float64{1, 2}, Pin{}) })
	if want := fmt.Sprintf("2 episodes already in %s\n", dir); out != want {
		t.Errorf("printed %q, want %q", out, want)
	}
	if n := b.listed(); n != 1 {
		t.Errorf("a finished range was listed again, %d listings in all", n)
	}
}

// a sidecar that failed is owed to its episode, and the next run over it fetches
// the sidecar from the provider the video came from without the video again
func TestDownloadFetchesOwedSubtitles(t *testing.T) {
	var up atomic.Bool
	var videos atomic.Int64
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/live.mp4":
			videos.Add(1)
			io.WriteString(w, episodeBody)
		case r.URL.Path == "/en.vtt" && up.Load():
			io.WriteString(w, "WEBVTT\n\n00:00.000 --> 00:01.000\nhello\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer cdn.Close()

	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{
		"bonk": func(upstream.Category) (*upstream.Result, error) {
			return &upstream.Result{
				Streams:   []upstream.Stream{{URL: cdn.URL + "/live.mp4", Kind: upstream.MP4, Server: "HD-1"}},
				Subtitles: []upstream.Subtitle{{File: cdn.URL + "/en.vtt", Label: "English", Lang: "en"}},
			}, nil
		},
	}}
	sv, dir := newSaver(t, b)
	side := filepath.Join(dir, "Show - E1.en.vtt")
	ctx := context.Background()

	out := printed(t, func() error { return sv.download(ctx, []float64{1}, Pin{}) })
	if want := fmt.Sprintf("saved 1 episode to %s\n", dir); out != want {
		t.Errorf("first run printed %q, want %q", out, want)
	}
	if _, err := os.Stat(side); err == nil {
		t.Fatal("the sidecar landed while its host was down")
	}

	up.Store(true)
	out = printed(t, func() error { return sv.download(ctx, []float64{1}, Pin{}) })
	if want := "fetched missing subtitles for 1 episode\n"; out != want {
		t.Errorf("second run printed %q, want %q", out, want)
	}
	if body, err := os.ReadFile(side); err != nil || !strings.HasPrefix(string(body), "WEBVTT") {
		t.Errorf("sidecar holds %q (%v), want the track", body, err)
	}
	if n := videos.Load(); n != 1 {
		t.Errorf("the video was fetched %d times, want once", n)
	}

	out = printed(t, func() error { return sv.download(ctx, []float64{1}, Pin{}) })
	if want := fmt.Sprintf("1 episode already in %s\n", dir); out != want {
		t.Errorf("third run printed %q, want %q", out, want)
	}
	if n := b.listed(); n != 2 {
		t.Errorf("listed %d times, want the download and the owed sidecar and nothing after", n)
	}
}

// a run interrupted between the video and its sidecars leaves the episode on
// disk, so the sidecars it never reached are owed as much as failed ones
func TestSaveOwesTheSidecarsAnInterruptedRunMissed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live.mp4" {
			io.WriteString(w, episodeBody)
			return
		}
		// the user interrupts while the sidecar is on its way
		cancel()
		<-r.Context().Done()
	}))
	defer cdn.Close()

	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{
		"bonk": func(upstream.Category) (*upstream.Result, error) {
			return &upstream.Result{
				Streams:   []upstream.Stream{{URL: cdn.URL + "/live.mp4", Kind: upstream.MP4, Server: "HD-1"}},
				Subtitles: []upstream.Subtitle{{File: cdn.URL + "/en.vtt", Label: "English", Lang: "en"}},
			}, nil
		},
	}}
	sv, dir := newSaver(t, b)
	if _, _, _, err := sv.save(ctx, 1, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the interruption", err)
	}
	savedEpisode(t, dir)
	o, ok := owing(filepath.Join(dir, "Show - E1.mp4"))
	if !ok {
		t.Fatal("the interrupted sidecar is owed nothing")
	}
	if o.Provider != "bonk" || o.Category != upstream.Sub || o.Server != "HD-1" {
		t.Errorf("owed %+v, want bonk's sub rendition from HD-1", o)
	}
}

// an owed sidecar whose provider no longer lists the episode is given up once
// rather than resolved on every rerun, and the episode counts as on disk
func TestDownloadGivesUpOwedSubtitlesThatAreGone(t *testing.T) {
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {1}}, replies: map[string]reply{}}
	sv, dir := newSaver(t, b)
	video := filepath.Join(dir, "Show - E1.mp4")
	if err := os.WriteFile(video, []byte(episodeBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (owed{Video: video, Provider: "gone", Category: upstream.Sub, Server: "HD-1"}).record(); err != nil {
		t.Fatal(err)
	}

	said := captureLog(t)
	out := printed(t, func() error { return sv.download(context.Background(), []float64{1}, Pin{}) })
	if want := fmt.Sprintf("1 episode already in %s\n", dir); out != want {
		t.Errorf("printed %q, want %q", out, want)
	}
	if !strings.Contains(said.String(), "provider no longer lists the episode") {
		t.Errorf("the log does not say why the subtitles were given up:\n%s", said)
	}
	if _, ok := owing(video); ok {
		t.Error("the record outlived a provider that no longer lists the episode")
	}
}

// a bulk run that falls to another rendition must say so, and the measure is
// the source the pinned pick would have resolved
func TestSaverWanted(t *testing.T) {
	l := &upstream.Listing{
		Providers: map[string]upstream.Provider{
			"kiwi": {Code: "kiwi", Sub: []upstream.Episode{{ID: "k1", Number: 1}}},
			"bee":  {Code: "bee", Sub: []upstream.Episode{{ID: "b1", Number: 1}}},
		},
		Caps: testCaps,
	}
	sv := saver{runState: &runState{category: upstream.Sub}}

	if _, ok := sv.wanted(l, 1); ok {
		t.Error("no pin still produced an expectation")
	}

	sv.pin = Pin{"kiwi", Hard}
	want, ok := sv.wanted(l, 1)
	if !ok || want.Category != upstream.Sub || want.Attach {
		t.Errorf("wanted = (%+v, %v), want kiwi's burned-in rendition", want, ok)
	}
	// bee's soft source is what a fallback would have served, and it has to read
	// as a swap against the hard pin
	swap := offer{Pin: Pin{"bee", Soft}, declared: true}.source(upstream.Sub)
	if swap.Category == want.Category && swap.Attach == want.Attach {
		t.Error("a soft fallback reads as the pinned rendition")
	}

	// a bare pin measures against what its provider declares
	sv.pin = Pin{Code: "kiwi"}
	if want, ok := sv.wanted(l, 1); !ok || want.Category != upstream.Sub || want.Attach {
		t.Errorf("wanted = (%+v, %v), want the declared hardsub for a bare kiwi pin", want, ok)
	}
}

// fakePlay stands in for the player, fetching what it was handed the way a real
// one does, so the proxy sees exactly what playback would have made it see
func fakePlay(t *testing.T, tried *[]string) func(context.Context, upstream.Stream, *play.Tally) error {
	return func(ctx context.Context, s upstream.Stream, tl *play.Tally) error {
		*tried = append(*tried, s.Server)
		resp, err := http.Get(tl.Stream(s).URL)
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

	px, err := play.StartProxy(ctx, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	dead := upstream.Stream{URL: cdn.URL + "/dead.mp4", Kind: upstream.MP4, Server: "HD-1"}
	live := upstream.Stream{URL: cdn.URL + "/live.mp4", Kind: upstream.MP4, Server: "HD-2"}

	t.Run("a stream that never started falls through", func(t *testing.T) {
		var tried []string
		if _, err := patience().playStreams(ctx, px, []upstream.Stream{dead, live}, fakePlay(t, &tried)); err != nil {
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
		_, err := patience().playStreams(ctx, px, []upstream.Stream{live, dead}, func(ctx context.Context, s upstream.Stream, tl *play.Tally) error {
			fakePlay(t, &tried)(ctx, s, tl)
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
		_, err := patience().playStreams(ctx, px, []upstream.Stream{dead, dead}, fakePlay(t, &tried))
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
		err    error
		served int
		want   bool
	}{
		{fail, 0, true},  // exited with an error having got no picture
		{fail, 4, false}, // played, then failed, so the user or the CDN quit
		{nil, 0, false},  // a clean exit is never retried
		{nil, 9, false},
	}
	for _, c := range cases {
		if got := deadStream(c.err, c.served); got != c.want {
			t.Errorf("deadStream(%v, %d) = %v, want %v", c.err, c.served, got, c.want)
		}
	}
}

// a catalog that names nothing must still render a bare number
func TestEpisodeLabel(t *testing.T) {
	label := episodeLabel(map[float64]upstream.Episode{
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

// the two sub renditions are separate tracks on the wire, and a provider lists
// only the ones it carries, so the variant has to reach Sources rather than
// stay a client-side attach decision
func TestAutoResolveAsksForTheDeclaredRendition(t *testing.T) {
	caps := upstream.Capabilities{
		"kiwi": {Hard: true},
		"bee":  {Soft: true},
		"bonk": {Hard: true, Soft: true},
	}

	for _, tc := range []struct {
		name     string
		pin      Pin
		category upstream.Category
		wantCat  string
		attach   bool
	}{
		{"a hardsub provider is asked for sub", Pin{"kiwi", Hard}, upstream.Sub, "sub", false},
		{"a softsub provider is asked for ssub", Pin{"bee", Soft}, upstream.Sub, "ssub", true},
		{"bonk soft is asked for ssub", Pin{"bonk", Soft}, upstream.Sub, "ssub", true},
		{"bonk hard is asked for sub", Pin{"bonk", Hard}, upstream.Sub, "sub", false},
		// a pin contradicting the listing would ask for a rendition the provider
		// does not serve, so the declared one wins over what was typed
		{"a soft pin on a hardsub provider is corrected", Pin{"kiwi", Soft}, upstream.Sub, "sub", false},
		{"a hard pin on a softsub provider is corrected", Pin{"bee", Hard}, upstream.Sub, "ssub", true},
		// the variant names a sub rendition, so a dub run ignores it rather than
		// suppressing one provider's tracks and no other's
		{"dub is never rewritten and keeps its tracks", Pin{"bonk", Hard}, upstream.Dub, "dub", true},
		{"dub with a soft pin keeps them too", Pin{"bonk", Soft}, upstream.Dub, "dub", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &stub{t: t, name: "miruro", caps: caps,
				sub:     map[string][]float64{"kiwi": {1}, "bee": {1}, "bonk": {1}},
				replies: map[string]reply{"kiwi": hls, "bee": hls, "bonk": hls}}
			if tc.category == upstream.Dub {
				b.sub, b.dub = nil, map[string][]float64{"bonk": {1}}
			}
			_, src, err := resolver(tc.category, b).autoResolve(context.Background(), 1, tc.pin, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := b.probed(), []string{tc.pin.Code + ":" + tc.wantCat}; !slices.Equal(got, want) {
				t.Errorf("sources asked %v, want %v", got, want)
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

	px, err := play.StartProxy(ctx, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"ally": {8}, "pewe": {8}}, replies: map[string]reply{
		"ally": streams(
			upstream.Stream{URL: cdn.URL + "/refused-a.mp4", Kind: upstream.MP4, Server: "Yt-mp4"},
			upstream.Stream{URL: cdn.URL + "/refused-b.mp4", Kind: upstream.MP4, Server: "Mp4"}),
		"pewe": streams(upstream.Stream{URL: cdn.URL + "/live.m3u8", Kind: upstream.MP4, Server: "AniDBApp"}),
	}}
	st := resolver(upstream.Sub, b)
	st.title, st.cfg = "Grow Up Show", config{Quality: "best"}

	var tried []string
	stage := playback{
		runState: st,
		px:       px,
		watch:    patience(),
		pin:      Pin{Code: "ally", Variant: Hard},
		ep:       8,
		launch: func(ctx context.Context, s upstream.Stream, tl *play.Tally, _ []upstream.Subtitle) error {
			tried = append(tried, s.Server)
			return fakePlay(t, new([]string))(ctx, s, tl)
		},
	}

	// ally is the pin, so it is what resolve would have handed over
	res, err := b.Sources(ctx, "ally-8", "ally", upstream.Sub)
	if err != nil {
		t.Fatal(err)
	}
	said := captureLog(t)
	if err := stage.run(ctx, res, offer{Pin: stage.pin, declared: true}.source(upstream.Sub)); err != nil {
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
// the proxy's handlers log from their own goroutines, and one still finishing
// a request the player dropped writes while the test reads, so the buffer is
// locked
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	b := &lockedBuffer{}
	level := log.GetLevel()
	log.SetOutput(b)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetLevel(level)
	})
	return b
}

// lockedBuffer is a log sink a test may read while writers are still running
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// a stream that produced picture and then failed is the user's to deal with,
// so the walk must not restart the episode somewhere else under them
func TestPlaybackKeepsAProviderThatPlayed(t *testing.T) {
	ctx := context.Background()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "picture")
	}))
	defer cdn.Close()

	px, err := play.StartProxy(ctx, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	// pewe is a second provider the walk could reach, since fallback is on, so
	// the guard has something to prevent rather than nothing to do
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"ally": {8}, "pewe": {8}}, replies: map[string]reply{
		"ally": streams(upstream.Stream{URL: cdn.URL + "/live.mp4", Kind: upstream.MP4, Server: "Yt-mp4"}),
		"pewe": streams(upstream.Stream{URL: cdn.URL + "/other.mp4", Kind: upstream.MP4, Server: "AniDBApp"}),
	}}
	st := resolver(upstream.Sub, b)
	st.cfg = config{Quality: "best"}

	quit := errors.New("player exit 4")
	var tried []string
	stage := playback{
		runState: st,
		px:       px,
		watch:    patience(),
		pin:      Pin{Code: "ally", Variant: Hard},
		ep:       8,
		launch: func(ctx context.Context, s upstream.Stream, tl *play.Tally, _ []upstream.Subtitle) error {
			tried = append(tried, s.Server)
			fakePlay(t, new([]string))(ctx, s, tl)
			return quit
		},
	}

	res, err := b.Sources(ctx, "ally-8", "ally", upstream.Sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.run(ctx, res, offer{Pin: stage.pin, declared: true}.source(upstream.Sub)); !errors.Is(err, quit) {
		t.Errorf("err = %v, want the player's own failure", err)
	}
	if !slices.Equal(tried, []string{"Yt-mp4"}) {
		t.Errorf("tried %v, want only the stream that played", tried)
	}
}

// change stream replays the episode past the streams moved past, and one that
// leaves a provider nothing moves on to the next as a dead one would
func TestPlaybackSkipsTheStreamsMovedPast(t *testing.T) {
	ctx := context.Background()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "picture")
	}))
	defer cdn.Close()

	px, err := play.StartProxy(ctx, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	// bonk and pewe both name a server HD-1, so a skip has to say whose it is
	b := &stub{t: t, name: "miruro", sub: map[string][]float64{"bonk": {8}, "pewe": {8}}, replies: map[string]reply{
		"bonk": streams(
			upstream.Stream{URL: cdn.URL + "/a.m3u8", Kind: upstream.HLS, Server: "HD-1", Default: true},
			upstream.Stream{URL: cdn.URL + "/b.m3u8", Kind: upstream.HLS, Server: "HD-2"}),
		"pewe": streams(upstream.Stream{URL: cdn.URL + "/c.m3u8", Kind: upstream.HLS, Server: "HD-1"}),
	}}
	st := resolver(upstream.Sub, b)
	st.cfg = config{Quality: "best"}

	for _, tc := range []struct {
		name  string
		skip  map[feed]bool
		tried []string
		shown feed
	}{
		{"the provider's next server", map[feed]bool{{"bonk", "HD-1"}: true}, []string{"HD-2"}, feed{"bonk", "HD-2"}},
		{"the next provider once none is left", map[feed]bool{{"bonk", "HD-1"}: true, {"bonk", "HD-2"}: true}, []string{"HD-1"}, feed{"pewe", "HD-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tried []string
			var shown feed
			stage := playback{
				runState: st,
				px:       px,
				watch:    patience(),
				pin:      Pin{Code: "bonk", Variant: Hard},
				ep:       8,
				skip:     tc.skip,
				shown:    &shown,
				launch: func(ctx context.Context, s upstream.Stream, tl *play.Tally, _ []upstream.Subtitle) error {
					return fakePlay(t, &tried)(ctx, s, tl)
				},
			}
			res, err := b.Sources(ctx, "bonk-8", "bonk", upstream.Sub)
			if err != nil {
				t.Fatal(err)
			}
			if err := stage.run(ctx, res, offer{Pin: stage.pin, declared: true}.source(upstream.Sub)); err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(tried, tc.tried) {
				t.Errorf("tried %v, want %v", tried, tc.tried)
			}
			if shown != tc.shown {
				t.Errorf("shown %+v, want %+v", shown, tc.shown)
			}
		})
	}
}

// ffmpeg's hls demuxer skips a segment it cannot fetch and asks for the next,
// so a stream whose CDN refuses every one runs forever without a frame
// bonk on Tensura S3 episode 19 did exactly that on 2026-08-23, its segments
// served from an ad CDN answering 403, and mpv was still running four minutes
// later having shown nothing
func TestAbandonStalled(t *testing.T) {
	wd := watchdog{grace: 5 * time.Second, budget: 3, check: 10 * time.Millisecond}

	px, err := play.StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	// skipping mimics the demuxer, fetching forever and never giving up
	skipping := func(ctx context.Context, s upstream.Stream, tl *play.Tally) error {
		for {
			if ctx.Err() != nil {
				return errors.New("signal: killed")
			}
			resp, err := http.Get(tl.Stream(s).URL)
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
			_, err := wd.playStreams(context.Background(), px,
				[]upstream.Stream{{URL: cdn.URL + "/seg.mp4", Kind: upstream.MP4, Server: "HD-1"}}, skipping)
			done <- err
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
			if refused.Load() <= int64(wd.budget) {
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
			_, err := wd.playStreams(ctx, px,
				[]upstream.Stream{{URL: cdn.URL + "/seg.mp4", Kind: upstream.MP4, Server: "HD-1"}},
				func(pctx context.Context, s upstream.Stream, tl *play.Tally) error {
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
			played <- err
		}()

		select {
		case <-played:
			t.Fatal("a stream that relayed picture was abandoned")
		case <-time.After(2 * time.Second):
			// still running well past the refusals it spent getting there
		}
		if got := int(refused.Load()); got <= wd.budget {
			t.Errorf("the stream was refused %d times, want more than the budget of %d", got, wd.budget)
		}
		cancel()
		<-played
	})
}

// walkHLS fetches a playlist and everything it names, a playlist's children
// in turn, the way a player starts a stream, and reports nothing of what came
// back since the proxy's tally is what a test reads
func walkHLS(u string) {
	resp, err := http.Get(u)
	if err != nil {
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(body), "#EXTM3U") {
		return
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, `URI="`):
			ref := strings.SplitN(strings.SplitN(line, `URI="`, 2)[1], `"`, 2)[0]
			walkHLS(ref)
		case line != "" && !strings.HasPrefix(line, "#"):
			walkHLS(line)
		}
	}
}

// a demuxed stream whose audio rendition relays nothing plays the picture
// silent, hop's shape, and the watch says so once the picture has run a grace
// without stopping a player the user is watching
func TestWatchdogHearsASilentStream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		audio  int
		silent bool
	}{
		{"audio refused", http.StatusForbidden, true},
		{"audio playing", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/master.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"ja\",DEFAULT=YES,URI=\"audio.m3u8\"\n"+
						"#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nvideo.m3u8\n")
				case "/video.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXTINF:1,\nv0.ts\n#EXT-X-ENDLIST\n")
				case "/audio.m3u8":
					io.WriteString(w, "#EXTM3U\n#EXTINF:1,\na0.ts\n#EXT-X-ENDLIST\n")
				case "/v0.ts":
					w.Write(bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 8))
				case "/a0.ts":
					if tc.audio != http.StatusOK {
						w.WriteHeader(tc.audio)
						return
					}
					w.Write(bytes.Repeat(append([]byte{0x47}, make([]byte, 187)...), 8))
				}
			}))
			defer cdn.Close()

			px, err := play.StartProxy(context.Background(), http.DefaultClient)
			if err != nil {
				t.Fatal(err)
			}
			defer px.Close()

			said := captureLog(t)
			wd := watchdog{grace: 200 * time.Millisecond, budget: 8, check: 10 * time.Millisecond}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := wd.playStreams(ctx, px, []upstream.Stream{{URL: cdn.URL + "/master.m3u8", Kind: upstream.HLS, Server: "Vid"}},
					func(pctx context.Context, s upstream.Stream, tl *play.Tally) error {
						walkHLS(tl.Stream(s).URL)
						<-pctx.Done()
						return errors.New("signal: killed")
					})
				done <- err
			}()
			time.Sleep(time.Second)
			cancel()
			<-done

			warned := strings.Contains(said.String(), "plays without sound")
			if warned != tc.silent {
				t.Errorf("warned = %v, want %v:\n%s", warned, tc.silent, said)
			}
			if strings.Contains(said.String(), "abandoning") {
				t.Errorf("a stream showing picture was abandoned:\n%s", said)
			}
		})
	}
}

// the menu is raised while the player runs, so what the run saves and what it
// returns depends on which of the two ended first, and on how
func TestPlayAndControl(t *testing.T) {
	ctx := context.Background()
	exitErr := exec.Command("false").Run()

	// until canceled is a player the user is still watching
	untilCanceled := func(pctx context.Context) error {
		<-pctx.Done()
		return pctx.Err()
	}
	dismissing := func(_ context.Context, _ string, _ []string, wait func() bool) (string, bool, error) {
		if wait() {
			return "", true, nil
		}
		// the user picks once the playback is over and the menu stayed up
		return "replay", true, nil
	}

	t.Run("an early pick stops the player and still counts as watched", func(t *testing.T) {
		saved := 0
		early := func(context.Context, string, []string, func() bool) (string, bool, error) { return "next", false, nil }
		action, err := playAndControl(ctx, early, "E1", []string{"next"}, false, untilCanceled, func() error { saved++; return nil })
		if err != nil || action != "next" {
			t.Fatalf("action = %q, %v, want next", action, err)
		}
		if saved != 1 {
			t.Errorf("saved %d times, want the interrupted episode recorded once", saved)
		}
	})

	t.Run("a player that never ran ends the run with its reason", func(t *testing.T) {
		saved := 0
		never := errors.New("exec: mpv: not found")
		action, err := playAndControl(ctx, dismissing, "E1", nil, true,
			func(context.Context) error { return never }, func() error { saved++; return nil })
		if !errors.Is(err, never) || action != "" {
			t.Fatalf("action = %q, err = %v, want the launch failure", action, err)
		}
		if saved != 0 {
			t.Errorf("saved %d times for an episode that never played", saved)
		}
	})

	t.Run("a player that failed keeps the menu and saves nothing", func(t *testing.T) {
		saved := 0
		said := captureLog(t)
		action, err := playAndControl(ctx, dismissing, "E1", []string{"replay"}, true,
			func(context.Context) error { return exitErr }, func() error { saved++; return nil })
		if err != nil || action != "replay" {
			t.Fatalf("action = %q, %v, want the pick made after the failure", action, err)
		}
		if saved != 0 {
			t.Errorf("saved %d times for a failed playback", saved)
		}
		if !strings.Contains(said.String(), "player exited") {
			t.Errorf("the failure left no line in scrollback:\n%s", said)
		}
	})

	t.Run("a history that cannot be saved is said and does not end the run", func(t *testing.T) {
		said := captureLog(t)
		action, err := playAndControl(ctx, dismissing, "E1", nil, true,
			func(context.Context) error { return nil }, func() error { return errors.New("read-only file system") })
		if err != nil || action != "" {
			t.Fatalf("action = %q, %v, want a clean dismissal mid-batch", action, err)
		}
		if !strings.Contains(said.String(), "history not saved") {
			t.Errorf("the failed save was not said:\n%s", said)
		}
	})
}

// the cache directory names every part of the key that picks a rendition, and
// what a provider sends in those parts cannot leave the cache root
func TestCacheDir(t *testing.T) {
	root := stateRoot(t)
	got := cacheDir(154587, 12.5, upstream.Ssub, "hop", "")
	if want := filepath.Join(root, "miruro", "segments", "154587-e12.5-ssub-hop-best"); got != want {
		t.Errorf("cacheDir = %q, want %q", got, want)
	}
	if got := safeKey("1-e1-sub-../../x y:z-best"); got != "1-e1-sub-.._.._x_y_z-best" {
		t.Errorf("safeKey = %q, want one path component", got)
	}
	if dir := cacheDir(1, 1, upstream.Sub, "../../etc", "720p"); filepath.Dir(dir) != filepath.Join(root, "miruro", "segments") {
		t.Errorf("a provider code left the cache root: %q", dir)
	}
}

// an upstream that accepts and never answers counts neither a body nor a
// refusal, so only the grace stops the player, the case the grace exists for
func TestWatchdogGivesUpOnAStreamThatNeverAnswers(t *testing.T) {
	hang := make(chan struct{})
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer cdn.Close()
	defer close(hang)

	px, err := play.StartProxy(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	said := captureLog(t)
	wd := watchdog{grace: 200 * time.Millisecond, budget: 3, check: 10 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := wd.playStreams(context.Background(), px, []upstream.Stream{{URL: cdn.URL + "/v.mp4", Kind: upstream.MP4, Server: "HD-1"}},
			func(pctx context.Context, s upstream.Stream, tl *play.Tally) error {
				req, _ := http.NewRequestWithContext(pctx, http.MethodGet, tl.Stream(s).URL, nil)
				if resp, err := http.DefaultClient.Do(req); err == nil {
					resp.Body.Close()
				}
				<-pctx.Done()
				return errors.New("signal: killed")
			})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a stream that showed nothing reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the player was never stopped")
	}
	if !strings.Contains(said.String(), "showed nothing in time") {
		t.Errorf("the grace did not say why it stopped the player:\n%s", said)
	}
}

// a pinned provider whose backend hiccups is asked once more before the walk
// leaves it, and only that one and only once
func TestAutoResolveRetriesThePinOnce(t *testing.T) {
	ctx := context.Background()
	flaky := func(fails int) reply {
		calls := 0
		return func(upstream.Category) (*upstream.Result, error) {
			calls++
			if calls <= fails {
				return nil, fmt.Errorf("%w: miruro status 502", upstream.ErrUnreachable)
			}
			return &upstream.Result{Streams: []upstream.Stream{{URL: "http://cdn/master.m3u8", Kind: upstream.HLS}}}, nil
		}
	}

	t.Run("one hiccup keeps the pin", func(t *testing.T) {
		b := twoProviders(t, map[string]reply{"bonk": flaky(1), "ally": hls})
		_, src, err := resolver(upstream.Sub, b).autoResolve(ctx, 1, Pin{Code: "bonk"}, nil)
		if err != nil || src.Code != "bonk" {
			t.Fatalf("served = %q, %v, want bonk on its second try", src.Code, err)
		}
		if got := b.probed(); !slices.Equal(got, []string{"bonk:sub", "bonk:sub"}) {
			t.Errorf("asked %v, want bonk twice and nothing else", got)
		}
	})

	t.Run("a second failure moves on", func(t *testing.T) {
		b := twoProviders(t, map[string]reply{"bonk": flaky(2), "ally": hls})
		_, src, err := resolver(upstream.Sub, b).autoResolve(ctx, 1, Pin{Code: "bonk"}, nil)
		if err != nil || src.Code != "ally" {
			t.Fatalf("served = %q, %v, want ally", src.Code, err)
		}
		if got := b.probed(); !slices.Equal(got, []string{"bonk:sub", "bonk:sub", "ally:sub"}) {
			t.Errorf("asked %v, want bonk twice then ally", got)
		}
	})

	t.Run("an unpinned provider and another failure are not retried", func(t *testing.T) {
		b := twoProviders(t, map[string]reply{"bonk": flaky(1), "ally": fails(errors.New("no such episode"))})
		_, src, err := resolver(upstream.Sub, b).autoResolve(ctx, 1, Pin{Code: "ally"}, nil)
		if err == nil || src.Code != "" {
			t.Fatalf("served = %q, %v, want nothing, bonk failed once and ally refused", src.Code, err)
		}
		if got := b.probed(); !slices.Equal(got, []string{"ally:sub", "bonk:sub"}) {
			t.Errorf("asked %v, want each once", got)
		}
	})
}

// a provider named in the config or on the command line is a stated choice, so
// the walk stops at it and says how to widen it, rather than quietly serving
// the episode from somewhere the run was never told to use
func TestAutoResolveHoldsToAPinnedProvider(t *testing.T) {
	ctx := context.Background()

	t.Run("no fallback stops at the pin", func(t *testing.T) {
		b := twoProviders(t, map[string]reply{"bonk": down, "ally": hls})
		st := resolver(upstream.Sub, b)
		st.fallback = false
		_, _, err := st.autoResolve(ctx, 1, Pin{Code: "bonk"}, nil)
		if err == nil {
			t.Fatal("the run served an episode from a provider it was not told to use")
		}
		if !strings.Contains(err.Error(), "--fallback") {
			t.Errorf("err = %v, want it to name the flag that widens the walk", err)
		}
		// the pin is asked once more on a backend failure, and nothing else is
		if got := b.probed(); !slices.Equal(got, []string{"bonk:sub", "bonk:sub"}) {
			t.Errorf("asked %v, want only the pinned provider", got)
		}
	})

	t.Run("a pin nothing carries names itself", func(t *testing.T) {
		st := resolver(upstream.Sub, twoProviders(t, map[string]reply{"ally": hls}))
		st.fallback = false
		_, _, err := st.autoResolve(ctx, 1, Pin{Code: "gone"}, nil)
		if err == nil || !strings.Contains(err.Error(), "gone does not serve episode 1") {
			t.Fatalf("err = %v, want it to name the pinned provider", err)
		}
	})

	t.Run("fallback restores the walk and says why", func(t *testing.T) {
		b := twoProviders(t, map[string]reply{"bonk": down, "ally": hls})
		_, src, err := resolver(upstream.Sub, b).autoResolve(ctx, 1, Pin{Code: "bonk"}, nil)
		if err != nil || src.Code != "ally" {
			t.Fatalf("served = %q, %v, want ally", src.Code, err)
		}

		said := captureLog(t)
		_, src, err = resolver(upstream.Sub, b).autoResolve(ctx, 1, Pin{Code: "gone"}, nil)
		if err != nil || src.Code != "ally" {
			t.Fatalf("served = %q, %v, want ally", src.Code, err)
		}
		if !strings.Contains(said.String(), "pinned provider does not serve the episode") {
			t.Errorf("the walk passed over the pin without a word:\n%s", said)
		}
	})
}

// a provider that names no host still has to be told apart from its own other
// streams, and "stream" reads as the name of a thing rather than the absence
// of one
func TestServerNamesAnUnnamedStream(t *testing.T) {
	for _, tc := range []struct {
		stream upstream.Stream
		want   string
	}{
		{upstream.Stream{Server: "HD-1", URL: "https://cdn.example/a.m3u8"}, "HD-1"},
		{upstream.Stream{URL: "https://hls.krussdomi.com/manifest/x/master.m3u8"}, "hls.krussdomi.com"},
		{upstream.Stream{URL: "https://bl.krussdomi.com/manifest/x/master.m3u8"}, "bl.krussdomi.com"},
		{upstream.Stream{URL: "not a url"}, "unnamed"},
		{upstream.Stream{}, "unnamed"},
	} {
		if got := server(tc.stream); got != tc.want {
			t.Errorf("server(%+v) = %q, want %q", tc.stream, got, tc.want)
		}
	}
}
