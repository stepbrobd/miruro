package ui

import "testing"

// a list that fits on screen is drawn whole, so moving the selector never
// scrolls an item that was visible out of the way
func TestFit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		items  int
		screen int
		want   int
	}{
		{"a short list on a tall terminal is drawn whole", 6, 50, 7},
		{"a list that exactly fills the room is drawn whole", 19, 24, 20},
		{"a longer list stops at the room there is", 1100, 24, 20},
		{"no terminal to measure falls back", 1100, 0, blindRows},
		{"a short list is unaffected by the fallback", 3, 0, 4},
		// a terminal too short to reserve anything still has to draw something
		{"a terminal with no room left keeps a usable window", 1100, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fit(tc.items, tc.screen); got != tc.want {
				t.Errorf("fit(%d, %d) = %d, want %d", tc.items, tc.screen, got, tc.want)
			}
		})
	}
}
