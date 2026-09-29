package main

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/ui"
	"ysun.co/miruro/internal/upstream"
)

// episodeLabel renders one picker row, the number plus what the catalog knows
// that tells episodes apart
// a number the catalog does not detail reads as the bare number
func episodeLabel(details map[float64]upstream.Episode) func(float64) string {
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
			log.Warn("flag ignored", "flag", "--episode", "because", "--all selects every episode")
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

func episodeSkips(cat *upstream.Catalog, ep float64) []upstream.SkipRange {
	var out []upstream.SkipRange
	for _, s := range cat.Aniskip {
		if s.Episode == ep {
			out = append(out, s)
		}
	}
	return out
}

func find(eps []upstream.Episode, n float64) *upstream.Episode {
	i := slices.IndexFunc(eps, func(e upstream.Episode) bool { return e.Number == n })
	if i < 0 {
		return nil
	}
	return &eps[i]
}

func neighbor(numbers []float64, ep float64, dir int) (float64, bool) {
	i := slices.Index(numbers, ep)
	if i < 0 {
		return 0, false
	}
	if j := i + dir; j >= 0 && j < len(numbers) {
		return numbers[j], true
	}
	return 0, false
}

// parseEpisodes reads an episode spec against the numbers the catalog lists
// a spec is one episode, latest for the newest listed, or a range lo-hi where
// a bound left off is the first or the last episode, so 5- runs to the end and
// -8 runs from the start
func parseEpisodes(spec string, numbers []float64) ([]float64, error) {
	spec = strings.TrimSpace(spec)
	if strings.EqualFold(spec, "latest") {
		if len(numbers) == 0 {
			return nil, errors.New("no episode listed to be the latest")
		}
		return numbers[len(numbers)-1:], nil
	}
	if from, to, isRange := strings.Cut(spec, "-"); isRange {
		lo, loSet, err1 := bound(from, numbers, 0)
		hi, hiSet, err2 := bound(to, numbers, len(numbers)-1)
		if err1 != nil || err2 != nil || !loSet && !hiSet {
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
		// a range wider than the catalog is clamped, and saying so is what tells
		// a short season from a provider that carries less than the rest
		// a bound left off asked for nothing, so it clamps nothing
		if loSet && out[0] != lo || hiSet && out[len(out)-1] != hi {
			log.Warn("range clamped to what the catalog carries", "asked", spec,
				"played", num(out[0])+"-"+num(out[len(out)-1]))
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

// bound reads one end of a range, the listed episode at edge when it is left
// off, and reports whether it was given
func bound(s string, numbers []float64, edge int) (float64, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		if len(numbers) == 0 {
			return 0, false, errors.New("no episode listed")
		}
		return numbers[edge], false, nil
	}
	n, err := strconv.ParseFloat(s, 64)
	return n, true, err
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
