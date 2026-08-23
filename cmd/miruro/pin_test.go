package main

import (
	"slices"
	"strings"
	"testing"

	"ysun.co/miruro"
)

func TestParsePin(t *testing.T) {
	cases := []struct {
		in   string
		want Pin
	}{
		{"bonk", Pin{"bonk", Soft}},
		{"bonk:hard", Pin{"bonk", Hard}},
		{"bonk:soft", Pin{"bonk", Soft}},
		{"bonk:xyz", Pin{"bonk", Soft}},
		{"", Pin{"", Soft}},
		{"ally", Pin{"ally", Soft}},
	}
	for _, c := range cases {
		if got := ParsePin(c.in); got != c.want {
			t.Errorf("ParsePin(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestPinString(t *testing.T) {
	cases := []struct {
		pin  Pin
		want string
	}{
		{Pin{"bonk", Hard}, "bonk:hard"},
		{Pin{"ally", Soft}, "ally:soft"},
		{Pin{"", Soft}, ""},
	}
	for _, c := range cases {
		if got := c.pin.String(); got != c.want {
			t.Errorf("%+v.String() = %q, want %q", c.pin, got, c.want)
		}
	}
	// round trip through the persisted form
	if got := ParsePin(Pin{"bonk", Hard}.String()); got != (Pin{"bonk", Hard}) {
		t.Errorf("round trip lost the pin: %+v", got)
	}
}

var testCaps = miruro.Capabilities{
	"kiwi": {Hard: true},
	"bee":  {Soft: true},
	"bonk": {Hard: true, Soft: true},
	"twin": {Hard: true, Soft: true, Embed: true},
	"void": {},
}

func providers(codes ...string) []miruro.Provider {
	out := make([]miruro.Provider, len(codes))
	for i, c := range codes {
		out[i] = miruro.Provider{Code: c}
	}
	return out
}

// a provider declaring one variant leaves no choice, so it must not be shown as
// one
// a provider the table omits must not be labelled with a claim it never made
func TestOffers(t *testing.T) {
	rows := offers(providers("kiwi", "bee", "bonk", "void", "ANIMEDUNYA"), testCaps, miruro.Sub, Pin{})
	var got []string
	for _, o := range rows {
		got = append(got, o.Pin.String())
	}
	want := []string{"kiwi:hard", "bee:soft", "bonk:soft", "bonk:hard", "void:soft", "ANIMEDUNYA:soft"}
	if !slices.Equal(got, want) {
		t.Fatalf("offers = %v, want %v", got, want)
	}

	width := widest(rows)
	for _, tc := range []struct {
		row  int
		want string
	}{
		{0, "kiwi        hardsub"},
		{2, "bonk        softsub"},
		{3, "bonk        hardsub"},
		// declared nothing and named by nobody are both bare
		{4, "void"},
		{5, "ANIMEDUNYA"},
	} {
		if got := rows[tc.row].label(width); got != tc.want {
			t.Errorf("row %d label = %q, want %q", tc.row, got, tc.want)
		}
	}
}

// the table describes the two sub renditions and says nothing about dub
func TestOffersForDub(t *testing.T) {
	rows := offers(providers("kiwi", "bonk"), testCaps, miruro.Dub, Pin{})
	if len(rows) != 2 {
		t.Fatalf("offers = %d rows, want one per provider", len(rows))
	}
	for _, o := range rows {
		if o.declared {
			t.Errorf("%q was labelled with a sub variant on a dub run", o.Code)
		}
		if got := o.source(miruro.Dub); got.Category != miruro.Dub || !got.Attach {
			t.Errorf("%q source = %+v, want the dub category with its subtitles", o.Code, got)
		}
	}
}

// without the table every provider is undeclared, so the run asks for the sub
// rendition and attaches whatever comes back
// an explicit code:hard still has to survive, since it is the only correction
// left when the table cannot be reached
func TestOffersWithoutTheTable(t *testing.T) {
	rows := offers(providers("kiwi", "bonk"), nil, miruro.Sub, Pin{})
	if len(rows) != 2 {
		t.Fatalf("offers = %d rows, want one per provider", len(rows))
	}
	for _, o := range rows {
		if o.declared || o.Variant != Soft {
			t.Errorf("%q = %+v, want an undeclared soft row", o.Code, o)
		}
		if got := o.source(miruro.Sub); got.Category != miruro.Sub || !got.Attach {
			t.Errorf("%q source = %+v, want the sub rendition with its subtitles", o.Code, got)
		}
	}

	pinned := offers(providers("kiwi", "bonk"), nil, miruro.Sub, Pin{"bonk", Hard})
	if got := pinned[1]; got.Variant != Hard {
		t.Errorf("pinned row = %+v, want the explicit hard variant carried through", got)
	}
	if got := pinned[1].source(miruro.Sub); got.Attach {
		t.Error("an explicit hard pin still attached the subtitle file")
	}
}

// asking a provider for the rendition it does not carry answers 444, and the
// burned-in rendition still ships a subtitle file on bonk, so the attach
// decision follows what was asked for rather than what arrived
func TestOfferSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    offer
		cat  miruro.Category
		want source
	}{
		{
			"declared hard asks for the burned-in rendition and drops the file",
			offer{Pin: Pin{"bonk", Hard}, declared: true}, miruro.Sub,
			source{Pin: Pin{"bonk", Hard}, Category: miruro.Sub},
		},
		{
			"declared soft asks for the detachable rendition",
			offer{Pin: Pin{"bonk", Soft}, declared: true}, miruro.Sub,
			source{Pin: Pin{"bonk", Soft}, Category: miruro.Ssub, Attach: true},
		},
		{
			"undeclared keeps the sub rendition and its file",
			offer{Pin: Pin{"ANIMEDUNYA", Soft}}, miruro.Sub,
			source{Pin: Pin{"ANIMEDUNYA", Soft}, Category: miruro.Sub, Attach: true},
		},
		{
			"dub never becomes ssub, and a hard pin still drops the file",
			offer{Pin: Pin{"bonk", Hard}, declared: true}, miruro.Dub,
			source{Pin: Pin{"bonk", Hard}, Category: miruro.Dub},
		},
		{
			"an undeclared provider honours an explicit hard pin",
			offer{Pin: Pin{"ANIMEDUNYA", Hard}}, miruro.Sub,
			source{Pin: Pin{"ANIMEDUNYA", Hard}, Category: miruro.Sub},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.source(tc.cat); got != tc.want {
				t.Errorf("source = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestOrderPinned(t *testing.T) {
	rows := offers(providers("kiwi", "bee", "bonk"), testCaps, miruro.Sub, Pin{})
	for _, tc := range []struct {
		name string
		pin  Pin
		want []string
	}{
		{"the pinned rendition leads", Pin{"bonk", Hard},
			[]string{"bonk:hard", "bonk:soft", "kiwi:hard", "bee:soft"}},
		{"a pin on the provider's other rendition still leads with the provider", Pin{"bee", Hard},
			[]string{"bee:soft", "kiwi:hard", "bonk:soft", "bonk:hard"}},
		{"an absent code keeps the order", Pin{"zzz", Soft},
			[]string{"kiwi:hard", "bee:soft", "bonk:soft", "bonk:hard"}},
		{"an empty pin keeps the order", Pin{},
			[]string{"kiwi:hard", "bee:soft", "bonk:soft", "bonk:hard"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, o := range orderPinned(rows, tc.pin) {
				got = append(got, o.Pin.String())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("orderPinned(%+v) = %v, want %v", tc.pin, got, tc.want)
			}
		})
	}
}

func TestCandidates(t *testing.T) {
	cat := &miruro.Catalog{Providers: map[string]miruro.Provider{
		"kiwi": {Code: "kiwi", Sub: []miruro.Episode{{ID: "k1", Number: 1}}},
		"twin": {Code: "twin", Sub: []miruro.Episode{{ID: "t1", Number: 1}, {ID: "t2", Number: 2}}},
	}}

	got, err := candidates(cat, 1, miruro.Sub, testCaps)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Code != "kiwi" {
		t.Errorf("candidates = %v, want the embed dropped", got)
	}

	// an episode only an embed carries is not the same as an episode nobody has
	if _, err := candidates(cat, 2, miruro.Sub, testCaps); err == nil || !strings.Contains(err.Error(), "embed") {
		t.Errorf("err = %v, want the embed reason", err)
	}
	if _, err := candidates(cat, 3, miruro.Sub, testCaps); err == nil || !strings.Contains(err.Error(), "no provider") {
		t.Errorf("err = %v, want the missing-episode reason", err)
	}
}
