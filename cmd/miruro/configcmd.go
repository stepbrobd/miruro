package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/BurntSushi/toml"
	"github.com/adrg/xdg"
	"github.com/spf13/cobra"

	"ysun.co/miruro"
	"ysun.co/miruro/play"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage the config file",
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a commented config file, refusing to overwrite one",
	Args:  cobra.NoArgs,
	RunE:  runConfigInit,
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the settings a run would use, after the file and the environment",
	Args:  cobra.NoArgs,
	RunE:  runConfigShow,
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Check the config file and report what a run would refuse or ignore",
	Args:  cobra.NoArgs,
	RunE:  runConfigValidate,
}

func init() {
	configCmd.AddCommand(configInitCmd, configShowCmd, configValidateCmd)
	root.AddCommand(configCmd)
}

// template is what init writes, every key commented out at its default so the
// file documents the surface without changing any behaviour
const template = `# miruro configuration
# every key is optional, and the value shown is the default

# player to launch, mpv or iina, empty picks whichever is installed
# player = ""

# best, worst, or an exact height such as 1080p
# quality = "best"

# provider to pin, as a code or code:variant where variant is soft or hard
# soft asks for the rendition with a detachable subtitle file, hard for the one
# with the subtitles burned in, and a provider carrying only one is corrected to it
# provider = ""

# preferred subtitle language, a tag such as en or a label such as English
# lang = ""

# where downloads land
# a leading ~ and any $NAME are expanded here, since no shell reads this file
# download = "."

# use dub instead of sub
# dub = false

# pipe origins to try, in order, replacing the built-in list
# mirrors = ["https://www.miruro.ru", "https://www.miruro.to", "https://www.miruro.bz", "https://www.miruro.tv"]
`

func configPath() (string, error) {
	return xdg.ConfigFile("miruro/config.toml")
}

func runConfigInit(*cobra.Command, []string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	// refuse rather than overwrite, since a pin and a download directory are
	// exactly what someone would hate to lose to a mistyped subcommand
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(template), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

func runConfigShow(*cobra.Command, []string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Println("no config file at", path)
	} else {
		fmt.Println("config", path)
	}

	c := loadConfig()
	return table(func(w *tabwriter.Writer) {
		fmt.Fprintf(w, "player\t%s\n", or(c.Player, "auto"))
		fmt.Fprintf(w, "quality\t%s\n", c.Quality)
		fmt.Fprintf(w, "provider\t%s\n", or(c.Provider, "ask"))
		fmt.Fprintf(w, "lang\t%s\n", or(c.Lang, "any"))
		fmt.Fprintf(w, "download\t%s\n", c.DownloadDir)
		fmt.Fprintf(w, "dub\t%v\n", c.Dub)
		fmt.Fprintf(w, "mirrors\t%s\n", or(strings.Join(c.Mirrors, " "), "built in"))
	})
}

func or(s, empty string) string {
	if s == "" {
		return empty
	}
	return s
}

func runConfigValidate(*cobra.Command, []string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("no config file at", path, "so every setting is a default")
		return nil
	}
	if err != nil {
		return err
	}

	var c config
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	problems := check(md, c)
	if len(problems) == 0 {
		fmt.Println(path, "is good")
		return nil
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, " ", p)
	}
	return fmt.Errorf("%s has %d problems", path, len(problems))
}

// check reports what a run would refuse or quietly ignore
// it reads the file's own values rather than the loaded ones, so an environment
// override cannot hide a broken key
func check(md toml.MetaData, c config) []string {
	var out []string
	for _, key := range md.Undecoded() {
		out = append(out, fmt.Sprintf("unknown key %q, nothing reads it", key.String()))
	}
	if c.Player != "" && play.Kind(c.Player) != play.MPV && play.Kind(c.Player) != play.IINA {
		out = append(out, fmt.Sprintf("player %q is not mpv or iina", c.Player))
	}
	if !miruro.ValidQuality(c.Quality) {
		out = append(out, fmt.Sprintf("quality %q is not best, worst, or a height such as 1080p", c.Quality))
	}
	if code, variant, found := strings.Cut(c.Provider, ":"); found {
		switch {
		case code == "":
			out = append(out, fmt.Sprintf("provider %q names a variant with no provider", c.Provider))
		case Variant(variant) != Soft && Variant(variant) != Hard:
			out = append(out, fmt.Sprintf("provider variant %q is not soft or hard, so it is ignored", variant))
		}
	}
	if dir := expand(c.DownloadDir); dir != "" {
		switch fi, err := os.Stat(dir); {
		case errors.Is(err, os.ErrNotExist):
			out = append(out, fmt.Sprintf("download %s does not exist", dir))
		case err != nil:
			out = append(out, fmt.Sprintf("download %s cannot be read: %v", dir, err))
		case !fi.IsDir():
			out = append(out, fmt.Sprintf("download %s is not a directory", dir))
		}
	}
	usable := 0
	for _, m := range c.Mirrors {
		if origin(m) == "" {
			out = append(out, fmt.Sprintf("mirror %q is not an http or https origin", m))
			continue
		}
		usable++
	}
	if len(c.Mirrors) > 0 && usable == 0 {
		out = append(out, "no mirror is usable, so the built-in origins are used")
	}
	return out
}
