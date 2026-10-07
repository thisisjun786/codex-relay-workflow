package gui

// Linking testsupport sets CRW_REFUSE_LIVE_STATE=1 for this test binary and every process it
// starts, so no test here opens a store under a live relay state directory (decisions.md 46).
// The package reaches the store through internal/policystore, and the guard in internal/testsupport
// requires every test binary that links the store to link this too.
import _ "github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
