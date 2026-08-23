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
	"sync/atomic"
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
	if flagLang != "" {
		cfg.Lang = flagLang
	}
	if flagDub {
		cfg.Dub = true
	}

	st, err := openStore()
	if err != nil {
		return err
	}

	// watching needs a player, so a missing one fails before any prompt
	var player play.Player
	if !flagDownload {
		if player, err = play.Detect(play.Kind(cfg.Player)); err != nil {
			return err
		}
	}

	client := miruro.New()
	if len(cfg.Mirrors) > 0 {
		client.Bases = cfg.Mirrors
	}

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
		anilistID, title, startEp = e.AnilistID, e.Title, e.Episode
		// an explicit flag overrides what the entry saved, so a sub run can be
		// corrected with --dub and a saved bonk:soft with --provider bonk:hard
		if !flagDub {
			category = e.Category
		}
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

	eps, err := chooseEpisodes(numbers, startEp, episodeLabel(cat.Details(category)))
	if err != nil {
		return err
	}

	// the capability table decides which rendition each provider is asked for and
	// which providers play only in an iframe
	// a run without it asks every provider for the plain category and offers the
	// embeds it would otherwise drop, so the table is a correction rather than a
	// requirement
	caps, err := client.Config(ctx)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		log.Warn("provider capabilities unavailable, renditions and embeds are unfiltered", "err", err)
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
	if flagDownload && flagSkip {
		log.Warn("--skip applies only to playback")
	}

	if flagDownload {
		return downloadEpisodes(ctx, client, cat, anilistID, title, eps, category, pin, caps, cfg)
	}
	return watch(ctx, client, st, cat, anilistID, title, numbers, eps, category, pin, caps, cfg, player)
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
	m, err := ui.Select("Select anime", media, mediaLabel)
	if err != nil {
		return 0, "", err
	}
	return m.ID, m.Title(), nil
}

// formatNames maps AniList's format enum to display names
var formatNames = map[string]string{
	"TV":       "TV",
	"TV_SHORT": "TV Short",
	"MOVIE":    "Movie",
	"SPECIAL":  "Special",
	"OVA":      "OVA",
	"ONA":      "ONA",
	"MUSIC":    "Music",
}

// mediaLabel renders one search hit with what tells same-titled media apart,
// the AniList format and the episode count
func mediaLabel(x miruro.Media) string {
	var meta []string
	if x.Format != "" {
		name, ok := formatNames[x.Format]
		if !ok {
			name = x.Format
		}
		meta = append(meta, name)
	}
	if x.Episodes > 0 {
		meta = append(meta, fmt.Sprintf("%d eps", x.Episodes))
	}
	if len(meta) == 0 {
		return x.Title()
	}
	return fmt.Sprintf("%s (%s)", x.Title(), strings.Join(meta, ", "))
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

// episodeLabel renders one picker row, the number plus what the catalog knows
// that tells episodes apart
// a number the catalog does not detail reads as the bare number
func episodeLabel(details map[float64]miruro.Episode) func(float64) string {
	return func(n float64) string {
		d := details[n]
		out := num(n)
		if d.Title != "" {
			out += "  " + d.Title
		}
		if d.Filler {
			out += "  (filler)"
		}
		return out
	}
}

func chooseEpisodes(numbers []float64, start float64, label func(float64) string) ([]float64, error) {
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
	ep, err := ui.Select("Select episode", numbers, label)
	if err != nil {
		return nil, err
	}
	return []float64{ep}, nil
}

func downloadEpisodes(ctx context.Context, client *miruro.Client, cat *miruro.Catalog, anilistID int, title string, eps []float64, category miruro.Category, pin Pin, caps miruro.Capabilities, cfg config) error {
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

	// workers run concurrently, so the tally of episodes that lost their
	// subtitles is shared state
	var bare atomic.Int64

	sv := saver{client: client, px: px, hc: hc, cat: cat, id: anilistID, title: title, category: category, pin: pin, caps: caps, cfg: cfg}

	errs := ui.Downloads(ctx, labels, flagParallel, func(dctx context.Context, i int, report func(done, total int64)) error {
		missed, err := sv.save(dctx, eps[i], report)
		if err != nil {
			return err
		}
		if missed > 0 {
			bare.Add(1)
		}
		return nil
	})

	var failed, cancelled int
	for i, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ui.ErrCancelled):
			cancelled++
		default:
			failed++
			// the TUI shows each failure on its task row, but a piped or scripted
			// run draws no rows and would otherwise report only a count
			log.Error("download failed", "episode", labels[i], "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d downloads failed", failed, len(eps))
	}
	if cancelled > 0 {
		// map an interrupt onto the same silent 130 exit every other abort takes
		return context.Canceled
	}
	// warn rather than log so a soft-subbed run that lost its sidecars is visible
	// without --verbose
	if n := bare.Load(); n > 0 {
		log.Warn("episodes saved without subtitles", "count", n)
	}
	log.Info("saved", "dir", cfg.DownloadDir, "episodes", len(eps))
	return nil
}

