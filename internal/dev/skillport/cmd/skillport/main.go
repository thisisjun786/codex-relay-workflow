//go:build dev

// Command skillport stages CXC skills outside the plugin root and checks the staged copies; see the
// skillport package. It is built only with -tags dev.
package main

import (
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/skillport"
)

func main() { os.Exit(skillport.Run(os.Args[1:], os.Stdout, os.Stderr)) }
