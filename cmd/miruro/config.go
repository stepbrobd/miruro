package main

import (
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/adrg/xdg"
	"github.com/charmbracelet/log"
)

type config struct {
	Player      string `toml:"player"`
	Quality     string `toml:"quality"`
	Provider    string `toml:"provider"`
	Lang        string `toml:"lang"`
	DownloadDir string `toml:"download_dir"`
	Dub         bool   `toml:"dub"`
	// Mirrors replaces the built-in pipe origins, in the order they are tried
	// a domain blocked at the resolver costs a timeout per run, so reordering
	// belongs to whoever is behind that resolver
	Mirrors []string `toml:"mirrors"`
}

func loadConfig() config {
	c := config{Quality: "best", DownloadDir: "."}

	if path, err := xdg.ConfigFile("miruro/config.toml"); err == nil {
		if data, err := os.ReadFile(path); err == nil {
			// a decode error drops the whole file, including a provider pin
			// warn rather than revert every setting to a default in silence
			if _, err := toml.Decode(string(data), &c); err != nil {
				log.Warn("ignoring malformed config", "path", path, "err", err)
			}
		}
	}

	for _, o := range []struct {
		env string
		dst *string
	}{
		{"MIRURO_PLAYER", &c.Player},
		{"MIRURO_QUALITY", &c.Quality},
		{"MIRURO_PROVIDER", &c.Provider},
		{"MIRURO_LANG", &c.Lang},
		{"MIRURO_DOWNLOAD_DIR", &c.DownloadDir},
	} {
		if v := os.Getenv(o.env); v != "" {
			*o.dst = v
		}
	}
	if v := os.Getenv("MIRURO_DUB"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Dub = b
		}
	}
	if v := os.Getenv("MIRURO_MIRRORS"); v != "" {
		c.Mirrors = strings.Split(v, ",")
	}
	c.Mirrors = origins(c.Mirrors)
	return c
}

// origins keeps the mirrors that name an http or https host
// a typo would otherwise fail every request against it with a scheme error,
// which reads like the pipe is down rather than like the config is wrong
func origins(mirrors []string) []string {
	var out []string
	for _, m := range mirrors {
		m = strings.TrimRight(strings.TrimSpace(m), "/")
		u, err := url.Parse(m)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			log.Warn("ignoring mirror that is not an http origin", "mirror", m)
			continue
		}
		out = append(out, m)
	}
	return out
}
