package upstream

import (
	"cmp"
	"maps"
	"slices"
)

// Category is a closed set
// an illegal value cannot flow downstream
// the api serves two subtitled renditions of one episode, the burned-in one
// under sub and the one with a detachable subtitle file under ssub, and a
// provider answers 444 for the rendition it does not carry
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
	ID     string  `json:"id"`
	Number float64 `json:"number"`
	// Title is the episode name, empty when the provider carries none
	Title string `json:"title"`
	// Filler marks an episode outside the source material
	Filler bool `json:"filler"`
}

type SkipRange struct {
	Episode float64
	Kind    SkipKind
	Start   float64
	End     float64
}

type Provider struct {
	Code string
	// Backend is the upstream that listed the provider and resolves its
	// episodes
	Backend Backend
	Sub     []Episode
	Dub     []Episode
}

// Episodes lists the episodes a provider carries in a category
// the api lists episodes under sub and dub only, and the ssub rendition is
// addressed with the ids from the sub list
func (p Provider) Episodes(cat Category) []Episode {
	if cat == Dub {
		return p.Dub
	}
	return p.Sub
}

type Catalog struct {
	Title     string
	Providers map[string]Provider
	Aniskip   []SkipRange
}

// Numbers is the sorted union of episode numbers across providers for a category
func (c *Catalog) Numbers(cat Category) []float64 {
	seen := map[float64]struct{}{}
	for _, p := range c.Providers {
		for _, e := range p.Episodes(cat) {
			seen[e.Number] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
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

// Details maps every episode number in a category to one record for the picker
// the first provider in preference order that carries an episode supplies its
// record
// a later provider only fills in a title the first left empty, and fills in
// nothing else, so the preferred provider keeps its filler mark
func (c *Catalog) Details(cat Category) map[float64]Episode {
	codes := make([]string, 0, len(c.Providers))
	for code := range c.Providers {
		codes = append(codes, code)
	}
	slices.SortFunc(codes, byPreference)

	out := make(map[float64]Episode)
	for _, code := range codes {
		for _, e := range c.Providers[code].Episodes(cat) {
			cur, seen := out[e.Number]
			switch {
			case !seen:
				out[e.Number] = e
			case cur.Title == "" && e.Title != "":
				cur.Title = e.Title
				out[e.Number] = cur
			}
		}
	}
	return out
}

// Available lists providers carrying the episode in the category, in preference
// order, with equal ranks by code so runs are reproducible
func (c *Catalog) Available(number float64, cat Category) []Provider {
	var out []Provider
	for _, p := range c.Providers {
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

// Caps is the set of renditions a provider declares it can serve
// the api names the burned-in one "sub" and the detachable one "ssub", which
// reads backwards here, so both are renamed at the parse boundary and nowhere
// else
type Caps struct {
	// Hard means the subtitles arrive burned into the picture
	Hard bool
	// Soft means the provider ships a subtitle file alongside the stream
	Soft bool
	// Embed means an iframe player, which nothing here can play
	Embed bool
}

// Capabilities is the provider capability table, keyed by provider code
// a code the table does not name is undeclared rather than incapable, since the
// episodes resource serves providers the config resource omits
type Capabilities map[string]Caps
