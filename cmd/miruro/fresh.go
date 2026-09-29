package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"text/tabwriter"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"ysun.co/miruro/internal/miruro"
	"ysun.co/miruro/internal/upstream"
)

var newCmd = &cobra.Command{
	Use:   "new",
	Short: "List titles in history with episodes past the one watched",
	Args:  cobra.NoArgs,
	RunE:  runNew,
}

func init() {
	root.AddCommand(newCmd)
}

// checks bounds how many catalogs new fetches at once, a handful being enough
// to hide the round trips without a burst against an edge already slow
const checks = 4

func runNew(cmd *cobra.Command, _ []string) error {
	entries, err := openStore().load()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("history is empty")
		return nil
	}
	cfg := loadConfig()
	client := miruro.New()
	if len(cfg.Mirrors) > 0 {
		client.Bases = cfg.Mirrors
	}

	found := newer(cmd.Context(), enabled(all(client), cfg.Backends), entries)
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	return showNew(os.Stdout, found)
}

// showNew lists the titles with episodes past the one watched, and warns for
// each one no backend answered for, since leaving it out would read as nothing
// new
func showNew(out io.Writer, found []fresh) error {
	var rows []fresh
	for _, f := range found {
		switch {
		case f.err != nil:
			log.Warn("title not checked", "title", f.entry.Title, "err", f.err)
		case len(f.newer) > 0:
			rows = append(rows, f)
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "nothing new past what history has watched")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, f := range rows {
		fmt.Fprintf(w, "%s\tep %s watched\t%d newer, up to %s\t%s\n",
			f.entry.Title, num(f.entry.Episode), len(f.newer),
			num(f.newer[len(f.newer)-1]), f.entry.Category)
	}
	return w.Flush()
}

// fresh is one title in history and the episodes its catalog lists past the
// one watched, in the category it was watched in
type fresh struct {
	entry entry
	newer []float64
	err   error
}

// newer asks the backends about every title in history, a few at a time, and
// keeps the order history lists them in
// a title no backend could answer for carries the failures, since reporting it
// as nothing new would say something nobody checked
func newer(ctx context.Context, backends upstream.Backends, entries []entry) []fresh {
	out := make([]fresh, len(entries))
	sem := make(chan struct{}, checks)
	var wg sync.WaitGroup
	for i, e := range entries {
		out[i].entry = e
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[i].err = ctx.Err()
				return
			}
			defer func() { <-sem }()
			cat, failed := backends.Episodes(ctx, upstream.Media{ID: e.AnilistID, Romaji: e.Title})
			numbers := cat.Numbers(e.Category)
			if len(numbers) == 0 && len(failed) > 0 {
				out[i].err = errors.Join(joined(failed)...)
				return
			}
			i0, _ := slices.BinarySearch(numbers, e.Episode)
			for _, n := range numbers[i0:] {
				if n > e.Episode {
					out[i].newer = append(out[i].newer, n)
				}
			}
		})
	}
	wg.Wait()
	return out
}
