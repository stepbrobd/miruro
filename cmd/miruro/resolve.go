package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"ysun.co/miruro"
)

var (
	resProvider string
	resDub      bool
	resQuality  string
	resJSON     bool
)

var resolveCmd = &cobra.Command{
	Use:   "resolve <query|anilistId> <episode>",
	Short: "Resolve an episode to a stream URL without playing",
	Long: `Resolve an episode to a stream URL without playing.

A numeric first argument is read as an AniList id, anything else is searched
on AniList and the top hit is used. The matched title is reported on stderr
so a script consumer knows which title served.`,
	Args:          cobra.ExactArgs(2),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runResolve,
}

func init() {
	f := resolveCmd.Flags()
	f.StringVar(&resProvider, "provider", "", "Pin a provider as code or code:variant, variant is soft or hard")
	f.BoolVar(&resDub, "dub", false, "Use dub instead of sub")
	f.StringVarP(&resQuality, "quality", "q", "", "Video quality, e.g. best or 1080p")
	f.BoolVar(&resJSON, "json", false, "Emit JSON with referer and subtitles")
	root.AddCommand(resolveCmd)
}

func runResolve(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	cfg := loadConfig()
	if resQuality != "" {
		cfg.Quality = resQuality
	}
	if resProvider != "" {
		cfg.Provider = resProvider
	}
	if resDub {
		cfg.Dub = true
	}

	category := miruro.Sub
	if cfg.Dub {
		category = miruro.Dub
	}

	client := miruro.New()

	id, err := strconv.Atoi(args[0])
	if err != nil {
		media, serr := client.Search(ctx, args[0])
		if serr != nil {
			return serr
		}
		if len(media) == 0 {
			return fmt.Errorf("no results for %q", args[0])
		}
		id = media[0].ID
		// name the match so a script consumer knows which title served
		fmt.Fprintf(os.Stderr, "matched %q (anilist %d)\n", media[0].Title(), id)
	}

	ep, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		return fmt.Errorf("invalid episode %q", args[1])
	}

	cat, err := client.Episodes(ctx, id)
	if err != nil {
		return err
	}

	pin := ParsePin(cfg.Provider)
	if pin.Code != "" {
		if _, ok := cat.Providers[pin.Code]; !ok {
			log.Warn("pinned provider not in catalog, using fallback order", "provider", pin.Code)
		}
	}

	res, served, err := autoResolve(ctx, client, cat, ep, category, pin.Code)
	if err != nil {
		return err
	}
	stream, err := client.Select(ctx, res, cfg.Quality)
	if err != nil {
		return err
	}
	return emit(stream, res, served)
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
