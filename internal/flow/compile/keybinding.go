package compile

import (
	"context"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Rules of the warnings that compare a project key with the owner's binding.
const (
	RuleProjectKeyBound     = "project_key_bound"
	RuleProjectKeyUnchecked = "project_key_unchecked"
)

// Binding is what a project key is compared with: the scope keys of the live parent bindings of the owner thread. Err is why they
// could not be read.
type Binding struct {
	Thread string
	Scopes []string
	Err    error
}

// KeyWarning compares a project key with the scope keys of the owner's live parent bindings, as `crw flow start` does (CRW-1317, the
// fact project_key_bound): a key the thread is bound to answers nothing, a key it holds no binding of is project_key_bound, a missing
// thread or unreadable bindings is project_key_unchecked, and none refuses. ref names what the key stands for when the caller knows it
// (the milestone ref of `crw flow plan-input`); show spells a bound key for the reader (nil: as it is).
func KeyWarning(key, ref string, b Binding, show func(string) string) (InputWarning, bool) {
	of := ""
	if ref != "" {
		of = " (milestone " + ref + ")"
	}
	switch {
	case b.Thread == "":
		return InputWarning{Rule: RuleProjectKeyUnchecked, Detail: "the project key is not compared with a binding: pass --thread (or run in a session that has CODEX_THREAD_ID) to name the owner thread"}, true
	case b.Err != nil:
		return InputWarning{Rule: RuleProjectKeyUnchecked, Detail: fmt.Sprintf("the live 자식 bindings of thread %s cannot be read, so the project key %s%s is not compared: %v", b.Thread, key, of, b.Err)}, true
	}
	// exactly, as `crw flow start` and the coordinator's first claim compare it: a key that differs only by case is another key there
	caseOnly := ""
	for _, s := range b.Scopes {
		switch {
		case s == key:
			return InputWarning{}, false
		case caseOnly == "" && strings.EqualFold(s, key):
			caseOnly = s
		}
	}
	holds := "thread " + b.Thread + " holds no live 자식 binding"
	if len(b.Scopes) > 0 {
		shown := make([]string, len(b.Scopes))
		for i, s := range b.Scopes {
			shown[i] = s
			if show != nil {
				shown[i] = show(s)
			}
		}
		holds = "thread " + b.Thread + " is the live 자식 of project " + strings.Join(shown, ", ")
	}
	hint := ""
	if caseOnly != "" {
		hint = fmt.Sprintf(" (the binding %s differs from %s only by case, and the keys are compared exactly: pass the key with the same spelling the binding was made under)", caseOnly, key)
	}
	return InputWarning{Rule: RuleProjectKeyBound, Detail: fmt.Sprintf("the plan's project key is %s%s; %s%s: a plan's project key is the scope_key of the orchestrator's 자식 binding, and the run's first claim is refused (scope_role_mismatch) under another key", key, of, holds, hint)}, true
}

// ReadScopes reads the project keys of the live parent bindings of thread from the relay store of the state, read-only: the reading
// `crw flow start` compares a plan's project key with (CRW-1317).
func ReadScopes(ctx context.Context, thread, state, socket string) ([]string, error) {
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return nil, err
		}
	}
	sel, err := store.ResolveStateDir(state, socket)
	if err != nil {
		return nil, err
	}
	db := sel.DBPath()
	if err := store.CheckReadable(db); err != nil {
		return nil, fmt.Errorf("there is no relay store at %s (%s): %w", db, sel.Detail, err)
	}
	st, err := store.OpenReadOnlyStore(ctx, db)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return dag.LiveParentScopes(ctx, st.DB, thread)
}
