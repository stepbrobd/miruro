package main

import (
	"slices"
	"testing"
)

// a failed episode drops to the control menu while the batch queue still holds
// the rest of the range, so the queue has to follow wherever the menu goes
// without this the next successful episode jumps back into the stale range and
// replays an episode the user already navigated past
func TestAheadReanchorsQueue(t *testing.T) {
	// -e 1-3 with episode 1 playing, so the queue holds what is left
	queue := []float64{2, 3}

	for _, tc := range []struct {
		name string
		ep   float64
		want []float64
	}{
		{"replay keeps the range", 1, []float64{2, 3}},
		{"change provider keeps the range", 1, []float64{2, 3}},
		{"next consumes the episode it moved to", 2, []float64{3}},
		{"select past the range empties it", 9, nil},
		{"previous keeps what is still ahead", 0.5, []float64{2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ahead(queue, tc.ep); !slices.Equal(got, tc.want) {
				t.Errorf("ahead(%v, %v) = %v, want %v", queue, tc.ep, got, tc.want)
			}
		})
	}
}

// the reported symptom was episode 2 playing twice during -e 1-3 when episode 1
// failed and the user chose next, so walk the loop's queue handling directly
func TestFailedEpisodeDoesNotReplayAfterNext(t *testing.T) {
	queue := []float64{1, 2, 3}
	ep := queue[0]
	queue = queue[1:]

	var order []float64
	for range 4 {
		order = append(order, ep)
		played := ep != 1 // episode 1 sits on a dead provider

		if played && len(queue) > 0 {
			ep = queue[0]
			queue = queue[1:]
			continue
		}
		if !played {
			// the control menu, where the user picks next
			ep = 2
			queue = ahead(queue, ep)
			continue
		}
		break
	}

	if want := []float64{1, 2, 3}; !slices.Equal(order, want) {
		t.Errorf("play order %v, want %v", order, want)
	}
}
