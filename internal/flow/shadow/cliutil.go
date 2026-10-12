package shadow

import (
	"errors"
	"io/fs"
	"path"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// loadSibling resolves workflow(name) to <name>.js and workflow({scriptPath}) to that relative path, both inside the
// calling script's workflows directory (the rule `crw flow run` applies).
func loadSibling(dir fs.FS, name, scriptPath string) (*script.Program, error) {
	rel := scriptPath
	if rel == "" {
		rel = name
		if !strings.HasSuffix(rel, ".js") {
			rel += ".js"
		}
	}
	rel = path.Clean(rel)
	if !fs.ValidPath(rel) {
		return nil, errors.New(rel + " is outside the workflows directory")
	}
	text, err := fs.ReadFile(dir, rel)
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(dir, path.Dir(rel))
	if err != nil {
		return nil, err
	}
	return script.Load(script.Source{Name: rel, Text: text, Dir: sub})
}
