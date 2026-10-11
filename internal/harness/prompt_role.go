package harness

import (
	"context"
	"time"

	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// promptRoleReadBudget bounds the registry read: the hook answers the host's prompt, so a slow store is no role evidence.
const promptRoleReadBudget = 2 * time.Second

// RegistryPromptRole is the production role reader of the hook legs that withhold the loop from a coordinating seat (CRW-1084
// for UserPromptSubmit; CRW-386 for Stop, the SessionStart map affordance and the compaction restore): the roles the relay
// registry's live scope bindings give this hook session, read only through store.SessionScopeRoles (no registry write, no store
// created or migrated). A session bound only as a parent is a project parent, only as a supervisor a supervisor seat, only as a
// child a dispatched task; no binding, two or more roles, any other role, or a store that cannot be selected or read within the
// read budget is unknown, so the oracle's behavior holds and nothing is claimed.
func RegistryPromptRole(_, sessionID string) pabcdhook.PromptRole {
	selection, err := store.ResolveStateDir("", "")
	if err != nil {
		return pabcdhook.PromptRoleUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), promptRoleReadBudget)
	defer cancel()
	roles, ok := store.SessionScopeRoles(ctx, selection, sessionID)
	if !ok || len(roles) != 1 {
		return pabcdhook.PromptRoleUnknown
	}
	switch roles[0] {
	case "parent":
		return pabcdhook.PromptRoleParent
	case "supervisor":
		return pabcdhook.PromptRoleSupervisor
	case "child":
		return pabcdhook.PromptRoleTask
	}
	return pabcdhook.PromptRoleUnknown
}
