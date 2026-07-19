package miruro

import "testing"

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

func TestBestSkipsEmpty(t *testing.T) {
	if got := bestSkips(nil); len(got) != 0 {
		t.Errorf("want no ranges, got %+v", got)
	}
	only := []skipEntry{{Episode: 1, Type: "recap", Start: 0, End: 60, Votes: 9}}
	if got := bestSkips(only); len(got) != 0 {
		t.Errorf("off-enum rows should yield nothing, got %+v", got)
	}
}
