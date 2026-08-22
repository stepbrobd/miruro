package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"ysun.co/miruro/play"
	"ysun.co/miruro/ui"
)

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Manage watch history",
}

var historyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List watch history, most recent first",
	Args:  cobra.NoArgs,
	RunE:  runHistoryList,
}

var historyClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear watch history",
	Args:  cobra.NoArgs,
	RunE:  runHistoryClear,
}

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage the segment cache",
}

var cacheListCmd = &cobra.Command{
	Use:   "list",
	Short: "List cached download segments",
	Args:  cobra.NoArgs,
	RunE:  runCacheList,
}

var cacheClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear cached download segments",
	Args:  cobra.NoArgs,
	RunE:  runCacheClear,
}

func init() {
	historyCmd.AddCommand(historyListCmd, historyClearCmd)
	cacheCmd.AddCommand(cacheListCmd, cacheClearCmd)
	root.AddCommand(historyCmd, cacheCmd)
}

func runHistoryList(*cobra.Command, []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	entries, err := st.load()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("history is empty")
		return nil
	}
	return table(func(w *tabwriter.Writer) {
		for _, e := range entries {
			fmt.Fprintf(w, "%s\tep %s\t%s\t%s\t%s\n",
				e.Title, num(e.Episode), e.Provider, e.Category, stamp(e.Updated))
		}
	})
}

// stamp renders when an entry was watched
// a hand-written history file can carry no time at all, which prints as the
// zero year rather than as nothing unless it is spelled out
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func runHistoryClear(*cobra.Command, []string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	n, err := clearHistory(st)
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("history already empty")
		return nil
	}
	fmt.Printf("cleared %d history entries\n", n)
	return nil
}

// clearHistory clears the store and reports how many entries it held
func clearHistory(st *store) (int, error) {
	entries, err := st.load()
	if err != nil {
		return 0, err
	}
	if err := st.clear(); err != nil {
		return 0, err
	}
	return len(entries), nil
}

// cached is one cache directory as both list and clear see it
// rest is the key past the anilist id, naming the episode, category, provider
// and quality that select this rendition
type cached struct {
	play.Cache
	id   string
	rest string
	// name is the title history knows the id by, the id itself when it does not
	name string
}

// cachedEpisodes reads every cache directory
// a directory that cannot be read is skipped rather than failing the command,
// since the point of both callers is to describe what is there
func cachedEpisodes() ([]cached, error) {
	root := segmentsRoot()
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []cached
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := play.Cached(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		id, rest, found := cutKey(e.Name())
		if !found {
			id, rest = e.Name(), ""
		}
		out = append(out, cached{Cache: c, id: id, rest: rest})
	}
	return out, nil
}

// cutKey splits a cache directory name into the anilist id and the rest
// only the leading id is parsed back, because a provider code or a quality
// label may itself carry a '-'
func cutKey(name string) (id, rest string, found bool) {
	for i, r := range name {
		if r == '-' {
			if i == 0 {
				return "", "", false
			}
			return name[:i], name[i+1:], true
		}
		if r < '0' || r > '9' {
			return "", "", false
		}
	}
	return "", "", false
}

func runCacheList(*cobra.Command, []string) error {
	episodes, err := cachedEpisodes()
	if err != nil {
		return err
	}
	if len(episodes) == 0 {
		fmt.Println("cache is empty")
		return nil
	}

	// every finished download removes its own directory, so a cache only ever
	// holds interrupted ones, and the title is what names them for a reader
	titles := historyTitles()
	for i, c := range episodes {
		if name := titles[c.id]; name != "" {
			episodes[i].name = name
		} else {
			episodes[i].name = c.id
		}
	}
	// the directory order is lexicographic by anilist id, which means nothing to
	// a reader deciding what to drop
	slices.SortFunc(episodes, func(a, b cached) int {
		if n := strings.Compare(a.name, b.name); n != 0 {
			return n
		}
		return strings.Compare(a.rest, b.rest)
	})

	var total int64
	err = table(func(w *tabwriter.Writer) {
		for _, c := range episodes {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.name, c.rest, segments(c.Cache), ui.Bytes(c.Bytes))
			total += c.Bytes
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("%d interrupted downloads, %s\n", len(episodes), ui.Bytes(total))
	return nil
}

// segments reports how far a cached download got
// reconcile writes the manifest before the first fetch, so an unknown total
// means the directory is not one this version wrote
func segments(c play.Cache) string {
	if c.Want == 0 {
		return fmt.Sprintf("%d/? segments", c.Have)
	}
	return fmt.Sprintf("%d/%d segments", c.Have, c.Want)
}

// historyTitles maps an anilist id to the title history knows it by
// an unreadable history only costs the listing its names, so it is not an error
func historyTitles() map[string]string {
	st, err := openStore()
	if err != nil {
		return nil
	}
	entries, err := st.load()
	if err != nil {
		return nil
	}
	titles := make(map[string]string, len(entries))
	for _, e := range entries {
		titles[fmt.Sprint(e.AnilistID)] = e.Title
	}
	return titles
}

func runCacheClear(*cobra.Command, []string) error {
	episodes, err := cachedEpisodes()
	if err != nil {
		return err
	}
	var total int64
	for _, c := range episodes {
		total += c.Bytes
	}
	if err := clearCache(); err != nil {
		return err
	}
	if len(episodes) == 0 {
		fmt.Println("cache already empty")
		return nil
	}
	fmt.Printf("removed %s of cached segments\n", ui.Bytes(total))
	return nil
}

// table writes aligned columns, so a listing stays readable whatever the widest
// title happens to be
func table(rows func(*tabwriter.Writer)) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	rows(w)
	return w.Flush()
}
