// Package contracttest runs the shared contract corpus (contract/fixtures/<domain>/*.json)
// against the Go build. The grammar is contract/README.md; the fixtures are read in place from
// the checkout, never copied or embedded.
package contracttest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ErrNoModuleRoot is returned when no go.mod is found above this source file.
var ErrNoModuleRoot = errors.New("contracttest: no go.mod above the package source")

// Root is the checkout directory: the nearest ancestor of this source file holding go.mod.
func Root() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", ErrNoModuleRoot
	}
	for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("contracttest: stat go.mod in %s: %w", dir, err)
		}
		if filepath.Dir(dir) == dir {
			return "", ErrNoModuleRoot
		}
	}
}
