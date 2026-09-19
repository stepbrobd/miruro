package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"

	"ysun.co/miruro"
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
	// fallback allows the walk past the pinned provider
	// a provider named in the config or on the command line is a stated choice,
	// so only --fallback trades it for another
	fallback bool
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
	if flagLang != "" {
		cfg.Lang = flagLang
	}
	if flagDub {
		cfg.Dub = true
	}
	// config validate reads the file, so a quality given on the command line or
	// in the environment reached the heuristic unchecked and was dropped in
	// silence when it named a height no provider carries
	if !miruro.ValidQuality(cfg.Quality) {
		return fmt.Errorf("quality %q is not best, worst, or a height such as 1080p", cfg.Quality)
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
	resumed := ""

	if flagContinue {
		if len(args) > 0 {
			log.Warn("flag ignored", "flag", "the query", "because", "--continue resumes from history")
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
		resumed = e.Provider
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

	pin, fallback := pinFor(cfg.Provider, flagProvider, resumed, flagFallback)
	if pin.Code != "" {
		if _, ok := cat.Providers[pin.Code]; !ok {
			if fallback {
				log.Warn("pinned provider is not in the catalog, trying the rest in preference order", "provider", pin.Code)
			} else {
				log.Warn("pinned provider is not in the catalog, pass --fallback to try the rest", "provider", pin.Code)
			}
		}
	}
	if !flagDownload && flagParallel > 1 {
		log.Warn("flag ignored", "flag", "--parallel", "because", "it applies only with --download")
	}
	if flagDownload && flagSkip {
		log.Warn("flag ignored", "flag", "--skip", "because", "it applies only to playback")
	}

	state := &runState{
		hc: client.HTTP, cat: cat, anilistID: media.ID, title: title,
		category: category, caps: caps, cfg: cfg, fallback: fallback,
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
	return miruro.Backends{client}
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
