package execution

import (
	"reflect"
	"strings"
	"testing"
)

// CRW-1196: the policy file's agent-roles section names the pair of each of the six roles a flow engine runs,
// and its agent-aliases section is the table that maps a Claude script's options onto one of them.

const (
	agentOpus   = "anthropic/claude-opus-5-5"
	agentHaiku  = "anthropic/claude-haiku-5"
	agentSonnet = "anthropic/claude-sonnet-5-5"
	agentSol    = "gpt-6.1-sol"
)

// agentRolesDoc is the agent-roles section of the design: one pair per role, the reviewers on another model family.
func agentRolesDoc() doc {
	return doc{
		"architect":      doc{"model": agentOpus, "reasoningEffort": "xhigh"},
		"executor":       doc{"model": agentHaiku, "reasoningEffort": "xhigh"},
		"executor-t1":    doc{"model": agentSonnet, "reasoningEffort": "xhigh"},
		"reviewer":       doc{"model": agentSol, "reasoningEffort": "high"},
		"reviewer-final": doc{"model": agentSol, "reasoningEffort": "xhigh"},
		"explorer":       doc{"model": agentHaiku, "reasoningEffort": "high"},
	}
}

// agentAllowed approves every pair agentRolesDoc declares.
func agentAllowed() []any {
	return []any{
		doc{"model": agentOpus, "efforts": []any{"xhigh"}},
		doc{"model": agentHaiku, "efforts": []any{"high", "xhigh"}},
		doc{"model": agentSonnet, "efforts": []any{"xhigh"}},
		doc{"model": agentSol, "efforts": []any{"high", "xhigh"}},
	}
}

func agentPolicy(roles doc) doc { return doc{"agent-roles": roles} }

func withRole(name string, entry doc) doc {
	roles := agentRolesDoc()
	roles[name] = entry
	return roles
}

func TestAgentRolesOnePairEach(t *testing.T) {
	p := mustLoad(t, agentPolicy(agentRolesDoc()))
	declared, ok := p.AgentRoles()
	if !ok {
		t.Fatal("a file with an agent-roles section reports it")
	}
	want := map[string][2]string{
		"architect": {agentOpus, "xhigh"}, "executor": {agentHaiku, "xhigh"}, "executor-t1": {agentSonnet, "xhigh"},
		"reviewer": {agentSol, "high"}, "reviewer-final": {agentSol, "xhigh"}, "explorer": {agentHaiku, "high"},
	}
	if len(declared.Roles) != len(want) {
		t.Fatalf("roles %+v", declared.Roles)
	}
	for _, role := range declared.Roles {
		pair, known := want[role.Name]
		if !known || role.Pair.Model != pair[0] || role.Pair.Effort != pair[1] {
			t.Fatalf("role %s: %+v", role.Name, role.Pair)
		}
	}
	// The same pair as a list of one is the one-pair form written another way.
	listed := agentRolesDoc()
	listed["executor"] = doc{"pairs": pairList(pairDoc(agentHaiku, "xhigh"))}
	mustLoad(t, agentPolicy(listed))

	refusals := []struct {
		name  string
		roles doc
		want  string
	}{
		{"two pairs", withRole("executor", doc{"pairs": pairList(pairDoc(agentHaiku, "xhigh"), pairDoc(agentSonnet, "xhigh"))}), "exactly one pair"},
		{"no pair", withRole("executor", doc{}), "executor"},
		{"model only", withRole("executor", doc{"model": agentHaiku}), "reasoningEffort"},
		{"pairs beside model", withRole("executor", doc{"model": agentHaiku, "reasoningEffort": "xhigh", "pairs": pairList(pairDoc(agentHaiku, "xhigh"))}), "both pairs"},
		{"unknown role", withRole("planner", pairDoc(agentHaiku, "xhigh")), "planner"},
		{"relay role", withRole("child", pairDoc(agentHaiku, "xhigh")), "child"},
		{"unknown key", withRole("executor", doc{"model": agentHaiku, "reasoningEffort": "xhigh", "temperature": 1}), "temperature"},
		{"empty section", doc{}, "architect"},
	}
	for _, row := range refusals {
		t.Run(row.name, func(t *testing.T) {
			_, err := load(t, agentPolicy(row.roles))
			if message := policyError(t, err).Error(); !strings.Contains(message, row.want) {
				t.Fatalf("want %q in %q", row.want, message)
			}
		})
	}
	// Every role is declared: a section that leaves one out names it.
	missing := agentRolesDoc()
	delete(missing, "reviewer-final")
	if _, err := load(t, agentPolicy(missing)); err == nil || !strings.Contains(err.Error(), "reviewer-final") {
		t.Fatalf("a missing role is refused by name: %v", err)
	}
	// An autoCompactTokenLimit belongs to the pair, as it does in a relay role.
	limited := agentRolesDoc()
	limited["executor"] = doc{"model": agentHaiku, "reasoningEffort": "xhigh", "autoCompactTokenLimit": 550000}
	declared, _ = mustLoad(t, agentPolicy(limited)).AgentRoles()
	for _, role := range declared.Roles {
		if role.Name == "executor" && (role.Pair.AutoCompactTokenLimit == nil || *role.Pair.AutoCompactTokenLimit != 550000) {
			t.Fatalf("limit %+v", role.Pair)
		}
	}
	// The relay roles keep their rules next to the new section, and a file without it reports none.
	both := agentPolicy(agentRolesDoc())
	both["roles"] = doc{"parent": doc{"model": parentModel, "reasoningEffort": parentEffort}}
	if _, ok := mustLoad(t, both).AgentRoles(); !ok {
		t.Fatal("agent-roles beside roles")
	}
	if _, ok := mustLoad(t, rolesPolicy(nil, true)).AgentRoles(); ok {
		t.Fatal("a file without agent-roles reports none")
	}
	// The section alone is a policy: it is not "neither allowed nor roles".
	if mode := mustLoad(t, agentPolicy(agentRolesDoc())).Mode(); mode != "presence_only" {
		t.Fatal(mode)
	}
}

