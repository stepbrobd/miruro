package main

import (
	"net/url"
	"os"
	"path/filepath"
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
	DownloadDir string `toml:"download"`
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
			md, err := toml.Decode(string(data), &c)
			if err != nil {
				log.Warn("ignoring malformed config", "path", path, "err", err)
			}
			// a key nothing reads is a typo or a name that has moved, and
			// dropping it without a word looks exactly like it took effect
			for _, key := range md.Undecoded() {
				log.Warn("ignoring unknown config key", "path", path, "key", key.String())
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
		{"MIRURO_DOWNLOAD", &c.DownloadDir},
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
	c.DownloadDir = expand(c.DownloadDir)
	// a configured list that loses every entry would otherwise revert to the
	// built-in origins with nothing said about it
	if named := len(c.Mirrors); named > 0 {
		if c.Mirrors = origins(c.Mirrors); len(c.Mirrors) == 0 {
			log.Warn("no usable mirror configured, using the built-in origins", "dropped", named)
		}
	}
	return c
}

// userDirs are the XDG user directory names, resolved by the xdg package
// a desktop writes these to user-dirs.dirs rather than exporting them, so the
// environment alone would report every one of them unset
var userDirs = map[string]string{
	"XDG_DESKTOP_DIR":     xdg.UserDirs.Desktop,
	"XDG_DOCUMENTS_DIR":   xdg.UserDirs.Documents,
	"XDG_DOWNLOAD_DIR":    xdg.UserDirs.Download,
	"XDG_MUSIC_DIR":       xdg.UserDirs.Music,
	"XDG_PICTURES_DIR":    xdg.UserDirs.Pictures,
	"XDG_PUBLICSHARE_DIR": xdg.UserDirs.PublicShare,
	"XDG_TEMPLATES_DIR":   xdg.UserDirs.Templates,
	"XDG_VIDEOS_DIR":      xdg.UserDirs.Videos,
}

// expand resolves a leading ~ and any $NAME in a configured path
// no shell reads a config file, so a path written the way one would be typed
// into one arrives literally and would otherwise put the episodes somewhere
// nobody asked for
// a name nothing resolves is left as written rather than replaced with nothing,
// since turning "$XDG_VIDEOS_DIR/anime" into "/anime" would write to the root of
// the filesystem
func expand(path string) string {
	if path == "" {
		return path
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Warn("cannot resolve ~ in a path, using it as written", "path", path, "err", err)
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	// no Clean here, since it would rewrite "./~/x" to "~/x" and leave a path
	// that reads like a home directory nothing expanded
	return os.Expand(path, lookup)
}

// lookup resolves one $NAME from the environment, then the XDG user directories
func lookup(name string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	if v := userDirs[name]; v != "" {
		return v
	}
	log.Warn("config names something nothing sets, leaving it as written", "name", name)
	return "$" + name
}

// origins keeps the mirrors that name an http or https host
// a typo would otherwise fail every request against it with a scheme error,
// which reads like the pipe is down rather than like the config is wrong
func origins(mirrors []string) []string {
	var out []string
	for _, m := range mirrors {
		o := origin(m)
		if o == "" {
			log.Warn("ignoring mirror that is not an http origin", "mirror", m)
			continue
		}
		out = append(out, o)
	}
	return out
}

// origin normalises one configured mirror, empty when it names no http host
func origin(mirror string) string {
	mirror = strings.TrimRight(strings.TrimSpace(mirror), "/")
	u, err := url.Parse(mirror)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return mirror
}
