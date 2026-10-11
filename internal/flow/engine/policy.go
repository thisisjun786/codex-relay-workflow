package engine

import "slices"

// Pair is the model and reasoning effort a role runs on. A role's pair is fixed: nothing changes it on a thread.
type Pair struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// RoleSpec is one role of the policy: its role family (executor and executor-t1 share one, reviewer and
// reviewer-final share one), its fixed pair, and the Claude-style model aliases that name its model family.
type RoleSpec struct {
	Family       string
	Pair         Pair
	ModelAliases []string
}

// AliasRule maps a Claude-style call (a non-role agentType string, else the model alias) with an effort and an
// isolation option to one role. An empty Effort or Isolation matches any; an empty Role refuses with Detail.
type AliasRule struct {
	Key, Effort, Isolation string
	Role                   string
	Detail                 string
}

// Policy is the role table an engine resolves calls against. Its content is the operator's execution policy file
// (F02 loads it); nothing in the engine ships a default pair for a real role. FakePolicy is the table of the fake
// backend.
type Policy struct {
	Roles map[string]RoleSpec
	// Aliases are tried in order; the first rule whose key, effort and isolation match decides.
	Aliases []AliasRule
}

// Refusal is why a call is refused before any thread starts.
type Refusal struct {
	Reason string
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
	for _, r := range p.Aliases {
		if r.Key != key || r.Effort != "" && r.Effort != o.Effort || r.Isolation != "" && r.Isolation != o.Isolation {
			continue
		}
		if r.Role == "" {
			return Resolved{}, &Refusal{Reason: ReasonPolicyRefused, Detail: r.Detail}
		}
		spec, ok := p.Roles[r.Role]
		if !ok {
			return Resolved{}, &Refusal{Reason: ReasonPolicyRefused, Detail: "the alias table names role " + r.Role + ", which the policy does not have"}
		}
		return Resolved{Role: r.Role, Spec: spec, RequestedPair: requested(o, spec)}, nil
	}
	return Resolved{}, &Refusal{Reason: ReasonPolicyRefused, Detail: "alias " + key + " is not in the alias table"}
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
	return &Policy{
		Roles: map[string]RoleSpec{
			"architect":      {Family: "architect", Pair: Pair{"fake-opus", "high"}, ModelAliases: []string{"opus"}},
			"executor":       {Family: "executor", Pair: Pair{"fake-haiku", "xhigh"}, ModelAliases: []string{"haiku"}},
			"executor-t1":    {Family: "executor", Pair: Pair{"fake-sonnet", "xhigh"}, ModelAliases: []string{"sonnet"}},
			"reviewer":       {Family: "reviewer", Pair: Pair{"fake-sol", "high"}, ModelAliases: []string{"ocx-gpt-6-1-sol"}},
			"reviewer-final": {Family: "reviewer", Pair: Pair{"fake-sol", "xhigh"}, ModelAliases: []string{"ocx-gpt-6-1-sol"}},
			"explorer":       {Family: "explorer", Pair: Pair{"fake-haiku", "high"}, ModelAliases: []string{"haiku"}},
		},
		Aliases: []AliasRule{
			{Key: "opus", Role: "architect"},
			{Key: "haiku", Isolation: "worktree", Role: "executor"},
			{Key: "haiku", Detail: "alias_ambiguous: haiku without isolation 'worktree' is a build, with agentType 'explorer' it is research"},
			{Key: "sonnet", Role: "executor-t1"},
			{Key: "ocx-gpt-6-1-sol", Effort: "xhigh", Role: "reviewer-final"},
			{Key: "ocx-gpt-6-1-sol", Role: "reviewer"},
		},
	}
}
