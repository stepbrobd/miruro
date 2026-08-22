package miruro

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// searchPage is how many hits one search asks for
// the resource pages, so this is a picker-size choice rather than an api ceiling
const searchPage = 30

type Media struct {
	ID       int
	Romaji   string
	English  string
	Episodes int
	// Format is AniList's media format enum, e.g. TV, MOVIE, OVA
	Format string
}

func (m Media) Title() string {
	if m.English != "" {
		return m.English
	}
	return m.Romaji
}

// Search resolves a query to anime through the pipe's search resource
// this used to POST graphql.anilist.co directly, which broke the moment AniList
// disabled its public API, and the same metadata is behind the pipe anyway, so
// going through it keeps one transport, one header set, and one WAF path
func (c *Client) Search(ctx context.Context, query string) ([]Media, error) {
	body, err := c.pipe(ctx, "search", map[string]string{
		"q":       query,
		"type":    "ANIME",
		"perPage": strconv.Itoa(searchPage),
	})
	if err != nil {
		return nil, err
	}

	var raw []struct {
		ID    int    `json:"id"`
		Type  string `json:"type"`
		Title struct {
			Romaji  string `json:"romaji"`
			English string `json:"english"`
		} `json:"title"`
		Episodes int    `json:"episodes"`
		Format   string `json:"format"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, searchFailure(body, err)
	}

	media := make([]Media, 0, len(raw))
	for _, m := range raw {
		// the resource answers with manga and light novels when the type filter
		// is dropped or renamed upstream, and neither resolves to an episode
		if m.Type != "ANIME" {
			continue
		}
		media = append(media, Media{
			ID:       m.ID,
			Romaji:   m.Title.Romaji,
			English:  m.Title.English,
			Episodes: m.Episodes,
			Format:   m.Format,
		})
	}
	return media, nil
}

// searchFailure names what the pipe said when the body is not a result list
// a resource that fails answers 200 with an error object, so a bare json type
// error would otherwise hide the reason
func searchFailure(body []byte, err error) error {
	var fail struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &fail) == nil && fail.Error != "" {
		return fmt.Errorf("%w: search: %s", ErrUpstream, fail.Error)
	}
	return err
}
