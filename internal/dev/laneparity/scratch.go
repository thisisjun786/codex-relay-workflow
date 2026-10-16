//go:build dev

package laneparity

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// makeScratch makes a run's temporary directory in parent once the account's real home has been
// ruled out as the place for it (CRW-1186): every case root, Codex home and runtime link of a run
// lies below it. An empty parent is the temporary directory from tmploc, the one place (CRW-1579);
// a TMPDIR or a parent that is not absolute is refused with the reason, and no other place is tried.
func makeScratch(parent, pattern string) (string, error) {
	if parent != "" {
		if err := homeguard.Refuse(parent); err != nil {
			return "", err
		}
		return tmploc.MkdirTempIn(parent, pattern)
	}
	dir, err := tmploc.Dir()
	if err != nil {
		return "", err
	}
	if err := homeguard.Refuse(dir); err != nil {
		return "", err
	}
	return tmploc.MkdirTemp(pattern)
}
