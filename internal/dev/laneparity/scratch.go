//go:build dev

package laneparity

import (
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// makeScratch makes a run's temporary directory in parent (the temporary directory, $TMPDIR, when
// parent is empty) once the account's real home has been ruled out as the place for it (CRW-1186):
// every case root, Codex home and runtime link of a run lies below it.
func makeScratch(parent, pattern string) (string, error) {
	dir := parent
	if dir == "" {
		dir = os.TempDir()
	}
	if err := homeguard.Refuse(dir); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, pattern)
}
