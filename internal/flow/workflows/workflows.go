// Package workflows holds the built-in workflow scripts of the binary: project-lane.js and, as the issues that own them land,
// the others. They are scripts of the CRW workflow format (docs/flow/script.md) whose helpers are the library under lib/.
package workflows

import (
	"embed"
	"fmt"
	"io/fs"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

//go:embed *.js
var files embed.FS

// FS is the embedded scripts: <name>.js at its root.
var FS fs.FS = files

// Source is the text of a built-in script.
func Source(name string) ([]byte, error) {
	b, err := files.ReadFile(name + ".js")
	if err != nil {
		return nil, fmt.Errorf("workflows: no built-in workflow %q", name)
	}
	return b, nil
}

// Load loads a built-in script.
func Load(name string) (*script.Program, error) {
	text, err := Source(name)
	if err != nil {
		return nil, err
	}
	return script.Load(script.Source{Name: name + ".js", Text: text, Dir: files})
}

// Names are the built-in scripts.
func Names() []string {
	entries, _ := fs.ReadDir(files, ".")
	var out []string
	for _, e := range entries {
		if n := e.Name(); len(n) > 3 && n[len(n)-3:] == ".js" {
			out = append(out, n[:len(n)-3])
		}
	}
	return out
}
