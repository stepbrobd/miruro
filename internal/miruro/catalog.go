package miruro

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
	// specialRange is where the catalog numbers a title's specials, after any
	// regular run, and the site keeps a special in that range off its list
	specialRange = 20000
)

// episode is one row of the catalog's episode list
type episode struct {
	// Number is a float so a fractional row parses, even though the play
	// resource addresses whole episodes only
	Number    float64    `json:"episode_number"`
	Kind      string     `json:"kind"`
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

	// the catalog refuses a kind filter, so every kind arrives and listed drops
	// what the site does not show
	q := url.Values{"limit": {strconv.Itoa(episodePage)}}
	body, err := c.get(ctx, "/api/v1/anime/"+url.PathEscape(t.ID)+"/episodes?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var eps struct {
		Data    []episode `json:"data"`
		HasMore bool      `json:"has_more"`
	}
	if err := decodePage(body, &eps, episodePage); err != nil {
		return nil, err
	}
	// the site never pages this list, so a request shape for the next page is
	// not known to be one the catalog accepts
	if eps.HasMore {
		return nil, fmt.Errorf("miruro lists more than %d episodes of %s", episodePage, plain(t.ID))
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
		rec := upstream.Episode{Number: n, Title: plain(e.Title), Filler: e.CanonType == "filler"}
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

// listed keeps the episodes the site's episode list shows, the way at() in its
// Seasons chunk filters them
// a special numbered in the special range is never listed
// the list runs ahead of what has aired, so it is cut at the title's own total,
// or at the raw count where the total is unknown
// past what the counts cover, an episode is listed once its air date has
// passed, and one with no air date only while the raw track is uncounted and
// the title has started
func (t title) listed(eps []episode, now time.Time) []episode {
	counts := t.EpisodeCounts
	limit := cmp.Or(t.EpisodeCount, counts.Raw)
	covered := max(deref(counts.Raw), deref(counts.Sub), deref(counts.Dub))

	var out []episode
	for _, e := range eps {
		switch {
		case e.Kind == "special" && e.Number >= specialRange:
		case limit != nil && e.Number > float64(*limit):
		case e.Number <= float64(covered):
			out = append(out, e)
		default:
			if aired, err := time.Parse(time.DateOnly, e.AiredOn); err == nil {
				if !aired.After(now) {
					out = append(out, e)
				}
			} else if counts.Raw == nil && t.Status != "NOT_YET_RELEASED" {
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

// lookup finds the catalog record carrying an AniList id, from what Preload
// kept when it covered the id
// two records carrying one id cannot be told apart, so that is an error rather
// than a guess
func (c *Client) lookup(ctx context.Context, anilist int) (title, bool, error) {
	c.preloadMu.Lock()
	found, ok := c.preloaded[anilist]
	c.preloadMu.Unlock()
	if !ok {
		carried, err := c.titles(ctx, []int{anilist})
		if err != nil {
			return title{}, false, err
		}
		found = carried[anilist]
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

// Preload looks many AniList ids up at once and keeps what the catalog
// answered for the life of the client, so Episodes on any of them sends no
// lookup of its own
// a failure still keeps what the requests before it answered
// nothing it keeps is refreshed, which suits a command listing many titles once
func (c *Client) Preload(ctx context.Context, ids []int) error {
	carried, err := c.titles(ctx, ids)
	c.preloadMu.Lock()
	defer c.preloadMu.Unlock()
	if c.preloaded == nil {
		c.preloaded = map[int][]title{}
	}
	maps.Copy(c.preloaded, carried)
	return err
}

// titles asks the catalog for the records carrying each AniList id, at most
// lookupPage ids to a request in ascending order, the way the site matches a
// list against the catalog
// every id of a request that succeeded is in the answer, with no records where
// the catalog carries none, and a failure returns what the requests before it
// answered
func (c *Client) titles(ctx context.Context, ids []int) (map[int][]title, error) {
	carried := map[int][]title{}
	for chunk := range slices.Chunk(slices.Compact(slices.Sorted(slices.Values(ids))), lookupPage) {
		found, err := c.carrying(ctx, chunk)
		if err != nil {
			return carried, err
		}
		maps.Copy(carried, found)
	}
	return carried, nil
}

// carrying asks one lookup request for the records carrying each of ids
func (c *Client) carrying(ctx context.Context, ids []int) (map[int][]title, error) {
	asked := make(map[string]int, len(ids))
	in := make([]string, len(ids))
	found := make(map[int][]title, len(ids))
	for i, id := range ids {
		in[i] = strconv.Itoa(id)
		asked[in[i]] = id
		found[id] = nil
	}
	q := url.Values{"anilist_id_in": {strings.Join(in, ",")}, "limit": {strconv.Itoa(lookupPage)}}
	body, err := c.get(ctx, "/api/v1/anime?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var hits struct {
		Data    []title `json:"data"`
		HasMore bool    `json:"has_more"`
	}
	if err := decodePage(body, &hits, lookupPage); err != nil {
		return nil, err
	}
	// a record on the next page would leave the id carrying it looking
	// uncarried
	if hits.HasMore {
		return nil, fmt.Errorf("miruro carries more than %d records for %d AniList ids", lookupPage, len(ids))
	}
	for _, t := range hits.Data {
		// a record naming one id twice is still one record
		for _, a := range slices.Compact(slices.Sorted(slices.Values(t.ExternalIDs["anilist"]))) {
			if id, ok := asked[a]; ok {
				found[id] = append(found[id], t)
			}
		}
	}
	return found, nil
}
