package miruro

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"ysun.co/miruro/internal/upstream"
)

const (
	// lookupPage is the page size of an AniList id lookup, the one the site asks
	// for when it matches a list against the catalog
	lookupPage = 100
	// episodePage is the page size of an episode list, the one the site asks
	// for, which holds every episode of the longest series in one page
	episodePage = 10000
)

// episode is one row of the catalog's episode list
type episode struct {
	// Number is a float so a fractional row parses, even though the play
	// resource addresses whole episodes only
	Number    float64    `json:"episode_number"`
	Title     string     `json:"title"`
	CanonType string     `json:"canon_type"`
	AiredOn   string     `json:"aired_on"`
	Duration  float64    `json:"duration_seconds"`
	SkipTimes []skipTime `json:"skip_times"`
}

type skipTime struct {
	Kind  string  `json:"kind"`
	Start float64 `json:"start_seconds"`
	End   float64 `json:"end_seconds"`
}

// plausible rejects a range whose position contradicts its kind
// the ranges come from aniskip, where upstreams mislabel rows, and an "ed"
// starting in the opening seconds would otherwise mark the wrong span
func (s skipTime) plausible(length float64) bool {
	if s.End <= s.Start {
		return false
	}
	if length <= 0 {
		return true
	}
	if upstream.SkipKind(s.Kind) == upstream.Outro {
		return s.Start >= length/2
	}
	return s.Start < length/2
}

// Episodes lists the episodes of a title
// the catalog keys a title by its own id, so the title is looked up by its
// AniList id first, and one the catalog does not carry is an empty catalog
func (c *Client) Episodes(ctx context.Context, m upstream.Media) (*upstream.Catalog, error) {
	t, ok, err := c.lookup(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &upstream.Catalog{}, nil
	}

	// the site lists a film's one episode under its own kind and everything
	// else as regular
	kind := "regular"
	if t.Format == "MOVIE" {
		kind = "film"
	}
	q := url.Values{"kind": {kind}, "limit": {strconv.Itoa(episodePage)}}
	body, err := c.get(ctx, "/api/v1/anime/"+url.PathEscape(t.ID)+"/episodes?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var eps struct {
		Data    []episode `json:"data"`
		HasMore bool      `json:"has_more"`
	}
	if err := json.Unmarshal(body, &eps); err != nil {
		return nil, err
	}
	// the site never pages this list, so a request shape for the next page is
	// not known to be one the catalog accepts
	if eps.HasMore {
		return nil, fmt.Errorf("miruro lists more than %d episodes of %s", episodePage, t.ID)
	}
	return t.catalog(c.Name(), eps.Data, time.Now()), nil
}

// catalog is the title's episodes as the site's own episode list shows them
// a count is what the catalog knows a track carries, and one it leaves empty is
// unknown: the site plays sub whether or not it counts one, and offers a dub
// only once it counts one, so an uncounted sub is every listed episode and an
// uncounted dub is none
func (t title) catalog(backend string, eps []episode, now time.Time) *upstream.Catalog {
	cat := &upstream.Catalog{Title: t.media(0).Title(), Refs: map[string]string{backend: t.ID}}
	for _, e := range t.listed(eps, now) {
		n := e.Number
		// the play resource addresses whole episodes, so a fractional one lists
		// something no request could reach
		if n != float64(int(n)) || n < 1 {
			continue
		}
		rec := upstream.Episode{Number: n, Title: e.Title, Filler: e.CanonType == "filler"}
		if sub := t.EpisodeCounts.Sub; sub == nil || n <= float64(*sub) {
			cat.Sub = append(cat.Sub, rec)
		}
		if dub := t.EpisodeCounts.Dub; dub != nil && n <= float64(*dub) {
			cat.Dub = append(cat.Dub, rec)
		}
		cat.Aniskip = append(cat.Aniskip, e.skips()...)
	}
	return cat
}

// listed keeps the episodes the site's episode list shows
// the list runs ahead of what has aired, so it is cut at the raw count or the
// title's own total, and a title that counts no raw episodes keeps what the
// sub and dub counts cover and what has aired
func (t title) listed(eps []episode, now time.Time) []episode {
	counts := t.EpisodeCounts
	var limit *int
	for _, n := range []*int{t.EpisodeCount, counts.Raw} {
		if n != nil && (limit == nil || *n < *limit) {
			limit = n
		}
	}
	covered := max(deref(counts.Sub), deref(counts.Dub))

	var out []episode
	for _, e := range eps {
		switch {
		case limit != nil && e.Number > float64(*limit):
		case counts.Raw != nil, e.Number <= float64(covered):
			out = append(out, e)
		default:
			if aired, err := time.Parse(time.DateOnly, e.AiredOn); err == nil {
				if !aired.After(now) {
					out = append(out, e)
				}
			} else if t.Status != "NOT_YET_RELEASED" {
				out = append(out, e)
			}
		}
	}
	return out
}

func deref(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// skips reduces an episode's skip times to at most one intro and one outro
// off-enum kinds such as recap and mixed_op are dropped, and so is a range
// whose position contradicts its kind
func (e episode) skips() []upstream.SkipRange {
	var out []upstream.SkipRange
	seen := map[upstream.SkipKind]bool{}
	for _, s := range e.SkipTimes {
		kind := upstream.SkipKind(s.Kind)
		if kind != upstream.Intro && kind != upstream.Outro || seen[kind] || !s.plausible(e.Duration) {
			continue
		}
		seen[kind] = true
		out = append(out, upstream.SkipRange{Episode: e.Number, Kind: kind, Start: s.Start, End: s.End})
	}
	slices.SortFunc(out, func(a, b upstream.SkipRange) int { return cmp.Compare(a.Start, b.Start) })
	return out
}

// lookup finds the catalog record carrying an AniList id
// two records carrying one id cannot be told apart, so that is an error rather
// than a guess
func (c *Client) lookup(ctx context.Context, anilist int) (title, bool, error) {
	id := strconv.Itoa(anilist)
	q := url.Values{"anilist_id_in": {id}, "limit": {strconv.Itoa(lookupPage)}}
	body, err := c.get(ctx, "/api/v1/anime?"+q.Encode())
	if err != nil {
		return title{}, false, err
	}
	var hits struct {
		Data []title `json:"data"`
	}
	if err := json.Unmarshal(body, &hits); err != nil {
		return title{}, false, err
	}
	var found []title
	for _, t := range hits.Data {
		if slices.Contains(t.ExternalIDs["anilist"], id) {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 0:
		return title{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return title{}, false, fmt.Errorf("miruro carries AniList %d as %d titles", anilist, len(found))
	}
}
