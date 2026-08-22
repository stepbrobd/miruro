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
	"slices"
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
	// Server is the provider's own name for the host behind this stream,
	// "HD-1" or "VidPlay-1", empty when it names none
	Server string
	// Default marks the stream the provider itself picks
	Default bool
}

type Subtitle struct {
	File  string `json:"file"`
	Label string `json:"label"`
	// Lang is the api's language tag, "en" or "pt-BR", empty when it names none
	Lang string `json:"lang,omitempty"`
	// Default marks the track the provider itself flags as the one to show
	Default bool `json:"default,omitempty"`
}

type Result struct {
	Streams   []Stream
	Subtitles []Subtitle
}

func (r *Result) Softsub() bool { return len(r.Subtitles) > 0 }

// Playable reports whether Rank can return a stream
// an embed-only result carries no hls or mp4, so a caller must skip it rather
// than accept it and fail later outside the fallback loop
func (r *Result) Playable() bool {
	for _, s := range r.Streams {
		if s.Kind == HLS || s.Kind == MP4 {
			return true
		}
	}
	return false
}

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
			Server  string `json:"server"`
			Default bool   `json:"default"`
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

	res := &Result{}
	for _, s := range raw.Streams {
		res.Streams = append(res.Streams, Stream{
			URL:     s.URL,
			Kind:    Kind(s.Type),
			Quality: s.Quality,
			Referer: s.Referer,
			Server:  s.Server,
			Default: s.Default,
		})
	}
	for _, s := range raw.Subtitles {
		if !attachable(s.Kind) {
			continue
		}
		res.Subtitles = append(res.Subtitles, Subtitle{
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

// Order returns subs with the track a player should show first at the front
// mpv selects the first external subtitle file it is handed, so this is what
// decides the default track
// the requested language wins, then the provider's own default flag, then the
// order the api returned, and the sort is stable so equal ranks keep that order
func Order(subs []Subtitle, lang string) []Subtitle {
	out := slices.Clone(subs)
	slices.SortStableFunc(out, func(a, b Subtitle) int { return rank(a, lang) - rank(b, lang) })
	return out
}

func rank(s Subtitle, lang string) int {
	switch {
	case s.speaks(lang):
		return 0
	case s.Default:
		return 1
	default:
		return 2
	}
}

// speaks reports whether s is the language that was asked for
// a provider names a track by tag, by label, or by both, and the user may have
// typed either, so "en" and "English" both select an English track
// a tag matches on its primary subtag, so "pt" selects "pt-BR"
func (s Subtitle) speaks(lang string) bool {
	if lang == "" {
		return false
	}
	if strings.EqualFold(s.Label, lang) {
		return true
	}
	return s.Lang != "" && strings.EqualFold(primary(s.Lang), primary(lang))
}

// primary is the language subtag before any region or script
func primary(tag string) string {
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		return tag[:i]
	}
	return tag
}

// Rank orders the streams worth trying, best first, and skips embeds since
// nothing here can play one
// one provider often serves an episode from several hosts, and the one it flags
// as default can be the dead one, so a caller that can retry walks past the head
// rather than giving up on the provider
func (c *Client) Rank(ctx context.Context, r *Result, quality string) []Stream {
	var hls, mp4 []Stream
	for _, s := range r.Streams {
		switch s.Kind {
		case HLS:
			hls = append(hls, s)
		case MP4:
			mp4 = append(mp4, s)
		}
	}
	// the provider's own default leads its kind, since the order the api happens
	// to list streams in is not a promise
	lead(hls)
	lead(mp4)

	out := make([]Stream, 0, 1+len(hls)+len(mp4))
	seen := map[string]bool{}
	add := func(s Stream) {
		if s.URL == "" || seen[s.URL] {
			return
		}
		seen[s.URL] = true
		out = append(out, s)
	}
	if s, ok := c.preferred(ctx, hls, mp4, quality); ok {
		add(s)
	}
	for _, s := range hls {
		add(s)
	}
	for _, s := range mp4 {
		add(s)
	}
	return out
}

// lead moves the streams the provider flags as default to the front, keeping
// the api order among equals
func lead(streams []Stream) {
	slices.SortStableFunc(streams, func(a, b Stream) int {
		switch {
		case a.Default == b.Default:
			return 0
		case a.Default:
			return -1
		default:
			return 1
		}
	})
}

// preferred applies the quality heuristic, an author-owned decision
// "best" hands mpv the hls master to negotiate
// "worst" and an explicit height pick from the API quality labels, or from an
// expanded master when the streams carry none
// it prefers hls over a direct mp4, and reports false only when there is
// nothing playable at all
func (c *Client) preferred(ctx context.Context, hls, mp4 []Stream, quality string) (Stream, bool) {
	if len(hls) > 0 {
		if quality == "" || quality == "best" {
			return hls[0], true
		}
		if s, ok := pickQuality(hls, quality); ok {
			return s, true
		}
		if variants, err := c.expandMaster(ctx, hls[0]); err == nil {
			if s, ok := pickQuality(variants, quality); ok {
				return s, true
			}
		}
		return hls[0], true
	}
	if len(mp4) > 0 {
		if s, ok := pickQuality(mp4, quality); ok {
			return s, true
		}
		return mp4[0], true
	}
	return Stream{}, false
}

// pickQuality selects a stream by request
// "best" or "" takes the tallest labelled height, "worst" the shortest, and an
// explicit "NNNp" an exact match
// it reports false when no stream carries a usable height, so the caller can
// expand a master or fall back to best
func pickQuality(streams []Stream, quality string) (Stream, bool) {
	if quality != "" && quality != "best" && quality != "worst" {
		want := parseHeight(quality)
		for _, s := range streams {
			if h := parseHeight(s.Quality); h != 0 && h == want {
				return s, true
			}
		}
		return Stream{}, false
	}

	tallest := quality != "worst"
	var pick Stream
	height := 0
	for _, s := range streams {
		h := parseHeight(s.Quality)
		if h == 0 {
			continue
		}
		// the comparison is strict, so equal heights keep the first
		if height == 0 || (tallest && h > height) || (!tallest && h < height) {
			pick, height = s, h
		}
	}
	return pick, height > 0
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

	// base must be the URL the master was ultimately served from after
	// redirects, or relative variants resolve against the wrong host
	base := resp.Request.URL
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
