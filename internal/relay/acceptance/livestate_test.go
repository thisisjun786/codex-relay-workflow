package acceptance

// Linking testsupport sets CRW_REFUSE_LIVE_STATE=1 for this test binary and every process it
// starts, so no test here opens a store under a live relay state directory (decisions.md 46).
// internal/testsupport's livestate_test.go holds this package to the rule because it links
// internal/relay/store.
import _ "github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
