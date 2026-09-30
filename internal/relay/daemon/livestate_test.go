package daemon

// Linking testsupport sets CRW_REFUSE_LIVE_STATE=1 for this test binary and every process it
// starts, so no test here opens a store under a live relay state directory (decisions.md 46).
import _ "github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
