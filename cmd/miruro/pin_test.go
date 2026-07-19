package main

import "testing"

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
