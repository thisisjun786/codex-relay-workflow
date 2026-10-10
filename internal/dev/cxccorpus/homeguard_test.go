//go:build dev

package cxccorpus

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// CRW-1186: a case root is made below the scratch directory the caller names; a scratch directory in the
// account's real home (.codex, .crw, .local/share/crw-runtime) is refused before anything is made.
func TestNewCase_refusesAScratchInTheAccountHome(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(homeguard.SetAccountHome(home))
	for _, rel := range []string{".codex", ".crw/scratch", ".local/share/crw-runtime/x"} {
		scratch := filepath.Join(home, rel)
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatal(err)
		}
		c, err := NewCase(scratch, "CRW_HOME", Given{})
		var refusal *homeguard.Error
		if !errors.As(err, &refusal) {
			t.Errorf("%s: want a refusal of the account home, got %v", rel, err)
		}
		if c != nil {
			t.Errorf("%s: a case came back at %s", rel, c.Root)
		}
		if entries, _ := os.ReadDir(scratch); len(entries) != 0 {
			t.Errorf("%s holds %d entries", rel, len(entries))
		}
	}
}
