package main

import "testing"

func TestSplitPin(t *testing.T) {
	cases := []struct {
		in, code, variant string
	}{
		{"bonk", "bonk", "soft"},
		{"bonk:hard", "bonk", "hard"},
		{"bonk:soft", "bonk", "soft"},
		{"bonk:xyz", "bonk", "soft"},
		{"", "", "soft"},
		{"ally", "ally", "soft"},
	}
	for _, c := range cases {
		code, variant := splitPin(c.in)
		if code != c.code || variant != c.variant {
			t.Errorf("splitPin(%q) = (%q, %q), want (%q, %q)", c.in, code, variant, c.code, c.variant)
		}
	}
}
