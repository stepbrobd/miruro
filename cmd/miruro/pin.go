package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/log"

	"ysun.co/miruro/internal/upstream"
)

// Variant decides whether a provider's external subtitle file is attached
// the zero value is a pin that names no rendition, so a bare "pewe" stays
// distinguishable from an explicit "pewe:soft"
type Variant string

const (
	Soft Variant = "soft" // attach the subtitle file when the source ships one
	Hard Variant = "hard" // play as delivered, subtitles already in the picture
)

// Pin is a provider choice, a code with an optional subtitle Variant
type Pin struct {
	Code    string
	Variant Variant
}

// ParsePin reads a "code" or "code:variant" pin
// a bare code and an unrecognized variant both leave the variant unstated, and
// problem says what a value that named a colon and got nothing usable from it
// did wrong, empty when it did nothing wrong
// it reports nothing itself, since a caller parsing one value twice would
// report it twice, which a resumed run did for every bad --provider until
// 2026-09-19
func ParsePin(s string) (pin Pin, problem string) {
	code, variant, named := strings.Cut(s, ":")
	v := Variant(variant)
	switch {
	case !named:
	case code == "":
		problem = "names a variant and no provider, so nothing is pinned"
	case v == "":
		problem = "names an empty variant, so the rendition is unstated"
	case v != Soft && v != Hard:
		problem = fmt.Sprintf("variant %q is not soft or hard, so the rendition is unstated", variant)
	}
	if v == Soft || v == Hard {
		return Pin{Code: code, Variant: v}, problem
	}
	return Pin{Code: code}, problem
}

// readPin parses a pin a run takes in and reports what is wrong with it
// a value that named a colon and got nothing usable from it is a mistake
// wherever it came from, so every value a run takes in passes here once
func readPin(s string) Pin {
	pin, problem := ParsePin(s)
	if problem != "" {
		log.Warn("provider "+problem, "provider", s)
	}
	return pin
}

// String is the form persisted to history and read back by resume, the bare
// code when the variant is unstated and "" when no provider was chosen
func (p Pin) String() string {
	if p.Code == "" || p.Variant == "" {
		return p.Code
	}
	return p.Code + ":" + string(p.Variant)
}

// offer is one row of the provider prompt, a pin plus whether the listing
// declared a subtitle variant for that provider
// an undeclared provider is offered bare, since labeling it with a variant it
// never promised would state more than is known
type offer struct {
	Pin
	declared bool
}

// pinFor decides which provider a run starts on and whether it may walk past it
// the flag wins over the config, and an entry resumed from history fills in for
// both when the flag named none
// only a provider the run was told to use holds the walk to it: one in history
// was picked from the menu once, and holding a later run to that would honor a
// choice the user never stated
func pinFor(config, flag, history string, widen bool) (Pin, bool) {
	// stated follows whichever source supplied the pin, and it is the provider
	// that counts rather than the string: a value naming a variant and no
	// provider states nothing, since the run prompts for one either way
	cfg, f := readPin(config), readPin(flag)
	pin, stated := cfg, cfg.Code != ""
	// a flag naming no provider states nothing, so it must not displace the
	// entry a resume is carrying either
	if history != "" && f.Code == "" {
		// an entry resuming the provider the config already names is that same
		// stated choice, and any other is one the menu picked once
		h := readPin(history)
		pin, stated = h, stated && h.Code == cfg.Code
	}
	if f.Code != "" {
		pin, stated = f, true
	}
	return pin, widen || !stated
}

