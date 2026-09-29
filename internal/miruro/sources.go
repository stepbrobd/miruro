package miruro

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"ysun.co/miruro/internal/upstream"
)

// playback is the play resource, one episode as every provider serves it
type playback struct {
	Tracks []struct {
		// Track is sub, ssub or dub, the renditions Category names
		Track     string   `json:"track"`
		Providers []source `json:"providers"`
	} `json:"tracks"`
}

// source is one provider serving one track of an episode
type source struct {
	// Provider is the api's id, anikoto rather than the bee the site shows
	Provider  string `json:"provider"`
	Subtitles []struct {
		Language string `json:"language"`
		Label    string `json:"label"`
		File     string `json:"file"`
		Format   string `json:"format"`
		Default  bool   `json:"default"`
	} `json:"subtitles"`
	Servers []struct {
		Server  string            `json:"server"`
		Headers map[string]string `json:"headers"`
		Streams []struct {
			URL     string `json:"url"`
			Format  string `json:"format"`
			Quality string `json:"quality"`
		} `json:"streams"`
	} `json:"servers"`
}

// kinds maps the api's stream format to the closed set
var kinds = map[string]upstream.Kind{
	"hls": upstream.HLS,
	"mp4": upstream.MP4,
}

// tracks maps the api's track to the category it is asked for by
var tracks = map[string]upstream.Category{
	"sub":  upstream.Sub,
	"ssub": upstream.Ssub,
	"dub":  upstream.Dub,
}

// result is what the provider serves the track as
// a server's embed is the site's iframe player, which nothing here plays, so
// only its streams are read, and only those of a server whose headers the
// proxy can send
func (s source) result() *upstream.Result {
	res := &upstream.Result{}
	for _, sv := range s.Servers {
		ref, ok := referer(sv.Headers)
		if !ok {
			continue
		}
		for _, st := range sv.Streams {
			// the kind is a closed set, so a container nothing here plays is
			// dropped where it arrives rather than carried as a free string
			kind, ok := kinds[st.Format]
			if !ok || st.URL == "" {
				continue
			}
			res.Streams = append(res.Streams, upstream.Stream{
				URL:     browserURL(st.URL),
				Kind:    kind,
				Quality: st.Quality,
				Referer: ref,
				Server:  sv.Server,
			})
		}
	}
	for _, sub := range s.Subtitles {
		// the site's player loads these two formats and nothing else, and the
		// thumbnail sprites that once rode this list now have their own field
		if sub.Format != "vtt" && sub.Format != "srt" {
			continue
		}
		res.Subtitles = append(res.Subtitles, upstream.Subtitle{
			File:    browserURL(sub.File),
			Label:   sub.Label,
			Lang:    upstream.Language(sub.Language, sub.Label),
			Default: sub.Default,
		})
	}
	return res
}

// referer is the referer a server's streams are fetched with, and whether the
// proxy can send what the server asks for at all
// the proxy sends a referer and the origin it belongs to, so a server asking
// for any other header, or for an origin its referer does not name, is one its
// streams cannot be fetched from
func referer(headers map[string]string) (string, bool) {
	var ref, origin string
	for k, v := range headers {
		switch strings.ToLower(k) {
		case "referer":
			ref = v
		case "origin":
			origin = v
		default:
			return "", false
		}
	}
	if origin != "" {
		h := http.Header{}
		upstream.SetReferer(h, ref)
		if h.Get("Origin") != origin {
			return "", false
		}
	}
	return ref, true
}

// episodeKey is what a listed episode's id carries, everything Sources needs to
// find the provider again
type episodeKey struct {
	// title is the catalog's own key for the title, which never holds a slash
	title    string
	number   int
	provider string
}

func (k episodeKey) String() string {
	return k.title + "/" + strconv.Itoa(k.number) + "/" + k.provider
}

func parseKey(id string) (episodeKey, error) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 {
		return episodeKey{}, fmt.Errorf("miruro episode id %q is not title/number/provider", id)
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil || n < 1 {
		return episodeKey{}, fmt.Errorf("miruro episode id %q names no episode", id)
	}
	return episodeKey{title: parts[0], number: n, provider: parts[2]}, nil
}

// play fetches every provider's answer for one episode
func (c *Client) play(ctx context.Context, ref string, n int) (*playback, error) {
	body, err := c.get(ctx, "/api/v1/anime/"+url.PathEscape(ref)+"/episodes/"+strconv.Itoa(n)+"/play")
	if err != nil {
		return nil, err
	}
	var p playback
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Listing names the providers serving one episode and the renditions each
// serves it in
// one play request answers every provider on every track, and a provider is
// listed under a track only where it has a stream this program can play
func (c *Client) Listing(ctx context.Context, ref string, number float64) (*upstream.Listing, error) {
	n := int(number)
	if float64(n) != number || n < 1 {
		return nil, fmt.Errorf("miruro addresses whole episodes only, not %v", number)
	}
	codes, err := c.providerCodes(ctx)
	if err != nil {
		return nil, err
	}
	p, err := c.play(ctx, ref, n)
	if err != nil {
		return nil, err
	}

	l := &upstream.Listing{Providers: map[string]upstream.Provider{}, Caps: upstream.Capabilities{}}
	for _, tr := range p.Tracks {
		cat, ok := tracks[tr.Track]
		if !ok {
			continue
		}
		for _, s := range tr.Providers {
			code, ok := codes[s.Provider]
			if !ok || len(s.result().Streams) == 0 {
				continue
			}
			pv := l.Providers[code]
			pv.Code, pv.Backend = code, c
			e := upstream.Episode{ID: episodeKey{ref, n, s.Provider}.String(), Number: number}
			caps := l.Caps[code]
			switch cat {
			case upstream.Dub:
				pv.Dub = []upstream.Episode{e}
			case upstream.Sub:
				pv.Sub, caps.Hard = []upstream.Episode{e}, true
			case upstream.Ssub:
				pv.Sub, caps.Soft = []upstream.Episode{e}, true
			}
			l.Providers[code] = pv
			if caps != (upstream.Caps{}) {
				l.Caps[code] = caps
			}
		}
	}
	return l, nil
}

// Sources resolves an episode on one provider in one rendition
// it asks the play resource again rather than keeping what a listing read, so
// a resolution never serves stream urls from a listing minutes older than it
func (c *Client) Sources(ctx context.Context, episodeID, provider string, cat upstream.Category) (*upstream.Result, error) {
	key, err := parseKey(episodeID)
	if err != nil {
		return nil, err
	}
	p, err := c.play(ctx, key.title, key.number)
	if err != nil {
		return nil, err
	}
	for _, tr := range p.Tracks {
		if tr.Track != string(cat) {
			continue
		}
		for _, s := range tr.Providers {
			if s.Provider == key.provider {
				return s.result(), nil
			}
		}
	}
	return nil, fmt.Errorf("%w: %s serves episode %d in no %s rendition", upstream.ErrNoStream, provider, key.number, cat)
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
