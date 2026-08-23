package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/adrg/xdg"
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
	for _, v := range []string{"MIRURO_PLAYER", "MIRURO_QUALITY", "MIRURO_PROVIDER", "MIRURO_LANG", "MIRURO_DOWNLOAD_DIR", "MIRURO_DUB", "MIRURO_MIRRORS"} {
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
