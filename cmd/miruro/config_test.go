package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/adrg/xdg"
	"github.com/charmbracelet/log"
)

// TestLoadConfig pins the precedence order, defaults under the file under the
// environment, and that a malformed file degrades to defaults rather than
// aborting
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	// registered before Setenv so it runs after the env restore
	t.Cleanup(xdg.Reload)
	t.Setenv("XDG_CONFIG_HOME", dir)
	xdg.Reload()
	for _, v := range []string{"MIRURO_PLAYER", "MIRURO_QUALITY", "MIRURO_PROVIDER", "MIRURO_LANG", "MIRURO_DOWNLOAD", "MIRURO_DUB", "MIRURO_MIRRORS", "MIRURO_BACKENDS"} {
		t.Setenv(v, "")
	}

	t.Run("defaults without a config file", func(t *testing.T) {
		c := loadConfig()
		want := config{Quality: "best", DownloadDir: "."}
		if !reflect.DeepEqual(c, want) {
			t.Errorf("loadConfig() = %+v, want %+v", c, want)
		}
	})

	path := filepath.Join(dir, "miruro", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("file values with env overrides", func(t *testing.T) {
		body := "quality = \"720p\"\nprovider = \"bonk:hard\"\nlang = \"ja\"\ndub = true\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MIRURO_QUALITY", "480p")
		t.Setenv("MIRURO_LANG", "en")
		t.Setenv("MIRURO_DUB", "false")
		c := loadConfig()
		want := config{Quality: "480p", Provider: "bonk:hard", Lang: "en", DownloadDir: "."}
		if !reflect.DeepEqual(c, want) {
			t.Errorf("loadConfig() = %+v, want %+v", c, want)
		}
	})

	t.Run("mirrors come from the file and the environment, bad origins dropped", func(t *testing.T) {
		body := "mirrors = [\"https://www.miruro.bz/\", \"miruro.tv\"]\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		c := loadConfig()
		if want := []string{"https://www.miruro.bz"}; !reflect.DeepEqual(c.Mirrors, want) {
			t.Errorf("Mirrors = %v, want %v with the schemeless entry dropped", c.Mirrors, want)
		}

		t.Setenv("MIRURO_MIRRORS", "https://a.example, https://b.example/")
		c = loadConfig()
		if want := []string{"https://a.example", "https://b.example"}; !reflect.DeepEqual(c.Mirrors, want) {
			t.Errorf("Mirrors = %v, want %v", c.Mirrors, want)
		}
	})

	// a list that loses every entry must not revert to the built-in origins with
	// nothing said about it
	t.Run("a list with no usable origin ends up empty", func(t *testing.T) {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MIRURO_MIRRORS", "miruro.tv,ftp://miruro.tv, ")
		if c := loadConfig(); len(c.Mirrors) != 0 {
			t.Errorf("Mirrors = %v, want none kept", c.Mirrors)
		}
	})

	t.Run("backends come from the file and the environment, trimmed", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("backends = [\"allanime\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if c := loadConfig(); !reflect.DeepEqual(c.Backends, []string{"allanime"}) {
			t.Errorf("Backends = %v, want allanime", c.Backends)
		}
		t.Setenv("MIRURO_BACKENDS", "miruro, allanime")
		if c := loadConfig(); !reflect.DeepEqual(c.Backends, []string{"miruro", "allanime"}) {
			t.Errorf("Backends = %v, want both", c.Backends)
		}
	})

	t.Run("malformed file keeps defaults", func(t *testing.T) {
		if err := os.WriteFile(path, []byte("not toml ["), 0o644); err != nil {
			t.Fatal(err)
		}
		c := loadConfig()
		want := config{Quality: "best", DownloadDir: "."}
		if !reflect.DeepEqual(c, want) {
			t.Errorf("loadConfig() = %+v, want %+v", c, want)
		}
	})
}

// no shell reads a config file, so a path typed the way it would be typed into
// one arrives literally and used to make a directory named "~"
func TestExpand(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to expand against")
	}
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"~/Videos/", filepath.Join(home, "Videos")},
		{"~", home},
		{"~/", home},
		{"/tmp/anime", "/tmp/anime"},
		{".", "."},
		// only a leading ~ that starts a path component is a home directory
		{"~notauser/x", "~notauser/x"},
		{"./~/x", "./~/x"},
		{"", ""},
	} {
		if got := expand(tc.in); got != tc.want {
			t.Errorf("expand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// a config file is not read by a shell, and an unset name expanded to nothing
// would turn "$XDG_VIDEOS_DIR/anime" into "/anime" and write to the filesystem
// root
func TestExpandVariables(t *testing.T) {
	t.Setenv("MIRURO_TEST_DIR", "/srv/media")
	t.Setenv("MIRURO_TEST_EMPTY", "")
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"$MIRURO_TEST_DIR/anime", "/srv/media/anime"},
		{"${MIRURO_TEST_DIR}/anime", "/srv/media/anime"},
		// left as written rather than collapsed to an absolute path at the root
		{"$MIRURO_TEST_UNSET/anime", "$MIRURO_TEST_UNSET/anime"},
		{"$MIRURO_TEST_EMPTY/anime", "$MIRURO_TEST_EMPTY/anime"},
	} {
		if got := expand(tc.in); got != tc.want {
			t.Errorf("expand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// an XDG user directory is written to user-dirs.dirs rather than exported,
	// so the environment alone would report it unset
	if want := userDirs["XDG_VIDEOS_DIR"]; want != "" {
		if got := expand("$XDG_VIDEOS_DIR/anime"); got != want+"/anime" {
			t.Errorf("expand($XDG_VIDEOS_DIR/anime) = %q, want %q", got, want+"/anime")
		}
	}
}

// a key nothing reads is a typo or a name that has moved, and dropping it
// without a word looks exactly like it took effect
func TestLoadConfigReportsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(xdg.Reload)
	t.Setenv("XDG_CONFIG_HOME", dir)
	xdg.Reload()
	path := filepath.Join(dir, "miruro", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("download_dir = \"/tmp/x\"\nquality = \"720p\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var b bytes.Buffer
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c := loadConfig()
	if c.Quality != "720p" {
		t.Errorf("Quality = %q, want the key that is still read to take effect", c.Quality)
	}
	if c.DownloadDir != "." {
		t.Errorf("DownloadDir = %q, want the default since download_dir is gone", c.DownloadDir)
	}
	if got := b.String(); !strings.Contains(got, "download_dir") {
		t.Errorf("the retired key was dropped without a word:\n%s", got)
	}
}

// withConfigHome points XDG at dir for the length of a test
func withConfigHome(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(xdg.Reload)
	t.Setenv("XDG_CONFIG_HOME", dir)
	xdg.Reload()
}
