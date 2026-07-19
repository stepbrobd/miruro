package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"ysun.co/miruro/catalog"
	"ysun.co/miruro/pipe"
	"ysun.co/miruro/sources"
)

var (
	resEpisode  string
	resProvider string
	resDub      bool
	resQuality  string
	resJSON     bool
)

var resolveCmd = &cobra.Command{
	Use:           "resolve <anilistId>",
	Short:         "Resolve an episode to a stream URL without playing",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runResolve,
}

func init() {
	f := resolveCmd.Flags()
	f.StringVarP(&resEpisode, "episode", "e", "", "Episode number")
	f.StringVar(&resProvider, "provider", "", "Provider code, else auto with fallback")
	f.BoolVar(&resDub, "dub", false, "Use dub instead of sub")
	f.StringVarP(&resQuality, "quality", "q", "best", "Video quality")
	f.BoolVar(&resJSON, "json", false, "Emit JSON with referer and subtitles")
	_ = resolveCmd.MarkFlagRequired("episode")
	root.AddCommand(resolveCmd)
}

func runResolve(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	id, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("anilistId must be numeric, got %q", args[0])
	}
	ep, err := strconv.ParseFloat(resEpisode, 64)
	if err != nil {
		return fmt.Errorf("invalid episode %q", resEpisode)
	}

	category := catalog.Sub
	if resDub {
		category = catalog.Dub
	}

	client := pipe.New()
	hc := &http.Client{Timeout: 60 * time.Second}

	cat, err := catalog.Fetch(ctx, client, id)
	if err != nil {
		return err
	}
	avail := cat.Available(ep, category)
	if len(avail) == 0 {
		return fmt.Errorf("no provider has episode %s", num(ep))
	}
	if resProvider != "" {
		avail = orderPinned(avail, resProvider)
	}

	var last error
	for _, p := range avail {
		e := find(p.Episodes(category), ep)
		if e == nil {
			continue
		}
		res, err := sources.Resolve(ctx, client, e.ID, p.Code, category)
		if err != nil {
			last = err
			continue
		}
		stream, err := sources.Select(ctx, hc, res, resQuality)
		if err != nil {
			last = err
			continue
		}
		return emit(stream, res, p.Code)
	}
	if last == nil {
		last = fmt.Errorf("no source resolved")
	}
	return last
}

func emit(s sources.Stream, res *sources.Result, provider string) error {
	if resJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"url":       s.URL,
			"kind":      s.Kind,
			"quality":   s.Quality,
			"referer":   s.Referer,
			"provider":  provider,
			"softsub":   res.Softsub(),
			"subtitles": res.Subtitles,
		})
	}
	fmt.Println(s.URL)
	fmt.Fprintf(os.Stderr, "provider=%s kind=%s referer=%s softsub=%v\n", provider, s.Kind, s.Referer, res.Softsub())
	return nil
}
