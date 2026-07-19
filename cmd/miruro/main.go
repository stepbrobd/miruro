package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"ysun.co/miruro/ui"
)

var version = "dev"

var (
	flagEpisode  string
	flagDownload bool
	flagQuality  string
	flagVLC      bool
	flagDub      bool
	flagContinue bool
	flagProvider string
	flagDelete   bool
	flagAll      bool
	flagParallel int
	flagSkip     bool
)

var root = &cobra.Command{
	Use:           "miruro [query]",
	Short:         "Watch anime from miruro.tv",
	Args:          cobra.ArbitraryArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          run,
}

func init() {
	f := root.Flags()
	f.StringVarP(&flagEpisode, "episode", "e", "", "Episode number or range, e.g. 5 or 5-8")
	f.BoolVarP(&flagDownload, "download", "d", false, "Download instead of playing")
	f.StringVarP(&flagQuality, "quality", "q", "", "Video quality, e.g. best or 1080p")
	f.BoolVarP(&flagVLC, "vlc", "v", false, "Use VLC")
	f.BoolVar(&flagDub, "dub", false, "Use dub instead of sub")
	f.BoolVarP(&flagContinue, "continue", "c", false, "Resume from history")
	f.StringVar(&flagProvider, "provider", "", "Pin a provider by code")
	f.BoolVarP(&flagDelete, "delete", "D", false, "Clear watch history")
	f.BoolVar(&flagAll, "all", false, "Select every episode, for use with --download")
	f.IntVarP(&flagParallel, "parallel", "p", 1, "Parallel download workers")
	f.BoolVar(&flagSkip, "skip", false, "Mark intro and outro as mpv chapters via aniskip")
}

func main() {
	root.Version = version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		if errors.Is(err, ui.ErrAborted) || errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		log.Error(err)
		os.Exit(1)
	}
}
