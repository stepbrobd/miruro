package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/adrg/xdg"

	"ysun.co/miruro/internal/upstream"
)

// shelf is a backend listing a catalog per AniList id, or failing for one,
// named miruro unless it names itself
type shelf struct {
	name string
	cats map[int]*upstream.Catalog
	errs map[int]error
}

func (s shelf) Name() string {
	if s.name != "" {
		return s.name
	}
	return "miruro"
}

func (s shelf) Episodes(_ context.Context, m upstream.Media) (*upstream.Catalog, error) {
	if err := s.errs[m.ID]; err != nil {
		return nil, err
	}
	if c, ok := s.cats[m.ID]; ok {
		return c, nil
	}
	return &upstream.Catalog{}, nil
}

func (shelf) Listing(context.Context, string, float64) (*upstream.Listing, error) {
	return nil, errors.New("new never lists an episode")
}

func (shelf) Sources(context.Context, string, string, upstream.Category) (*upstream.Result, error) {
	return nil, errors.New("new never resolves a stream")
}

func episodes(from, to int) []upstream.Episode {
	var out []upstream.Episode
	for n := from; n <= to; n++ {
		out = append(out, upstream.Episode{Number: float64(n)})
	}
	return out
}

// a title counts what its catalog lists past the episode watched, in the
// category it was watched in, and one no backend answered for says so rather
// than reading as nothing new
func TestNewer(t *testing.T) {
	down := errors.New("upstream unreachable")
	b := upstream.Backends{shelf{
		cats: map[int]*upstream.Catalog{
			154587: {Sub: episodes(1, 28), Dub: episodes(1, 20)},
			21:     {Sub: episodes(1, 1180), Dub: episodes(1, 1155)},
			16498:  {Sub: episodes(1, 25)},
		},
		errs: map[int]error{5114: down},
	}}
	entries := []entry{
		{AnilistID: 154587, Title: "Frieren", Episode: 12, Category: upstream.Sub},
		{AnilistID: 21, Title: "ONE PIECE", Episode: 1150, Category: upstream.Dub},
		{AnilistID: 16498, Title: "Shingeki no Kyojin", Episode: 25, Category: upstream.Sub},
		{AnilistID: 5114, Title: "Fullmetal Alchemist", Episode: 3, Category: upstream.Sub},
	}

	got := newer(context.Background(), b, entries)
	if len(got) != len(entries) {
		t.Fatalf("got %d titles, want every one of %d", len(got), len(entries))
	}
	for i, e := range entries {
		if got[i].entry.AnilistID != e.AnilistID {
			t.Errorf("title %d is %s, want history's order", i, got[i].entry.Title)
		}
	}
	if n := got[0].newer; len(n) != 16 || n[0] != 13 || n[len(n)-1] != 28 {
		t.Errorf("Frieren newer = %v, want 13 through 28", n)
	}
	// watched in dub, so the dub list is the one that counts
	if n := got[1].newer; len(n) != 5 || n[len(n)-1] != 1155 {
		t.Errorf("ONE PIECE newer = %v, want 1151 through 1155", n)
	}
	if len(got[2].newer) != 0 || got[2].err != nil {
		t.Errorf("a title watched to its end = %+v, want nothing new", got[2])
	}
	if !errors.Is(got[3].err, down) || len(got[3].newer) != 0 {
		t.Errorf("a title nobody answered for = %+v, want its failure", got[3])
	}
	if !slices.IsSorted(got[0].newer) {
		t.Errorf("newer = %v, want ascending", got[0].newer)
	}
}

// a title another backend lists is checked whatever one backend failed on it,
// since the failure only matters when nothing answered
func TestNewerKeepsWhatAnotherBackendListed(t *testing.T) {
	b := upstream.Backends{
		shelf{name: "down", errs: map[int]error{154587: errors.New("upstream unreachable")}},
		shelf{name: "up", cats: map[int]*upstream.Catalog{154587: {Sub: episodes(1, 28)}}},
	}
	got := newer(context.Background(), b, []entry{{AnilistID: 154587, Title: "Frieren", Episode: 27, Category: upstream.Sub}})
	if len(got) != 1 || got[0].err != nil || !slices.Equal(got[0].newer, []float64{28}) {
		t.Errorf("newer = %+v, want 28 from the backend that answered", got)
	}
}

// a title with nothing past the episode watched is left out, one nobody answered
// for is warned about rather than dropped in silence, and the rest are listed
// with how many and up to which
func TestShowNew(t *testing.T) {
	said := captureLog(t)
	found := []fresh{
		{entry: entry{Title: "Frieren", Episode: 12, Category: upstream.Sub}, newer: []float64{13, 14, 28}},
		{entry: entry{Title: "Shingeki no Kyojin", Episode: 25, Category: upstream.Sub}},
		{entry: entry{Title: "Fullmetal Alchemist", Episode: 3, Category: upstream.Sub}, err: errors.New("upstream unreachable")},
	}
	var out bytes.Buffer
	if err := showNew(&out, found); err != nil {
		t.Fatal(err)
	}
	if want := "Frieren  ep 12 watched  3 newer, up to 28  sub\n"; out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
	if !strings.Contains(said.String(), `title not checked title="Fullmetal Alchemist"`) {
		t.Errorf("the title nobody answered for was not warned about:\n%s", said)
	}
	out.Reset()
	if err := showNew(&out, found[1:2]); err != nil || out.String() != "nothing new past what history has watched\n" {
		t.Errorf("printed %q, %v, want nothing new", out.String(), err)
	}
}

// an empty history is said to be empty before anything is asked of a backend,
// and a run interrupted while asking stops rather than reporting nothing new
func TestRunNew(t *testing.T) {
	stateRoot(t)
	t.Cleanup(xdg.Reload)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	xdg.Reload()
	if out := printed(t, func() error { return runNew(newCmd, nil) }); out != "history is empty\n" {
		t.Errorf("printed %q, want an empty history said to be", out)
	}

	if err := openStore().save(entry{AnilistID: 154587, Title: "Frieren", Episode: 1, Category: upstream.Sub}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prev := newCmd.Context()
	newCmd.SetContext(ctx)
	t.Cleanup(func() { newCmd.SetContext(prev) })
	if err := runNew(newCmd, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the interruption", err)
	}
}
