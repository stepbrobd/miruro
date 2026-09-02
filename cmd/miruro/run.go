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
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"ysun.co/miruro"
	"ysun.co/miruro/backend/allanime"
	"ysun.co/miruro/backend/mirurotv"
	"ysun.co/miruro/play"
	"ysun.co/miruro/ui"
)

// runState is the state shared by every episode in one command
type runState struct {
	// hc fetches a master playlist when a quality pick needs one expanded
	hc        *http.Client
	cat       *miruro.Catalog
	anilistID int
	title     string
	category  miruro.Category
	caps      miruro.Capabilities
	cfg       config
	// refused remembers the backends that refused the run
	refused refusals
}

// refusals is the set of backends whose firewall refused this client
// a refusal holds for the process, since every later request would meet the
// same one, and download workers share it, so it is probed once and reported
// once
type refusals struct {
	mu   sync.Mutex
	dead map[string]error
}

// add records a refusal and reports whether it is the first for the backend
func (r *refusals) add(b miruro.Backend, err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dead == nil {
		r.dead = map[string]error{}
	}
	if _, seen := r.dead[b.Name()]; seen {
		return false
	}
	r.dead[b.Name()] = err
	return true
}

// get returns the refusal a backend answered, nil when it answered none
func (r *refusals) get(b miruro.Backend) error {
	if b == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dead[b.Name()]
}

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

	client := mirurotv.New()
	if len(cfg.Mirrors) > 0 {
		client.Bases = cfg.Mirrors
	}
	backends := enabled(all(client), cfg.Backends)

	category := miruro.Sub
	if cfg.Dub {
		category = miruro.Dub
	}

	var media miruro.Media
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
		media, startEp = miruro.Media{ID: e.AnilistID, Romaji: e.Title}, e.Episode
		// an explicit flag overrides what the entry saved, so a sub run can be
		// corrected with --dub and a saved bonk:soft with --provider bonk:hard
		if !flagDub {
			category = e.Category
		}
		if e.Provider != "" && flagProvider == "" {
			pinned = e.Provider
		}
	} else {
		media, err = findAnime(ctx, client, args)
		if err != nil {
			return err
		}
	}

	// a backend that cannot answer costs the run its providers and nothing
	// else, so the failure is reported and the rest of the catalog stands
	cat, failed := backends.Episodes(ctx, media)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, f := range failed {
		log.Warn("backend did not answer", "backend", f.Backend, "err", f.Err)
	}
	if len(cat.Providers) == 0 {
		if len(failed) > 0 {
			return errors.Join(joined(failed)...)
		}
		return fmt.Errorf("no provider carries %s", media.Title())
	}
	title := media.Title()
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
	caps, failed := backends.Capabilities(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, f := range failed {
		log.Warn("provider capabilities unavailable, renditions uncorrected and embeds offered", "backend", f.Backend, "err", f.Err)
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

	state := &runState{
		hc: client.HTTP, cat: cat, anilistID: media.ID, title: title,
		category: category, caps: caps, cfg: cfg,
	}
	if flagDownload {
		return state.download(ctx, eps, pin)
	}
	return state.watch(ctx, st, numbers, eps, pin, player)
}

func findAnime(ctx context.Context, client *mirurotv.Client, args []string) (miruro.Media, error) {
	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" {
		q, err := ui.Prompt("Search anime")
		if err != nil {
			return miruro.Media{}, err
		}
		query = q
	}
	if query == "" {
		return miruro.Media{}, errors.New("empty query")
	}

	media, err := client.Search(ctx, query)
	if err != nil {
		return miruro.Media{}, err
	}
	if len(media) == 0 {
		return miruro.Media{}, fmt.Errorf("no results for %q", query)
	}
	return ui.Select("Select anime", media, mediaLabel)
}

// all is every upstream this build resolves against, in the order the merged
// catalog lists them
// a new backend is one package implementing miruro.Backend and one entry here
func all(client *mirurotv.Client) miruro.Backends {
	return miruro.Backends{client, allanime.New()}
}

// enabled keeps the backends a config names, every one when it names none
// a name nothing implements is warned about, since a typo would otherwise
// read as the backend being down
func enabled(backends miruro.Backends, names []string) miruro.Backends {
	if len(names) == 0 {
		return backends
	}
	var out miruro.Backends
	for _, name := range names {
		i := slices.IndexFunc(backends, func(b miruro.Backend) bool { return b.Name() == name })
		if i < 0 {
			log.Warn("ignoring unknown backend", "backend", name)
			continue
		}
		out = append(out, backends[i])
	}
	if len(out) == 0 {
		log.Warn("no usable backend configured, using every backend")
		return backends
	}
	return out
}

