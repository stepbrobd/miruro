package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"ysun.co/miruro/anilist"
	"ysun.co/miruro/catalog"
	"ysun.co/miruro/download"
	"ysun.co/miruro/history"
	"ysun.co/miruro/pipe"
	"ysun.co/miruro/player"
	"ysun.co/miruro/sources"
	"ysun.co/miruro/ui"
)

func run(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	cfg := loadConfig()
	if flagQuality != "" {
		cfg.Quality = flagQuality
	}
	if flagProvider != "" {
		cfg.Provider = flagProvider
	}
	if flagDub {
		cfg.Dub = true
	}

	store, err := history.Open()
	if err != nil {
		return err
	}
	if flagDelete {
		return store.Clear()
	}

	hc := &http.Client{Timeout: 60 * time.Second}
	client := pipe.New()

	category := catalog.Sub
	if cfg.Dub {
		category = catalog.Dub
	}

	var anilistID int
	var title string
	startEp := -1.0
	pinned := cfg.Provider

	if flagContinue {
		id, t, cat, prov, ep, err := resume(store)
		if err != nil {
			return err
		}
		anilistID, title, category, startEp = id, t, cat, ep
		if prov != "" {
			pinned = prov
		}
	} else {
		id, t, err := findAnime(ctx, hc, args)
		if err != nil {
			return err
		}
		anilistID, title = id, t
	}

	cat, err := catalog.Fetch(ctx, client, anilistID)
	if err != nil {
		return err
	}
	if cat.Mappings.Title != "" {
		title = cat.Mappings.Title
	}

	numbers := cat.Numbers(category)
	if len(numbers) == 0 {
		return fmt.Errorf("no %s episodes available", category)
	}

	ep, err := chooseEpisode(numbers, startEp)
	if err != nil {
		return err
	}

	if flagDownload {
		return fetch(ctx, client, hc, cat, title, ep, category, pinned, cfg.DownloadDir, cfg.Quality)
	}
	return watch(ctx, client, hc, store, cat, anilistID, title, numbers, ep, category, pinned, cfg)
}

func findAnime(ctx context.Context, hc *http.Client, args []string) (int, string, error) {
	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" {
		q, err := ui.Prompt("Search anime")
		if err != nil {
			return 0, "", err
		}
		query = q
	}
	if query == "" {
		return 0, "", errors.New("empty query")
	}

	media, err := anilist.Search(ctx, hc, query)
	if err != nil {
		return 0, "", err
	}
	if len(media) == 0 {
		return 0, "", fmt.Errorf("no results for %q", query)
	}
	m, err := ui.Select("Select anime", media, func(x anilist.Media) string {
		if x.Episodes > 0 {
			return fmt.Sprintf("%s (%d eps)", x.Title(), x.Episodes)
		}
		return x.Title()
	})
	if err != nil {
		return 0, "", err
	}
	return m.ID, m.Title(), nil
}

func resume(store *history.Store) (int, string, catalog.Category, string, float64, error) {
	entries, err := store.Load()
	if err != nil {
		return 0, "", "", "", 0, err
	}
	if len(entries) == 0 {
		return 0, "", "", "", 0, errors.New("no history yet")
	}
	e, err := ui.Select("Resume", entries, func(x history.Entry) string {
		return fmt.Sprintf("%s  ep %s  [%s %s]", x.Title, num(x.Episode), x.Provider, x.Category)
	})
	if err != nil {
		return 0, "", "", "", 0, err
	}
	return e.AnilistID, e.Title, e.Category, e.Provider, e.Episode, nil
}

func chooseEpisode(numbers []float64, start float64) (float64, error) {
	if flagEpisode != "" {
		ep, err := parseEpisode(flagEpisode)
		if err != nil {
			return 0, err
		}
		if contains(numbers, ep) {
			return ep, nil
		}
		return 0, fmt.Errorf("episode %s not available", num(ep))
	}
	if start >= 0 && contains(numbers, start) {
		return start, nil
	}
	return ui.Select("Select episode", numbers, num)
}

func fetch(ctx context.Context, c *pipe.Client, hc *http.Client, cat *catalog.Catalog, title string, ep float64, category catalog.Category, pinned, dir, quality string) error {
	res, prov, err := resolve(ctx, c, cat, ep, category, pinned)
	if err != nil {
		return err
	}
	stream, err := sources.Select(ctx, hc, res, quality)
	if err != nil {
		return err
	}
	log.Info("downloading", "title", title, "ep", num(ep), "provider", prov, "softsub", res.Softsub())
	name := fmt.Sprintf("%s - E%s", title, num(ep))
	if err := download.Fetch(ctx, hc, stream, res.Subtitles, dir, name); err != nil {
		return err
	}
	log.Info("saved", "dir", dir)
	return nil
}

