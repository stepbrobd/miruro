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

var testCaps = miruro.Config{
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
// one, and a provider the table omits must not be labelled with a claim
func TestOffers(t *testing.T) {
	rows := offers(providers("kiwi", "bee", "bonk", "void", "ANIMEDUNYA"), testCaps)
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

// without the table every provider is undeclared, which is the behaviour that
// predates it
func TestOffersWithoutTheTable(t *testing.T) {
	rows := offers(providers("kiwi", "bonk"), nil)
	if len(rows) != 2 {
		t.Fatalf("offers = %d rows, want one per provider", len(rows))
	}
	for _, o := range rows {
		if o.declared || o.Variant != Soft {
			t.Errorf("%q = %+v, want an undeclared soft row", o.Code, o)
		}
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
