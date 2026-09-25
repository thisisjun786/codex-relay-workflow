package execution

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Fixture values copied from packages/codex-thread-bridge/tests/conftest.py and test_execution.py.
const (
	model        = "anthropic/claude-opus-5-5" // conftest MODEL
	effort       = "xhigh"                     // conftest EFFORT
	parentModel  = "anthropic/claude-opus-5-5" // PARENT_MODEL
	parentEffort = "xhigh"                     // PARENT_EFFORT
	unapproved   = "openai/gpt-6-astra"        // UNAPPROVED
	solModel     = "openai/gpt-5.6-sol"
)

// supersededParent is SUPERSEDED_PARENT; supersededChild is SUPERSEDED_CHILD.
var (
	supersededParent = [2]string{"devin/swe-2", "max"}
	supersededChild  = [2]string{"anthropic/claude-opus-5", "xhigh"}
)

type doc = map[string]any

func load(t *testing.T, document doc) (Policy, error) {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return FromBytes(raw, "test-policy")
}

func mustLoad(t *testing.T, document doc) Policy {
	t.Helper()
	p, err := load(t, document)
	if err != nil {
		t.Fatalf("policy refused: %v", err)
	}
	return p
}

// rolesPolicy is test_execution.py roles_policy.
func rolesPolicy(roles doc, allowed bool) doc {
	policy := doc{}
	if allowed {
		policy["allowed"] = []any{doc{"model": model, "efforts": []any{effort}}, doc{"model": supersededParent[0], "efforts": []any{supersededParent[1]}}}
	}
	if roles == nil {
		roles = doc{"supervisor": doc{"expectation": "record"}, "parent": doc{"model": parentModel, "reasoningEffort": parentEffort}, "child": doc{"model": model, "reasoningEffort": effort}}
	}
	policy["roles"] = roles
	return policy
}

// policyFor is test_execution.py policy_for with the directory already resolved.
func policyFor(directory string) doc {
	return doc{
		"allowed":    []any{doc{"model": model, "efforts": []any{effort}}, doc{"model": solModel, "efforts": []any{"high"}}},
		"exceptions": doc{"one-task": doc{"model": unapproved, "reasoningEffort": "high", "cwd": []any{directory}, "reason": "the operator's note, which no caller ever sees"}},
	}
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func refused(t *testing.T, err error) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want ExecutionRefused, got %v", err)
	}
	return r
}

func policyError(t *testing.T, err error) *PolicyError {
	t.Helper()
	var e *PolicyError
	if !errors.As(err, &e) {
		t.Fatalf("want ExecutionPolicyError, got %v", err)
	}
	return e
}

func authorize(t *testing.T, p Policy, in Input) Authorized {
	t.Helper()
	got, err := p.Authorize(in)
	if err != nil {
		t.Fatalf("refused %+v: %v", in, err)
	}
	return got
}

func writePolicy(t *testing.T, dir string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sameStrings(got []any, want ...string) bool {
	return slices.Equal(got, anys(want))
}
