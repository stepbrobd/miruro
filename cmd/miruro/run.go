package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"ysun.co/miruro"
	"ysun.co/miruro/play"
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

	st, err := openStore()
	if err != nil {
		return err
	}
	if flagDelete {
		return st.clear()
	}

	client := miruro.New()

	category := miruro.Sub
	if cfg.Dub {
		category = miruro.Dub
	}

	var anilistID int
	var title string
	startEp := -1.0
	pinned := cfg.Provider

	if flagContinue {
		id, t, cat, prov, ep, err := resume(st)
		if err != nil {
			return err
		}
		anilistID, title, category, startEp = id, t, cat, ep
		if prov != "" {
			pinned = prov
		}
	} else {
		id, t, err := findAnime(ctx, client, args)
		if err != nil {
			return err
		}
		anilistID, title = id, t
	}

	cat, err := client.Episodes(ctx, anilistID)
	if err != nil {
		return err
	}
	if cat.Title != "" {
		title = cat.Title
	}

	numbers := cat.Numbers(category)
	if len(numbers) == 0 {
		return fmt.Errorf("no %s episodes available", category)
	}

	eps, err := chooseEpisodes(numbers, startEp)
	if err != nil {
		return err
	}

	if flagDownload {
		return downloadEpisodes(ctx, client, cat, title, eps, category, pinned, cfg)
	}
	return watch(ctx, client, st, cat, anilistID, title, numbers, eps, category, pinned, cfg)
}

