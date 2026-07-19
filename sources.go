package miruro

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Kind is a closed set of stream container kinds
type Kind string

const (
	HLS   Kind = "hls"
	MP4   Kind = "mp4"
	Embed Kind = "embed"
)

// ErrNoStream means the resolved source held nothing playable
var ErrNoStream = errors.New("no playable stream")

type Stream struct {
	URL     string
	Kind    Kind
	Quality string
	Referer string
}

type Subtitle struct {
	File  string
	Label string
}

type Result struct {
	Streams   []Stream
	Subtitles []Subtitle
}

func (r *Result) Softsub() bool { return len(r.Subtitles) > 0 }

// Sources resolves an episode on a provider to playable streams and subtitles.
func (c *Client) Sources(ctx context.Context, episodeID, provider string, cat Category) (*Result, error) {
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
			URL     string `json:"url"`
			Type    string `json:"type"`
			Quality string `json:"quality"`
			Referer string `json:"referer"`
		} `json:"streams"`
		Subtitles []struct {
			File  string `json:"file"`
			Label string `json:"label"`
		} `json:"subtitles"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	res := &Result{}
	for _, s := range raw.Streams {
		res.Streams = append(res.Streams, Stream{
			URL:     s.URL,
			Kind:    Kind(s.Type),
			Quality: s.Quality,
			Referer: s.Referer,
		})
	}
	for _, s := range raw.Subtitles {
		res.Subtitles = append(res.Subtitles, Subtitle{File: s.File, Label: s.Label})
	}
	return res, nil
}

// Select applies the default quality heuristic, an author-owned decision
// "best" hands mpv the hls master to negotiate
// "worst" and an explicit height pick from the API quality labels, or from an
// expanded master when the streams carry none
// it prefers hls, then a direct mp4, and skips embeds
func (c *Client) Select(ctx context.Context, r *Result, quality string) (Stream, error) {
	var hls, mp4 []Stream
	for _, s := range r.Streams {
		switch s.Kind {
		case HLS:
			hls = append(hls, s)
		case MP4:
			mp4 = append(mp4, s)
		}
	}

	if len(hls) > 0 {
		if quality == "" || quality == "best" {
			return hls[0], nil
		}
		if s, ok := pickQuality(hls, quality); ok {
			return s, nil
		}
		if variants, err := c.expandMaster(ctx, hls[0]); err == nil {
			if s, ok := pickQuality(variants, quality); ok {
				return s, nil
			}
		}
		return hls[0], nil
	}
	if len(mp4) > 0 {
		if s, ok := pickQuality(mp4, quality); ok {
			return s, nil
		}
		return mp4[0], nil
	}
	return Stream{}, ErrNoStream
}

// pickQuality selects a stream by request
// "best" or "" takes the tallest labelled height, "worst" the shortest, and an
// explicit "NNNp" an exact match
// it reports false when no stream carries a usable height, so the caller can
// expand a master or fall back to best
func pickQuality(streams []Stream, quality string) (Stream, bool) {
	best, worst := -1, -1
	var bs, ws Stream
	want := parseHeight(quality)
	for _, s := range streams {
		h := parseHeight(s.Quality)
		if h == 0 {
			continue
		}
		if quality != "" && quality != "best" && quality != "worst" {
			if h == want {
				return s, true
			}
			continue
		}
		if h > best {
			best, bs = h, s
		}
		if worst < 0 || h < worst {
			worst, ws = h, s
		}
	}
	switch {
	case quality == "worst" && worst >= 0:
		return ws, true
	case (quality == "" || quality == "best") && best >= 0:
		return bs, true
	}
	return Stream{}, false
}

func parseHeight(q string) int {
	q = strings.TrimSuffix(strings.TrimSpace(q), "p")
	n, err := strconv.Atoi(q)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

var resolution = regexp.MustCompile(`RESOLUTION=\d+x(\d+)`)

// expandMaster fetches an hls master playlist and returns its variant streams
// labelled by height
// it errors on a non-200, on a non-master body, or on a master with no
// height-labelled variants, so a media playlist or an error page never becomes
// fabricated variants
func (c *Client) expandMaster(ctx context.Context, s Stream) ([]Stream, error) {
	req, err := newGet(ctx, s.URL, s.Referer)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("master playlist: status %d", resp.StatusCode)
	}

	base, err := url.Parse(s.URL)
	if err != nil {
		return nil, err
	}
	var variants []Stream
	var height string
	master := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			master = true
			if m := resolution.FindStringSubmatch(line); m != nil {
				height = m[1] + "p"
			}
		case line != "" && !strings.HasPrefix(line, "#"):
			if height == "" {
				continue
			}
			ref, err := url.Parse(line)
			if err != nil {
				height = ""
				continue
			}
			v := s
			v.URL = base.ResolveReference(ref).String()
			v.Quality = height
			variants = append(variants, v)
			height = ""
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !master || len(variants) == 0 {
		return nil, fmt.Errorf("not a master playlist: %s", s.URL)
	}
	return variants, nil
}
