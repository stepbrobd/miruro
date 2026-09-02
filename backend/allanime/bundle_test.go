package allanime

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) string {
	t.Helper()
	js, err := os.ReadFile("testdata/chunk.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(js)
}

// the expected values were read off the same chunk by running its own code in
// node, so they pin the evaluator to what the site computes
func TestParseBuild(t *testing.T) {
	b, err := parseBuild(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "153" {
		t.Errorf("id = %q, want 153", b.ID)
	}
	want := []string{"A7tlxWxBS+Q=", "1xH/0IilEck=", "dT6l9ZzxQB4=", "DbBuTSwi4j8="}
	for i, f := range b.frags {
		if got := base64.StdEncoding.EncodeToString(f); got != want[i] {
			t.Errorf("frag %d = %s, want %s", i, got, want[i])
		}
	}
	if b.saltMul != 6 || b.saltAdd != 244 || b.fragMul != 190 || b.fragAdd != 88 {
		t.Errorf("salts = %d %d %d %d", b.saltMul, b.saltAdd, b.fragMul, b.fragAdd)
	}
	if b.prefix != "FD0xZhgI:" {
		t.Errorf("prefix = %q", b.prefix)
	}
	if b.join != "." || strings.Join(b.parts, ",") != "host,epoch,group,lane,buildId" {
		t.Errorf("message shape = %q %v", b.join, b.parts)
	}
	if b.epoch != 7*24*time.Hour || b.grace != 24*time.Hour {
		t.Errorf("epoch %s grace %s", b.epoch, b.grace)
	}
}

func TestParseBuildRefusesOtherShapes(t *testing.T) {
	js := fixture(t)
	for name, mangled := range map[string]string{
		"no config":      strings.ReplaceAll(js, "saltMul:", "saltMu:"),
		"no table":       strings.ReplaceAll(js, "function Bc(){const e=[", "function Bc(){const e=("),
		"no rotation":    strings.ReplaceAll(js, "})(Bc,986495)", "})(Bd,986495)"),
		"wrong checksum": strings.ReplaceAll(js, "})(Bc,986495)", "})(Bc,986496)"),
		"short fragment": strings.ReplaceAll(js, `"xBS+Q="`, `"xBS+Q"`),
	} {
		if _, err := parseBuild(mangled); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestParseInt(t *testing.T) {
	for in, want := range map[string]float64{"1901115gtKRcn": 1901115, "  42x": 42, "-7a": -7} {
		if got := parseInt(in); got != want {
			t.Errorf("parseInt(%q) = %v, want %v", in, got, want)
		}
	}
	if got := parseInt("abc"); got == got {
		t.Errorf("parseInt of a word = %v, want NaN", got)
	}
}

func TestStringLiteral(t *testing.T) {
	for in, want := range map[string]string{
		`"plain"`:   "plain",
		`'his")('`:  `his")(`,
		`"a\"b"`:    `a"b`,
		`"\x41B\n"`: "AB\n",
	} {
		got, n, err := stringLiteral(in)
		if err != nil || got != want || n != len(in) {
			t.Errorf("stringLiteral(%s) = %q, %d, %v, want %q", in, got, n, err, want)
		}
	}
	if _, _, err := stringLiteral(`"open`); err == nil {
		t.Error("unterminated literal parsed")
	}
}

func TestEvalArithmetic(t *testing.T) {
	ev := &evaluator{js: "", tables: map[string]*table{}}
	for expr, want := range map[string]float64{
		"1+2*3":                               7,
		"-4*1427+-124+5848":                   16,
		"9190073*27+833471*1057+1*-524310818": 604800000,
		"(1+2)*3":                             9,
		"7/2":                                 3.5,
	} {
		v, err := ev.eval(expr, "", nil)
		if err != nil || v != want {
			t.Errorf("eval(%q) = %v, %v, want %v", expr, v, err, want)
		}
	}
	if v, err := ev.eval(`"ab"+"c"+1`, "", nil); err != nil || v != "abc1" {
		t.Errorf("concat = %v, %v", v, err)
	}
	if v, err := ev.eval("!1", "", nil); err != nil || v != false {
		t.Errorf("not = %v, %v", v, err)
	}
}
