package upstream

import (
	"cmp"
	"maps"
	"slices"
)

// Category is a closed set
// an illegal value cannot flow downstream
// the api serves two subtitled renditions of one episode, the burned-in one
// under sub and the one with a detachable subtitle file under ssub, and lists a
// provider under only the renditions it carries
type Category string

const (
	Sub  Category = "sub"
	Ssub Category = "ssub"
	Dub  Category = "dub"
)

// SkipKind marks an aniskip interval
// op is the intro and ed is the outro
type SkipKind string

const (
	Intro SkipKind = "op"
	Outro SkipKind = "ed"
)

type Episode struct {
	// ID is what the backend resolves the episode by, set only on the episodes
	// of a Listing
	ID     string
	Number float64
	// Title is the episode name, empty when the upstream carries none
	Title string
	// Filler marks an episode outside the source material
	Filler bool
}

type SkipRange struct {
	Episode float64
	Kind    SkipKind
	Start   float64
	End     float64
}

// Provider is one provider of a Listing
// Sub carries the episode when the provider serves it subtitled, in either
// rendition, and Dub when it serves it dubbed
type Provider struct {
	Code string
	// Backend is the upstream that listed the provider and resolves its
	// episodes
	Backend Backend
	Sub     []Episode
	Dub     []Episode
}

// Episodes lists the episodes a provider carries in a category
// the ssub rendition is a cut of the sub episode, so it is addressed with the
// ids from the sub list
func (p Provider) Episodes(cat Category) []Episode {
	if cat == Dub {
		return p.Dub
	}
	return p.Sub
}

// Catalog is a title as its backends list it, before any provider is asked for
// one of its episodes
// Refs is the key each backend that listed the title gave it, which is what
// the backend's Listing is asked with
type Catalog struct {
	Title string
	// Sub and Dub are the episodes the title carries in each category
	Sub     []Episode
	Dub     []Episode
	Aniskip []SkipRange
	Refs    map[string]string
}

// Episodes lists what the title carries in a category, the ssub rendition being
// a cut of the sub episodes
func (c *Catalog) Episodes(cat Category) []Episode {
	if cat == Dub {
		return c.Dub
	}
	return c.Sub
}

// Numbers is the sorted episode numbers of a category
func (c *Catalog) Numbers(cat Category) []float64 {
	seen := map[float64]struct{}{}
	for _, e := range c.Episodes(cat) {
		seen[e.Number] = struct{}{}
	}
	return slices.Sorted(maps.Keys(seen))
}

// Details maps every episode number in a category to its record for the picker
// the backends' records are merged into one per number already, so the first
// record of a number is the only one
func (c *Catalog) Details(cat Category) map[float64]Episode {
	out := make(map[float64]Episode)
	for _, e := range c.Episodes(cat) {
		if _, seen := out[e.Number]; !seen {
			out[e.Number] = e
		}
	}
	return out
}

// Listing is one episode as its backends serve it, keyed by provider code
// an upstream names the providers carrying an episode only when asked about
// that episode, so a Listing is fetched per episode where a Catalog is fetched
// per title
type Listing struct {
	Providers map[string]Provider
	// Caps is the renditions each provider serves the episode in
	Caps Capabilities
}

// order is the provider preference, an author-owned default
// ally and pewe lead because they are the two the 2026-08-23 integration run
// watched carry a whole episode end to end, and kiwi follows on AnimeTV-Fork's
// note that it is the best quality of the set
// the tail is what the same run saw fail, in the order it sits in below: bonk
// refused a segment partway through, hop relayed no segment, bee's playlist
// answered 502, and AnimeTV-Fork annotates moo lowest quality
// hop's failure was this client omitting the origin header rather than the
// provider, so its place here is the one entry the evidence no longer supports
// miruro publishes its own order in the config resource and rewrites it between
// deploys, which would move the default provider under a resumed history entry
// and a filled segment cache, so this list stays here instead
var order = []string{"ally", "pewe", "kiwi", "bonk", "hop", "bee", "moo"}

// preference places a provider in order, with an unnamed code after every named
// one rather than interleaved, since nothing is known about it
func preference(code string) int {
	if i := slices.Index(order, code); i >= 0 {
		return i
	}
	return len(order)
}

// Available lists providers carrying the episode in the category, in preference
// order, with equal ranks by code so runs are reproducible
func (l *Listing) Available(number float64, cat Category) []Provider {
	var out []Provider
	for _, p := range l.Providers {
		if slices.ContainsFunc(p.Episodes(cat), func(e Episode) bool { return e.Number == number }) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Provider) int { return byPreference(a.Code, b.Code) })
	return out
}

// byPreference orders provider codes by the preference list, then by code so
// equal ranks stay reproducible
func byPreference(a, b string) int {
	if n := cmp.Compare(preference(a), preference(b)); n != 0 {
		return n
	}
	return cmp.Compare(a, b)
}

type Media struct {
	ID       int
	Romaji   string
	English  string
	Episodes int
	// Format is AniList's media format enum, e.g. TV, MOVIE, OVA
	Format string
}

func (m Media) Title() string {
	if m.English != "" {
		return m.English
	}
	return m.Romaji
}

// Caps is the set of renditions a provider serves an episode in
// the api names the burned-in one "sub" and the detachable one "ssub", which
// reads backwards here, so both are renamed at the parse boundary and nowhere
// else
type Caps struct {
	// Hard means the subtitles arrive burned into the picture
	Hard bool
	// Soft means the provider ships a subtitle file alongside the stream
	Soft bool
}

// Capabilities is what the providers of a Listing serve, keyed by provider code
// a code the table does not name is undeclared rather than incapable, since a
// backend may list a provider without saying which rendition it carries
type Capabilities map[string]Caps