// saver holds what every episode of one download run shares
type saver struct {
	client   *miruro.Client
	px       *play.Proxy
	hc       *http.Client
	cat      *miruro.Catalog
	id       int
	title    string
	category miruro.Category
	pin      Pin
	caps     miruro.Capabilities
	cfg      config
}

// save writes one episode, dropping to the next provider when a download fails
// after its own retries
// a provider that resolves and then dies mid-episode is common enough that
// failing the episode over it would waste the rest of the run
// it reports the sidecars that were lost, the way play.Download does
func (s saver) save(ctx context.Context, ep float64, report play.Progress) (int, error) {
	tried := map[string]bool{}
	var last error
	for {
		res, src, err := autoResolve(ctx, s.client, s.cat, ep, s.category, s.pin, tried, s.caps)
		if err != nil {
			if last != nil {
				return 0, last
			}
			return 0, err
		}
		tried[src.Code] = true

		// one provider serves an episode from several hosts, so a dead default
		// stream is not a dead provider
		for _, stream := range s.client.Rank(ctx, res, s.cfg.Quality) {
			missed, err := s.from(ctx, res, src, stream, ep, report)
			if err == nil {
				return missed, nil
			}
			if ctx.Err() != nil {
				return 0, err
			}
			// name the provider, since the next attempt reports its own failure
			last = fmt.Errorf("%s: %w", src.Code, err)
			log.Warn("download failed, trying the next stream", "episode", num(ep), "provider", src.Code, "err", err)
		}
	}
}

// from downloads one episode from one stream of the source that served it
// the cache is keyed by the rendition asked for, since sub and ssub are
// different cuts of the episode and must not share a segment directory
func (s saver) from(ctx context.Context, res *miruro.Result, src source, stream miruro.Stream, ep float64, report play.Progress) (int, error) {
	subs := miruro.Order(res.Subtitles, s.cfg.Lang)
	if !src.Attach {
		subs = nil
	}
	name := fmt.Sprintf("%s - E%s", s.title, num(ep))
	cache := cacheDir(s.id, ep, src.Category, src.Code, s.cfg.Quality)
	return play.Download(ctx, s.hc, s.px.Stream(stream), s.px.Subtitles(subs, stream.Referer), s.cfg.DownloadDir, name, cache, report)
}

