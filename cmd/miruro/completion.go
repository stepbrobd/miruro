package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// shells are the completion scripts this build can generate, named in one place
// so the usage line, the tab completion and the refusal cannot drift apart
var shells = []string{"bash", "zsh", "fish"}

func init() {
	root.AddCommand(&cobra.Command{
		Use:       "completion [" + strings.Join(shells, "|") + "]",
		Short:     "Generate shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: shells,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(os.Stdout)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			default:
				return fmt.Errorf("shell %q is not one of %s", args[0], strings.Join(shells, ", "))
			}
		},
	})
}
