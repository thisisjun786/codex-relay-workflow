package guidancerecord

import (
	"fmt"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// guard is the check a test package installs (RefuseAccountHome); a record path it refuses is neither read nor written.
var guard struct {
	sync.Mutex
	check func(path string) bool
}

// allowed reports whether a record path passes the installed check (always, when none is installed).
func allowed(path string) bool {
	guard.Lock()
	check := guard.check
	guard.Unlock()
	return check == nil || check(path)
}

// RefuseAccountHome is a testsupport.Setup (the root it is given is unused). From the moment it runs, a record under the
// account's own Codex home, <passwd home>/.codex, which a lookup without CODEX_HOME and HOME falls back to, is neither read nor
// written, and the cleanup it returns fails naming every such path a test reached for. A test package whose code reaches Record
// runs it from TestMain, so a test whose lookup was not given a temporary home fails instead of writing the real one.
func RefuseAccountHome(string) (func() error, error) {
	account, err := user.Current()
	if err != nil || account.HomeDir == "" {
		return nil, nil // without an account home the fallback reaches none either
	}
	homes := []string{filepath.Join(account.HomeDir, ".codex")}
	if resolved, err := filepath.EvalSymlinks(account.HomeDir); err == nil {
		homes = append(homes, filepath.Join(resolved, ".codex"))
	}
	var mu sync.Mutex
	var refused []string
	guard.Lock()
	guard.check = func(path string) bool {
		for _, home := range homes {
			if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				mu.Lock()
				if !slices.Contains(refused, path) {
					refused = append(refused, path)
				}
				mu.Unlock()
				return false
			}
		}
		return true
	}
	guard.Unlock()
	return func() error {
		guard.Lock()
		guard.check = nil
		guard.Unlock()
		mu.Lock()
		defer mu.Unlock()
		if len(refused) > 0 {
			return fmt.Errorf("tests reached for %d guidance records in the account's real Codex home (give the lookup a temporary CODEX_HOME): %s",
				len(refused), strings.Join(refused, ", "))
		}
		return nil
	}, nil
}
