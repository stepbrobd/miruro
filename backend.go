package miruro

import (
	"context"
	"fmt"
	"maps"
	"sync"
)

// Backend is one upstream serving the episodes and streams of a title
// every backend is keyed by AniList id, the one identity a title carries
// across upstreams, and the providers a backend lists point back at it so a
// resolution routes to the backend that listed the provider
type Backend interface {
	// Name identifies the backend in a log line and a config entry
	Name() string
	// Episodes lists what the backend carries for a title
	// a backend that carries nothing answers an empty catalog, and an error
	// means it could not answer at all
	Episodes(ctx context.Context, m Media) (*Catalog, error)
	// Sources resolves an episode on one of the backend's own providers
	Sources(ctx context.Context, episodeID, provider string, cat Category) (*Result, error)
	// Capabilities is the backend's provider capability table, empty when it
	// declares nothing
	Capabilities(ctx context.Context) (Capabilities, error)
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
// skip ranges come from the first that carries them and the provider set is
// the union
// a provider code two backends both list is refused from the second, since a
// resolution could otherwise route to the wrong upstream without a word
func (b Backends) Episodes(ctx context.Context, m Media) (*Catalog, []Failure) {
	cats := make([]*Catalog, len(b))
	errs := make([]error, len(b))
	var wg sync.WaitGroup
	for i, be := range b {
		wg.Go(func() { cats[i], errs[i] = be.Episodes(ctx, m) })
	}
	wg.Wait()

	out := &Catalog{Providers: map[string]Provider{}}
	owner := map[string]string{}
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
		for code, p := range cat.Providers {
			if first, dup := owner[code]; dup {
				failed = append(failed, Failure{be.Name(), fmt.Errorf("provider %s is already served by %s", code, first)})
				continue
			}
			owner[code] = be.Name()
			out.Providers[code] = p
		}
	}
	return out, failed
}

// Capabilities merges every backend's capability table
func (b Backends) Capabilities(ctx context.Context) (Capabilities, []Failure) {
	tables := make([]Capabilities, len(b))
	errs := make([]error, len(b))
	var wg sync.WaitGroup
	for i, be := range b {
		wg.Go(func() { tables[i], errs[i] = be.Capabilities(ctx) })
	}
	wg.Wait()

	out := Capabilities{}
	var failed []Failure
	for i, be := range b {
		if errs[i] != nil {
			failed = append(failed, Failure{be.Name(), errs[i]})
			continue
		}
		maps.Copy(out, tables[i])
	}
	return out, failed
}

// Sources resolves an episode through the backend that listed its provider
func (c *Catalog) Sources(ctx context.Context, episodeID, provider string, cat Category) (*Result, error) {
	p, ok := c.Providers[provider]
	if !ok || p.Backend == nil {
		return nil, fmt.Errorf("provider %s has no backend", provider)
	}
	return p.Backend.Sources(ctx, episodeID, provider, cat)
}