// joined widens backend failures to errors for errors.Join
func joined(failed []miruro.Failure) []error {
	out := make([]error, len(failed))
	for i, f := range failed {
		out[i] = f
	}
	return out
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

func (s *runState) download(ctx context.Context, eps []float64, pin Pin) error {
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
	// worker i alone writes swapped[i], published by Downloads joining them
	swapped := make([]bool, len(eps))

	sv := saver{runState: s, px: px, hc: hc, pin: pin}

	errs := ui.Downloads(ctx, labels, flagParallel, func(dctx context.Context, i int, report func(done, total int64)) error {
		src, missed, err := sv.save(dctx, eps[i], report)
		if err != nil {
			return err
		}
		if missed > 0 {
			bare.Add(1)
		}
		if want, ok := sv.wanted(eps[i]); ok && (src.Category != want.Category || src.Attach != want.Attach) {
			swapped[i] = true
			log.Warn("episode saved with a different rendition than pinned", "episode", labels[i], "provider", src.Pin)
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
	// name the episodes so a re-fetch knows which files to delete first, since a
	// rerun skips whatever is already on disk
	var off []string
	for i, s := range swapped {
		if s {
			off = append(off, labels[i])
		}
	}
	if len(off) > 0 {
		log.Warn("episodes saved with a different rendition than pinned", "episodes", strings.Join(off, " "))
	}
	log.Info("saved", "dir", s.cfg.DownloadDir, "episodes", len(eps))
	return nil
}

// saver holds what every episode of one download run shares
type saver struct {
	*runState
	px  *play.Proxy
	hc  *http.Client
	pin Pin
}

// save writes one episode, dropping to the next provider when a download fails
// after its own retries
// a provider that resolves and then dies mid-episode is common enough that
// failing the episode over it would waste the rest of the run
// it reports the source that served the episode and the sidecars that were
// lost, the way play.Download does
func (s saver) save(ctx context.Context, ep float64, report play.Progress) (source, int, error) {
	tried := map[string]bool{}
	var last error
	for {
		res, src, err := s.autoResolve(ctx, ep, s.pin, tried)
		if err != nil {
			if last != nil {
				return source{}, 0, last
			}
			return source{}, 0, err
		}
		tried[src.Code] = true

		// one provider serves an episode from several hosts, so a dead default
		// stream is not a dead provider
		for _, stream := range miruro.Rank(ctx, s.hc, res, s.cfg.Quality) {
			missed, err := s.from(ctx, res, src, stream, ep, report)
			if err == nil {
				return src, missed, nil
			}
			if ctx.Err() != nil {
				return source{}, 0, err
			}
			// name the provider, since the next attempt reports its own failure
			last = fmt.Errorf("%s: %w", src.Code, err)
			log.Warn("download failed, trying the next stream", "episode", num(ep), "provider", src.Code, "err", err)
		}
	}
}

// wanted is the source the pinned pick would resolve, what an episode that fell
// elsewhere is measured against when the run reports a rendition swap
// without a pin there is no expectation to diverge from
func (s saver) wanted(ep float64) (source, bool) {
	if s.pin.Code == "" {
		return source{}, false
	}
	avail, err := candidates(s.cat, ep, s.category, s.caps)
	if err != nil {
		return source{}, false
	}
	rows := orderPinned(offers(avail, s.caps, s.category, s.pin), s.pin)
	return rows[0].source(s.category), true
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
	cache := cacheDir(s.anilistID, ep, src.Category, src.Code, s.cfg.Quality)
	return play.Download(ctx, s.hc, s.px.Stream(stream), s.px.Subtitles(subs, stream.Referer), s.cfg.DownloadDir, name, cache, report)
}

func (s *runState) watch(ctx context.Context, st *store, numbers, queue []float64, pin Pin, player play.Player) error {
	px, err := play.StartProxy(ctx)
	if err != nil {
		return err
	}
	defer px.Close()

	details := s.cat.Details(s.category)
	ep := queue[0]
	queue = queue[1:]

	for {
		res, src, carry, err := s.resolve(ctx, ep, pin)
		if err != nil {
			return err
		}
		// carry the user's intent across episodes
		// a transient fallback serves another provider but must not overwrite the pin
		pin = carry

		var skips []miruro.SkipRange
		if flagSkip {
			skips = episodeSkips(s.cat, ep)
		}

		mediaTitle := fmt.Sprintf("%s Episode %s", s.title, num(ep))
		if d := details[ep]; d.Title != "" {
			mediaTitle += " - " + d.Title
		}

		stage := playback{
			runState: s, px: px, pin: pin, ep: ep, kind: player.Kind,
			launch: func(pctx context.Context, stream miruro.Stream, subs []miruro.Subtitle) error {
				return player.Play(pctx, px.Stream(stream), px.Subtitles(subs, stream.Referer), skips, mediaTitle)
			},
		}

		e := entry{AnilistID: s.anilistID, Title: s.title, Provider: pin.String(), Category: s.category, Episode: ep}
		action, err := playAndControl(ctx,
			fmt.Sprintf("Episode %s of %s", num(ep), s.title),
			controls(numbers, ep),
			len(queue) > 0,
			func(pctx context.Context) error { return stage.run(pctx, res, src) },
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

// playback is one episode's attempt, the ambient state the stream and provider
// walk needs
type playback struct {
	*runState
	px   *play.Proxy
	pin  Pin
	ep   float64
	kind play.Kind
	// launch runs the player on one stream with the subtitles chosen for the
	// provider that served it, a field so a test needs no player binary
	launch func(ctx context.Context, s miruro.Stream, subs []miruro.Subtitle) error
}

// run plays the episode, walking the streams of a provider and then the
// providers, and reports the last failure when none of them produced picture
// a provider that relayed no media body at all is dead for this episode however
// many streams it listed, and the download path has always moved off one
func (p playback) run(pctx context.Context, res *miruro.Result, src source) error {
	tried := map[string]bool{}
	var last error
	for {
		subs := miruro.Order(res.Subtitles, p.cfg.Lang)
		if !src.Attach {
			subs = nil
		}

		if ranked := miruro.Rank(pctx, p.hc, res, p.cfg.Quality); len(ranked) > 0 {
			log.Info("playing", "title", p.title, "ep", num(p.ep), "provider", src.Code,
				"server", server(ranked[0]), "rendition", src.Category,
				"player", p.kind, "subs", len(subs))

			before := p.px.Served()
			last = playStreams(pctx, p.px, ranked, func(ctx context.Context, s miruro.Stream) error {
				return p.launch(ctx, s, subs)
			})
			if last == nil || pctx.Err() != nil || p.px.Served() != before {
				return last
			}
		} else {
			last = fmt.Errorf("%s: %w", src.Code, miruro.ErrNoStream)
		}

		tried[src.Code] = true
		next, nsrc, err := p.autoResolve(pctx, p.ep, p.pin, tried)
		switch {
		case errors.Is(err, miruro.ErrBlocked):
			// the session is over, and saying the player exited would hide that
			return err
		case err != nil:
			log.Warn("no provider left to try", "provider", src.Code, "err", last)
			// report what failed to play rather than what failed to resolve after
			return last
		}
		log.Warn("nothing played, trying the next provider", "provider", src.Code, "next", nsrc.Code, "err", last)
		res, src = next, nsrc
	}
}

// playStreams hands each stream in turn to play until one of them plays
// a provider serving an episode from several hosts is not dead when the first
// of them is, and the action menu stays raised throughout because this runs
// inside the playback goroutine
func playStreams(ctx context.Context, px *play.Proxy, ranked []miruro.Stream, play func(context.Context, miruro.Stream) error) error {
	var err error
	for i, s := range ranked {
		before := px.Served()
		pctx, stop := context.WithCancel(ctx)
		settled := abandonStalled(pctx, px, server(s), stop)
		err = play(pctx, s)
		stop()
		abandoned := <-settled
		if !deadStream(err, before, px.Served()) || ctx.Err() != nil {
			return err
		}
		// the watcher already said why it stopped this one
		if !abandoned && i+1 < len(ranked) {
			log.Warn("stream did not play, trying the next", "server", server(s), "err", err)
		}
	}
	return err
}

// server names a stream for the log, since a provider does not always name its
// own host
func server(s miruro.Stream) string {
	if s.Server == "" {
		return "stream"
	}
	return s.Server
}

// startGrace is how long a stream has to relay its first media body
// pewe and bee reached theirs in 1.0s and 2.3s through the proxy on 2026-08-23,
// so this is an order of magnitude of headroom rather than a tuned value
// it is a variable so a test can shorten it rather than wait it out
var startGrace = 20 * time.Second

// refusalBudget is how many media bodies a stream may be refused before it has
// relayed one
// bee played after two refusals, while bonk and hop reached 25 and 538 in forty
// seconds without ever relaying one, so this separates them with room to spare
var refusalBudget = 8

// refusalCheck is how often a running player is asked whether its stream is
// getting anything
// a second is far below the wait it replaces and far above the cost of reading
// two counters
var refusalCheck = time.Second

// abandonStalled stops a player whose stream has shown nothing
// ffmpeg's hls demuxer skips a segment it cannot fetch and asks for the next,
// so a stream whose CDN refuses every one runs forever without a frame and the
// player never exits for playStreams to move on
// once any media body is relayed the user is watching, and a later gap is theirs
// to deal with rather than grounds for restarting the episode elsewhere
// the returned channel yields whether the player was stopped and closes when the
// watcher is done, and a caller must receive from it before it returns, since
// nothing else joins the goroutine that reports through note
func abandonStalled(ctx context.Context, px *play.Proxy, name string, stop context.CancelFunc) <-chan bool {
	served, refused := px.Served(), px.Refused()
	settled := make(chan bool, 1)
	go func() {
		abandoned := false
		defer func() { settled <- abandoned; close(settled) }()
		grace := time.NewTimer(startGrace)
		defer grace.Stop()
		tick := time.NewTicker(refusalCheck)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-grace.C:
				if px.Served() == served {
					log.Warn("stream showed nothing in time, abandoning it", "server", name, "after", startGrace)
					abandoned = true
					stop()
				}
				return
			case <-tick.C:
				if px.Served() > served {
					return
				}
				if n := px.Refused() - refused; n >= refusalBudget {
					log.Warn("stream refused before it played, abandoning it", "server", name, "refused", n)
					abandoned = true
					stop()
					return
				}
			}
		}
	}()
	return settled
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
func (s *runState) resolve(ctx context.Context, ep float64, pin Pin) (*miruro.Result, source, Pin, error) {
	if pin.Code != "" {
		res, src, err := s.autoResolve(ctx, ep, pin, nil)
		return res, src, pin, err
	}

	avail, err := candidates(s.cat, ep, s.category, s.caps)
	if err != nil {
		return nil, source{}, pin, err
	}

	rows := offers(avail, s.caps, s.category, pin)
	width := widest(rows)
	pick, err := ui.Select("Select provider", rows, func(o offer) string { return o.label(width) })
	if err != nil {
		return nil, source{}, pin, err
	}
	res, src, err := s.autoResolve(ctx, ep, pick.Pin, nil)
	return res, src, pick.Pin, err
}

// autoResolve tries the pinned pick first then the rest, never prompting
// skip names providers a caller has already used, so an episode being retried
// moves on instead of resolving the same dead source again
// a backend that refuses this client takes its other providers out of the walk
// with it for the rest of the run, since each would cost a request against the
// same refusal, and the refusal is what is reported when nothing else served
func (s *runState) autoResolve(ctx context.Context, ep float64, pin Pin, skip map[string]bool) (*miruro.Result, source, error) {
	avail, err := candidates(s.cat, ep, s.category, s.caps)
	if err != nil {
		return nil, source{}, err
	}

	var last, blocked error
	for _, o := range orderPinned(offers(avail, s.caps, s.category, pin), pin) {
		p := s.cat.Providers[o.Code]
		if skip[o.Code] {
			continue
		}
		if err := s.refused.get(p.Backend); err != nil {
			blocked = err
			continue
		}
		src := o.source(s.category)
		e := find(p.Episodes(src.Category), ep)
		if e == nil {
			continue
		}
		res, err := s.cat.Sources(ctx, e.ID, o.Code, src.Category)
		if err != nil {
			if ctx.Err() != nil {
				return nil, source{}, ctx.Err()
			}
			if errors.Is(err, miruro.ErrBlocked) {
				if s.refused.add(p.Backend, err) {
					log.Warn("backend refused the run, skipping its providers", "backend", p.Backend.Name(), "err", err)
				}
				blocked = err
				continue
			}
			// a provider that fails to resolve is reported only when none served,
			// so the pinned one says so as it happens and the rest under --verbose
			if o.Code == pin.Code {
				log.Warn("pinned provider did not resolve, trying the next", "provider", o.Code, "err", err)
			} else {
				log.Debug("provider did not resolve, trying the next", "provider", o.Code, "err", err)
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
	switch {
	case blocked != nil:
		return nil, source{}, blocked
	case last == nil:
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
