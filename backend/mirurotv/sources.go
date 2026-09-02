package mirurotv

import (
	"context"
	"encoding/json"
	"strings"

	"ysun.co/miruro"
)

// Sources resolves an episode on a provider to playable streams and subtitles.
func (c *Client) Sources(ctx context.Context, episodeID, provider string, cat miruro.Category) (*miruro.Result, error) {
	body, err := c.pipe(ctx, "sources", map[string]string{
		"episodeId": episodeID,
		"provider":  provider,
		"category":  string(cat),
	})
	if err != nil {
		return nil, err
	}

	var raw struct {
		Streams []struct {
			URL      string `json:"url"`
			Type     string `json:"type"`
			Quality  string `json:"quality"`
			Referer  string `json:"referer"`
			Server   string `json:"server"`
			Default  bool   `json:"default"`
			IsActive *bool  `json:"isActive"`
		} `json:"streams"`
		Subtitles []struct {
			File     string `json:"file"`
			Label    string `json:"label"`
			Kind     string `json:"kind"`
			Language string `json:"language"`
			Default  bool   `json:"default"`
		} `json:"subtitles"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	res := &miruro.Result{}
	for _, s := range raw.Streams {
		res.Streams = append(res.Streams, miruro.Stream{
			URL:     s.URL,
			Kind:    miruro.Kind(s.Type),
			Quality: s.Quality,
			Referer: s.Referer,
			Server:  s.Server,
			Default: s.Default,
			Dead:    s.IsActive != nil && !*s.IsActive,
		})
	}
	for _, s := range raw.Subtitles {
		if !attachable(s.Kind) {
			continue
		}
		res.Subtitles = append(res.Subtitles, miruro.Subtitle{
			File:    s.File,
			Label:   s.Label,
			Lang:    s.Language,
			Default: s.Default,
		})
	}
	return res, nil
}

// attachable reports whether a subtitle entry carries dialogue
// the api mirrors the html5 track kinds, where "thumbnails" is a sprite index a
// player must never load as subtitles, so an unrecognised kind is refused rather
// than attached
func attachable(kind string) bool {
	switch strings.ToLower(kind) {
	case "", "captions", "subtitles":
		return true
	}
	return false
}
