package miruro

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
)

// Category is a closed set
// an illegal value cannot flow downstream
type Category string

const (
	Sub Category = "sub"
	Dub Category = "dub"
)

// SkipKind marks an aniskip interval
// op is the intro and ed is the outro
type SkipKind string

const (
	Intro SkipKind = "op"
	Outro SkipKind = "ed"
)

type Episode struct {
	ID     string  `json:"id"`
	Number float64 `json:"number"`
}

type SkipRange struct {
	Episode float64
	Kind    SkipKind
	Start   float64
	End     float64
}

// skipEntry is one raw aniskip row
// the api returns one per upstream per interval, disambiguated by votes
type skipEntry struct {
	Episode float64 `json:"episode"`
	Type    string  `json:"type"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Votes   int     `json:"votes"`
}

type Provider struct {
	Code string
	Sub  []Episode
	Dub  []Episode
}

func (p Provider) Episodes(cat Category) []Episode {
	if cat == Dub {
		return p.Dub
	}
	return p.Sub
}

type Catalog struct {
	Title     string
	Providers map[string]Provider
	Aniskip   []SkipRange
}

// Episodes fetches the provider and episode map for an AniList id
func (c *Client) Episodes(ctx context.Context, anilistID int) (*Catalog, error) {
	body, err := c.pipe(ctx, "episodes", map[string]string{"anilistId": strconv.Itoa(anilistID)})
	if err != nil {
		return nil, err
	}

	var raw struct {
		Mappings struct {
			Title   string      `json:"title"`
			Aniskip []skipEntry `json:"aniskip"`
		} `json:"mappings"`
		Providers map[string]struct {
			Episodes struct {
				Sub []Episode `json:"sub"`
				Dub []Episode `json:"dub"`
			} `json:"episodes"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	cat := &Catalog{
		Title:     raw.Mappings.Title,
		Providers: make(map[string]Provider, len(raw.Providers)),
	}
	for code, p := range raw.Providers {
		cat.Providers[code] = Provider{Code: code, Sub: p.Episodes.Sub, Dub: p.Episodes.Dub}
	}
	cat.Aniskip = bestSkips(raw.Mappings.Aniskip)
	return cat, nil
}

// bestSkips reduces raw aniskip rows to at most one intro and one outro per
// episode
// off-enum types such as recap and mixed are dropped, and among the rows for one
// episode and kind the highest-voted one wins
func bestSkips(rows []skipEntry) []SkipRange {
	type key struct {
		ep   float64
		kind SkipKind
	}
	best := map[key]skipEntry{}
	for _, r := range rows {
		kind := SkipKind(r.Type)
		if kind != Intro && kind != Outro {
			continue
		}
		k := key{r.Episode, kind}
		if cur, ok := best[k]; !ok || r.Votes > cur.Votes {
			best[k] = r
		}
	}

	out := make([]SkipRange, 0, len(best))
	for k, r := range best {
		out = append(out, SkipRange{Episode: k.ep, Kind: k.kind, Start: r.Start, End: r.End})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Episode != out[j].Episode {
			return out[i].Episode < out[j].Episode
		}
		return out[i].Start < out[j].Start
	})
	return out
}

// Numbers is the sorted union of episode numbers across providers for a category
func (c *Catalog) Numbers(cat Category) []float64 {
	seen := map[float64]struct{}{}
	for _, p := range c.Providers {
		for _, e := range p.Episodes(cat) {
			seen[e.Number] = struct{}{}
		}
	}
	out := make([]float64, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Float64s(out)
	return out
}

// Available lists providers carrying the episode in the category, ordered by code
// the default order is a placeholder
// provider ranking is an author-owned decision
func (c *Catalog) Available(number float64, cat Category) []Provider {
	var out []Provider
	for _, p := range c.Providers {
		for _, e := range p.Episodes(cat) {
			if e.Number == number {
				out = append(out, p)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
