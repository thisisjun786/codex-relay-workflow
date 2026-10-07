package install

import (
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/execfile"
)

// writeExecutable writes a file this process will execute: the fork-locked write itself lives in
// execfile, so the install package's subpackages can reach it too. See execfile.WriteExecutable
// for why the lock is held.
func writeExecutable(target string, body []byte, mode os.FileMode) error {
	return execfile.WriteExecutable(target, body, mode)
}
