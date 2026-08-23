package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrg/xdg"

	"ysun.co/miruro"
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

// only the leading anilist id is parsed back, since a provider code or a
// quality label may carry a '-' of its own
func TestCutKey(t *testing.T) {
	for _, tc := range []struct {
		name, id, rest string
		found          bool
	}{
		{name: "16498-e6-sub-bonk-best", id: "16498", rest: "e6-sub-bonk-best", found: true},
		{name: "1-e13.5-dub-ally-1080p", id: "1", rest: "e13.5-dub-ally-1080p", found: true},
		{name: "5-e1-sub-hd-1-best", id: "5", rest: "e1-sub-hd-1-best", found: true},
		{name: "-e1-sub-x-best"},
		{name: "notanid-e1"},
		{name: "16498"},
	} {
		id, rest, found := cutKey(tc.name)
		if found != tc.found || id != tc.id || rest != tc.rest {
			t.Errorf("cutKey(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.name, id, rest, found, tc.id, tc.rest, tc.found)
		}
	}
}

// a listing has to describe an interrupted download without trusting the
// directory to hold anything in particular
func TestCachedEpisodes(t *testing.T) {
	root := t.TempDir()
	// registered before Setenv so it runs after the env restore
	t.Cleanup(xdg.Reload)
	t.Setenv("XDG_STATE_HOME", root)
	xdg.Reload()
	seg := filepath.Join(root, "miruro", "segments")

	// every file occupies the disk, so the tally is what was written
	onDisk := map[string]int64{}
	write := func(dir, name string, body []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(seg, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seg, dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
		onDisk[dir] += int64(len(body))
	}

	write("16498-e6-sub-bonk-best", "00000.ts", make([]byte, 100))
	write("16498-e6-sub-bonk-best", "00001.ts", make([]byte, 100))
	// a fetch in flight occupies disk without being a whole segment
	write("16498-e6-sub-bonk-best", "00002.ts.part", make([]byte, 40))
	write("16498-e6-sub-bonk-best", "manifest.json", []byte(`{"count":9,"durations":[1,1,1,1,1,1,1,1,1]}`))
	// interrupted before it wrote a manifest, so it knows no total
	write("7-e1-dub-ally-best", "00000.ts", make([]byte, 50))

	got, err := cachedEpisodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("found %d cached episodes, want 2", len(got))
	}
	slices.SortFunc(got, func(a, b cached) int { return strings.Compare(a.id, b.id) })

	if c := got[0]; c.id != "16498" || c.rest != "e6-sub-bonk-best" || c.Have != 2 || c.Want != 9 ||
		c.Bytes != onDisk["16498-e6-sub-bonk-best"] {
		t.Errorf("cached = %+v, want 2 of 9 segments over %d bytes", c, onDisk["16498-e6-sub-bonk-best"])
	}
	if c := got[1]; c.id != "7" || c.Have != 1 || c.Want != 0 || c.Bytes != onDisk["7-e1-dub-ally-best"] {
		t.Errorf("cached = %+v, want one segment with no total", c)
	}
	if want := "2/9 segments"; segments(got[0].Cache) != want {
		t.Errorf("segments = %q, want %q", segments(got[0].Cache), want)
	}
	if want := "1/? segments"; segments(got[1].Cache) != want {
		t.Errorf("segments = %q, want %q", segments(got[1].Cache), want)
	}
}

// an absent cache is the same answer as an empty one, and never an error
func TestCachedEpisodesWithoutARoot(t *testing.T) {
	t.Cleanup(xdg.Reload)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "gone"))
	xdg.Reload()
	got, err := cachedEpisodes()
	if err != nil || len(got) != 0 {
		t.Errorf("cachedEpisodes() = (%v, %v), want no episodes and no error", got, err)
	}
}

// Category also names the ssub rendition, which is derived per resolution
// a stored entry carrying it would ask every hardsub provider for a rendition
// it does not have, and would write itself back unchanged every run
func TestLoadNarrowsTheStoredCategory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	body := `[{"anilistId":1,"title":"A","category":"ssub","episode":1},
		{"anilistId":2,"title":"B","category":"dub","episode":1},
		{"anilistId":3,"title":"C","category":"","episode":1}]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := (&store{path: path}).load()
	if err != nil {
		t.Fatal(err)
	}
	want := []miruro.Category{miruro.Sub, miruro.Dub, miruro.Sub}
	for i, e := range entries {
		if e.Category != want[i] {
			t.Errorf("entry %d category = %q, want %q", i, e.Category, want[i])
		}
	}
}
