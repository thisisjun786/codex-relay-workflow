package harness

import (
	"context"
	"time"

	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// promptRoleReadBudget bounds the registry read: the hook answers the host's prompt, so a slow store is no role evidence.
const promptRoleReadBudget = 2 * time.Second

// registryPromptRole is the production role reader of the UserPromptSubmit leg (CRW-1084; the CRW-386 seam): the roles the
// relay registry's live scope bindings give this hook session, read only through store.SessionScopeRoles. A session bound only as
// a parent is a project parent, one bound only as a child is a dispatched task; no binding, both roles, any other role, or a
// store that cannot be selected or read is unknown, so the prompt's own scope words decide and nothing is claimed.
func registryPromptRole(_, sessionID string) pabcdhook.PromptRole {
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
	case "child":
		return pabcdhook.PromptRoleTask
	}
	return pabcdhook.PromptRoleUnknown
}
