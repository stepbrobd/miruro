package miruro

import (
	"context"
	"encoding/json"
	"slices"
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
	// Title is the episode name, empty when the provider carries none
	Title string `json:"title"`
	// Filler marks an episode outside the source material
	Filler bool `json:"filler"`
}

type SkipRange struct {
	Episode float64
	Kind    SkipKind
	Start   float64
	End     float64
}

// skipEntry is one raw aniskip row
// the api returns one per upstream per interval, disambiguated by votes
// Length is that upstream's own episode duration, which the range is relative to
type skipEntry struct {
	Episode float64 `json:"episode"`
	Type    string  `json:"type"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Votes   int     `json:"votes"`
	Length  float64 `json:"episode_length"`
}

// plausible rejects a range whose position contradicts its kind
// upstreams mislabel rows, and a highly voted "ed" starting in the opening
// seconds would otherwise win and mark the wrong span
func (e skipEntry) plausible() bool {
	if e.End <= e.Start {
		return false
	}
	if e.Length <= 0 {
		return true
	}
	mid := e.Length / 2
	if SkipKind(e.Type) == Outro {
		return e.Start >= mid
	}
	return e.Start < mid
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
// off-enum types such as recap and mixed are dropped, rows whose position
// contradicts their kind are dropped, and among what remains for one episode and
// kind the highest-voted row wins
func bestSkips(rows []skipEntry) []SkipRange {
	type key struct {
		ep   float64
		kind SkipKind
	}
	best := map[key]skipEntry{}
	for _, r := range rows {
		kind := SkipKind(r.Type)
		if kind != Intro && kind != Outro || !r.plausible() {
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

// order is the provider preference, an author-owned default.
// It leads with the providers this project has watched carry a whole episode,
// then the ones whose failure it has recorded: bonk stops serving segments
// partway through a run, bee and hop depend on a subtitle CDN that has answered
// with something other than HTTP, and moo is the lowest quality.
// miruro publishes its own order in the config resource and rewrites it between
// deploys. Following that would move the default provider under a resumed
// history entry and a filled segment cache, so this list stays here instead.
var order = []string{"kiwi", "pewe", "ally", "bonk", "hop", "bee", "moo"}

// preference places a provider in order, with an unnamed code after every named
// one rather than interleaved, since nothing is known about it
func preference(code string) int {
	if i := slices.Index(order, code); i >= 0 {
		return i
	}
	return len(order)
}

// Details maps every episode number in a category to the record worth showing
// the first provider in preference order that carries an episode supplies its
// record, and a later one only fills in a title the first left empty
func (c *Catalog) Details(cat Category) map[float64]Episode {
	codes := make([]string, 0, len(c.Providers))
	for code := range c.Providers {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool {
		if a, b := preference(codes[i]), preference(codes[j]); a != b {
			return a < b
		}
		return codes[i] < codes[j]
	})

	out := make(map[float64]Episode)
	for _, code := range codes {
		for _, e := range c.Providers[code].Episodes(cat) {
			cur, seen := out[e.Number]
			if !seen || (cur.Title == "" && e.Title != "") {
				out[e.Number] = e
			}
		}
	}
	return out
}

// Available lists providers carrying the episode in the category, in preference
// order, with equal ranks by code so runs are reproducible
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
	sort.Slice(out, func(i, j int) bool {
		if a, b := preference(out[i].Code), preference(out[j].Code); a != b {
			return a < b
		}
		return out[i].Code < out[j].Code
	})
	return out
}