func watch(ctx context.Context, client *miruro.Client, st *store, cat *miruro.Catalog, anilistID int, title string, numbers, queue []float64, category miruro.Category, pin Pin, caps miruro.Capabilities, cfg config, player play.Player) error {
	px, err := play.StartProxy(ctx)
	if err != nil {
		return err
	}
	defer px.Close()

	details := cat.Details(category)
	ep := queue[0]
	queue = queue[1:]

	for {
		res, src, carry, err := resolve(ctx, client, cat, ep, category, pin, caps)
		if err != nil {
			return err
		}
		// carry the user's intent across episodes
		// a transient fallback serves another provider but must not overwrite the pin
		pin = carry

		ranked := client.Rank(ctx, res, cfg.Quality)
		if len(ranked) == 0 {
			return miruro.ErrNoStream
		}

		var skips []miruro.SkipRange
		if flagSkip {
			skips = episodeSkips(cat, ep)
		}

		subs := miruro.Order(res.Subtitles, cfg.Lang)
		if !src.Attach {
			subs = nil
		}

		log.Info("playing", "title", title, "ep", num(ep), "provider", src.Code, "server", ranked[0].Server, "rendition", src.Category, "player", player.Kind, "subs", len(subs) > 0)

		mediaTitle := fmt.Sprintf("%s Episode %s", title, num(ep))
		if d := details[ep]; d.Title != "" {
			mediaTitle += " - " + d.Title
		}
		launch := func(pctx context.Context, s miruro.Stream) error {
			return player.Play(pctx, px.Stream(s), px.Subtitles(subs, s.Referer), skips, mediaTitle)
		}

		e := entry{AnilistID: anilistID, Title: title, Provider: pin.String(), Category: category, Episode: ep}
		action, err := playAndControl(ctx,
			fmt.Sprintf("Episode %s of %s", num(ep), title),
			controls(numbers, ep),
			len(queue) > 0,
			func(pctx context.Context) error { return playStreams(pctx, px, ranked, launch) },
			func() error { return st.save(e) },
		)
		if err != nil {
			return err
		}

		if action == "" {
			// the menu only dismisses itself mid-batch, so the queue is not empty
			ep = queue[0]
			queue = queue[1:]
			continue
		}

		next, quit := apply(action, numbers, ep)
		if quit {
			return nil
		}
		if next.reprovide {
			pin = Pin{}
		}
		if next.reselect {
			ep, err = ui.Select("Select episode", numbers, episodeLabel(details))
			if err != nil {
				return err
			}
		} else {
			ep = next.ep
		}
		queue = ahead(queue, ep)
	}
}

// playStreams hands each stream in turn to play until one of them plays
// a provider serving an episode from several hosts is not dead when the first
// of them is, and the action menu stays raised throughout because this runs
// inside the playback goroutine
func playStreams(ctx context.Context, px *play.Proxy, ranked []miruro.Stream, play func(context.Context, miruro.Stream) error) error {
	var err error
	for _, s := range ranked {
		before := px.Served()
		err = play(ctx, s)
		if !deadStream(err, before, px.Served()) || ctx.Err() != nil {
			return err
		}
		log.Warn("stream did not play, trying the next", "server", s.Server, "err", err)
	}
	return err
}

// deadStream reports whether a finished playback is worth retrying on another
// stream
// a player that exits with an error before the proxy relayed a single media
// body never started, which is what tells a dead stream from one the user quit
// a few seconds in
func deadStream(err error, before, after int) bool {
	return err != nil && after == before
}

// playAndControl runs one playback with the action menu raised over it and
// joins both before returning the picked action, "" on a dismissal
// the menu is up while the player runs, so a pick races playback ending
// an early pick interrupts the player, a clean end mid-batch dismisses the
// menu to auto-advance
func playAndControl(ctx context.Context, title string, actions []string, batch bool, run func(context.Context) error, save func() error) (string, error) {
	pctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	var perr, werr error
	go func() {
		perr = run(pctx)
		// save the moment playback ends cleanly rather than when the menu
		// closes, so killing an idle menu cannot lose the watched entry
		if perr == nil {
			werr = save()
		}
		close(done)
	}()

	wait := func() bool {
		<-done
		return outcome(perr, batch)
	}
	action, ended, err := ui.Control(ctx, title, actions, wait)
	stop()
	<-done
	if err != nil {
		return "", err
	}

	switch {
	case action == "" && perr != nil:
		// outcome dismisses a failed playback only when the player never ran,
		// which no menu action can fix
		return "", perr
	case perr != nil && ended:
		// keep the failure in scrollback after the menu clears
		log.Warn("player exited", "err", perr)
	case perr != nil:
		// an early pick interrupts the player and still counts as watched
		werr = save()
	}
	if werr != nil {
		log.Warn("history not saved", "err", werr)
	}
	return action, nil
}

