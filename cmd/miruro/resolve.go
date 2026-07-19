package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"ysun.co/miruro"
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

	category := miruro.Sub
	if resDub {
		category = miruro.Dub
	}

	client := miruro.New()

	cat, err := client.Episodes(ctx, id)
	if err != nil {
		return err
	}
	avail := cat.Available(ep, category)
	if len(avail) == 0 {
		return fmt.Errorf("no provider has episode %s", num(ep))
	}
	if resProvider != "" {
		// the variant is meaningless here since resolve prints the full subtitles
		// array and leaves the attach decision to the caller
		pin := ParsePin(resProvider)
		if _, ok := cat.Providers[pin.Code]; !ok {
			return fmt.Errorf("provider %q not in catalog", pin.Code)
		}
		avail = orderPinned(avail, pin.Code)
	}

	var last error
	for _, p := range avail {
		e := find(p.Episodes(category), ep)
		if e == nil {
			continue
		}
		res, err := client.Sources(ctx, e.ID, p.Code, category)
		if err != nil {
			if errors.Is(err, miruro.ErrBlocked) {
				return err
			}
			last = err
			continue
		}
		stream, err := client.Select(ctx, res, resQuality)
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

func emit(s miruro.Stream, res *miruro.Result, provider string) error {
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
