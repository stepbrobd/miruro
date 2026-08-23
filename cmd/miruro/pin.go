package main

import (
	"fmt"
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
// table named the provider
// an undeclared provider is offered bare, since labelling it with a variant it
// never promised would state more than is known
type offer struct {
	Pin
	declared bool
}

// offers expands the available providers into the rows worth showing, one per
// subtitle variant a provider declares
// only a provider declaring both leaves a real choice, which is why the prompt
// this replaced had one sensible answer nearly every time it appeared
func offers(avail []miruro.Provider, caps miruro.Config) []offer {
	out := make([]offer, 0, len(avail))
	for _, p := range avail {
		c, ok := caps[p.Code]
		switch {
		case !ok || (!c.Hard && !c.Soft):
			out = append(out, offer{Pin: Pin{Code: p.Code, Variant: Soft}})
		case c.Hard && c.Soft:
			out = append(out,
				offer{Pin: Pin{Code: p.Code, Variant: Soft}, declared: true},
				offer{Pin: Pin{Code: p.Code, Variant: Hard}, declared: true})
		case c.Hard:
			out = append(out, offer{Pin: Pin{Code: p.Code, Variant: Hard}, declared: true})
		default:
			out = append(out, offer{Pin: Pin{Code: p.Code, Variant: Soft}, declared: true})
		}
	}
	return out
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

// candidates lists the providers worth resolving for an episode, those carrying
// it minus the ones the capability table puts behind an iframe
// resolving an embed only to have Playable reject it costs a request, and
// offering one costs the user a pick that cannot work
func candidates(cat *miruro.Catalog, ep float64, category miruro.Category, caps miruro.Config) ([]miruro.Provider, error) {
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