// outcome reports whether the menu dismisses itself when playback stops
// a clean end mid-batch advances, a failure keeps the menu up so a broken
// provider does not burn the range, and a player that never ran is fatal
func outcome(err error, batch bool) bool {
	switch {
	case err == nil:
		return batch
	case errors.As(err, new(*exec.ExitError)):
		return false
	default:
		return true
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

type step struct {
	ep        float64
	reprovide bool
	reselect  bool
}

func controls(numbers []float64, ep float64) []string {
	_, hasNext := neighbor(numbers, ep, +1)
	_, hasPrev := neighbor(numbers, ep, -1)

	var actions []string
	if hasNext {
		actions = append(actions, "next")
	}
	actions = append(actions, "replay")
	if hasPrev {
		actions = append(actions, "previous")
	}
	return append(actions, "select", "change provider", "quit")
}

// apply maps a menu action to the next step, quit included
func apply(action string, numbers []float64, ep float64) (step, bool) {
	switch action {
	case "next":
		n, _ := neighbor(numbers, ep, +1)
		return step{ep: n}, false
	case "previous":
		p, _ := neighbor(numbers, ep, -1)
		return step{ep: p}, false
	case "replay":
		return step{ep: ep}, false
	case "select":
		return step{reselect: true}, false
	case "change provider":
		return step{ep: ep, reprovide: true}, false
	default:
		return step{}, true
	}
}

// resolve resolves an episode and returns the source that served it and the pin
// to carry forward
// with a pinned provider it resolves with fallback and carries the pin unchanged
// with no pin it asks once, for a provider and its subtitle rendition together,
// and the pick is the pin whether or not that provider ends up serving
func resolve(ctx context.Context, client *miruro.Client, cat *miruro.Catalog, ep float64, category miruro.Category, pin Pin, caps miruro.Capabilities) (*miruro.Result, source, Pin, error) {
	if pin.Code != "" {
		res, src, err := autoResolve(ctx, client, cat, ep, category, pin, nil, caps)
		return res, src, pin, err
	}

	avail, err := candidates(cat, ep, category, caps)
	if err != nil {
		return nil, source{}, pin, err
	}

	rows := offers(avail, caps, category, pin)
	width := widest(rows)
	pick, err := ui.Select("Select provider", rows, func(o offer) string { return o.label(width) })
	if err != nil {
		return nil, source{}, pin, err
	}
	res, src, err := autoResolve(ctx, client, cat, ep, category, pick.Pin, nil, caps)
	return res, src, pick.Pin, err
}

// autoResolve tries the pinned pick first then the rest, never prompting
// skip names providers a caller has already used, so an episode being retried
// moves on instead of resolving the same dead source again
func autoResolve(ctx context.Context, client *miruro.Client, cat *miruro.Catalog, ep float64, category miruro.Category, pin Pin, skip map[string]bool, caps miruro.Capabilities) (*miruro.Result, source, error) {
	avail, err := candidates(cat, ep, category, caps)
	if err != nil {
		return nil, source{}, err
	}

	var last error
	for _, o := range orderPinned(offers(avail, caps, category, pin), pin) {
		if skip[o.Code] {
			continue
		}
		src := o.source(category)
		e := find(cat.Providers[o.Code].Episodes(src.Category), ep)
		if e == nil {
			continue
		}
		res, err := client.Sources(ctx, e.ID, o.Code, src.Category)
		if err != nil {
			if errors.Is(err, miruro.ErrBlocked) {
				return nil, source{}, err
			}
			if ctx.Err() != nil {
				return nil, source{}, ctx.Err()
			}
			// name the provider so a report points at the one that failed
			last = fmt.Errorf("%s: %w", o.Code, err)
			continue
		}
		// an embed-only result is not playable, so skip it inside the loop rather
		// than fail later at Select outside it
		if !res.Playable() {
			last = fmt.Errorf("%s has no playable stream", o.Code)
			continue
		}
		return res, src, nil
	}
	if last == nil {
		last = fmt.Errorf("no source resolved for episode %s", num(ep))
	}
	return nil, source{}, last
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
