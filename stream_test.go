package miruro

import (
	"testing"
)

func TestOrder(t *testing.T) {
	en := Subtitle{Label: "English", Lang: "en"}
	es := Subtitle{Label: "Spanish", Lang: "es", Default: true}
	pt := Subtitle{Label: "Portugues", Lang: "pt-BR"}
	subs := []Subtitle{pt, en, es}

	first := func(lang string) string {
		t.Helper()
		out := Order(subs, lang)
		if len(out) != len(subs) {
			t.Fatalf("Order(%q) returned %d tracks, want %d", lang, len(out), len(subs))
		}
		return out[0].Label
	}

	if got := first(""); got != "Spanish" {
		t.Errorf("with no preference the provider default leads, got %q", got)
	}
	if got := first("en"); got != "English" {
		t.Errorf("Order by tag = %q, want English", got)
	}
	if got := first("English"); got != "English" {
		t.Errorf("Order by label = %q, want English", got)
	}
	if got := first("pt"); got != "Portugues" {
		t.Errorf("a primary subtag must select its regional track, got %q", got)
	}
	if got := first("de"); got != "Spanish" {
		t.Errorf("an absent language falls back to the default track, got %q", got)
	}

	// the input must survive, since the caller still holds it
	if subs[0].Label != "Portugues" {
		t.Error("Order reordered the slice it was given")
	}
}
