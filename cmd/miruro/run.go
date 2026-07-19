package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
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
		if len(args) > 0 {
			log.Warn("--continue ignores the query")
		}
		e, err := resume(st)
		if err != nil {
			return err
		}
		anilistID, title, category, startEp = e.AnilistID, e.Title, e.Category, e.Episode
		// an explicit --provider overrides the resumed pin
		// so a saved bonk:soft can be corrected with --provider bonk:hard
		if e.Provider != "" && flagProvider == "" {
			pinned = e.Provider
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

	pin := ParsePin(pinned)
	if pin.Code != "" {
		if _, ok := cat.Providers[pin.Code]; !ok {
			log.Warn("pinned provider not in catalog, using fallback order", "provider", pin.Code)
		}
	}
	if !flagDownload && flagParallel > 1 {
		log.Warn("--parallel applies only with --download")
	}

	if flagDownload {
		return downloadEpisodes(ctx, client, cat, title, eps, category, pin, cfg)
	}
	return watch(ctx, client, st, cat, anilistID, title, numbers, eps, category, pin, cfg)
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

func resume(st *store) (entry, error) {
	entries, err := st.load()
	if err != nil {
		return entry{}, err
	}
	if len(entries) == 0 {
		return entry{}, errors.New("no history yet")
	}
	return ui.Select("Resume", entries, func(x entry) string {
		return fmt.Sprintf("%s  ep %s  [%s %s]", x.Title, num(x.Episode), x.Provider, x.Category)
	})
}

func chooseEpisodes(numbers []float64, start float64) ([]float64, error) {
	if flagAll {
		if flagEpisode != "" {
			log.Warn("--all overrides --episode")
		}
		return numbers, nil
	}
	if flagEpisode != "" {
		return parseEpisodes(flagEpisode, numbers)
	}
	if start >= 0 && slices.Contains(numbers, start) {
		return []float64{start}, nil
	}
	ep, err := ui.Select("Select episode", numbers, num)
	if err != nil {
		return nil, err
	}
	return []float64{ep}, nil
}

func downloadEpisodes(ctx context.Context, client *miruro.Client, cat *miruro.Catalog, title string, eps []float64, category miruro.Category, pin Pin, cfg config) error {
	px, err := play.StartProxy(ctx)
	if err != nil {
		return err
	}
	defer px.Close()

	// bound only the header wait so a slow episode is not truncated mid-body
	hc := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}

	labels := make([]string, len(eps))
	for i, ep := range eps {
		labels[i] = "E" + num(ep)
	}

	errs := ui.Downloads(ctx, labels, flagParallel, func(dctx context.Context, i int, report func(done, total int64)) error {
		ep := eps[i]
		res, served, err := autoResolve(dctx, client, cat, ep, category, pin.Code)
		if err != nil {
			return err
		}
		stream, err := client.Select(dctx, res, cfg.Quality)
		if err != nil {
			return err
		}
		subs := res.Subtitles
		if applied(pin, served) == Hard {
			subs = nil
		}
		name := fmt.Sprintf("%s - E%s", title, num(ep))
		return play.Download(dctx, hc, px.Stream(stream), proxySubs(px, subs, stream.Referer), cfg.DownloadDir, name, report)
	})

	var failed int
	for i, err := range errs {
		if err != nil {
			failed++
			// the TUI shows each failure on its task row, but a piped or scripted
			// run draws no rows and would otherwise report only a count
			log.Error("download failed", "episode", labels[i], "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d downloads failed", failed, len(eps))
	}
	log.Info("saved", "dir", cfg.DownloadDir, "episodes", len(eps))
	return nil
}

func watch(ctx context.Context, client *miruro.Client, st *store, cat *miruro.Catalog, anilistID int, title string, numbers, queue []float64, category miruro.Category, pin Pin, cfg config) error {
	player := detectPlayer(cfg)
	px, err := play.StartProxy(ctx)
	if err != nil {
		return err
	}
	defer px.Close()

	ep := queue[0]
	queue = queue[1:]

	for {
		res, served, carry, err := resolve(ctx, client, cat, ep, category, pin)
		if err != nil {
			return err
		}
		// carry the user's intent across episodes
		// a transient fallback serves another provider but must not overwrite the pin
		pin = carry

		stream, err := client.Select(ctx, res, cfg.Quality)
		if err != nil {
			return err
		}

		var skips []miruro.SkipRange
		if flagSkip {
			skips = episodeSkips(cat, ep)
		}

		subs := res.Subtitles
		variant := applied(pin, served)
		if variant == Hard {
			subs = nil
		}

		played := false
		log.Info("playing", "title", title, "ep", num(ep), "provider", served, "variant", variant, "player", player.Kind, "subs", len(subs) > 0)
		err = player.Play(ctx, px.Stream(stream), proxySubs(px, subs, stream.Referer), skips, fmt.Sprintf("%s Episode %s", title, num(ep)))
		switch {
		case err == nil:
			played = true
			if serr := st.save(entry{AnilistID: anilistID, Title: title, Provider: pin.String(), Category: category, Episode: ep}); serr != nil {
				log.Warn("history not saved", "err", serr)
			}
		case ctx.Err() != nil:
			// exec.CommandContext reports a context kill as an ExitError, so this
			// guard has to come before the ExitError check below
			return ctx.Err()
		case !errors.As(err, new(*exec.ExitError)):
			return err
		default:
			// the player ran and failed on an unplayable stream, a proxy 502, or a
			// nonzero quit
			log.Warn("player exited", "err", err)
		}

		// only a played episode auto-advances
		// a failure drops to the menu so a broken provider does not burn the range
		if played && len(queue) > 0 {
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
			pin = Pin{}
		}
		if next.reselect {
			ep, err = ui.Select("Select episode", numbers, num)
			if err != nil {
				return err
			}
		} else {
			ep = next.ep
		}
		queue = ahead(queue, ep)
	}
}

// ahead re-anchors the batch queue to ep
// the queue holds only the episodes still in front of the current one, so a
// replay or a provider change leaves it whole while a manual jump discards
// whatever it moved past
func ahead(queue []float64, ep float64) []float64 {
	i := slices.IndexFunc(queue, func(q float64) bool { return q > ep })
	if i < 0 {
		return nil
	}
	return queue[i:]
}

// applied is the subtitle variant for one playback
// the pin's variant describes the pinned provider only, so a fallback provider
// resets to soft rather than suppressing its subtitles
func applied(pin Pin, served string) Variant {
	if served != pin.Code {
		return Soft
	}
	return pin.Variant
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

// resolve resolves an episode and returns the provider that served it and the
// pin to carry forward
// with a pinned provider it resolves with fallback and carries the pin unchanged
// with no pin it runs the async picker, then asks soft or hard only when the
// source ships a subtitle file, and carries the pick as the new pin
func resolve(ctx context.Context, client *miruro.Client, cat *miruro.Catalog, ep float64, category miruro.Category, pin Pin) (*miruro.Result, string, Pin, error) {
	if pin.Code != "" {
		res, served, err := autoResolve(ctx, client, cat, ep, category, pin.Code)
		if err != nil {
			return nil, "", pin, err
		}
		return res, served, pin, nil
	}

	avail := cat.Available(ep, category)
	if len(avail) == 0 {
		return nil, "", pin, fmt.Errorf("no provider has episode %s", num(ep))
	}

	codes := make([]string, len(avail))
	for i, p := range avail {
		codes[i] = p.Code
	}

	// the picker shows codes at once and fills each label as its probe returns
	// a provider that ships a .vtt gets a soft or hard follow-up inside the same
	// program, so the choice cannot be skipped by a leaked keypress
	probe := func(i int) (string, bool, bool) {
		p := avail[i]
		e := find(p.Episodes(category), ep)
		if e == nil {
			return "unavailable", false, false
		}
		pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		res, err := client.Sources(pctx, e.ID, p.Code, category)
		if err != nil || !res.Playable() {
			return "unavailable", false, false
		}
		subs := res.Softsub()
		has := "no subs"
		if subs {
			has = "subs"
		}
		return fmt.Sprintf("%s %s", category, has), true, subs
	}

	idx, variant, err := ui.PickProvider("Select provider", codes, probe)
	if err != nil {
		return nil, "", pin, err
	}
	picked := avail[idx].Code
	res, served, err := autoResolve(ctx, client, cat, ep, category, picked)
	if err != nil {
		return nil, "", pin, err
	}
	return res, served, Pin{Code: picked, Variant: Variant(variant)}, nil
}

// autoResolve tries the pinned provider first then the rest, never prompting
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
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			// name the provider so a report points at the one that failed
			last = fmt.Errorf("%s: %w", p.Code, err)
			continue
		}
		// an embed-only result is not playable, so skip it inside the loop rather
		// than fail later at Select outside it
		if !res.Playable() {
			last = fmt.Errorf("%s has no playable stream", p.Code)
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
	if !slices.Contains(numbers, n) {
		return nil, fmt.Errorf("episode %s not available", num(n))
	}
	return []float64{n}, nil
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
