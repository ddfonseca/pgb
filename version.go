package main

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version vem do build da release: go build -ldflags "-X main.version=v0.1.1".
var version string

// versionString: a da release; num go build local, o commit (com -dirty se havia mudanças).
func versionString() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "-dirty"
	}
	return "dev-" + rev
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Mostra a versão do pgb",
		Args:  cobra.NoArgs,
		Run:   func(*cobra.Command, []string) { fmt.Println("pgb", versionString()) },
	}
}
