package miruro

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"ysun.co/miruro/internal/upstream"
)

// searchPage is how many hits one search asks for
// the catalog answers only the page sizes the site's own views ask for, and
// refuses any other limit, so this is the size of the site's search page
const searchPage = 15

// title is one record of the catalog's anime resource, the fields read here
type title struct {
	ExternalIDs map[string][]string `json:"external_ids"`
	Title       struct {
		Romaji  string `json:"romaji"`
		English string `json:"english"`
	} `json:"title"`
	Format        string `json:"format"`
	EpisodeCount  *int   `json:"episode_count"`
	EpisodeCounts struct {
		Raw *int `json:"raw"`
	} `json:"episode_counts"`
}

// anilist is the one AniList id the record names, which is what every backend,
// the history, and the segment cache key a title by
// a record naming none or several cannot be keyed, so it reports false rather
// than picking one
func (t title) anilist() (int, bool) {
	ids := t.ExternalIDs["anilist"]
	if len(ids) != 1 {
		return 0, false
	}
	n, err := strconv.Atoi(ids[0])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// media is the record as the rest of the program sees it
// the episode count falls back to the raw count the way the site's own info
// page does, since a title still airing names no total
func (t title) media(id int) upstream.Media {
	m := upstream.Media{ID: id, Romaji: t.Title.Romaji, English: t.Title.English, Format: t.Format}
	switch {
	case t.EpisodeCount != nil:
		m.Episodes = *t.EpisodeCount
	case t.EpisodeCounts.Raw != nil:
		m.Episodes = *t.EpisodeCounts.Raw
	}
	return m
}

// Search resolves a query to anime through the catalog's anime resource
// the resource is anime only, where the pipe's search mixed in manga
func (c *Client) Search(ctx context.Context, query string) ([]upstream.Media, error) {
	q := url.Values{"q": {query}, "limit": {strconv.Itoa(searchPage)}}
	body, err := c.get(ctx, "/api/v1/anime?"+q.Encode())
	if err != nil {
		return nil, err
	}

	var hits struct {
		Data []title `json:"data"`
	}
	if err := json.Unmarshal(body, &hits); err != nil {
		return nil, err
	}
	media := make([]upstream.Media, 0, len(hits.Data))
	for _, t := range hits.Data {
		id, ok := t.anilist()
		if !ok {
			continue
		}
		media = append(media, t.media(id))
	}
	return media, nil
}