func findAnime(ctx context.Context, client *miruro.Client, args []string) (int, string, error) {
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

	media, err := client.Search(ctx, query)
	if err != nil {
		return 0, "", err
	}
	if len(media) == 0 {
		return 0, "", fmt.Errorf("no results for %q", query)
	}
	m, err := ui.Select("Select anime", media, func(x miruro.Media) string {
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

func resume(st *store) (int, string, miruro.Category, string, float64, error) {
	entries, err := st.load()
	if err != nil {
		return 0, "", "", "", 0, err
	}
	if len(entries) == 0 {
		return 0, "", "", "", 0, errors.New("no history yet")
	}
	e, err := ui.Select("Resume", entries, func(x entry) string {
		return fmt.Sprintf("%s  ep %s  [%s %s]", x.Title, num(x.Episode), x.Provider, x.Category)
	})
	if err != nil {
		return 0, "", "", "", 0, err
	}
	return e.AnilistID, e.Title, e.Category, e.Provider, e.Episode, nil
}

func chooseEpisodes(numbers []float64, start float64) ([]float64, error) {
	if flagAll {
		return numbers, nil
	}
	if flagEpisode != "" {
		return parseEpisodes(flagEpisode, numbers)
	}
	if start >= 0 && contains(numbers, start) {
		return []float64{start}, nil
	}
	ep, err := ui.Select("Select episode", numbers, num)
	if err != nil {
		return nil, err
	}
	return []float64{ep}, nil
}

func downloadEpisodes(ctx context.Context, client *miruro.Client, cat *miruro.Catalog, title string, eps []float64, category miruro.Category, pinned string, cfg config) error {
	code, pinVariant := splitPin(pinned)
	px, err := play.StartProxy(ctx)
	if err != nil {
		return err
	}
	defer px.Close()

	// client.HTTP's deadline spans the body read and truncates long episodes.
	// a stalled cdn is bounded by the header timeout rather than a whole-request one
	hc := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}

	labels := make([]string, len(eps))
	for i, ep := range eps {
		labels[i] = "E" + num(ep)
	}

	errs := ui.Downloads(labels, flagParallel, func(i int, report func(done, total int64)) error {
		ep := eps[i]
		res, served, err := autoResolve(ctx, client, cat, ep, category, code)
		if err != nil {
			return err
		}
		stream, err := client.Select(ctx, res, cfg.Quality)
		if err != nil {
			return err
		}
		// As in resolve: the pin's variant describes the pinned provider only.
		variant := pinVariant
		if served != code {
			variant = "soft"
		}
		subs := res.Subtitles
		if variant == "hard" {
			subs = nil
		}
		name := fmt.Sprintf("%s - E%s", title, num(ep))
		return play.Download(ctx, hc, px.Stream(stream), proxySubs(px, subs, stream.Referer), cfg.DownloadDir, name, report)
	})

	var failed int
	for _, err := range errs {
		if err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d downloads failed", failed, len(eps))
	}
	log.Info("saved", "dir", cfg.DownloadDir, "episodes", len(eps))
	return nil
}

func watch(ctx context.Context, client *miruro.Client, st *store, cat *miruro.Catalog, anilistID int, title string, numbers, queue []float64, category miruro.Category, pinned string, cfg config) error {
	player := detectPlayer(cfg)
	px, err := play.StartProxy(ctx)
	if err != nil {
		return err
	}
	defer px.Close()

	ep := queue[0]
	queue = queue[1:]

	for {
		res, prov, variant, err := resolve(ctx, client, cat, ep, category, pinned)
		if err != nil {
			return err
		}
		pinned = prov + ":" + variant

		stream, err := client.Select(ctx, res, cfg.Quality)
		if err != nil {
			return err
		}

		var skips []miruro.SkipRange
		if flagSkip {
			skips = episodeSkips(cat, ep)
		}

		subs := res.Subtitles
		if variant == "hard" {
			subs = nil
		}

		log.Info("playing", "title", title, "ep", num(ep), "provider", prov, "variant", variant, "player", player.Kind, "subs", len(subs) > 0)
		err = player.Play(ctx, px.Stream(stream), proxySubs(px, subs, stream.Referer), skips, fmt.Sprintf("%s Episode %s", title, num(ep)))
		switch {
		case err == nil:
			_ = st.save(entry{AnilistID: anilistID, Title: title, Provider: pinned, Category: category, Episode: ep})
		case ctx.Err() != nil:
			// exec.CommandContext reports a context kill as an ExitError, so this
			// guard has to come before the ExitError check below.
			return err
		case !errors.As(err, new(*exec.ExitError)):
			return err
		default:
			// The player ran and failed: an unplayable stream, a proxy 502, or the
			// user quitting mpv with a nonzero status. Fall through to the control
			// menu, which offers changing provider, instead of ending the session.
			log.Warn("player exited", "err", err)
		}

		if len(queue) > 0 {
			ep = queue[0]
			queue = queue[1:]
			continue
		}

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

// resolve prompts for a provider when none is pinned, then resolves with
// fallback. It returns the served provider code and the subtitle variant
// ("soft" attaches the subtitle file, "hard" plays the video as delivered).
// A pinned provider carries its variant as "code:variant"; the interactive
// picker asks only when the source actually ships a subtitle file.
//
// The picker shows codes at once and fills each label as its background probe
// returns, so the choice is informed without a wait up front. A probe only
// knows whether a .vtt exists, so it says so plainly and leaves the soft/hard
// vocabulary to the follow-up prompt, which is the only place the user's
// attach/don't-attach decision is made.
func resolve(ctx context.Context, client *miruro.Client, cat *miruro.Catalog, ep float64, category miruro.Category, pinned string) (*miruro.Result, string, string, error) {
	if pinned != "" {
		code, variant := splitPin(pinned)
		res, served, err := autoResolve(ctx, client, cat, ep, category, code)
		if err != nil {
			return nil, "", "", err
		}
		// The pin's variant describes the pinned provider. When fallback served a
		// different one, that judgement does not transfer: drop to the soft
		// default rather than suppressing a legitimate subtitle file.
		if served != code {
			variant = "soft"
		}
		return res, served, variant, nil
	}

	avail := cat.Available(ep, category)
	if len(avail) == 0 {
		return nil, "", "", fmt.Errorf("no provider has episode %s", num(ep))
	}

	codes := make([]string, len(avail))
	for i, p := range avail {
		codes[i] = p.Code
	}

	probe := func(i int) (string, bool) {
		p := avail[i]
		e := find(p.Episodes(category), ep)
		if e == nil {
			return "unavailable", false
		}
		pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		res, err := client.Sources(pctx, e.ID, p.Code, category)
		if err != nil || len(res.Streams) == 0 {
			return "unavailable", false
		}
		has := "no subs"
		if res.Softsub() {
			has = "subs"
		}
		return fmt.Sprintf("%s %s", category, has), true
	}

	idx, err := ui.PickProvider("Select provider", codes, probe)
	if err != nil {
		return nil, "", "", err
	}
	res, served, err := autoResolve(ctx, client, cat, ep, category, avail[idx].Code)
	if err != nil {
		return nil, "", "", err
	}

	variant := "soft"
	if len(res.Subtitles) > 0 {
		variant, err = ui.Select("Subtitles", []string{"soft", "hard"}, func(v string) string {
			if v == "soft" {
				return "soft, attach subtitle file"
			}
			return "hard, subtitles already in the picture"
		})
		if err != nil {
			return nil, "", "", err
		}
	}
	return res, served, variant, nil
}

// autoResolve tries the pinned provider first then the rest, never prompting.
func autoResolve(ctx context.Context, client *miruro.Client, cat *miruro.Catalog, ep float64, category miruro.Category, pinned string) (*miruro.Result, string, error) {
	avail := cat.Available(ep, category)
	if len(avail) == 0 {
		return nil, "", fmt.Errorf("no provider has episode %s", num(ep))
	}

	var last error
	for _, p := range orderPinned(avail, pinned) {
		e := find(p.Episodes(category), ep)
		if e == nil {
			continue
		}
		res, err := client.Sources(ctx, e.ID, p.Code, category)
		if err != nil {
			if errors.Is(err, miruro.ErrBlocked) {
				return nil, "", err
			}
			last = err
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

func episodeSkips(cat *miruro.Catalog, ep float64) []miruro.SkipRange {
	var out []miruro.SkipRange
	for _, s := range cat.Aniskip {
		if s.Episode == ep {
			out = append(out, s)
		}
	}
	return out
}

func detectPlayer(cfg config) play.Player {
	return play.Detect(play.Kind(cfg.Player))
}

// proxySubs routes sidecar subtitle fetches through the proxy so CDN gating is
// handled the same way it is for video. Subtitles carry no referer of their own,
// so they inherit the video stream's.
func proxySubs(px *play.Proxy, subs []miruro.Subtitle, referer string) []miruro.Subtitle {
	out := make([]miruro.Subtitle, len(subs))
	for i, s := range subs {
		out[i] = miruro.Subtitle{File: px.Opaque(s.File, referer), Label: s.Label}
	}
	return out
}

func orderPinned(providers []miruro.Provider, code string) []miruro.Provider {
	out := make([]miruro.Provider, 0, len(providers))
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

func find(eps []miruro.Episode, n float64) *miruro.Episode {
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

func parseEpisodes(spec string, numbers []float64) ([]float64, error) {
	spec = strings.TrimSpace(spec)
	if i := strings.IndexByte(spec, '-'); i > 0 {
		lo, err1 := strconv.ParseFloat(strings.TrimSpace(spec[:i]), 64)
		hi, err2 := strconv.ParseFloat(strings.TrimSpace(spec[i+1:]), 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("invalid range %q", spec)
		}
		var out []float64
		for _, n := range numbers {
			if n >= lo && n <= hi {
				out = append(out, n)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no episodes in range %s", spec)
		}
		return out, nil
	}
	n, err := strconv.ParseFloat(spec, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid episode %q", spec)
	}
	if !contains(numbers, n) {
		return nil, fmt.Errorf("episode %s not available", num(n))
	}
	return []float64{n}, nil
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
