package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"

	"ysun.co/miruro"
)

// stateRoot points the xdg state directory at a fresh one, which is where the
// history file and the segment cache live
func stateRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// registered before Setenv so it runs after the env restore
	t.Cleanup(xdg.Reload)
	t.Setenv("XDG_STATE_HOME", root)
	xdg.Reload()
	return root
}

// printed runs f with stdout redirected and returns what it wrote
// the listing commands write straight to stdout, so that is what a test has to
// read to see a whole command through
func printed(t *testing.T, f func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	real := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	ferr := f()
	os.Stdout = real
	w.Close()
	out := <-done
	r.Close()
	if ferr != nil {
		t.Fatalf("command failed: %v", ferr)
	}
	return out
}

func TestOpenStore(t *testing.T) {
	root := stateRoot(t)
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "miruro", "history.json"); st.path != want {
		t.Errorf("store path = %q, want %q", st.path, want)
	}
}

// resume asks which entry to pick once there is one, so only the empty history
// answers without a terminal
func TestResumeWithoutHistory(t *testing.T) {
	st := &store{path: filepath.Join(t.TempDir(), "history.json")}
	if _, err := resume(st); err == nil {
		t.Error("an empty history resumed something")
	}
}

// a history entry is upserted, so rewatching a title moves its episode rather
// than leaving two rows for one show
func TestStoreSaveUpserts(t *testing.T) {
	st := &store{path: filepath.Join(t.TempDir(), "history.json")}
	for _, ep := range []float64{1, 2} {
		if err := st.save(entry{AnilistID: 9, Title: "Show", Episode: ep}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := st.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("history holds %d rows, want one per title", len(entries))
	}
	if entries[0].Episode != 2 {
		t.Errorf("episode = %v, want the later one", entries[0].Episode)
	}
}

func TestStamp(t *testing.T) {
	if got := stamp(time.Time{}); got != "-" {
		t.Errorf("a missing time stamped %q, want a dash", got)
	}
	at := time.Date(2026, 9, 19, 21, 59, 0, 0, time.Local)
	if got := stamp(at); got != "2026-09-19 21:59" {
		t.Errorf("stamp = %q", got)
	}
}

func TestEpisodeSkips(t *testing.T) {
	cat := &miruro.Catalog{Aniskip: []miruro.SkipRange{
		{Episode: 1, Kind: miruro.Intro, Start: 0, End: 90},
		{Episode: 2, Kind: miruro.Intro, Start: 5, End: 95},
		{Episode: 1, Kind: miruro.Outro, Start: 1300, End: 1400},
	}}
	got := episodeSkips(cat, 1)
	if len(got) != 2 {
		t.Fatalf("episode 1 has %d ranges, want both of its own", len(got))
	}
	for _, s := range got {
		if s.Episode != 1 {
			t.Errorf("range from episode %v leaked in", s.Episode)
		}
	}
	if len(episodeSkips(cat, 3)) != 0 {
		t.Error("an episode with no ranges got some")
	}
}

func TestJoined(t *testing.T) {
	sentinel := errors.New("refused")
	errs := joined([]miruro.Failure{{Backend: "miruro", Err: sentinel}})
	if len(errs) != 1 {
		t.Fatalf("widened %d errors, want 1", len(errs))
	}
	if !errors.Is(errors.Join(errs...), sentinel) {
		t.Error("the joined error lost what the backend reported")
	}
}

func TestOr(t *testing.T) {
	if got := or("", "unset"); got != "unset" {
		t.Errorf("or of an empty string = %q", got)
	}
	if got := or("set", "unset"); got != "set" {
		t.Errorf("or of a value = %q", got)
	}
}

// the listing commands are what a user reads, so each is driven end to end and
// its output checked rather than its helpers alone
func TestHistoryCommands(t *testing.T) {
	stateRoot(t)

	if out := printed(t, func() error { return runHistoryList(nil, nil) }); !strings.Contains(out, "history is empty") {
		t.Errorf("an empty history listed %q", out)
	}

	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.save(entry{
		AnilistID: 16498, Title: "Shingeki no Kyojin", Provider: "hop:soft",
		Category: miruro.Sub, Episode: 8, Updated: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	out := printed(t, func() error { return runHistoryList(nil, nil) })
	for _, want := range []string{"Shingeki no Kyojin", "ep 8", "hop:soft", "sub"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing does not mention %q:\n%s", want, out)
		}
	}

	if out := printed(t, func() error { return runHistoryClear(nil, nil) }); !strings.Contains(out, "1") {
		t.Errorf("clearing reported %q, want the count", out)
	}
	entries, err := st.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("history still holds %d entries", len(entries))
	}
}

func TestCacheCommands(t *testing.T) {
	root := stateRoot(t)

	if out := printed(t, func() error { return runCacheList(nil, nil) }); !strings.Contains(out, "cache is empty") {
		t.Errorf("an empty cache listed %q", out)
	}

	// a directory the cache path would have written, named the way cacheDir names it
	dir := filepath.Join(root, "miruro", "segments", "16498-e8-sub-hop-best")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "00000.ts"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}

	// no history names the id, so the listing falls back to the id itself
	out := printed(t, func() error { return runCacheList(nil, nil) })
	for _, want := range []string{"16498", "e8-sub-hop-best", "2.0 KB"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing does not mention %q:\n%s", want, out)
		}
	}

	if out := printed(t, func() error { return runCacheClear(nil, nil) }); !strings.Contains(out, "removed") {
		t.Errorf("clearing reported %q", out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the cached episode survived the clear: %v", err)
	}
}

