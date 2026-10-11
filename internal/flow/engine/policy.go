package engine

import (
	"fmt"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// Pair is the model and reasoning effort a role runs on. A role's pair is fixed: nothing changes it on a thread.
type Pair struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// RoleSpec is one role of the policy: its role family (executor and executor-t1 share one, reviewer and
// reviewer-final share one), its fixed pair, and the Claude-style model aliases that name its model family.
type RoleSpec struct {
	Family string
	Pair   Pair
	// Sandbox is the sandbox the role's thread runs in: workspace-write for the roles that author a change (executor,
	// executor-t1), read-only for every other role.
	Sandbox      string
	ModelAliases []string
}

// Sandboxes of an engine thread.
const (
	SandboxReadOnly       = "read-only"
	SandboxWorkspaceWrite = "workspace-write"
)

// roleSandbox is the sandbox a role's thread runs in. Only an executor writes (to its own call-private cwd), so a
// build that resolved to any other role could not edit and a golden that fixes the sandbox of each call fails.
func roleSandbox(role string) string {
	if role == "executor" || role == "executor-t1" {
		return SandboxWorkspaceWrite
	}
	return SandboxReadOnly
}

// AliasRule maps a Claude-style call (a non-role agentType string, else the model alias) with an effort and an
// isolation option to one role. An empty Effort or Isolation matches any; an empty Role refuses with Detail.
type AliasRule struct {
	Key, Effort, Isolation string
	Role                   string
	// Cause is the detail kind of a refusal ("alias_ambiguous"); Detail is its guidance.
	Cause  string
	Detail string
}

// Policy is the role table an engine resolves calls against. Its content is the operator's execution policy file
// (PolicyFromExecution reads it); nothing in the engine ships a default pair for a real role. FakePolicy is the table of
// the fake backend.
type Policy struct {
	Roles map[string]RoleSpec
	// Aliases are the rows of the alias table. Of the rows that match a call, the one that names the most of effort
	// and isolation decides; the policy file's loader refuses rows that overlap without one being more specific.
	Aliases []AliasRule
	// Source is the policy file the table was read from and Digest the SHA-256 of its bytes; both are empty for the
	// fake policy. A run records them when it starts and a resumed run that reads another digest records the change.
	Source, Digest string
}

// Refusal is why a call is refused before any thread starts.
type Refusal struct {
	Reason string
	// Cause is the finer kind of a policy_refused call ("alias_ambiguous"); empty when the reason says it all.
	Cause  string
	Detail string
}

// CallOpts are the options of agent() and agent.call that identify what runs: the Claude-compatible ones and the
// CRW ones.
type CallOpts struct {
	AgentType, Model, Effort, Isolation string
	Node, StepDecision                  string
	Label, Phase, Cwd, Head             string
}

// Resolved is a fresh call's role.
type Resolved struct {
	Role          string
	Spec          RoleSpec
	RequestedPair Pair
}

// ResolveFresh maps the options of a call that starts a thread to one role, or refuses it.
//
// An agentType that is a role key names the role; its model option must be absent, the role's model or an alias of
// the role's family, otherwise pair_refused. Any other call is looked up in the alias table by (a non-role agentType
// string, else the model alias; effort; isolation); a key outside the table, or one the table marks ambiguous, is
// policy_refused. A requested effort is never applied: a differing effort is kept in RequestedPair.
func (p *Policy) ResolveFresh(o CallOpts) (Resolved, *Refusal) {
	if spec, ok := p.Roles[o.AgentType]; ok && o.AgentType != "" {
		if o.Model != "" && o.Model != spec.Pair.Model && !slices.Contains(spec.ModelAliases, o.Model) {
			return Resolved{}, &Refusal{Reason: ReasonPairRefused, Detail: "model " + o.Model + " is not the " + o.AgentType + " model"}
		}
		return Resolved{Role: o.AgentType, Spec: spec, RequestedPair: requested(o, spec)}, nil
	}
	key := o.AgentType
	if key == "" {
		key = o.Model
	}
	if key == "" {
		return Resolved{}, &Refusal{Reason: ReasonPolicyRefused, Detail: "no agentType or model: name a role"}
	}
	var row *AliasRule
	for i, r := range p.Aliases {
		if r.Key != key || r.Effort != "" && r.Effort != o.Effort || r.Isolation != "" && r.Isolation != o.Isolation {
			continue
		}
		if row == nil || r.specificity() > row.specificity() {
			row = &p.Aliases[i]
		}
	}
	if row == nil {
		return Resolved{}, &Refusal{Reason: ReasonPolicyRefused, Detail: "alias " + key + " is not in the alias table"}
	}
	if row.Role == "" {
		return Resolved{}, &Refusal{Reason: ReasonPolicyRefused, Cause: row.Cause, Detail: row.Detail}
	}
	spec, ok := p.Roles[row.Role]
	if !ok {
		return Resolved{}, &Refusal{Reason: ReasonPolicyRefused, Detail: "the alias table names role " + row.Role + ", which the policy does not have"}
	}
	return Resolved{Role: row.Role, Spec: spec, RequestedPair: requested(o, spec)}, nil
}

// specificity is how many of the optional options the rule names.
func (r AliasRule) specificity() int {
	n := 0
	for _, v := range [...]string{r.Effort, r.Isolation} {
		if v != "" {
			n++
		}
	}
	return n
}

func requested(o CallOpts, spec RoleSpec) Pair {
	rp := spec.Pair
	if o.Model != "" {
		rp.Model = o.Model
	}
	if o.Effort != "" {
		rp.Effort = o.Effort
	}
	return rp
}

// RoleKey reports whether name is a role of the policy.
func (p *Policy) RoleKey(name string) bool {
	_, ok := p.Roles[name]
	return ok
}

// FakePolicy is the role table of the fake backend: the six roles with placeholder pairs and the alias table of the
// design (opus -> architect; haiku with isolation worktree -> executor; sonnet -> executor-t1; ocx-gpt-6-1-sol with
// xhigh -> reviewer-final, otherwise reviewer; haiku without isolation is ambiguous).
func FakePolicy() *Policy {
	roles := map[string]RoleSpec{
		"architect":      {Family: "architect", Pair: Pair{"fake-opus", "high"}, ModelAliases: []string{"opus"}},
		"executor":       {Family: "executor", Pair: Pair{"fake-haiku", "xhigh"}, ModelAliases: []string{"haiku"}},
		"executor-t1":    {Family: "executor", Pair: Pair{"fake-sonnet", "xhigh"}, ModelAliases: []string{"sonnet"}},
		"reviewer":       {Family: "reviewer", Pair: Pair{"fake-sol", "high"}, ModelAliases: []string{"ocx-gpt-6-1-sol"}},
		"reviewer-final": {Family: "reviewer", Pair: Pair{"fake-sol", "xhigh"}, ModelAliases: []string{"ocx-gpt-6-1-sol"}},
		"explorer":       {Family: "explorer", Pair: Pair{"fake-haiku", "high"}, ModelAliases: []string{"haiku"}},
	}
	for name, spec := range roles {
		spec.Sandbox = roleSandbox(name)
		roles[name] = spec
	}
	return &Policy{
		Roles: roles,
		Aliases: []AliasRule{
			{Key: "opus", Role: "architect"},
			{Key: "haiku", Isolation: "worktree", Role: "executor"},
			{Key: "haiku", Cause: execution.AliasAmbiguous, Detail: execution.AliasAmbiguousHaikuDetail},
			{Key: "sonnet", Role: "executor-t1"},
			{Key: "ocx-gpt-6-1-sol", Effort: "xhigh", Role: "reviewer-final"},
			{Key: "ocx-gpt-6-1-sol", Role: "reviewer"},
		},
	}
}

// roleFamily is the role family of an agent role: executor and executor-t1 are one, reviewer and reviewer-final are
// one, architect and explorer are alone.
func roleFamily(role string) string {
	switch role {
	case execution.AgentExecutor, execution.AgentExecutorT1:
		return "executor"
	case execution.AgentReviewer, execution.AgentReviewerFinal:
		return "reviewer"
	}
	return role
}

// PolicyFromExecution is the role table of an operator's execution policy file: the agent-roles section gives each
// role its pair and model aliases, the agent-aliases section (or the design's table) gives the alias table. The
// source and the file's digest are kept for the run record.
func PolicyFromExecution(p execution.Policy, source string) (*Policy, error) {
	declared, ok := p.AgentRoles()
	if !ok {
		return nil, fmt.Errorf("the execution policy %s declares no agent-roles section: a flow run needs one pair for each of %v", source, execution.AgentRoleNames)
	}
	out := &Policy{Roles: map[string]RoleSpec{}, Source: source, Digest: p.Digest()}
	for _, role := range declared.Roles {
		out.Roles[role.Name] = RoleSpec{
			Family: roleFamily(role.Name), Pair: Pair{Model: role.Pair.Model, Effort: role.Pair.Effort},
			Sandbox: roleSandbox(role.Name), ModelAliases: slices.Clone(role.Aliases),
		}
	}
	for _, row := range declared.Aliases {
		out.Aliases = append(out.Aliases, AliasRule{Key: row.Key, Effort: row.Effort, Isolation: row.Isolation, Role: row.Role, Cause: row.Refuse, Detail: row.Detail})
	}
	return out, nil
}
