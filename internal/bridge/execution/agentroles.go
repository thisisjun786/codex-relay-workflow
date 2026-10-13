package execution

import (
	"fmt"
	"slices"
	"strings"
)

// Agent role names: the six roles a flow engine runs a call as. They are not the relay's roles (supervisor,
// parent, child); the policy file declares them in their own sections, agent-roles and agent-aliases.
const (
	AgentArchitect     = "architect"
	AgentExecutor      = "executor"
	AgentExecutorT1    = "executor-t1"
	AgentReviewer      = "reviewer"
	AgentReviewerFinal = "reviewer-final"
	AgentExplorer      = "explorer"
)

// AgentRoleNames is the six agent roles in the order the design lists them; every section that iterates roles
// follows it.
var AgentRoleNames = [...]string{AgentArchitect, AgentExecutor, AgentExecutorT1, AgentReviewer, AgentReviewerFinal, AgentExplorer}

const supportedAgentRoles = `"architect", "executor", "executor-t1", "explorer", "reviewer", "reviewer-final"`

// AgentRole is one role of the agent-roles section: its one pair and the Claude model aliases that name its model
// (a call whose agentType is the role may state its model option as the pair's model or as one of these).
type AgentRole struct {
	Name    string
	Pair    RolePair
	Aliases []string
}

// AliasRow is one row of the alias table: it maps a Claude script's call options (a key, an optional effort and an
// optional isolation) to one role, or refuses them. An empty Effort or Isolation matches any value. A row has a Role
// or a Refuse, never both; Refuse is the cause the refusal reports and Detail its guidance.
type AliasRow struct {
	Key, Effort, Isolation string
	Role                   string
	Refuse, Detail         string
}

// matches reports whether the row covers a call stating key, effort and isolation.
func (r AliasRow) matches(key, effort, isolation string) bool {
	return r.Key == key && (r.Effort == "" || r.Effort == effort) && (r.Isolation == "" || r.Isolation == isolation)
}

// specificity is how many of the optional options the row names.
func (r AliasRow) specificity() int {
	n := 0
	for _, v := range [...]string{r.Effort, r.Isolation} {
		if v != "" {
			n++
		}
	}
	return n
}

// within reports whether every call r covers is also covered by o's key: r names no option o does not name with the
// same value, so r is at most as specific as o in every dimension.
func (r AliasRow) within(o AliasRow) bool {
	return (r.Effort == "" || r.Effort == o.Effort) && (r.Isolation == "" || r.Isolation == o.Isolation)
}

// AgentRoles is the agent-roles and agent-aliases sections of a policy file.
type AgentRoles struct {
	// Roles holds the six roles in AgentRoleNames order.
	Roles []AgentRole
	// Aliases are the rows of the alias table, in the order the file states them; no two overlap unless one is
	// strictly more specific, so a call matches the most specific row and the order decides nothing.
	Aliases []AliasRow
}

// Role is the declared role with that name.
func (a AgentRoles) Role(name string) (AgentRole, bool) {
	for _, role := range a.Roles {
		if role.Name == name {
			return role, true
		}
	}
	return AgentRole{}, false
}

// Match is the row that decides a call stating key, effort and isolation: the most specific row covering it.
func (a AgentRoles) Match(key, effort, isolation string) (AliasRow, bool) {
	best, found := AliasRow{}, false
	for _, row := range a.Aliases {
		if row.matches(key, effort, isolation) && (!found || row.specificity() > best.specificity()) {
			best, found = row, true
		}
	}
	return best, found
}

// AgentRoles reports the agent roles the file declares; ok is false for a file without an agent-roles section.
func (p Policy) AgentRoles() (AgentRoles, bool) {
	if p.agentRoles == nil {
		return AgentRoles{}, false
	}
	declared := *p.agentRoles
	declared.Roles = slices.Clone(declared.Roles)
	declared.Aliases = slices.Clone(declared.Aliases)
	return declared, true
}

// defaultAliasOf lists, per role, the Claude model aliases that name its model when a file states none.
var defaultAliasOf = map[string][]string{
	AgentArchitect: {"opus"}, AgentExecutor: {"haiku"}, AgentExecutorT1: {"sonnet"},
	AgentReviewer: {"ocx-gpt-6-1-sol"}, AgentReviewerFinal: {"ocx-gpt-6-1-sol"}, AgentExplorer: {"haiku"},
}

// AliasAmbiguous is the cause of the refusal of a haiku call without isolation.
const AliasAmbiguous = "alias_ambiguous"

