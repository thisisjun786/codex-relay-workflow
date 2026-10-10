package migrate

// Linking testsupport sets CRW_REFUSE_LIVE_STATE=1 for this test binary and every process it
// starts, so no test here opens a store under a live relay state directory (decisions.md 46).
// The package links the relay store through the harness, whose UserPromptSubmit leg reads the registry role (CRW-1084).
import _ "github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
