package upstream

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
)

// Backend is one upstream serving the episodes and streams of a title
// every backend is keyed by AniList id, the one identity a title carries
// across upstreams, and the providers a backend lists point back at it so a
// resolution routes to the backend that listed the provider
type Backend interface {
	// Name identifies the backend in a log line and a config entry
	Name() string
	// Episodes lists what the backend carries of a title
	// a backend that carries nothing answers an empty catalog, and an error
	// means it could not answer at all
	Episodes(ctx context.Context, m Media) (*Catalog, error)
	// Listing names the providers serving one episode of a title, ref being the
	// key the backend's own catalog gave the title
	Listing(ctx context.Context, ref string, number float64) (*Listing, error)
	// Sources resolves an episode on one of the backend's own providers
	Sources(ctx context.Context, episodeID, provider string, cat Category) (*Result, error)
}

// Backends is the ordered set of upstreams one run resolves against
type Backends []Backend

// Failure is one backend that could not answer
// it is kept apart from the merged answer so the backends that did answer are
// not lost over the one that did not
type Failure struct {
	Backend string
	Err     error
}

func (f Failure) Error() string { return f.Backend + ": " + f.Err.Error() }

func (f Failure) Unwrap() error { return f.Err }

// Episodes merges every backend's catalog for a title
// the backends are asked at once and merged in order, so the title and the
// skip ranges come from the first that carries them, the episodes are the
// union, and each backend keeps its own key for the title
func (b Backends) Episodes(ctx context.Context, m Media) (*Catalog, []Failure) {
	cats := make([]*Catalog, len(b))
	errs := make([]error, len(b))
	var wg sync.WaitGroup
	for i, be := range b {
		wg.Go(func() { cats[i], errs[i] = be.Episodes(ctx, m) })
	}
	wg.Wait()

	out := &Catalog{Refs: map[string]string{}}
	var failed []Failure
	for i, be := range b {
		if errs[i] != nil {
			failed = append(failed, Failure{be.Name(), errs[i]})
			continue
		}
		cat := cats[i]
		if out.Title == "" {
			out.Title = cat.Title
		}
		if len(out.Aniskip) == 0 {
			out.Aniskip = cat.Aniskip
		}
		out.Sub = union(out.Sub, cat.Sub)
		out.Dub = union(out.Dub, cat.Dub)
		// a backend speaks for its own key only, so one cannot point a listing
		// at another's
		if ref, ok := cat.Refs[be.Name()]; ok {
			out.Refs[be.Name()] = ref
		}
	}
	return out, failed
}

// union adds the episodes of later to those of first, one record per number
// the first record of a number wins and a later one only fills in a title it
// left empty, so the earlier backend keeps its filler mark
func union(first, later []Episode) []Episode {
	out := slices.Clone(first)
	at := make(map[float64]int, len(out))
	for i, e := range out {
		at[e.Number] = i
	}
	for _, e := range later {
		i, seen := at[e.Number]
		switch {
		case !seen:
			at[e.Number] = len(out)
			out = append(out, e)
		case out[i].Title == "" && e.Title != "":
			out[i].Title = e.Title
		}
	}
	slices.SortStableFunc(out, func(a, b Episode) int { return cmp.Compare(a.Number, b.Number) })
	return out
}

// Listing merges what every backend that listed the title serves one episode as
// the backends are asked at once, and a provider code two backends both list is
// refused from the second, since a resolution could otherwise route to
// whichever merged last without a word
func (b Backends) Listing(ctx context.Context, cat *Catalog, number float64) (*Listing, []Failure) {
	var asked Backends
	for _, be := range b {
		if _, ok := cat.Refs[be.Name()]; ok {
			asked = append(asked, be)
		}
	}
	lists := make([]*Listing, len(asked))
	errs := make([]error, len(asked))
	var wg sync.WaitGroup
	for i, be := range asked {
		wg.Go(func() { lists[i], errs[i] = be.Listing(ctx, cat.Refs[be.Name()], number) })
	}
	wg.Wait()

	out := &Listing{Providers: map[string]Provider{}, Caps: Capabilities{}}
	owner := map[string]string{}
	var failed []Failure
	for i, be := range asked {
		if errs[i] != nil {
			failed = append(failed, Failure{be.Name(), errs[i]})
			continue
		}
		for code, p := range lists[i].Providers {
			if first, dup := owner[code]; dup {
				failed = append(failed, Failure{be.Name(), fmt.Errorf("provider %s is already served by %s", code, first)})
				continue
			}
			owner[code] = be.Name()
			out.Providers[code] = p
			if c, ok := lists[i].Caps[code]; ok {
				out.Caps[code] = c
			}
		}
	}
	return out, failed
}

// Sources resolves an episode through the backend that listed its provider
func (l *Listing) Sources(ctx context.Context, episodeID, provider string, cat Category) (*Result, error) {
	p, ok := l.Providers[provider]
	if !ok || p.Backend == nil {
		return nil, fmt.Errorf("provider %s has no backend", provider)
	}
	return p.Backend.Sources(ctx, episodeID, provider, cat)
}
