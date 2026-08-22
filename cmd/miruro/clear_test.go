package main

import (
	"path/filepath"
	"testing"
)

func TestClearHistory(t *testing.T) {
	st := &store{path: filepath.Join(t.TempDir(), "history.json")}

	n, err := clearHistory(st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("empty store cleared %d entries, want 0", n)
	}

	for i := 1; i <= 3; i++ {
		if err := st.save(entry{AnilistID: i, Title: "t", Episode: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	n, err = clearHistory(st)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("cleared %d entries, want 3", n)
	}

	entries, err := st.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%d entries survived the clear", len(entries))
	}
}
