package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// validate reads the file rather than the loaded settings, so a problem cannot
// hide behind an environment override
func TestCheck(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{"a template with everything commented out", template, nil},
		{"the defaults spelled out", "quality = \"best\"\ndub = false\ndownload = \"" + dir + "\"\n", nil},
		{"an exact height", "quality = \"1080p\"\n", nil},
		{"a bare provider code", "provider = \"pewe\"\n", nil},
		{"a variant the pin parser would silently take as soft", "provider = \"bonk:medium\"\n",
			[]string{`variant "medium" is not soft or hard`}},
		{"a variant with no provider", "provider = \":hard\"\n",
			[]string{"names a variant with no provider"}},
		{"a key that has moved", "download_dir = \"/tmp\"\n",
			[]string{`unknown key "download_dir"`}},
		{"a player nothing can launch", "player = \"vlc\"\n",
			[]string{`player "vlc" is not mpv or iina`}},
		{"a quality the heuristic cannot read", "quality = \"veryhigh\"\n",
			[]string{`quality "veryhigh"`}},
		{"a download directory that is not there", "download = \"" + filepath.Join(dir, "nope") + "\"\n",
			[]string{"does not exist"}},
		{"a mirror that is not an origin", "mirrors = [\"miruro.tv\"]\n",
			[]string{`mirror "miruro.tv" is not an http`, "no mirror is usable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c config
			md, err := toml.Decode(tc.body, &c)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(check(md, c), "\n")
			if len(tc.want) == 0 {
				if got != "" {
					t.Fatalf("want no problems, got:\n%s", got)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("problems do not mention %q:\n%s", want, got)
				}
			}
		})
	}
}

// a pin and a download directory are exactly what someone would hate to lose to
// a mistyped subcommand
func TestConfigInitRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "miruro", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("provider = \"bonk:hard\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	withConfigHome(t, dir)
	if err := runConfigInit(nil, nil); err == nil {
		t.Fatal("init overwrote an existing config")
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), "bonk:hard") {
		t.Errorf("the existing config was disturbed: %q (%v)", body, err)
	}
}

// the template has to survive its own validation, or init writes a file that
// validate rejects
func TestConfigInitWritesAValidFile(t *testing.T) {
	dir := t.TempDir()
	withConfigHome(t, dir)
	if err := runConfigInit(nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := runConfigValidate(nil, nil); err != nil {
		t.Errorf("the file init wrote does not validate: %v", err)
	}
}
