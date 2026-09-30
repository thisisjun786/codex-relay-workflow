package install

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// goProvides answers whether a Go runtime directory serves rel, a path relative to it as a
// registration or the Stop settings reach it through the pointer: bin/crw and its compatibility
// links (docs/port/decisions.md 10), and nothing else a command could run. Every promotion and
// rollback moves the pointer to a Go runtime, so it needs no reading of the target to answer; a
// path it does not serve is one the swap would leave naming nothing.
func goProvides(rel string) bool {
	switch rel {
	case "", "bin", "bin/" + Binary:
		return true
	}
	for _, link := range definition.Links() {
		if rel == "bin/"+link {
			return true
		}
	}
	return false
}

// throughPointer is the part of an absolute path that lies beyond the pointer: "" for the
// pointer itself, and ok false for a path the pointer does not lead to. The pointer is found by
// identity, not only by spelling: an ancestor of path that is the pointer link itself (Lstat,
// os.SameFile) - reached through a symlinked home, /tmp -> /private/tmp or a bind mount - is the
// pointer as much as its recorded spelling is.
func throughPointer(path, pointerPath string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	path, pointerPath = filepath.Clean(path), filepath.Clean(pointerPath)
	if path == pointerPath {
		return "", true
	}
	if rest, ok := strings.CutPrefix(path, pointerPath+"/"); ok {
		return rest, true
	}
	link, err := os.Lstat(pointerPath)
	if err != nil {
		return "", false
	}
	for dir, rest := path, ""; ; {
		if info, err := os.Lstat(dir); err == nil && os.SameFile(info, link) {
			return rest, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		if rest == "" {
			rest = filepath.Base(dir)
		} else {
			rest = filepath.Base(dir) + "/" + rest
		}
		dir = parent
	}
}

// installsAt is every install entry of component name whose runtime directory is exactly
// environment (both resolved), newest last. A directory that merely contains one - the
// destination, or / - is not a runtime the record lists.
func installsAt(rec Object, name, environment string) []Object {
	want, err := record.Resolve(environment)
	if err != nil {
		return nil
	}
	components, _ := record.Get(rec, "components").(Object)
	component, _ := record.Get(components, name).(Object)
	entries, _ := record.Get(component, "installs").([]any)
	var found []Object
	for _, raw := range entries {
		install, ok := raw.(Object)
		if !ok {
			continue
		}
		if location, _ := record.Get(install, "location").(string); location == "" {
			continue
		}
		if resolved, err := record.Resolve(environmentOf(install)); err == nil && resolved == want {
			found = append(found, install)
		}
	}
	return found
}
