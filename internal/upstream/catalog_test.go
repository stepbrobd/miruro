package upstream

import (
	"slices"
	"testing"
)

func TestNumbers(t *testing.T) {
	cat := &Catalog{
		Sub: []Episode{{Number: 2}, {Number: 1}, {Number: 2.5}},
		Dub: []Episode{{Number: 1}},
	}
	if got, want := cat.Numbers(Sub), []float64{1, 2, 2.5}; !slices.Equal(got, want) {
		t.Errorf("Numbers(Sub) = %v, want %v", got, want)
	}
	// the softsub rendition is a cut of the sub episodes
	if got, want := cat.Numbers(Ssub), []float64{1, 2, 2.5}; !slices.Equal(got, want) {
		t.Errorf("Numbers(Ssub) = %v, want %v", got, want)
	}
	if got, want := cat.Numbers(Dub), []float64{1}; !slices.Equal(got, want) {
		t.Errorf("Numbers(Dub) = %v, want %v", got, want)
	}
}

func TestDetails(t *testing.T) {
	cat := &Catalog{Sub: []Episode{
		{Number: 1, Title: "The Journey's End"},
		{Number: 2, Filler: true},
	}}
	got := cat.Details(Sub)
	if len(got) != 2 || got[1].Title != "The Journey's End" || !got[2].Filler {
		t.Errorf("Details = %+v, want one record per number", got)
	}
	if len(cat.Details(Dub)) != 0 {
		t.Error("Details(Dub) invented entries for a category with no episodes")
	}
}

func TestAvailableOrdersByPreference(t *testing.T) {
	l := &Listing{Providers: map[string]Provider{
		"bonk": {Code: "bonk", Sub: []Episode{{ID: "b2", Number: 2}}, Dub: []Episode{{ID: "bd2", Number: 2}}},
		"ally": {Code: "ally", Sub: []Episode{{ID: "a2", Number: 2}}},
		// alphabetically first and ranked last, so a code sort and a preference
		// sort disagree
		"bee": {Code: "bee", Sub: []Episode{{ID: "e2", Number: 2}}},
		// unnamed by the preference list, so it sorts after everything named
		"sun": {Code: "sun", Sub: []Episode{{ID: "s2", Number: 2}}},
	}}
	var got []string
	for _, p := range l.Available(2, Sub) {
		got = append(got, p.Code)
	}
	if want := []string{"ally", "bonk", "bee", "sun"}; !slices.Equal(got, want) {
		t.Errorf("Available(2, Sub) = %v, want %v", got, want)
	}
	got = nil
	for _, p := range l.Available(2, Dub) {
		got = append(got, p.Code)
	}
	if want := []string{"bonk"}; !slices.Equal(got, want) {
		t.Errorf("Available(2, Dub) = %v, want %v", got, want)
	}
	if avail := l.Available(3, Sub); len(avail) != 0 {
		t.Errorf("Available(3, Sub) = %v, want none", avail)
	}
}
