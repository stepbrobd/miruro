package miruro

import (
	"context"
	"encoding/json"
	"strings"

	"ysun.co/miruro/internal/upstream"
)

// Sources resolves an episode on a provider to playable streams and subtitles
func (c *Client) Sources(ctx context.Context, episodeID, provider string, cat upstream.Category) (*upstream.Result, error) {
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

	res := &upstream.Result{}
	for _, s := range raw.Streams {
		// the kind is a closed set, so a container nothing here plays is dropped
		// where it arrives rather than carried as a free string
		kind, ok := kinds[s.Type]
		if !ok {
			continue
		}
		res.Streams = append(res.Streams, upstream.Stream{
			URL:     browserURL(s.URL),
			Kind:    kind,
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
		res.Subtitles = append(res.Subtitles, upstream.Subtitle{
			File:    browserURL(s.File),
			Label:   s.Label,
			Lang:    s.Language,
			Default: s.Default,
		})
	}
	return res, nil
}

// kinds maps the api's stream type to the closed set
var kinds = map[string]upstream.Kind{
	"hls":   upstream.HLS,
	"mp4":   upstream.MP4,
	"embed": upstream.Embed,
}

// browserURL reads a url from the api the way the site's own player reads it
// a browser takes https:///host as https://host however many slashes follow the
// first two, where net/url reads the third as an empty host
// hop lists the subtitle files of its older encodes in that shape
func browserURL(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	switch strings.ToLower(scheme) {
	case "http", "https":
		return scheme + "://" + strings.TrimLeft(rest, "/")
	}
	return raw
}

// attachable reports whether a subtitle entry carries dialogue
// the api mirrors the html5 track kinds, where "thumbnails" is a sprite index a
// player must never load as subtitles, so an unrecognized kind is refused rather
// than attached
func attachable(kind string) bool {
	switch strings.ToLower(kind) {
	case "", "captions", "subtitles":
		return true
	}
	return false
}
