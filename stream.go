package miruro

import (
	"errors"
	"slices"
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
	// Height restricts an hls master to the variants of one picture height,
	// applied by the stream proxy while rewriting, zero restricts nothing
	// the master travels whole so the audio renditions it associates through
	// EXT-X-MEDIA stay attached, where following a variant URL out of it would
	// sever them and download a silent episode
	Height  int
	Referer string
	// Server is the provider's own name for the host behind this stream,
	// "HD-1" or "VidPlay-1", empty when it names none
	Server string
	// Default marks the stream the provider itself picks
	Default bool
	// Dead marks a stream the provider reports as inactive
	// most streams carry no such flag, so only an explicit false sets this and
	// an absent one stays worth trying
	Dead bool
}

type Subtitle struct {
	File  string
	Label string
	// Lang is the api's language tag, "en" or "pt-BR", empty when it names none
	Lang string
	// Default marks the track the provider itself flags as the one to show
	Default bool
}

type Result struct {
	Streams   []Stream
	Subtitles []Subtitle
}

func playable(s Stream) bool {
	return !s.Dead && (s.Kind == HLS || s.Kind == MP4)
}

// Playable reports whether Rank can return a stream
// an embed-only result carries no hls or mp4, so a caller must skip it rather
// than accept it and fail later outside the fallback loop
// the two agree on dead streams for the same reason
func (r *Result) Playable() bool {
	return slices.ContainsFunc(r.Streams, playable)
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
