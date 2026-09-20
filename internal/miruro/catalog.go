package miruro

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"ysun.co/miruro/internal/upstream"
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
	if upstream.SkipKind(e.Type) == upstream.Outro {
		return e.Start >= mid
	}
	return e.Start < mid
}

// Episodes fetches the provider and episode map for a title
func (c *Client) Episodes(ctx context.Context, m upstream.Media) (*upstream.Catalog, error) {
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
				Sub []upstream.Episode `json:"sub"`
				Dub []upstream.Episode `json:"dub"`
			} `json:"episodes"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	cat := &upstream.Catalog{
		Title:     raw.Mappings.Title,
		Providers: make(map[string]upstream.Provider, len(raw.Providers)),
	}
	for code, p := range raw.Providers {
		cat.Providers[code] = upstream.Provider{Code: code, Backend: c, Sub: p.Episodes.Sub, Dub: p.Episodes.Dub}
	}
	cat.Aniskip = bestSkips(raw.Mappings.Aniskip)
	return cat, nil
}

// bestSkips reduces raw aniskip rows to at most one intro and one outro per
// episode
// off-enum types such as recap and mixed are dropped, rows whose position
// contradicts their kind are dropped, and among what remains for one episode and
// kind the highest-voted row wins
func bestSkips(rows []skipEntry) []upstream.SkipRange {
	type key struct {
		ep   float64
		kind upstream.SkipKind
	}
	best := map[key]skipEntry{}
	for _, r := range rows {
		kind := upstream.SkipKind(r.Type)
		if kind != upstream.Intro && kind != upstream.Outro || !r.plausible() {
			continue
		}
		k := key{r.Episode, kind}
		if cur, ok := best[k]; !ok || r.Votes > cur.Votes {
			best[k] = r
		}
	}

	out := make([]upstream.SkipRange, 0, len(best))
	for k, r := range best {
		out = append(out, upstream.SkipRange{Episode: k.ep, Kind: k.kind, Start: r.Start, End: r.End})
	}
	slices.SortFunc(out, func(a, b upstream.SkipRange) int {
		return cmp.Or(cmp.Compare(a.Episode, b.Episode), cmp.Compare(a.Start, b.Start))
	})
	return out
}
