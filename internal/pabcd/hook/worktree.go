package hook

import (
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// Red-phase stub: the signatures the tests need, answering nothing.

type WorktreeIdentity struct {
	Managed      bool
	WorktreesDir string
	Slot         string
	SlotRoot     string
	CheckoutRoot string

	cwd string
}

func candidateWorktreeRoots(host.LookupEnv) ([]string, error)            { return nil, nil }
func canonicalize(p string) string                                       { return p }
func detectManagedWorktree(string, host.LookupEnv) WorktreeIdentity      { return WorktreeIdentity{} }
func detectRenameIntent(string) bool                                     { return false }
func buildSessionStartContext(WorktreeIdentity, string) string           { return "" }
func parseRaw(string) map[string]any                                     { return nil }
func ensureStateDir(*os.Root) error                                      { return nil }
func HandleWorktreeGuard(string, host.LookupEnv) (event, context string) { return "", "" }
