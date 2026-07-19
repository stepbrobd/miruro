package main

import (
	"os"

	"github.com/BurntSushi/toml"
	"github.com/adrg/xdg"
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
			_, _ = toml.Decode(string(data), &c)
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
	return c
}
