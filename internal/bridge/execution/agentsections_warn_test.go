package execution

import (
	"strings"
	"testing"
)

// CRW-1330 (evaluation 5a1a64c2 d1, criterion c3): the flow no longer reads agent-roles or agent-aliases, so a section that
// does not parse is a warning the loader keeps, with the section ignored; the file still loads for the relay and the bridge,
// and still reports that it declares the section so crw flow roles and crw doctor flow-roles can say the flow no longer reads it.
func TestCRW1330_MalformedAgentSectionsLoadWithAWarning(t *testing.T) {
	missing := agentRolesDoc()
	delete(missing, "executor-t1")
	withAliases := func(rows ...any) doc {
		policy := agentPolicy(agentRolesDoc())
		policy["agent-aliases"] = rows
		return policy
	}
	for _, row := range []struct {
		name     string
		document doc
		want     string
	}{
		{"a missing role", agentPolicy(missing), "executor-t1"},
		{"two pairs", agentPolicy(withRole("executor", doc{"pairs": pairList(pairDoc(agentHaiku, "xhigh"), pairDoc(agentSonnet, "xhigh"))})), "exactly one pair"},
		{"a missing field", agentPolicy(withRole("executor", doc{"model": agentHaiku})), "reasoningEffort"},
		{"an unknown role", agentPolicy(withRole("planner", pairDoc(agentHaiku, "xhigh"))), "planner"},
		{"not an object", doc{"agent-roles": []any{}}, "JSON object"},
		{"aliases without roles", doc{"allowed": agentAllowed(), "agent-aliases": []any{doc{"key": "opus", "role": "architect"}}}, "declares no agent-roles"},
		{"overlapping alias rows", withAliases(doc{"key": "opus", "role": "architect"}, doc{"key": "opus", "role": "executor"}), "overlap"},
		{"an empty alias table", withAliases(), "agent-aliases"},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, err := load(t, row.document)
			if err != nil {
				t.Fatalf("the file is refused: %v", err)
			}
			joined := strings.Join(p.Warnings(), "\n")
			if !strings.Contains(joined, row.want) || !strings.Contains(joined, "ignored") || !strings.Contains(joined, "helper role store") {
				t.Fatalf("want %q in the warnings %q", row.want, p.Warnings())
			}
			if !p.DeclaresAgentRoles() {
				t.Fatal("a file with the sections reports that it declares them")
			}
			if _, ok := p.AgentRoles(); ok {
				t.Fatal("an ignored section is not described as declared roles")
			}
			if _, listed := p.Summary()["agentRoles"]; listed {
				t.Fatal("the policy description lists no roles of an ignored section")
			}
		})
	}
	// A malformed section alone is still a policy (presence only), as a well-formed one is.
	p := mustLoad(t, agentPolicy(missing))
	if p.Mode() != "presence_only" {
		t.Fatal(p.Mode())
	}
	// A well-formed section and a file without one: no warning; only the first declares the section.
	if p := mustLoad(t, agentPolicy(agentRolesDoc())); len(p.Warnings()) != 0 || !p.DeclaresAgentRoles() {
		t.Fatalf("%q %v", p.Warnings(), p.DeclaresAgentRoles())
	}
	if p := mustLoad(t, rolesPolicy(nil, true)); len(p.Warnings()) != 0 || p.DeclaresAgentRoles() {
		t.Fatalf("%q %v", p.Warnings(), p.DeclaresAgentRoles())
	}
	// The sections the relay and the bridge run on keep their refusals.
	broken := agentPolicy(missing)
	broken["allowed"] = []any{}
	policyError(t, func() error { _, err := load(t, broken); return err }())
}
