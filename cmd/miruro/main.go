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
	flagDub      bool
	flagContinue bool
	flagProvider string
	flagFallback bool
	flagLang     string
	flagAll      bool
	flagParallel int
	flagSkip     bool
	flagVerbose  bool
)

var root = &cobra.Command{
	Use:           "miruro [query]",
	Short:         "Watch anime from the command line",
	Args:          cobra.ArbitraryArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          run,
}

func init() {
	f := root.Flags()
	f.StringVarP(&flagEpisode, "episode", "e", "", "Episode number or range, e.g. 5 or 5-8")
	f.BoolVarP(&flagDownload, "download", "d", false, "Download instead of playing")
	f.StringVarP(&flagQuality, "quality", "q", "", "Video quality, best, worst, or a height such as 1080p")
	f.BoolVar(&flagDub, "dub", false, "Use dub instead of sub")
	f.BoolVarP(&flagContinue, "continue", "c", false, "Resume from history")
	f.StringVar(&flagProvider, "provider", "", "Pin a provider as code or code:variant, held unless -f")
	f.BoolVarP(&flagFallback, "fallback", "f", false, "Try other providers when the pinned one fails")
	f.StringVar(&flagLang, "lang", "", "Subtitle language, a tag or label like en or English")
	f.BoolVar(&flagAll, "all", false, "Select every episode, to binge or to download")
	f.IntVarP(&flagParallel, "parallel", "p", 1, "Parallel download workers")
	f.BoolVar(&flagSkip, "skip", false, "Mark intro and outro as player chapters via aniskip")
	root.PersistentFlags().BoolVar(&flagVerbose, "verbose", false, "Log resolution and playback detail")

	// a record here is read as it happens, so the wall clock says nothing, and
	// under a live view it costs twenty of the eighty columns a narrow terminal
	// has and pushes the keys the record was written for off the end
	// it is set here rather than in PersistentPreRun because a bad flag never
	// reaches that, and around each view because the logger reads the setting
	// outside the mutex it takes for the output, so moving it while a download
	// worker logs is a race
	log.SetReportTimestamp(false)

	// keep routine progress quiet by default, warnings and errors still show
	root.PersistentPreRun = func(*cobra.Command, []string) {
		if flagVerbose {
			log.SetLevel(log.DebugLevel)
		} else {
			log.SetLevel(log.WarnLevel)
		}
	}
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