// AliasAmbiguousHaikuDetail is the guidance the refusal of a haiku call without isolation carries: the two ways to
// say which of the two things the call is.
const AliasAmbiguousHaikuDetail = "alias_ambiguous: model 'haiku' without isolation 'worktree' is ambiguous. Claude runs it in a shared checkout, but CRW gives every call its own write cwd, so it cannot be both. " +
	"Fix 1, a build: add isolation: 'worktree' (role executor). Fix 2, an investigation: use agentType: 'explorer' (role explorer)."

// defaultAliasRows is the design's alias table, used when a file declares agent-roles and no agent-aliases. It
// holds no model name: its keys are the names Claude scripts give a model.
func defaultAliasRows() []AliasRow {
	return []AliasRow{
		{Key: "opus", Role: AgentArchitect},
		{Key: "haiku", Isolation: "worktree", Role: AgentExecutor},
		{Key: "haiku", Refuse: AliasAmbiguous, Detail: AliasAmbiguousHaikuDetail},
		{Key: "sonnet", Role: AgentExecutorT1},
		{Key: "ocx-gpt-6-1-sol", Effort: "xhigh", Role: AgentReviewerFinal},
		{Key: "ocx-gpt-6-1-sol", Role: AgentReviewer},
	}
}

// ModelFamily is the vendor family a model name belongs to: the name without its provider prefix, lower case, up to
// its first dash ("anthropic/claude-haiku-5" and "claude-opus-5-5" are claude, "gpt-6.1-sol" and "openai/gpt-5.6-sol"
// are gpt). Two roles on models of one family do not give an independent second opinion.
func ModelFamily(model string) string {
	name := strings.ToLower(model[strings.LastIndexByte(model, '/')+1:])
	family, _, _ := strings.Cut(name, "-")
	return family
}

// parseAgentSections reads agent-roles and agent-aliases from the policy document; nil when the file declares neither.
func parseAgentSections(data object) (*AgentRoles, error) {
	if !has(data, "agent-roles") {
		if has(data, "agent-aliases") {
			return nil, &PolicyError{"agent-aliases maps onto the agent roles, and this file declares no agent-roles"}
		}
		return nil, nil
	}
	byName, err := parseAgentRoles(data.Get("agent-roles"))
	if err != nil {
		return nil, err
	}
	declared := &AgentRoles{}
	for _, name := range AgentRoleNames {
		declared.Roles = append(declared.Roles, byName[name])
	}
	declared.Aliases = defaultAliasRows()
	if has(data, "agent-aliases") {
		if declared.Aliases, err = parseAgentAliases(data.Get("agent-aliases")); err != nil {
			return nil, err
		}
	}
	return declared, nil
}

func (a *AgentRoles) byName() map[string]AgentRole {
	out := make(map[string]AgentRole, len(a.Roles))
	for _, role := range a.Roles {
		out[role.Name] = role
	}
	return out
}

