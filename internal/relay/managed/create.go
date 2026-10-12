package managed

import (
	"context"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// create is the one place a managed start asks the host for its child. Every refusal that can be decided
// without the child is decided here, in the order it was always decided, and the last of them directly
// before the host is asked: readiness, the project scope, the ledger, then the project scope again with
// the request's own fields (requestRefusal), and last the live parent's settings against the request's declaration. A request that names a project holds the project's lock
// (LockProject) from before the first of these, and the caller keeps it until the child is registered,
// so a writer of the project's parent binding, which takes the lock exclusively, can change the binding
// neither between the last ask and the creation nor between the creation and the registration (where
// registration would refuse a child whose thread already exists).
//
// It returns the host's receipt, or the readiness refusal code that stands in for it, the function
// that lets the lock go (never nil, may be called more than once, and must be called on every way out of
// the caller), and, when the adapter keeps the creation's turn watch open (HeldCreator), the hold on it:
// the caller waits for the standby turn under it and lets it go after the wait.
func (m *Start) create(ctx context.Context, id Identity, req, ledger map[string]any, check *parentCheck) (map[string]any, string, func(), CreationHold, error) {
	release := func() {}
	if project := pyjson.Text(req["projectKey"]); project != "" {
		held, err := LockProject(ctx, m.Store.Path, project)
		if err != nil {
			return nil, "", release, nil, err
		}
		var once sync.Once
		release = func() { once.Do(func() { _ = held() }) }
	}
	readiness, err := m.ready(ctx, req)
	if err != nil {
		return nil, "", release, nil, err
	}
	if readiness != "" {
		return nil, readiness, release, nil, nil
	}
	// Asked right before the effect, as readiness is: the binding may have moved since the first ask.
	if err := m.scopeRefusal(ctx, id, req); err != nil {
		return nil, "", release, nil, err
	}
	if err := m.Adapter.RequireLedger(ctx, ledger); err != nil {
		return nil, "", release, nil, err
	}
	// The ledger check is an adapter call and takes time: the last ask is the one after it, and nothing but
	// the host call follows it.
	if err := m.scopeRefusal(ctx, id, req); err != nil {
		return nil, "", release, nil, err
	}
	if err := m.requestRefusal(ctx, req); err != nil {
		return nil, "", release, nil, err
	}
	// The one thing the host is asked before the creation: whether the parent runs the settings the request declares for it
	// (parent_settings.go). The declaration is not believed, and nothing is created or recorded for one the thread does not bear out.
	*check = parentCheck{}
	live, reason, observed, err := m.checkParentSettings(ctx, req)
	if err != nil {
		return nil, "", release, nil, err
	}
	if reason != "" {
		check.observed = observed
		return nil, reason, release, nil, nil
	}
	check.live = live
	child := pyjson.Map(req["child"])
	settings := pyjson.Map(child["settings"])
	sandbox := pyjson.Map(settings["sandbox"])
	kind := map[string]string{"workspaceWrite": "workspace-write", "readOnly": "read-only", "dangerFullAccess": "danger-full-access"}[pyjson.Text(sandbox["type"])]
	// The name the host is given is normalized; the request and its fingerprint keep the title the parent wrote.
	request := CreateThreadRequest{RequestID: id.CreateRequestID, CWD: pyjson.Text(settings["cwd"]), Prompt: bootstrap, Title: childTitle(id.IssueKey, pyjson.Text(child["title"])), Sandbox: kind, Model: pyjson.Text(settings["model"]), ReasoningEffort: pyjson.Text(settings["reasoningEffort"]), RuntimeWorkspaceRoots: stringsOf(settings["runtimeWorkspaceRoots"]), ExpectedSandboxPolicy: sandbox, Role: "child", MCPProfile: pyjson.Text(settings["mcpProfile"])}
	if held, ok := m.Adapter.(HeldCreator); ok {
		receipt, hold, err := held.CreateThreadHeld(ctx, request)
		return receipt, "", release, hold, err
	}
	receipt, err := m.Adapter.CreateThread(ctx, request)
	return receipt, "", release, nil, err
}
