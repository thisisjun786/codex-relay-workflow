package registry

import "github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"

// BridgePolicy is the exact process snapshot used by role checks. A daemon must
// not reopen the file and hand its transport a different policy after publishing
// the worker receipt. An unresolved snapshot keeps the presence-only policy.
func (p RolePolicy) BridgePolicy() execution.Policy { return p.policy }