func TestAgentRolesPairsMustBeAllowed(t *testing.T) {
	document := agentPolicy(agentRolesDoc())
	document["allowed"] = agentAllowed()
	mustLoad(t, document)

	for _, row := range []struct{ role, model, effort string }{
		{"architect", agentOpus, "high"},            // an allowed model at an effort the list does not approve
		{"executor", "openai/gpt-6-astra", "xhigh"}, // a model the list does not name
		{"reviewer-final", agentSol, "max"},
	} {
		roles := withRole(row.role, pairDoc(row.model, row.effort))
		policy := agentPolicy(roles)
		policy["allowed"] = agentAllowed()
		_, err := load(t, policy)
		message := policyError(t, err).Error()
		if !strings.Contains(message, row.role) || !strings.Contains(message, "allowed") {
			t.Fatalf("%+v: %s", row, message)
		}
	}
	// Without an allowlist nothing is compared, as for the relay roles.
	mustLoad(t, agentPolicy(withRole("executor", pairDoc("inferhub/deepseek-v4.1-flash", "xhigh"))))
}

func TestReviewerFamilyDiffersFromExecutor(t *testing.T) {
	for _, row := range []struct {
		name  string
		roles doc
	}{
		{"reviewer on the executor's model", withRole("reviewer", pairDoc(agentHaiku, "high"))},
		{"reviewer-final on the executor-t1's model", withRole("reviewer-final", pairDoc(agentSonnet, "xhigh"))},
		{"reviewer on another model of the executor's vendor", withRole("reviewer", pairDoc("anthropic/claude-sonnet-5", "high"))},
		{"reviewer under a bare name of the executor's family", withRole("reviewer", pairDoc("claude-opus-5-5", "high"))},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := load(t, agentPolicy(row.roles))
			message := policyError(t, err).Error()
			if !strings.Contains(message, "reviewer") || !strings.Contains(message, "family") {
				t.Fatal(message)
			}
		})
	}
	// The architect and the explorer are not authors of a reviewed change: they may share a family with a reviewer.
	roles := agentRolesDoc()
	roles["architect"] = pairDoc(agentSol, "xhigh")
	mustLoad(t, agentPolicy(roles))

	for model, family := range map[string]string{
		"anthropic/claude-haiku-5": "claude", "claude-opus-5-5": "claude", "gpt-6.1-sol": "gpt", "openai/gpt-5.6-sol": "gpt",
		"inferhub/deepseek-v4.1-flash": "deepseek", "sol": "sol",
	} {
		if got := ModelFamily(model); got != family {
			t.Fatalf("ModelFamily(%q) = %q, want %q", model, got, family)
		}
	}
}

