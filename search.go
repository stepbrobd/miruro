package miruro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const anilistEndpoint = "https://graphql.anilist.co"

const searchQuery = `query ($search: String, $perPage: Int) {
  Page(perPage: $perPage) {
    media(search: $search, type: ANIME, sort: SEARCH_MATCH) {
      id
      title { romaji english }
      episodes
      format
    }
  }
}`

type Media struct {
	ID       int
	Romaji   string
	English  string
	Episodes int
}

func (m Media) Title() string {
	if m.English != "" {
		return m.English
	}
	return m.Romaji
}

// Search resolves a query to AniList media through the public GraphQL API
func (c *Client) Search(ctx context.Context, query string) ([]Media, error) {
	payload, err := json.Marshal(map[string]any{
		"query":     searchQuery,
		"variables": map[string]any{"search": query, "perPage": 30},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anilistEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// a gateway html page or a 429 would otherwise fail as a json parse error
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anilist: status %d", resp.StatusCode)
	}

	var out struct {
		Data struct {
			Page struct {
				Media []struct {
					ID    int `json:"id"`
					Title struct {
						Romaji  string `json:"romaji"`
						English string `json:"english"`
					} `json:"title"`
					Episodes int `json:"episodes"`
				} `json:"media"`
			} `json:"Page"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("anilist: %s", out.Errors[0].Message)
	}

	media := make([]Media, 0, len(out.Data.Page.Media))
	for _, m := range out.Data.Page.Media {
		media = append(media, Media{
			ID:       m.ID,
			Romaji:   m.Title.Romaji,
			English:  m.Title.English,
			Episodes: m.Episodes,
		})
	}
	return media, nil
}
