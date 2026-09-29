package upstream

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// fake is a backend that answers from memory
type fake struct {
	name    string
	cat     *Catalog
	list    *Listing
	err     error
	listErr error
	// listed records the refs the backend was asked to list
	listed []string
	asked  []string
}

func (f *fake) Name() string { return f.name }

func (f *fake) Episodes(context.Context, Media) (*Catalog, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.cat, nil
}

func (f *fake) Listing(_ context.Context, ref string, _ float64) (*Listing, error) {
	f.listed = append(f.listed, ref)
	if f.listErr != nil {
		return nil, f.listErr
	}
	for code, p := range f.list.Providers {
		p.Backend = f
		f.list.Providers[code] = p
	}
	return f.list, nil
}

func (f *fake) Sources(_ context.Context, episodeID, provider string, cat Category) (*Result, error) {
	f.asked = append(f.asked, provider+"/"+episodeID+"/"+string(cat))
	return &Result{Streams: []Stream{{URL: f.name, Kind: HLS}}}, nil
}

// titled is a catalog of one backend carrying the given sub episodes
func titled(backend, title string, eps ...Episode) *Catalog {
	return &Catalog{Title: title, Sub: eps, Refs: map[string]string{backend: backend + "-ref"}}
}

// listing is a listing of the given providers, each serving episode 1 hardsub
func listing(codes ...string) *Listing {
	l := &Listing{Providers: map[string]Provider{}, Caps: Capabilities{}}
	for _, code := range codes {
		l.Providers[code] = Provider{Code: code, Sub: []Episode{{ID: code + "-1", Number: 1}}}
		l.Caps[code] = Caps{Hard: true}
	}
	return l
}

func TestBackendsMergeCatalogs(t *testing.T) {
	first := &fake{name: "first", cat: titled("first", "Frieren",
		Episode{Number: 1, Title: "The Journey's End"},
		Episode{Number: 2, Filler: true})}
	first.cat.Aniskip = []SkipRange{{Episode: 1, Kind: Intro, Start: 0, End: 90}}
	first.cat.Dub = []Episode{{Number: 1}}
	second := &fake{name: "second", cat: titled("second", "Sousou no Frieren",
		Episode{Number: 3},
		Episode{Number: 2, Title: "It Didn't Have to Be Magic"},
		Episode{Number: 1, Title: "Another Title"})}
	// a backend naming another's key must not be able to point its listings
	second.cat.Refs["first"] = "hijacked"

	cat, failed := Backends{first, second}.Episodes(context.Background(), Media{ID: 154587})
	if len(failed) != 0 {
		t.Fatalf("failures = %v", failed)
	}
	if cat.Title != "Frieren" {
		t.Errorf("title = %q, want the first backend's", cat.Title)
	}
	if len(cat.Aniskip) != 1 {
		t.Errorf("aniskip = %v, want the first backend's", cat.Aniskip)
	}
	want := []Episode{
		{Number: 1, Title: "The Journey's End"},
		// the first record keeps its filler mark and takes only the missing title
		{Number: 2, Title: "It Didn't Have to Be Magic", Filler: true},
		{Number: 3},
	}
	if !slices.Equal(cat.Sub, want) {
		t.Errorf("sub = %+v, want the union in order with the first record winning", cat.Sub)
	}
	if len(cat.Dub) != 1 {
		t.Errorf("dub = %+v, want the first backend's", cat.Dub)
	}
	if want := map[string]string{"first": "first-ref", "second": "second-ref"}; !maps.Equal(cat.Refs, want) {
		t.Errorf("refs = %v, want each backend's own", cat.Refs)
	}
}

