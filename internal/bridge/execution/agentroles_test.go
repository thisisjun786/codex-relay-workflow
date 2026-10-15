package execution

import (
	"reflect"
	"strings"
	"testing"
)

// CRW-1196: the policy file's agent-roles section names the pair of each of the roles a flow engine runs (seven since CRW-1434),
// and its agent-aliases section is the table that maps a Claude script's options onto one of them.

const (
	agentOpus   = "anthropic/claude-opus-5-5"
	agentHaiku  = "anthropic/claude-haiku-5"
	agentSonnet = "anthropic/claude-sonnet-5-5"
	agentSol    = "gpt-6.1-sol"
)

// agentRolesDoc is the agent-roles section of the design: one pair per role, the reviewers and the critic on another model family.
func agentRolesDoc() doc {
	return doc{
		"architect":      doc{"model": agentOpus, "reasoningEffort": "xhigh"},
		"executor":       doc{"model": agentHaiku, "reasoningEffort": "xhigh"},
		"executor-t1":    doc{"model": agentSonnet, "reasoningEffort": "xhigh"},
		"reviewer":       doc{"model": agentSol, "reasoningEffort": "high"},
		"reviewer-final": doc{"model": agentSol, "reasoningEffort": "xhigh"},
		"explorer":       doc{"model": agentHaiku, "reasoningEffort": "high"},
		"critic":         doc{"model": agentSol, "reasoningEffort": "xhigh"},
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
		"critic": {agentSol, "xhigh"},
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
	// CRW-1330: the flow no longer reads the section, so what would have refused it is a warning naming the same mistake, the
	// section ignored and the file loaded (agentsections_warn_test.go).
	for _, row := range refusals {
		t.Run(row.name, func(t *testing.T) {
			if message := agentSectionWarning(t, agentPolicy(row.roles)); !strings.Contains(message, row.want) {
				t.Fatalf("want %q in %q", row.want, message)
			}
		})
	}
	// Every role is declared: a section that leaves one out names it.
	missing := agentRolesDoc()
	delete(missing, "reviewer-final")
	if message := agentSectionWarning(t, agentPolicy(missing)); !strings.Contains(message, "reviewer-final") {
		t.Fatalf("a missing role is named: %v", message)
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
	if warnings := mustLoad(t, document).Warnings(); len(warnings) != 0 {
		t.Fatalf("agreeing sections warn: %q", warnings)
	}

	// CRW-1330: a flow run no longer reads agent-roles, so a disagreement with the allowed list is a warning the
	// loader keeps (crw flow roles and crw doctor flow-roles show it), never a refusal of the whole policy.
	for _, row := range []struct{ role, model, effort string }{
		{"architect", agentOpus, "high"},            // an allowed model at an effort the list does not approve
		{"executor", "openai/gpt-6-astra", "xhigh"}, // a model the list does not name
		{"reviewer-final", agentSol, "max"},
	} {
		roles := withRole(row.role, pairDoc(row.model, row.effort))
		policy := agentPolicy(roles)
		policy["allowed"] = agentAllowed()
		message := strings.Join(mustLoad(t, policy).Warnings(), "\n")
		if !strings.Contains(message, row.role) || !strings.Contains(message, "allowed") {
			t.Fatalf("%+v: %s", row, message)
		}
	}
	// Without an allowlist nothing is compared, as for the relay roles.
	if warnings := mustLoad(t, agentPolicy(withRole("executor", pairDoc("inferhub/deepseek-v4.1-flash", "xhigh")))).Warnings(); len(warnings) != 0 {
		t.Fatalf("%q", warnings)
	}
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
			// CRW-1330: a warning, not a PolicyError; the runtime independence gate judges each review.
			message := strings.Join(mustLoad(t, agentPolicy(row.roles)).Warnings(), "\n")
			if !strings.Contains(message, "reviewer") || !strings.Contains(message, "family") {
				t.Fatal(message)
			}
		})
	}
	// The architect and the explorer are not authors of a reviewed change: they may share a family with a reviewer. The
	// architect is the author of the design the critic audits, so the critic moves off the architect's family here
	// (TestAgentRoleNamesIncludeCriticAndOldPolicyFilesStillLoad pins that warning).
	roles := agentRolesDoc()
	roles["architect"] = pairDoc(agentSol, "xhigh")
	roles["critic"] = pairDoc("inferhub/deepseek-v4.1-flash", "xhigh")
	if warnings := mustLoad(t, agentPolicy(roles)).Warnings(); len(warnings) != 0 {
		t.Fatalf("%q", warnings)
	}

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
	if reached["explorer"] || reached["critic"] {
		t.Fatal("the explorer and the critic are named by agentType and never picked by an alias")
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

	// A file states its own table; rows that overlap without one being more specific are warned about when it is read, and the
	// sections are ignored (CRW-1330).
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
			if message := agentSectionWarning(t, own(row.rows...)); !strings.Contains(message, row.want) {
				t.Fatalf("want %q in %q", row.want, message)
			}
		})
	}
	// A table without the section it maps onto is a mistake.
	if message := agentSectionWarning(t, doc{"allowed": agentAllowed(), "agent-aliases": []any{doc{"key": "opus", "role": "architect"}}}); !strings.Contains(message, "agent-roles") {
		t.Fatal(message)
	}
}

