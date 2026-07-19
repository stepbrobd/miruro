package main

import (
	"os"
	"strconv"

	"github.com/BurntSushi/toml"
	"github.com/adrg/xdg"
	"github.com/charmbracelet/log"
)

type config struct {
	Player      string `toml:"player"`
	Quality     string `toml:"quality"`
	Provider    string `toml:"provider"`
	DownloadDir string `toml:"download_dir"`
	Dub         bool   `toml:"dub"`
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
	return c
}