// the listing borrows its titles from history, and an unreadable history costs
// it the names rather than the listing
func TestHistoryTitles(t *testing.T) {
	stateRoot(t)
	if got := historyTitles(); len(got) != 0 {
		t.Errorf("titles from an absent history = %v", got)
	}

	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.save(entry{AnilistID: 16498, Title: "Shingeki no Kyojin"}); err != nil {
		t.Fatal(err)
	}
	if got := historyTitles()["16498"]; got != "Shingeki no Kyojin" {
		t.Errorf("title for 16498 = %q", got)
	}
}

// chooseEpisodes has three non-interactive paths, and only the fourth needs a
// terminal to answer
func TestChooseEpisodes(t *testing.T) {
	numbers := []float64{1, 2, 3, 4, 5}
	label := func(n float64) string { return num(n) }

	t.Run("a resumed episode is what plays", func(t *testing.T) {
		got, err := chooseEpisodes(numbers, 3, label)
		if err != nil || len(got) != 1 || got[0] != 3 {
			t.Errorf("chose %v, %v, want the resumed episode", got, err)
		}
	})

	t.Run("all takes every episode", func(t *testing.T) {
		defer func(v bool) { flagAll = v }(flagAll)
		flagAll = true
		got, err := chooseEpisodes(numbers, -1, label)
		if err != nil || len(got) != len(numbers) {
			t.Errorf("chose %v, %v, want every episode", got, err)
		}
	})

	t.Run("a range takes what it names", func(t *testing.T) {
		defer func(v string) { flagEpisode = v }(flagEpisode)
		flagEpisode = "2-4"
		got, err := chooseEpisodes(numbers, -1, label)
		if err != nil || len(got) != 3 || got[0] != 2 || got[2] != 4 {
			t.Errorf("chose %v, %v, want 2 through 4", got, err)
		}
	})

	t.Run("an episode nothing carries is refused", func(t *testing.T) {
		defer func(v string) { flagEpisode = v }(flagEpisode)
		flagEpisode = "9"
		if _, err := chooseEpisodes(numbers, -1, label); err == nil {
			t.Error("an episode outside the catalog was accepted")
		}
	})
}
