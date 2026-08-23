package main

import (
	"fmt"
	"slices"
	"strings"

	"ysun.co/miruro"
)

// Variant decides whether a provider's external subtitle file is attached
type Variant string

const (
	Soft Variant = "soft" // attach the subtitle file when the source ships one
	Hard Variant = "hard" // play as delivered, subtitles already in the picture
)

// Pin is a provider choice, a code with a subtitle Variant
type Pin struct {
	Code    string
	Variant Variant
}

// ParsePin reads a "code" or "code:variant" pin
// a bare code or an unrecognised variant means Soft
func ParsePin(s string) Pin {
	code, variant, _ := strings.Cut(s, ":")
	if Variant(variant) == Hard {
		return Pin{Code: code, Variant: Hard}
	}
	return Pin{Code: code, Variant: Soft}
}

// String is the "code:variant" form persisted to history and read back by resume
// an empty code is the empty pin, meaning no provider was chosen
func (p Pin) String() string {
	if p.Code == "" {
		return ""
	}
	return p.Code + ":" + string(p.Variant)
}

// offer is one row of the provider prompt, a pin plus whether the capability
// table declared a subtitle variant for that provider
// an undeclared provider is offered bare, since labelling it with a variant it
// never promised would state more than is known
type offer struct {
	Pin
	declared bool
}

// offers expands the available providers into the rows worth showing, one per
// subtitle variant a provider declares
// the table describes the two sub renditions and says nothing about dub, so a
// dub run gets one bare row per provider
// an undeclared provider takes the pinned variant when the pin names it, which
// is the only way an explicit code:hard survives a run that could not reach the
// table
func offers(avail []miruro.Provider, caps miruro.Capabilities, cat miruro.Category, pin Pin) []offer {
	out := make([]offer, 0, len(avail))
	for _, p := range avail {
		c, ok := caps[p.Code]
		switch {
		case cat != miruro.Sub, !ok, !c.Hard && !c.Soft:
			v := Soft
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
	Category miruro.Category
	// Attach reports whether the subtitle file belongs over the picture
	Attach bool
}

// source is what to ask for to serve one offer
// the burned-in rendition still ships a subtitle file on some providers, so
// whether to attach it follows the rendition that was asked for rather than
// whether one arrived
// an undeclared provider keeps the pre-table behaviour, the sub rendition with
// whatever subtitle file comes back, unless the pin said hard
func (o offer) source(cat miruro.Category) source {
	switch {
	case cat != miruro.Sub, !o.declared:
		return source{Pin: o.Pin, Category: cat, Attach: o.Variant != Hard}
	case o.Variant == Hard:
		return source{Pin: o.Pin, Category: miruro.Sub, Attach: false}
	default:
		return source{Pin: o.Pin, Category: miruro.Ssub, Attach: true}
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
// rendition, then everything else in preference order
// a pin naming a rendition the provider stopped carrying still reaches the one
// it does carry before the run moves to another provider
func orderPinned(rows []offer, pin Pin) []offer {
	if pin.Code == "" {
		return rows
	}
	out := make([]offer, 0, len(rows))
	for _, want := range []func(offer) bool{
		func(o offer) bool { return o.Pin == pin },
		func(o offer) bool { return o.Code == pin.Code },
		func(offer) bool { return true },
	} {
		for _, o := range rows {
			if want(o) && !slices.Contains(out, o) {
				out = append(out, o)
			}
		}
	}
	return out
}

// candidates lists the providers worth resolving for an episode, those carrying
// it minus the ones the capability table puts behind an iframe
// resolving an embed only to have Playable reject it costs a request, and
// offering one costs the user a pick that cannot work
func candidates(cat *miruro.Catalog, ep float64, category miruro.Category, caps miruro.Capabilities) ([]miruro.Provider, error) {
	avail := cat.Available(ep, category)
	if len(avail) == 0 {
		return nil, fmt.Errorf("no provider has episode %s", num(ep))
	}
	out := make([]miruro.Provider, 0, len(avail))
	for _, p := range avail {
		if c, ok := caps[p.Code]; ok && c.Embed {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("every provider for episode %s plays in an embed", num(ep))
	}
	return out, nil
}
