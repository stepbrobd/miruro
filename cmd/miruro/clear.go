package main

import (
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/adrg/xdg"
	"github.com/spf13/cobra"
)

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Manage watch history",
}

var historyClearCmd = &cobra.Command{
	Use:           "clear",
	Short:         "Clear watch history",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runHistoryClear,
}

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage the segment cache",
}

var cacheClearCmd = &cobra.Command{
	Use:           "clear",
	Short:         "Clear cached download segments",
	Args:          cobra.NoArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runCacheClear,
}

func init() {
	historyCmd.AddCommand(historyClearCmd)
	cacheCmd.AddCommand(cacheClearCmd)
	root.AddCommand(historyCmd, cacheCmd)
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

func runCacheClear(*cobra.Command, []string) error {
	segments, err := xdg.StateFile("miruro/segments")
	if err != nil {
		return err
	}
	// a file that vanishes mid-walk only skews the printed size, so walk errors
	// are skipped rather than failed
	var total int64
	_ = filepath.WalkDir(segments, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	if err := clearCache(); err != nil {
		return err
	}
	if total == 0 {
		fmt.Println("cache already empty")
		return nil
	}
	fmt.Printf("removed %s of cached segments\n", humanBytes(total))
	return nil
}

// humanBytes mirrors ui's progress formatting so the freed size reads the same
// as the download that produced it
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