func watch(ctx context.Context, c *pipe.Client, hc *http.Client, store *history.Store, cat *catalog.Catalog, anilistID int, title string, numbers []float64, ep float64, category catalog.Category, pinned string, cfg config) error {
	p := detectPlayer(cfg)
	for {
		res, prov, err := resolve(ctx, c, cat, ep, category, pinned)
		if err != nil {
			return err
		}
		pinned = prov

		stream, err := sources.Select(ctx, hc, res, cfg.Quality)
		if err != nil {
			return err
		}

		log.Info("playing", "title", title, "ep", num(ep), "provider", prov, "player", p.Kind, "softsub", res.Softsub())
		if err := p.Play(ctx, stream, res.Subtitles, fmt.Sprintf("%s Episode %s", title, num(ep))); err != nil {
			return err
		}
		_ = store.Save(history.Entry{AnilistID: anilistID, Title: title, Provider: prov, Category: category, Episode: ep})

		next, stop, err := control(numbers, ep, title)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
		if next.reprovide {
			pinned = ""
		}
		if next.reselect {
			ep, err = ui.Select("Select episode", numbers, num)
			if err != nil {
				return err
			}
			continue
		}
		ep = next.ep
	}
}

type step struct {
	ep        float64
	reprovide bool
	reselect  bool
}

func control(numbers []float64, ep float64, title string) (step, bool, error) {
	next, hasNext := neighbor(numbers, ep, +1)
	prev, hasPrev := neighbor(numbers, ep, -1)

	var actions []string
	if hasNext {
		actions = append(actions, "next")
	}
	actions = append(actions, "replay")
	if hasPrev {
		actions = append(actions, "previous")
	}
	actions = append(actions, "select", "change provider", "quit")

	choice, err := ui.Menu(fmt.Sprintf("Episode %s of %s", num(ep), title), actions)
	if err != nil {
		if errors.Is(err, ui.ErrAborted) {
			return step{}, true, nil
		}
		return step{}, false, err
	}

	switch choice {
	case "next":
		return step{ep: next}, false, nil
	case "previous":
		return step{ep: prev}, false, nil
	case "replay":
		return step{ep: ep}, false, nil
	case "select":
		return step{reselect: true}, false, nil
	case "change provider":
		return step{ep: ep, reprovide: true}, false, nil
	default:
		return step{}, true, nil
	}
}

func resolve(ctx context.Context, c *pipe.Client, cat *catalog.Catalog, ep float64, category catalog.Category, pinned string) (*sources.Result, string, error) {
	avail := cat.Available(ep, category)
	if len(avail) == 0 {
		return nil, "", fmt.Errorf("no provider has episode %s", num(ep))
	}

	if pinned == "" {
		p, err := ui.Select("Select provider", avail, func(x catalog.Provider) string { return x.Code })
		if err != nil {
			return nil, "", err
		}
		pinned = p.Code
	}

	var last error
	for _, p := range orderPinned(avail, pinned) {
		e := find(p.Episodes(category), ep)
		if e == nil {
			continue
		}
		res, err := sources.Resolve(ctx, c, e.ID, p.Code, category)
		if err != nil {
			if errors.Is(err, pipe.ErrBlocked) {
				return nil, "", err
			}
			last = err
			log.Warn("provider unavailable, trying next", "provider", p.Code)
			continue
		}
		if len(res.Streams) == 0 {
			last = fmt.Errorf("%s returned no streams", p.Code)
			continue
		}
		return res, p.Code, nil
	}
	if last == nil {
		last = fmt.Errorf("no source resolved for episode %s", num(ep))
	}
	return nil, "", last
}

func detectPlayer(cfg config) player.Player {
	prefer := player.Kind(cfg.Player)
	if flagVLC {
		prefer = player.VLC
	}
	return player.Detect(prefer)
}

func orderPinned(providers []catalog.Provider, code string) []catalog.Provider {
	out := make([]catalog.Provider, 0, len(providers))
	for _, p := range providers {
		if p.Code == code {
			out = append(out, p)
		}
	}
	for _, p := range providers {
		if p.Code != code {
			out = append(out, p)
		}
	}
	return out
}

func find(eps []catalog.Episode, n float64) *catalog.Episode {
	for i := range eps {
		if eps[i].Number == n {
			return &eps[i]
		}
	}
	return nil
}

func neighbor(numbers []float64, ep float64, dir int) (float64, bool) {
	for i, n := range numbers {
		if n == ep {
			j := i + dir
			if j >= 0 && j < len(numbers) {
				return numbers[j], true
			}
			return 0, false
		}
	}
	return 0, false
}

func parseEpisode(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '-'); i > 0 {
		s = s[:i]
	}
	return strconv.ParseFloat(s, 64)
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func contains(xs []float64, x float64) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