// agentSectionWarning loads a document whose agent sections do not parse and answers the loader's warnings (CRW-1330): the file
// loads, the sections are ignored and described as such.
func agentSectionWarning(t *testing.T, document doc) string {
	t.Helper()
	p := mustLoad(t, document)
	if _, ok := p.AgentRoles(); ok {
		t.Fatalf("a section that does not parse is described as declared roles")
	}
	message := strings.Join(p.Warnings(), "\n")
	if !strings.Contains(message, "ignored") {
		t.Fatalf("no ignored-section warning: %q", message)
	}
	return message
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
	if len(roles) != len(AgentRoleNames) || roles[0].(map[string]any)["role"] != "architect" || roles[5].(map[string]any)["role"] != "explorer" || roles[6].(map[string]any)["role"] != "critic" {
		t.Fatalf("roles in declaration order of the design: %#v", roles)
	}
	if len(section["aliases"].([]any)) != 6 {
		t.Fatalf("%#v", section["aliases"])
	}
}

// CRW-1434 (D1-1): the critic is a seventh agent role, the design audit's. A policy file written before it (six roles) still loads:
// its critic takes the compiled default pair with one warning that says so, a file that declares the critic keeps its own pair
// without that warning, and a file that misses another role is still ignored with the old warning.
func TestAgentRoleNamesIncludeCriticAndOldPolicyFilesStillLoad(t *testing.T) {
	if AgentRoleNames[len(AgentRoleNames)-1] != AgentCritic || AgentCritic != "critic" {
		t.Fatalf("the critic is the last agent role: %v", AgentRoleNames)
	}
	if DefaultCriticPair.Model != agentSol || DefaultCriticPair.Effort != "xhigh" {
		t.Fatalf("the critic's compiled default pair: %+v", DefaultCriticPair)
	}
	critic := func(p Policy) AgentRole {
		t.Helper()
		declared, ok := p.AgentRoles()
		if !ok {
			t.Fatalf("the agent roles are not reported: %q", p.Warnings())
		}
		if len(declared.Roles) != len(AgentRoleNames) || declared.Roles[len(declared.Roles)-1].Name != AgentCritic {
			t.Fatalf("roles %+v", declared.Roles)
		}
		role, _ := declared.Role(AgentCritic)
		return role
	}
	filled := func(p Policy) []string {
		var out []string
		for _, w := range p.Warnings() {
			if strings.Contains(w, "declares no critic") {
				out = append(out, w)
			}
		}
		return out
	}

	// A six-role file, as every file written before the critic is.
	old := sixRolesDoc()
	p := mustLoad(t, agentPolicy(old))
	role := critic(p)
	if role.Pair.Model != agentSol || role.Pair.Effort != "xhigh" || !reflect.DeepEqual(role.Aliases, []string{"ocx-gpt-6-1-sol"}) {
		t.Fatalf("the filled critic %+v", role)
	}
	if got := filled(p); len(got) != 1 || len(p.Warnings()) != 1 || !strings.Contains(got[0], "gpt-6.1-sol/xhigh") {
		t.Fatalf("one fill warning, got %q", p.Warnings())
	}
	if !p.DeclaresAgentRoles() {
		t.Fatal("the file declares its agent roles")
	}

	// A seven-role file keeps its own critic and has no fill warning.
	own := sixRolesDoc()
	own["critic"] = pairDoc("openai/gpt-5.6-sol", "high")
	p = mustLoad(t, agentPolicy(own))
	if role := critic(p); role.Pair.Model != "openai/gpt-5.6-sol" || role.Pair.Effort != "high" {
		t.Fatalf("the declared critic %+v", role)
	}
	if len(p.Warnings()) != 0 {
		t.Fatalf("a seven-role file warns: %q", p.Warnings())
	}

	// The alias table gains no row: the critic is named by agentType, as the explorer is.
	declared, _ := p.AgentRoles()
	if len(declared.Aliases) != 6 {
		t.Fatalf("aliases %+v", declared.Aliases)
	}
	for _, row := range declared.Aliases {
		if row.Role == AgentCritic {
			t.Fatalf("an alias row names the critic: %+v", row)
		}
	}

	// A file that misses another role is still ignored with the old warning, and names no critic.
	missing := sixRolesDoc()
	delete(missing, "executor-t1")
	message := agentSectionWarning(t, agentPolicy(missing))
	if !strings.Contains(message, "executor-t1") || strings.Contains(message, "critic") {
		t.Fatalf("the missing role warning %q", message)
	}

	// The critic audits the architect's design: a critic on the architect's model family is warned about.
	same := agentRolesDoc()
	same["critic"] = pairDoc("anthropic/claude-sonnet-5", "xhigh")
	if message := strings.Join(mustLoad(t, agentPolicy(same)).Warnings(), "\n"); !strings.Contains(message, "critic") || !strings.Contains(message, "architect") || !strings.Contains(message, "family") {
		t.Fatalf("no critic family warning: %q", message)
	}
}

// sixRolesDoc is agentRolesDoc without the critic: the agent-roles section of a file written before the critic existed.
func sixRolesDoc() doc {
	roles := agentRolesDoc()
	delete(roles, "critic")
	return roles
}