func TestBackendsMergeListings(t *testing.T) {
	ctx := context.Background()
	first := &fake{name: "first", cat: titled("first", "Frieren"), list: listing("ally")}
	second := &fake{name: "second", cat: titled("second", "Frieren"), list: listing("other")}
	second.list.Caps["other"] = Caps{Soft: true}
	// listed nothing for the title, so it must not be asked about an episode
	absent := &fake{name: "absent", cat: &Catalog{}, list: listing("never")}

	all := Backends{first, second, absent}
	cat, _ := all.Episodes(ctx, Media{})
	l, failed := all.Listing(ctx, cat, 1)
	if len(failed) != 0 {
		t.Fatalf("failures = %v", failed)
	}
	if codes := slices.Sorted(maps.Keys(l.Providers)); strings.Join(codes, ",") != "ally,other" {
		t.Errorf("providers = %v, want the union", codes)
	}
	if l.Caps["other"] != (Caps{Soft: true}) || l.Caps["ally"] != (Caps{Hard: true}) {
		t.Errorf("caps = %v, want each backend's own", l.Caps)
	}
	if !slices.Equal(first.listed, []string{"first-ref"}) || !slices.Equal(second.listed, []string{"second-ref"}) {
		t.Errorf("listed with %v and %v, want each backend's own key", first.listed, second.listed)
	}
	if len(absent.listed) != 0 {
		t.Errorf("a backend that listed nothing was asked with %v", absent.listed)
	}

	// a resolution routes to the backend that listed the provider
	if _, err := l.Sources(ctx, "other-1", "other", Ssub); err != nil {
		t.Fatal(err)
	}
	if len(first.asked) != 0 {
		t.Errorf("first backend asked %v, want nothing", first.asked)
	}
	if strings.Join(second.asked, ",") != "other/other-1/ssub" {
		t.Errorf("second backend asked %v", second.asked)
	}
}

// a provider code both backends claim would route a resolution to whichever
// one merged last, so the second is refused and named
func TestBackendsRefuseADuplicateProvider(t *testing.T) {
	ctx := context.Background()
	first := &fake{name: "first", cat: titled("first", "Frieren"), list: listing("ally")}
	second := &fake{name: "second", cat: titled("second", "Frieren"), list: listing("ally", "other")}

	all := Backends{first, second}
	cat, _ := all.Episodes(ctx, Media{})
	l, failed := all.Listing(ctx, cat, 1)
	if len(failed) != 1 || failed[0].Backend != "second" || !strings.Contains(failed[0].Error(), "already served by first") {
		t.Fatalf("failures = %v", failed)
	}
	if l.Providers["ally"].Backend != first {
		t.Error("the duplicate provider was overwritten")
	}
	if _, ok := l.Providers["other"]; !ok {
		t.Error("the rest of the second backend was dropped with the duplicate")
	}
}

// one backend failing costs the run its providers and nothing else
func TestBackendsReportOneFailure(t *testing.T) {
	ctx := context.Background()
	dead := errors.New("upstream down")
	first := &fake{name: "first", err: dead}
	second := &fake{name: "second", cat: titled("second", "Frieren", Episode{Number: 1}), list: listing("other")}

	cat, failed := Backends{first, second}.Episodes(ctx, Media{})
	if len(failed) != 1 || !errors.Is(failed[0], dead) {
		t.Fatalf("failures = %v", failed)
	}
	if len(cat.Sub) != 1 {
		t.Error("the healthy backend's episodes were lost")
	}

	first = &fake{name: "first", cat: titled("first", "Frieren"), listErr: dead}
	all := Backends{first, second}
	cat, _ = all.Episodes(ctx, Media{})
	l, failed := all.Listing(ctx, cat, 1)
	if len(failed) != 1 || !errors.Is(failed[0], dead) {
		t.Fatalf("listing failures = %v", failed)
	}
	if _, ok := l.Providers["other"]; !ok {
		t.Error("the healthy backend's providers were lost")
	}
}

func TestListingSourcesRefusesAnUnlistedProvider(t *testing.T) {
	l := listing("ally")
	if _, err := l.Sources(context.Background(), "ally-1", "ally", Sub); err == nil {
		t.Error("a provider with no backend resolved")
	}
	if _, err := l.Sources(context.Background(), "x-1", "nobody", Sub); err == nil {
		t.Error("an unlisted provider resolved")
	}
}
