package main

import (
	"os"

	"github.com/charmbracelet/log"
	"github.com/spf13/cobra"
)

var version = "dev"

var root = &cobra.Command{
	Use:           "miruro [query]",
	Short:         "Watch anime from miruro.tv",
	Version:       version,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func main() {
	root.Version = version
	if err := root.Execute(); err != nil {
		log.Error(err)
		os.Exit(1)
	}
}