// TestAliasTableMapsEachPairToOneRole walks every (key, effort, isolation) combination of the table.
func TestAliasTableMapsEachPairToOneRole(t *testing.T) {
	declared, _ := mustLoad(t, agentPolicy(agentRolesDoc())).AgentRoles()
	if len(declared.Aliases) != 6 {
		t.Fatalf("the design's alias table has six rows, got %+v", declared.Aliases)
	}
	type outcome struct{ role, refuse string }
	want := map[[3]string]outcome{
		{"opus", "", ""}:                         {"architect", ""},
		{"haiku", "", "worktree"}:                {"executor", ""},
		{"haiku", "", ""}:                        {"", "alias_ambiguous"},
		{"sonnet", "", ""}:                       {"executor-t1", ""},
		{"ocx-gpt-6-1-sol", "xhigh", ""}:         {"reviewer-final", ""},
		{"ocx-gpt-6-1-sol", "high", ""}:          {"reviewer", ""},
		{"ocx-gpt-6-1-sol", "", ""}:              {"reviewer", ""},
		{"haiku", "medium", "worktree"}:          {"executor", ""},
		{"haiku", "medium", ""}:                  {"", "alias_ambiguous"},
		{"ocx-gpt-6-1-sol", "xhigh", "worktree"}: {"reviewer-final", ""},
		{"gpt-x", "", ""}:                        {"", ""},
	}
	keys := []string{"opus", "haiku", "sonnet", "ocx-gpt-6-1-sol", "gpt-x"}
	efforts := []string{"", "low", "medium", "high", "xhigh"}
	isolations := []string{"", "worktree", "none"}
	for _, key := range keys {
		for _, effort := range efforts {
			for _, isolation := range isolations {
				matches := 0
				for _, row := range declared.Aliases {
					if row.matches(key, effort, isolation) {
						matches++
					}
				}
				row, found := declared.Match(key, effort, isolation)
				if matches > 0 != found {
					t.Fatalf("%s/%s/%s: %d rows match, Match found=%v", key, effort, isolation, matches, found)
				}
				// Whatever matches maps to exactly one target: a role, or one refusal.
				if found && (row.Role == "") == (row.Refuse == "") {
					t.Fatalf("%s/%s/%s: row %+v is not exactly one role or one refusal", key, effort, isolation, row)
				}
				if expected, listed := want[[3]string{key, effort, isolation}]; listed {
					if row.Role != expected.role || row.Refuse != expected.refuse {
						t.Fatalf("%s/%s/%s: got %+v, want %+v", key, effort, isolation, row, expected)
					}
				}
			}
		}
	}
	// Every role a row names is one of the six, and every role is reachable by its key as a role or an alias.
	roles := map[string]bool{}
	for _, role := range declared.Roles {
		roles[role.Name] = true
	}
	reached := map[string]bool{}
	for _, row := range declared.Aliases {
		if row.Role != "" {
			if !roles[row.Role] {
				t.Fatalf("row %+v names no role", row)
			}
			reached[row.Role] = true
		}
	}
	for _, name := range []string{"architect", "executor", "executor-t1", "reviewer", "reviewer-final"} {
		if !reached[name] {
			t.Fatalf("role %s has no alias row", name)
		}
	}
	if reached["explorer"] {
		t.Fatal("the explorer is named by agentType and never picked by an alias")
	}
	// The aliases that name a role's model: haiku names the executor and the explorer.
	aliases := map[string][]string{}
	for _, role := range declared.Roles {
		aliases[role.Name] = role.Aliases
	}
	if !reflect.DeepEqual(aliases["executor"], []string{"haiku"}) || !reflect.DeepEqual(aliases["explorer"], []string{"haiku"}) ||
		!reflect.DeepEqual(aliases["reviewer-final"], []string{"ocx-gpt-6-1-sol"}) {
		t.Fatalf("aliases %+v", aliases)
	}

	// A file states its own table; rows that overlap without one being more specific are refused when it is read.
	own := func(rows ...any) doc {
		policy := agentPolicy(agentRolesDoc())
		policy["agent-aliases"] = rows
		return policy
	}
	mustLoad(t, own(doc{"key": "opus", "role": "architect"}, doc{"key": "opus", "effort": "xhigh", "role": "reviewer"}))
	for _, row := range []struct {
		name string
		rows []any
		want string
	}{
		{"identical rows", []any{doc{"key": "opus", "role": "architect"}, doc{"key": "opus", "role": "executor"}}, "overlap"},
		{"neither more specific", []any{doc{"key": "haiku", "effort": "high", "role": "executor"}, doc{"key": "haiku", "isolation": "worktree", "role": "executor"}}, "overlap"},
		{"a role and a refusal", []any{doc{"key": "opus", "role": "architect", "refuse": "alias_ambiguous"}}, "exactly one"},
		{"neither", []any{doc{"key": "opus"}}, "exactly one"},
		{"unknown role", []any{doc{"key": "opus", "role": "planner"}}, "planner"},
		{"no key", []any{doc{"role": "architect"}}, "key"},
		{"unknown field", []any{doc{"key": "opus", "role": "architect", "model": "x"}}, "model"},
		{"empty table", []any{}, "agent-aliases"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := load(t, own(row.rows...))
			if message := policyError(t, err).Error(); !strings.Contains(message, row.want) {
				t.Fatalf("want %q in %q", row.want, message)
			}
		})
	}
	// A table without the section it maps onto is a mistake.
	_, err := load(t, doc{"allowed": agentAllowed(), "agent-aliases": []any{doc{"key": "opus", "role": "architect"}}})
	policyError(t, err)
}

// TestAgentRolesDigestAndSummary pins what a reader of the policy sees: the digest is the file's bytes and the
// description lists the roles only when the section is declared.
func TestAgentRolesDigestAndSummary(t *testing.T) {
	plain := mustLoad(t, rolesPolicy(nil, true)).Summary()
	if _, listed := plain["agentRoles"]; listed {
		t.Fatalf("a file without the section describes none: %#v", plain)
	}
	summary := mustLoad(t, agentPolicy(agentRolesDoc())).Summary()
	section, ok := summary["agentRoles"].(map[string]any)
	if !ok {
		t.Fatalf("%#v", summary)
	}
	roles := section["roles"].([]any)
	if len(roles) != 6 || roles[0].(map[string]any)["role"] != "architect" || roles[5].(map[string]any)["role"] != "explorer" {
		t.Fatalf("roles in declaration order of the design: %#v", roles)
	}
	if len(section["aliases"].([]any)) != 6 {
		t.Fatalf("%#v", section["aliases"])
	}
}
