package miruro

import (
	"slices"
	"testing"
)

func testCatalog() *Catalog {
	return &Catalog{Providers: map[string]Provider{
		"bonk": {
			Code: "bonk",
			Sub:  []Episode{{ID: "b1", Number: 1}, {ID: "b2", Number: 2}},
			Dub:  []Episode{{ID: "bd1", Number: 1}},
		},
		"ally": {
			Code: "ally",
			Sub:  []Episode{{ID: "a2", Number: 2}, {ID: "a3", Number: 2.5}},
		},
		// alphabetically first and ranked last, so a code sort and a preference
		// sort disagree
		"bee": {
			Code: "bee",
			Sub:  []Episode{{ID: "e2", Number: 2}},
		},
		// unnamed by the preference list, so it sorts after everything named
		"ANIMEDUNYA": {
			Code: "ANIMEDUNYA",
			Sub:  []Episode{{ID: "d2", Number: 2}},
		},
	}}
}

func TestNumbersUnion(t *testing.T) {
	cat := testCatalog()
	if got, want := cat.Numbers(Sub), []float64{1, 2, 2.5}; !slices.Equal(got, want) {
		t.Errorf("Numbers(Sub) = %v, want %v", got, want)
	}
	if got, want := cat.Numbers(Dub), []float64{1}; !slices.Equal(got, want) {
		t.Errorf("Numbers(Dub) = %v, want %v", got, want)
	}
}

func TestAvailableOrdersByPreference(t *testing.T) {
	cat := testCatalog()
	var got []string
	for _, p := range cat.Available(2, Sub) {
		got = append(got, p.Code)
	}
	if want := []string{"ally", "bonk", "bee", "ANIMEDUNYA"}; !slices.Equal(got, want) {
		t.Errorf("Available(2, Sub) = %v, want %v", got, want)
	}
	if avail := cat.Available(2, Dub); len(avail) != 0 {
		t.Errorf("Available(2, Dub) = %v, want none", avail)
	}
}

func TestBestSkips(t *testing.T) {
	rows := []skipEntry{
		{Episode: 1, Type: "op", Start: 10, End: 100, Votes: -1},
		{Episode: 1, Type: "op", Start: 12, End: 102, Votes: 11},
		{Episode: 1, Type: "recap", Start: 0, End: 60, Votes: 50},
		{Episode: 1, Type: "ed", Start: 1300, End: 1400, Votes: 3},
		{Episode: 2, Type: "mixed-op", Start: 0, End: 90, Votes: 5},
	}
	got := bestSkips(rows)

	if len(got) != 2 {
		t.Fatalf("want 2 ranges, got %d: %+v", len(got), got)
	}
	// the highest-voted op wins, recap and mixed are dropped
	if got[0].Kind != Intro || got[0].Start != 12 {
		t.Errorf("intro is not the highest-voted row: %+v", got[0])
	}
	if got[1].Kind != Outro || got[1].Start != 1300 {
		t.Errorf("outro missing or wrong: %+v", got[1])
	}
}

// a mislabelled row can outvote the real one, so position decides first
// this is real payload shape, an "ed" starting at 0.9s of a 1470s episode
func TestBestSkipsRejectsMisplacedRange(t *testing.T) {
	rows := []skipEntry{
		{Episode: 1, Type: "ed", Start: 0.864, End: 90.864, Votes: 1, Length: 1470},
		{Episode: 1, Type: "ed", Start: 1326.083, End: 1416.083, Votes: -1, Length: 1422},
		{Episode: 1, Type: "op", Start: 275.794, End: 365.794, Votes: -1, Length: 1423},
		{Episode: 1, Type: "op", Start: 1300, End: 1390, Votes: 9, Length: 1423},
	}
	got := bestSkips(rows)

	if len(got) != 2 {
		t.Fatalf("want 2 ranges, got %d: %+v", len(got), got)
	}
	if got[0].Kind != Intro || got[0].Start != 275.794 {
		t.Errorf("intro should be the early row despite fewer votes: %+v", got[0])
	}
	if got[1].Kind != Outro || got[1].Start != 1326.083 {
		t.Errorf("outro should be the late row despite fewer votes: %+v", got[1])
	}
}

func TestBestSkipsEmpty(t *testing.T) {
	if got := bestSkips(nil); len(got) != 0 {
		t.Errorf("want no ranges, got %+v", got)
	}
	only := []skipEntry{{Episode: 1, Type: "recap", Start: 0, End: 60, Votes: 9}}
	if got := bestSkips(only); len(got) != 0 {
		t.Errorf("off-enum rows should yield nothing, got %+v", got)
	}
}

// the picker shows one row per number while the records come from whichever
// provider carries them, so the merge has to be deterministic and has to keep
// the preferred provider's flags when a later one only supplies the title
func TestDetails(t *testing.T) {
	cat := &Catalog{Providers: map[string]Provider{
		// ranked last, titles everything, and disagrees about filler
		"moo": {Code: "moo", Sub: []Episode{
			{ID: "m1", Number: 1, Title: "Moo One"},
			{ID: "m2", Number: 2, Title: "Moo Two"},
		}},
		// ranked first, and untitled on episode 2
		"ally": {Code: "ally", Sub: []Episode{
			{ID: "a1", Number: 1, Title: "Ally One"},
			{ID: "a2", Number: 2, Filler: true},
		}},
	}}

	got := cat.Details(Sub)
	if got[1].Title != "Ally One" || got[1].ID != "a1" {
		t.Errorf("episode 1 = %+v, want the preferred provider's whole record", got[1])
	}
	if got[2].Title != "Moo Two" {
		t.Errorf("episode 2 title = %q, want the fill-in from a later provider", got[2].Title)
	}
	if !got[2].Filler || got[2].ID != "a2" {
		t.Errorf("episode 2 = %+v, want the preferred provider's record with only its title filled in", got[2])
	}
	if len(got) != 2 {
		t.Errorf("Details = %d entries, want one per number", len(got))
	}
	if len(cat.Details(Dub)) != 0 {
		t.Error("Details(Dub) invented entries for a category with no episodes")
	}
}
