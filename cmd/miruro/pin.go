package main

import "strings"

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
	code, variant, found := strings.Cut(s, ":")
	if found && Variant(variant) == Hard {
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