// parseAgentRoles reads the agent-roles section: every one of the six roles, each with exactly one pair.
func parseAgentRoles(declared any) (map[string]AgentRole, error) {
	section, ok := declared.(object)
	if !ok {
		return nil, &PolicyError{"agent-roles must be a JSON object keyed by agent role"}
	}
	roles := map[string]AgentRole{}
	for _, name := range keys(section) {
		if !slices.Contains(AgentRoleNames[:], name) {
			return nil, &PolicyError{fmt.Sprintf("%s is not an agent role; supported are %s", repr(name), supportedAgentRoles)}
		}
		entry, ok := section.Get(name).(object)
		if !ok {
			return nil, &PolicyError{fmt.Sprintf("agent role %s must be an object", repr(name))}
		}
		role, err := parseAgentRole(name, entry)
		if err != nil {
			return nil, err
		}
		roles[name] = role
	}
	var missing []string
	for _, name := range AgentRoleNames {
		if _, declared := roles[name]; !declared {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, &PolicyError{fmt.Sprintf("agent-roles does not declare %s; every agent role has exactly one pair", repr(missing))}
	}
	return roles, nil
}

func parseAgentRole(name string, entry object) (AgentRole, error) {
	where := "agent role " + repr(name)
	if err := only(entry, []string{"aliases", "autoCompactTokenLimit", "model", "pairs", "reasoningEffort"}, where); err != nil {
		return AgentRole{}, err
	}
	pairEntry := entry
	if has(entry, "pairs") {
		if named := present(entry, "autoCompactTokenLimit", "model", "reasoningEffort"); len(named) > 0 {
			return AgentRole{}, &PolicyError{fmt.Sprintf("%s declares both pairs and %s; state one pair as model, reasoningEffort and autoCompactTokenLimit, or as a list of one", where, repr(named))}
		}
		pairs, err := parsePairs(name, entry.Get("pairs"))
		if err != nil {
			return AgentRole{}, err
		}
		if len(pairs) != 1 {
			return AgentRole{}, &PolicyError{fmt.Sprintf("%s declares %d pairs; an agent role runs on exactly one pair, because a thread's pair never changes", where, len(pairs))}
		}
		role := AgentRole{Name: name, Pair: pairs[0]}
		return withAliases(role, entry)
	}
	if absent := absent(pairEntry, "model", "reasoningEffort"); len(absent) > 0 {
		return AgentRole{}, &PolicyError{fmt.Sprintf("%s is missing %s", where, repr(absent))}
	}
	model, err := identifier(entry.Get("model"), "the model of "+where, Maximum)
	if err != nil {
		return AgentRole{}, err
	}
	effort, err := identifier(entry.Get("reasoningEffort"), "the effort of "+where, Maximum)
	if err != nil {
		return AgentRole{}, err
	}
	var limit *int64
	if has(entry, "autoCompactTokenLimit") {
		if limit, err = autoCompactTokenLimit(entry.Get("autoCompactTokenLimit"), where); err != nil {
			return AgentRole{}, err
		}
	}
	return withAliases(AgentRole{Name: name, Pair: RolePair{Model: model, Effort: effort, AutoCompactTokenLimit: limit}}, entry)
}

// withAliases reads the role's model aliases: a non-empty list of distinct names, or the design's when absent.
func withAliases(role AgentRole, entry object) (AgentRole, error) {
	if !has(entry, "aliases") {
		role.Aliases = slices.Clone(defaultAliasOf[role.Name])
		return role, nil
	}
	where := "the aliases of agent role " + repr(role.Name)
	list, ok := entry.Get("aliases").([]any)
	if !ok || len(list) == 0 {
		return AgentRole{}, &PolicyError{where + " must be a non-empty list of names"}
	}
	for _, item := range list {
		alias, err := identifier(item, where, Maximum)
		if err != nil {
			return AgentRole{}, err
		}
		if slices.Contains(role.Aliases, alias) {
			return AgentRole{}, &PolicyError{fmt.Sprintf("%s lists %s twice", where, repr(alias))}
		}
		role.Aliases = append(role.Aliases, alias)
	}
	return role, nil
}

// parseAgentAliases reads the alias table: rows of a key, an optional effort and isolation, and exactly one of a role
// or a refusal. Two rows may cover a common call only when one of them is strictly more specific.
func parseAgentAliases(declared any) ([]AliasRow, error) {
	list, ok := declared.([]any)
	if !ok || len(list) == 0 {
		return nil, &PolicyError{"agent-aliases must be a non-empty list of rows"}
	}
	var rows []AliasRow
	for i, item := range list {
		where := fmt.Sprintf("agent-aliases row %d", i+1)
		entry, ok := item.(object)
		if !ok {
			return nil, &PolicyError{where + " must be an object"}
		}
		if err := only(entry, []string{"detail", "effort", "isolation", "key", "refuse", "role"}, where); err != nil {
			return nil, err
		}
		if !has(entry, "key") {
			return nil, &PolicyError{where + " is missing the key"}
		}
		var row AliasRow
		fields := []struct {
			name string
			into *string
		}{{"key", &row.Key}, {"effort", &row.Effort}, {"isolation", &row.Isolation}, {"role", &row.Role}, {"refuse", &row.Refuse}, {"detail", &row.Detail}}
		for _, field := range fields {
			if !has(entry, field.name) {
				continue
			}
			value, err := identifier(entry.Get(field.name), "the "+field.name+" of "+where, Maximum)
			if err != nil {
				return nil, err
			}
			*field.into = value
		}
		if (row.Role == "") == (row.Refuse == "") {
			return nil, &PolicyError{where + " must state exactly one of role and refuse"}
		}
		if row.Role != "" && !slices.Contains(AgentRoleNames[:], row.Role) {
			return nil, &PolicyError{fmt.Sprintf("%s names %s, which is not an agent role; supported are %s", where, repr(row.Role), supportedAgentRoles)}
		}
		if row.Refuse != "" && row.Detail == "" {
			row.Detail = row.Refuse + ": alias " + row.Key + " is refused by this host's alias table"
		}
		if row.Role != "" && row.Detail != "" {
			return nil, &PolicyError{where + " gives a detail to a row that names a role; only a refusal has guidance"}
		}
		rows = append(rows, row)
	}
	if err := checkAliasOverlap(rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// checkAliasOverlap refuses rows that cover a common call without one being strictly more specific than the other.
func checkAliasOverlap(rows []AliasRow) error {
	for i, a := range rows {
		for _, b := range rows[i+1:] {
			if a.Key != b.Key {
				continue
			}
			// Rows with one key cover a common call unless they state different values for an option both name.
			common := (a.Effort == "" || b.Effort == "" || a.Effort == b.Effort) && (a.Isolation == "" || b.Isolation == "" || a.Isolation == b.Isolation)
			if !common {
				continue
			}
			ordered := a.within(b) != b.within(a) // one is strictly inside the other
			if !ordered {
				return &PolicyError{fmt.Sprintf("agent-aliases rows for %s overlap: %s and %s cover a common call and neither is more specific", repr(a.Key), a.describe(), b.describe())}
			}
		}
	}
	return nil
}

func (r AliasRow) describe() string {
	parts := []string{"key " + repr(r.Key)}
	if r.Effort != "" {
		parts = append(parts, "effort "+repr(r.Effort))
	}
	if r.Isolation != "" {
		parts = append(parts, "isolation "+repr(r.Isolation))
	}
	return strings.Join(parts, ", ")
}

// checkAgentRoles applies the cross-role rules and answers what breaks them as warnings: the pairs the allowed list
// approves, and a reviewer family that is not an author's. A flow run no longer reads agent-roles (CRW-1330: its pairs come
// from the helper role store), so a disagreement in a section nothing runs on is reported, never a refusal of the policy
// the relay and the bridge run on; the runtime independence gate still judges every review.
func checkAgentRoles(roles map[string]AgentRole, allowed map[string][]string) []string {
	var warnings []string
	for _, name := range AgentRoleNames {
		role := roles[name]
		if allowed != nil && !slices.Contains(allowed[role.Pair.Model], role.Pair.Effort) {
			warnings = append(warnings, fmt.Sprintf("agent role %s is declared to run %s at %s, which this file's allowed list does not approve; no such task could be created, so the two sections disagree rather than one narrowing the other", repr(name), repr(role.Pair.Model), repr(role.Pair.Effort)))
		}
	}
	pairs := map[string]string{}
	for name, role := range roles {
		pairs[name] = role.Pair.Model
	}
	return append(warnings, FamilyOverlaps(pairs, AgentExecutor, AgentExecutorT1)...)
}

// FamilyOverlaps describes each reviewer role (reviewer, reviewer-final) whose model is of the family of one of the author
// roles named, in a table of role -> model; a role with no model is not compared. A review is independent only when it comes
// from another family than the author's, so each overlap is a warning for whoever decided the pairs.
func FamilyOverlaps(models map[string]string, authors ...string) []string {
	var out []string
	for _, reviewer := range [...]string{AgentReviewer, AgentReviewerFinal} {
		for _, author := range authors {
			if models[reviewer] == "" || models[author] == "" {
				continue
			}
			family := ModelFamily(models[reviewer])
			if family == ModelFamily(models[author]) {
				out = append(out, fmt.Sprintf("agent role %s runs %s, the same model family (%s) as agent role %s on %s; a review is independent only when it comes from another family than the author's", repr(reviewer), repr(models[reviewer]), repr(family), repr(author), repr(models[author])))
			}
		}
	}
	return out
}

// DefaultModelAliases is the model aliases of an agent role a file states none for, and of every role of a table the flow
// builds from the helper role store.
func DefaultModelAliases(role string) []string { return slices.Clone(defaultAliasOf[role]) }

// DefaultAliasRows is the design's alias table: the one a file that declares agent-roles and no agent-aliases has, and the
// only one a flow run reads (CRW-1330).
func DefaultAliasRows() []AliasRow { return defaultAliasRows() }

// SummaryOf describes roles and rows as get_capabilities carries them under executionPolicy.agentRoles.
func SummaryOf(roles []AgentRole, rows []AliasRow) map[string]any {
	return AgentRoles{Roles: roles, Aliases: rows}.summary()
}

// summary describes the agent roles as the policy description carries them.
func (a AgentRoles) summary() map[string]any {
	roles := make([]any, len(a.Roles))
	for i, role := range a.Roles {
		aliases := make([]any, len(role.Aliases))
		for k, alias := range role.Aliases {
			aliases[k] = alias
		}
		roles[i] = map[string]any{"role": role.Name, "model": role.Pair.Model, "reasoningEffort": role.Pair.Effort, "aliases": aliases}
	}
	rows := make([]any, len(a.Aliases))
	for i, row := range a.Aliases {
		entry := map[string]any{"key": row.Key, "effort": nullable(row.Effort), "isolation": nullable(row.Isolation), "role": nullable(row.Role)}
		if row.Refuse != "" {
			entry["refuse"], entry["detail"] = row.Refuse, row.Detail
		}
		rows[i] = entry
	}
	return map[string]any{"roles": roles, "aliases": rows}
}
