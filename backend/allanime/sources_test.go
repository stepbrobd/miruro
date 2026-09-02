package allanime

import (
	"strings"
	"testing"
)

// the encoded path is one the live api handed out for Frieren episode 1 on
// 2026-09-02
func TestDecode(t *testing.T) {
	enc := "175948514e4c4f57175b54575b5307515c050f5c0a0c0f0b0f0c0e590a0c0b5b0a0c0f0d0f0b0e0c0a5a0f590a5a0f090e0f0f0a0e0d0e5d0a010e080f0c0e5e0e0b0f0c0e0b0e000a5a0e0c0e0b0f5e0e010e000e0a0a5a0e5b0e010f0b0f0c0e000e0b0f5e0f0d0a5a0e0b0e000e0a0a5a0b0f0b5d0b0b0b0a0b0c0b010e0b0f0e0b5a0b0f0b0e0b090b0c0b0b0b090a0c0a590a0c0f0d0f0a0f0c0e0b0e0f0e5a0e0b0f0c0c5e0e0a0a0c0b5b0a0c0e000f0d0e0b0f0c0f080e0b0f0c0a0c0a590a0c0e0a0e0f0f0a0e0b0a0c0b5b0a0c0b0c0b0e0b0c0b080a5a0b0e0b5e0a5a0b0e0b0c0d0a0b0f0b0f0b5b0b0e0b0c0b5b0b0e0b0e0a000b0e0b0e0b0e0d5b0a0c0f5a"
	got := decode(enc)
	if !strings.HasPrefix(got, "/apivtwo/clock?id=7d2473746a243c247573642b7a2b716772656e") {
		t.Errorf("decode = %q", got)
	}
	// an unknown pair is kept as written rather than dropped
	if got := decode("5959zz"); got != "aazz" {
		t.Errorf("decode with an unknown pair = %q", got)
	}
}

func TestParseNumber(t *testing.T) {
	for in, want := range map[string]float64{"1": 1, "12.5": 12.5, " 0 ": 0} {
		if got, err := parseNumber(in); err != nil || got != want {
			t.Errorf("parseNumber(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", "-1", "1e400"} {
		if _, err := parseNumber(in); err == nil {
			t.Errorf("parseNumber(%q) accepted", in)
		}
	}
}
