package main

import "strings"

// splitPin separates a provider pin "code:variant" into its parts. variant is
// normalised to "soft" or "hard"; a bare code or an unrecognised variant means
// "soft" (attach the subtitle file when the source carries one).
func splitPin(s string) (code, variant string) {
	code, variant, found := strings.Cut(s, ":")
	if found && variant == "hard" {
		return code, "hard"
	}
	return code, "soft"
}
