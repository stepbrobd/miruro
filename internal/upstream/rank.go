package upstream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Rank orders the streams worth trying, best first, and skips embeds since
// nothing here can play one, along with any the provider reports as dead
// one provider often serves an episode from several hosts, and the one it flags
// as default can be the dead one, so a caller that can retry walks past the head
// rather than giving up on the provider
// hc fetches a master playlist when the quality asked for is not among the
// labels the provider gave, and must keep HTTP/2 for the CDNs that need it
func Rank(ctx context.Context, hc *http.Client, r *Result, quality string) []Stream {
	var hls, mp4 []Stream
	for _, s := range r.Streams {
		if !playable(s) {
			continue
		}
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

	// the quality pick can be the master restricted to one height, and the same
	// master unrestricted still belongs behind it as the fallback
	type key struct {
		url    string
		height int
	}
	out := make([]Stream, 0, 1+len(hls)+len(mp4))
	seen := map[key]bool{}
	add := func(s Stream) {
		k := key{s.URL, s.Height}
		if s.URL == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, s)
	}
	if s, ok := preferred(ctx, hc, hls, mp4, quality); ok {
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
// "worst" and an explicit height pick from the API quality labels, or from what
// an expanded master carries when the streams carry none
// a height found by expansion restricts the master rather than following the
// variant URL out of it, since a bare variant loses the audio renditions the
// master associates
// it prefers hls over a direct mp4, and reports false only when there is
// nothing playable at all
func preferred(ctx context.Context, hc *http.Client, hls, mp4 []Stream, quality string) (Stream, bool) {
	if len(hls) > 0 {
		if quality == "" || quality == "best" {
			return hls[0], true
		}
		if s, ok := pickQuality(hls, quality); ok {
			return s, true
		}
		if variants, err := expandMaster(ctx, hc, hls[0]); err == nil {
			if v, ok := pickQuality(variants, quality); ok {
				s := hls[0]
				s.Quality = v.Quality
				s.Height = parseHeight(v.Quality)
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
// "best" or "" takes the tallest labeled height, "worst" the shortest, and an
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

// ValidQuality reports whether a quality request is one Rank understands
// the rule lives here so a caller checking a config cannot drift from the
// heuristic that acts on it
func ValidQuality(q string) bool {
	return q == "" || q == "best" || q == "worst" || parseHeight(q) > 0
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

// maxMaster caps a master playlist read
// a real master is a few kilobytes, so only a hostile or broken host reaches
// this, and the read must end somewhere short of memory
const maxMaster = 16 << 20

// expandMaster fetches an hls master playlist and returns its variant streams
// labeled by height
// it errors on a non-200, on a non-master body, or on a master with no
// height-labeled variants, so a media playlist or an error page never becomes
// fabricated variants
func expandMaster(ctx context.Context, hc *http.Client, s Stream) ([]Stream, error) {
	req, err := newGet(ctx, s.URL, s.Referer)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
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
	lr := &io.LimitedReader{R: resp.Body, N: maxMaster + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
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
	if lr.N == 0 {
		return nil, fmt.Errorf("master playlist exceeds %d bytes", maxMaster)
	}
	if len(variants) == 0 {
		return nil, errors.New("not a master playlist")
	}
	return variants, nil
}
