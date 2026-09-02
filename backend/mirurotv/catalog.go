package mirurotv

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"ysun.co/miruro"
)

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
	if miruro.SkipKind(e.Type) == miruro.Outro {
		return e.Start >= mid
	}
	return e.Start < mid
}

// Episodes fetches the provider and episode map for a title
func (c *Client) Episodes(ctx context.Context, m miruro.Media) (*miruro.Catalog, error) {
	body, err := c.pipe(ctx, "episodes", map[string]string{"anilistId": strconv.Itoa(m.ID)})
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
				Sub []miruro.Episode `json:"sub"`
				Dub []miruro.Episode `json:"dub"`
			} `json:"episodes"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	cat := &miruro.Catalog{
		Title:     raw.Mappings.Title,
		Providers: make(map[string]miruro.Provider, len(raw.Providers)),
	}
	for code, p := range raw.Providers {
		cat.Providers[code] = miruro.Provider{Code: code, Backend: c, Sub: p.Episodes.Sub, Dub: p.Episodes.Dub}
	}
	cat.Aniskip = bestSkips(raw.Mappings.Aniskip)
	return cat, nil
}

// bestSkips reduces raw aniskip rows to at most one intro and one outro per
// episode
// off-enum types such as recap and mixed are dropped, rows whose position
// contradicts their kind are dropped, and among what remains for one episode and
// kind the highest-voted row wins
func bestSkips(rows []skipEntry) []miruro.SkipRange {
	type key struct {
		ep   float64
		kind miruro.SkipKind
	}
	best := map[key]skipEntry{}
	for _, r := range rows {
		kind := miruro.SkipKind(r.Type)
		if kind != miruro.Intro && kind != miruro.Outro || !r.plausible() {
			continue
		}
		k := key{r.Episode, kind}
		if cur, ok := best[k]; !ok || r.Votes > cur.Votes {
			best[k] = r
		}
	}

	out := make([]miruro.SkipRange, 0, len(best))
	for k, r := range best {
		out = append(out, miruro.SkipRange{Episode: k.ep, Kind: k.kind, Start: r.Start, End: r.End})
	}
	slices.SortFunc(out, func(a, b miruro.SkipRange) int {
		return cmp.Or(cmp.Compare(a.Episode, b.Episode), cmp.Compare(a.Start, b.Start))
	})
	return out
}
