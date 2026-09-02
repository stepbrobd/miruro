package miruro

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
	name  string
	cat   *Catalog
	caps  Capabilities
	err   error
	asked []string
}

func (f *fake) Name() string { return f.name }

func (f *fake) Episodes(context.Context, Media) (*Catalog, error) {
	if f.err != nil {
		return nil, f.err
	}
	for code, p := range f.cat.Providers {
		p.Backend = f
		f.cat.Providers[code] = p
	}
	return f.cat, nil
}

func (f *fake) Sources(_ context.Context, episodeID, provider string, cat Category) (*Result, error) {
	f.asked = append(f.asked, provider+"/"+episodeID+"/"+string(cat))
	return &Result{Streams: []Stream{{URL: f.name, Kind: HLS}}}, nil
}

func (f *fake) Capabilities(context.Context) (Capabilities, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.caps, nil
}

func catalog(title string, codes ...string) *Catalog {
	c := &Catalog{Title: title, Providers: map[string]Provider{}}
	for _, code := range codes {
		c.Providers[code] = Provider{Code: code, Sub: []Episode{{ID: code + "-1", Number: 1}}}
	}
	return c
}

func TestBackendsMergeCatalogs(t *testing.T) {
	ctx := context.Background()
	first := &fake{name: "first", cat: catalog("Frieren", "ally")}
	first.cat.Aniskip = []SkipRange{{Episode: 1, Kind: Intro, Start: 0, End: 90}}
	second := &fake{name: "second", cat: catalog("Sousou no Frieren", "allanime")}

	cat, failed := Backends{first, second}.Episodes(ctx, Media{ID: 154587})
	if len(failed) != 0 {
		t.Fatalf("failures = %v", failed)
	}
	if cat.Title != "Frieren" {
		t.Errorf("title = %q, want the first backend's", cat.Title)
	}
	if len(cat.Aniskip) != 1 {
		t.Errorf("aniskip = %v, want the first backend's", cat.Aniskip)
	}
	codes := slices.Sorted(maps.Keys(cat.Providers))
	if strings.Join(codes, ",") != "allanime,ally" {
		t.Errorf("providers = %v, want the union", codes)
	}

	// a resolution routes to the backend that listed the provider
	if _, err := cat.Sources(ctx, "allanime-1", "allanime", Sub); err != nil {
		t.Fatal(err)
	}
	if len(first.asked) != 0 {
		t.Errorf("first backend asked %v, want nothing", first.asked)
	}
	if strings.Join(second.asked, ",") != "allanime/allanime-1/sub" {
		t.Errorf("second backend asked %v", second.asked)
	}
}

// a provider code both backends claim would route a resolution to whichever
// one merged last, so the second is refused and named
func TestBackendsRefuseADuplicateProvider(t *testing.T) {
	first := &fake{name: "first", cat: catalog("Frieren", "ally")}
	second := &fake{name: "second", cat: catalog("Frieren", "ally", "allanime")}

	cat, failed := Backends{first, second}.Episodes(context.Background(), Media{})
	if len(failed) != 1 || failed[0].Backend != "second" || !strings.Contains(failed[0].Error(), "already served by first") {
		t.Fatalf("failures = %v", failed)
	}
	if cat.Providers["ally"].Backend != first {
		t.Error("the duplicate provider was overwritten")
	}
	if _, ok := cat.Providers["allanime"]; !ok {
		t.Error("the rest of the second backend was dropped with the duplicate")
	}
}

// one backend failing costs the run its providers and nothing else
func TestBackendsReportOneFailure(t *testing.T) {
	dead := errors.New("upstream down")
	first := &fake{name: "first", err: dead}
	second := &fake{name: "second", cat: catalog("Frieren", "allanime"), caps: Capabilities{"allanime": {Hard: true}}}

	cat, failed := Backends{first, second}.Episodes(context.Background(), Media{})
	if len(failed) != 1 || !errors.Is(failed[0], dead) {
		t.Fatalf("failures = %v", failed)
	}
	if _, ok := cat.Providers["allanime"]; !ok {
		t.Error("the healthy backend's providers were lost")
	}

	caps, failed := Backends{first, second}.Capabilities(context.Background())
	if len(failed) != 1 || !errors.Is(failed[0], dead) {
		t.Fatalf("capability failures = %v", failed)
	}
	if !caps["allanime"].Hard {
		t.Errorf("capabilities = %v, want the healthy backend's", caps)
	}
}

func TestCatalogSourcesRefusesAnUnlistedProvider(t *testing.T) {
	cat := catalog("Frieren", "ally")
	if _, err := cat.Sources(context.Background(), "ally-1", "ally", Sub); err == nil {
		t.Error("a provider with no backend resolved")
	}
	if _, err := cat.Sources(context.Background(), "x-1", "nobody", Sub); err == nil {
		t.Error("an unlisted provider resolved")
	}
}
