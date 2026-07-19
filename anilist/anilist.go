// Package anilist resolves a search query to AniList media, the id miruro keys on.
package anilist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const endpoint = "https://graphql.anilist.co"

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
	Format   string
}

func (m Media) Title() string {
	if m.English != "" {
		return m.English
	}
	return m.Romaji
}

type request struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type response struct {
	Data struct {
		Page struct {
			Media []struct {
				ID    int `json:"id"`
				Title struct {
					Romaji  string `json:"romaji"`
					English string `json:"english"`
				} `json:"title"`
				Episodes int    `json:"episodes"`
				Format   string `json:"format"`
			} `json:"media"`
		} `json:"Page"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func Search(ctx context.Context, hc *http.Client, query string) ([]Media, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	payload, err := json.Marshal(request{
		Query:     searchQuery,
		Variables: map[string]any{"search": query, "perPage": 30},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out response
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
			Format:   m.Format,
		})
	}
	return media, nil
}