// offers expands the available providers into the rows worth showing, one per
// subtitle rendition a provider declares
// the table describes the two sub renditions only, so a dub run gets one bare
// row per provider and keeps every track the dub ships
// an undeclared provider is offered with its variant unstated, the way its row
// reads and the way its pin is persisted, and takes the pinned variant when
// the pin names one, which is the only way an explicit code:hard survives a
// backend that lists providers without their renditions
func offers(avail []upstream.Provider, caps upstream.Capabilities, category upstream.Category, pin Pin) []offer {
	out := make([]offer, 0, len(avail))
	for _, p := range avail {
		c, ok := caps[p.Code]
		switch {
		case category != upstream.Sub:
			out = append(out, offer{Pin: Pin{Code: p.Code}})
		case !ok, !c.Hard && !c.Soft:
			var v Variant
			if pin.Code == p.Code {
				v = pin.Variant
			}
			out = append(out, offer{Pin: Pin{Code: p.Code, Variant: v}})
		case c.Hard && c.Soft:
			out = append(out,
				offer{Pin: Pin{Code: p.Code, Variant: Soft}, declared: true},
				offer{Pin: Pin{Code: p.Code, Variant: Hard}, declared: true})
		case c.Hard:
			out = append(out, offer{Pin: Pin{Code: p.Code, Variant: Hard}, declared: true})
		case c.Soft:
			out = append(out, offer{Pin: Pin{Code: p.Code, Variant: Soft}, declared: true})
		}
	}
	return out
}

// source is the provider and rendition one resolution settled on
type source struct {
	Pin
	// Category is what sources was asked for, ssub for the rendition carrying a
	// detachable subtitle file and sub for the burned-in one
	Category upstream.Category
	// Attach reports whether the subtitle file belongs over the picture
	Attach bool
}

// source resolves one row to what the resolution should ask for
// the burned-in rendition still ships a subtitle file on some providers, so
// whether to attach it follows the rendition that was asked for rather than
// whether one arrived
// an undeclared provider keeps the pre-table behavior, the sub rendition with
// whatever subtitle file comes back, unless the pin said hard
func (o offer) source(category upstream.Category) source {
	switch {
	case category != upstream.Sub:
		// the variant names a sub rendition, so it says nothing here
		return source{Pin: o.Pin, Category: category, Attach: true}
	case !o.declared:
		return source{Pin: o.Pin, Category: category, Attach: o.Variant != Hard}
	case o.Variant == Hard:
		return source{Pin: o.Pin, Category: upstream.Sub, Attach: false}
	default:
		return source{Pin: o.Pin, Category: upstream.Ssub, Attach: true}
	}
}

// label renders one row, padded to width so the variants line up
func (o offer) label(width int) string {
	if !o.declared {
		return o.Code
	}
	return fmt.Sprintf("%-*s  %ssub", width, o.Code, o.Variant)
}

func widest(rows []offer) int {
	w := 0
	for _, o := range rows {
		w = max(w, len(o.Code))
	}
	return w
}

// orderPinned puts the pinned pick first, then the pinned provider's other
// rendition, then the providers declaring the pinned rendition, then everything
// else in preference order
// a pin naming a rendition the provider stopped carrying still reaches the one
// it does carry before the run moves to another provider
// past the pinned provider the rendition is what the pin still asks for, so a
// hard pin falls to another provider's hardsub before any softsub, while an
// unstated variant prefers nothing beyond its provider
func orderPinned(rows []offer, pin Pin) []offer {
	if pin.Code == "" {
		return rows
	}
	priority := func(o offer) int {
		switch {
		case o.Pin == pin:
			return 0
		case o.Code == pin.Code:
			return 1
		case pin.Variant != "" && o.declared && o.Variant == pin.Variant:
			return 2
		default:
			return 3
		}
	}
	out := slices.Clone(rows)
	slices.SortStableFunc(out, func(a, b offer) int { return priority(a) - priority(b) })
	return out
}

// candidates lists the providers worth resolving for an episode
// a listing names a provider only where it has a stream this program can play,
// so an embed never reaches the prompt as a pick that cannot work
func candidates(l *upstream.Listing, ep float64, category upstream.Category) ([]upstream.Provider, error) {
	avail := l.Available(ep, category)
	if len(avail) == 0 {
		return nil, fmt.Errorf("no provider has episode %s", num(ep))
	}
	return avail, nil
}
