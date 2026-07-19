// Package sources resolves an episode on a provider to playable streams and
// subtitles through the miruro secure pipe.
package sources

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"ysun.co/miruro/catalog"
	"ysun.co/miruro/pipe"
)

// ErrNoStream means the resolved source held nothing playable
var ErrNoStream = errors.New("no playable stream")

// Kind is a closed set of stream container kinds
type Kind string

const (
	HLS   Kind = "hls"
	MP4   Kind = "mp4"
	Embed Kind = "embed"
)

type Stream struct {
	URL     string
	Kind    Kind
	Quality string
	Server  string
	Referer string
}

type Subtitle struct {
	File    string
	Label   string
	Kind    string
	Default bool
}

type Result struct {
	Streams   []Stream
	Subtitles []Subtitle
	Download  string
	Thumbnail string
}

// Softsub reports whether the source carries external subtitle tracks.
func (r *Result) Softsub() bool { return len(r.Subtitles) > 0 }

func Resolve(ctx context.Context, c *pipe.Client, episodeID, provider string, cat catalog.Category) (*Result, error) {
	body, err := c.Get(ctx, "sources", map[string]string{
		"episodeId": episodeID,
		"provider":  provider,
		"category":  string(cat),
	})
	if err != nil {
		return nil, err
	}

	var raw struct {
		Streams []struct {
			URL     string `json:"url"`
			Type    string `json:"type"`
			Quality string `json:"quality"`
			Server  string `json:"server"`
			Referer string `json:"referer"`
		} `json:"streams"`
		Subtitles []struct {
			File    string `json:"file"`
			Label   string `json:"label"`
			Kind    string `json:"kind"`
			Default bool   `json:"default"`
		} `json:"subtitles"`
		Download  string `json:"download"`
		Thumbnail string `json:"thumbnail"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	res := &Result{Download: raw.Download, Thumbnail: raw.Thumbnail}
	for _, s := range raw.Streams {
		res.Streams = append(res.Streams, Stream{
			URL:     s.URL,
			Kind:    Kind(s.Type),
			Quality: s.Quality,
			Server:  s.Server,
			Referer: s.Referer,
		})
	}
	for _, s := range raw.Subtitles {
		res.Subtitles = append(res.Subtitles, Subtitle{
			File:    s.File,
			Label:   s.Label,
			Kind:    s.Kind,
			Default: s.Default,
		})
	}
	return res, nil
}

// Select applies the default quality heuristic, an author-owned decision.
// it honours an explicit height when hls variants expose one, then prefers the
// hls master for mpv to negotiate, then a direct mp4, and skips embeds
func Select(ctx context.Context, hc *http.Client, r *Result, quality string) (Stream, error) {
	var hls, mp4 []Stream
	for _, s := range r.Streams {
		switch s.Kind {
		case HLS:
			hls = append(hls, s)
		case MP4:
			mp4 = append(mp4, s)
		}
	}

	if len(hls) > 0 && quality != "" && quality != "best" {
		if variants, err := expandMaster(ctx, hc, hls[0]); err == nil {
			for _, v := range variants {
				if strings.HasPrefix(v.Quality, strings.TrimSuffix(quality, "p")) {
					return v, nil
				}
			}
		}
	}
	if len(hls) > 0 {
		return hls[0], nil
	}
	if len(mp4) > 0 {
		return mp4[0], nil
	}
	return Stream{}, ErrNoStream
}

var resolution = regexp.MustCompile(`RESOLUTION=\d+x(\d+)`)

// expandMaster fetches an hls master playlist and returns its variant streams
// labelled by height, falling back to the master itself when parsing yields none.
func expandMaster(ctx context.Context, hc *http.Client, s Stream) ([]Stream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return nil, err
	}
	if s.Referer != "" {
		req.Header.Set("Referer", s.Referer)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	base := s.URL[:strings.LastIndex(s.URL, "/")+1]
	var variants []Stream
	var height string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			if m := resolution.FindStringSubmatch(line); m != nil {
				height = m[1] + "p"
			}
		case line != "" && !strings.HasPrefix(line, "#"):
			v := s
			v.Quality = height
			if strings.HasPrefix(line, "http") {
				v.URL = line
			} else {
				v.URL = base + line
			}
			variants = append(variants, v)
			height = ""
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(variants) == 0 {
		return []Stream{s}, nil
	}
	return variants, nil
}
