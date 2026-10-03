package registry

import (
	"context"
	"os"
	"sort"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// rolepolicy.py: whether a task's recorded authorization still matches the role it holds. The
// policy is the bridge's file read through the bridge's own parser (internal/bridge/execution).

// RoleRecovery is rolepolicy.RECOVERY, caller-visible.
const RoleRecovery = "Re-record this task's authorized settings from a source attributable to the user, with " +
	"relay settings record --source user_transition, once its current settings have been read."

// roleScope is linkage.ROLE_SCOPE.
var roleScope = map[string]string{"supervisor": "initiative", "parent": "project", "child": "issue"}

// RolePolicy is rolepolicy.Declared or Unresolved: Declared is true when a policy declaring roles
// was read. Detail is the Unresolved detail.
type RolePolicy struct {
	Declared     bool
	Detail       string
	PublicDetail string
	policy       execution.Policy
	digest       string
}

// ResolveRolePolicy is rolepolicy._resolve over one environment: the configured value
// str.strip()ped, read through the bridge's parser as from_file reads Path(value), and a
// document nested deeper than that parser descends answered as rolepolicy's RecursionError
// branch answers it, naming the value as configured.
func ResolveRolePolicy(env map[string]string) RolePolicy {
	configured := pyvalue.Strip(env[execution.EnvPolicy])
	if configured == "" {
		return RolePolicy{Detail: execution.EnvPolicy + " is not set in this process, so no role policy can be read",
			PublicDetail: "execution policy environment is not configured in this process"}
	}
	const unreadable = "configured execution policy is unreadable or invalid"
	source := store.PathlibSpelling(configured)
	raw, err := execution.ReadFile(source)
	if err == nil && nestedTooDeep(raw) {
		return RolePolicy{Detail: configured + " is nested deeper than the execution policy parser can read", PublicDetail: unreadable}
	}
	var policy execution.Policy
	if err == nil {
		policy, err = execution.FromBytes(raw, source)
	}
	if err != nil {
		return RolePolicy{Detail: err.Error(), PublicDetail: unreadable}
	}
	summary := policy.Summary()
	roles, _ := summary["roles"].(map[string]any)
	if len(roles) == 0 {
		return RolePolicy{Detail: "this host's execution policy declares no roles", PublicDetail: "execution policy declares no roles"}
	}
	digest, _ := summary["digest"].(string)
	return RolePolicy{Declared: true, policy: policy, digest: digest}
}

// nestedTooDeep is whether json.loads(raw, object_pairs_hook=_no_duplicates) raises
// RecursionError before any other refusal: the bytes decoded as json.loads decodes them first.
func nestedTooDeep(raw []byte) bool {
	text, err := pyjson.DecodeBytes(raw)
	return err == nil && pyjson.HookedRecursion(text, execution.PolicyDepth)
}

// EnvironmentRolePolicy is rolepolicy.declared(): the policy named by this process's
// environment, read ONCE per process. An edit to the file after that is not adopted until a
// restart, exactly as the bridge keeps the policy its main() built.
func EnvironmentRolePolicy() RolePolicy {
	snapshot.Lock()
	defer snapshot.Unlock()
	if snapshot.taken == nil {
		policy := ResolveRolePolicy(map[string]string{execution.EnvPolicy: os.Getenv(execution.EnvPolicy)})
		snapshot.taken = &policy
	}
	return *snapshot.taken
}

// ResetRolePolicySnapshot is rolepolicy.reset(): drop this process's snapshot (tests only).
func ResetRolePolicySnapshot() {
	snapshot.Lock()
	defer snapshot.Unlock()
	snapshot.taken = nil
}

var snapshot struct {
	sync.Mutex
	taken *RolePolicy
}

// Summary is Declared.summary / Unresolved.summary: the public reading of this policy.
func (p RolePolicy) Summary() contract.OrderedObject {
	if !p.Declared {
		return contract.OrderedObject{{Key: "state", Value: "unresolved"}, {Key: "digest", Value: nil},
			{Key: "roles", Value: contract.OrderedObject{}}, {Key: "detail", Value: p.PublicDetail}}
	}
	roles, _ := p.policy.Summary()["roles"].(map[string]any)
	ordered := contract.OrderedObject{}
	for _, name := range p.policy.RoleOrder() {
		entry, _ := roles[name].(map[string]any)
		fields := contract.OrderedObject{{Key: "role", Value: entry["role"]},
			{Key: "expectation", Value: entry["expectation"]}, {Key: "model", Value: entry["model"]}, {Key: "reasoningEffort", Value: entry["reasoningEffort"]}}
		if pairs, listed := entry["pairs"].([]any); listed {
			// Only a role with several pairs lists them; a role with one is described as it always was.
			values := make([]any, len(pairs))
			for i, item := range pairs {
				pair, _ := item.(map[string]any)
				values[i] = pairObject(pair["model"], pair["reasoningEffort"])
			}
			fields = append(fields, contract.Field{Key: "pairs", Value: values})
		}
		ordered = append(ordered, contract.Field{Key: name, Value: fields})
	}
	return contract.OrderedObject{{Key: "state", Value: "declared"}, {Key: "digest", Value: p.digest}, {Key: "roles", Value: ordered}, {Key: "detail", Value: nil}}
}

// Digest is Declared.digest, "" for Unresolved (None).
func (p RolePolicy) Digest() string {
	if !p.Declared {
		return ""
	}
	return p.digest
}

func (p RolePolicy) digestValue() any {
	if !p.Declared {
		return nil
	}
	return p.digest
}

// expectation is what the policy declares for role: its expectation and the pairs it may run on.
func (p RolePolicy) expectation(role string) (execution.Role, bool) {
	if !p.Declared || role == "" {
		return execution.Role{}, false
	}
	return p.policy.Role(role)
}

// allows is whether a recorded or requested pair is one of the role's pairs. A value that is not
// text is no pair of any role.
func allows(role execution.Role, model, effort any) bool {
	m, modelOK := model.(string)
	e, effortOK := effort.(string)
	return modelOK && effortOK && role.Allows(m, e)
}

// expectedPairs is how a finding names what the role runs: the pair, as an object, for a role with
// one, and a list of pair objects for a role with several.
func expectedPairs(role execution.Role) any {
	if len(role.Pairs) == 1 {
		return pairObject(role.Pairs[0].Model, role.Pairs[0].Effort)
	}
	pairs := make([]any, len(role.Pairs))
	for i, pair := range role.Pairs {
		pairs[i] = pairObject(pair.Model, pair.Effort)
	}
	return pairs
}

// SummaryAllowsPair is whether a role entry of a policy summary declares the pair: a member of its
// pairs list, or the model and reasoningEffort it states when it lists none. The entry is read the
// way RolePolicy.Summary spells it or the way a worker's receipt of it was decoded.
func SummaryAllowsPair(entry any, model, effort any) bool {
	wantModel, modelOK := model.(string)
	wantEffort, effortOK := effort.(string)
	if !modelOK || !effortOK {
		return false
	}
	read := func(from any, key string) any {
		switch v := from.(type) {
		case contract.OrderedObject:
			return v.Get(key)
		case map[string]any:
			return v[key]
		}
		return nil
	}
	matches := func(pair any) bool {
		m, mOK := read(pair, "model").(string)
		e, eOK := read(pair, "reasoningEffort").(string)
		return mOK && eOK && m == wantModel && e == wantEffort
	}
	if listed, ok := read(entry, "pairs").([]any); ok {
		for _, pair := range listed {
			if matches(pair) {
				return true
			}
		}
		return false
	}
	return matches(entry)
}

// exceptionCovers is ExecutionPolicy.exception_covers, decided by the bridge's own Authorize.
func (p RolePolicy) exceptionCovers(name any, role string, model, effort, cwd any) bool {
	id, ok := name.(string)
	if !p.Declared || !ok || id == "" {
		return false
	}
	cwdText, ok := cwd.(string)
	if !ok || cwdText == "" {
		return false
	}
	_, err := p.policy.Authorize(execution.Input{Model: model, Effort: effort, CWD: cwdText, Exception: id, Role: role})
	return err == nil
}

func pairOf(settings contract.OrderedObject) (any, any) {
	model, _ := settings.Lookup("model")
	effort, _ := settings.Lookup("reasoningEffort")
	return model, effort
}

// citedRole and citedException are rolepolicy.cited_role / cited_exception.
func citedRole(settings contract.OrderedObject) any {
	v, _ := settings.Lookup("citedRole")
	return v
}
func citedException(settings contract.OrderedObject) any {
	v, _ := settings.Lookup("citedException")
	return v
}

func authorizedByException(settings contract.OrderedObject, role string, policy RolePolicy) bool {
	model, effort := pairOf(settings)
	cwd, _ := settings.Lookup("cwd")
	return policy.exceptionCovers(citedException(settings), role, model, effort, cwd)
}

// declaredRoleFor is rolepolicy.declared_pair_for: the role's declaration when it expects a pair;
// ok=false is None.
func declaredRoleFor(role string, policy RolePolicy) (execution.Role, bool) {
	expectation, ok := policy.expectation(role)
	if !ok || expectation.Expectation != "pair" {
		return execution.Role{}, false
	}
	return expectation, true
}

func pairObject(model, effort any) contract.OrderedObject {
	return contract.OrderedObject{{Key: "model", Value: model}, {Key: "reasoningEffort", Value: effort}}
}

func unverifiedCitation(settings contract.OrderedObject, role string, policy RolePolicy) contract.OrderedObject {
	name := citedException(settings)
	if name == nil || !policy.Declared || authorizedByException(settings, role, policy) {
		return nil
	}
	return contract.OrderedObject{
		{Key: "code", Value: string(contract.RefusalRoleBindingMismatch)},
		{Key: "role", Value: role},
		{Key: "citedException", Value: name},
		{Key: "digest", Value: policy.digest},
		{Key: "detail", Value: "this record cites exception " + pyvalue.Repr(name) + ", and this host's execution policy does not " +
			"authorize that id for role " + pyvalue.StrRepr(role) + " with this pair and directory. Record what " +
			"actually authorized the creation, or nothing at all"},
		{Key: "recovery", Value: RoleRecovery},
	}
}

// CheckRecord is rolepolicy.check_record for a Declared policy.
func CheckRecord(settings contract.OrderedObject, role string, policy RolePolicy) contract.OrderedObject {
	if unverified := unverifiedCitation(settings, role, policy); unverified != nil {
		return unverified
	}
	expectation, ok := policy.expectation(role)
	if !ok {
		if authorizedByException(settings, role, policy) {
			return nil
		}
		return contract.OrderedObject{
			{Key: "code", Value: string(contract.RefusalRolePolicyUnconfigured)},
			{Key: "role", Value: role},
			{Key: "digest", Value: policy.digest},
			{Key: "undeclared", Value: true},
			{Key: "recovery", Value: "declare role " + pyvalue.StrRepr(role) + " in this host's execution policy"},
		}
	}
	if expectation.Expectation != "pair" {
		return nil
	}
	model, effort := pairOf(settings)
	if allows(expectation, model, effort) {
		return nil
	}
	if authorizedByException(settings, role, policy) {
		return nil
	}
	return contract.OrderedObject{
		{Key: "code", Value: string(contract.RefusalSettingsRecordStaleForRole)},
		{Key: "role", Value: role},
		{Key: "recorded", Value: pairObject(model, effort)},
		{Key: "expected", Value: expectedPairs(expectation)},
		{Key: "digest", Value: policy.digest},
		{Key: "recovery", Value: RoleRecovery},
	}
}

// CheckBinding is rolepolicy.check_binding. cited is nil for None.
func CheckBinding(cited any, bound string, settings contract.OrderedObject, policy RolePolicy) contract.OrderedObject {
	if _, ok := roleScope[bound]; !ok {
		return nil
	}
	if unverified := unverifiedCitation(settings, bound, policy); unverified != nil {
		return append(unverified, contract.Field{Key: "citedRole", Value: cited}, contract.Field{Key: "boundRole", Value: bound})
	}
	if cited != nil && cited != any(bound) {
		return contract.OrderedObject{
			{Key: "code", Value: string(contract.RefusalRoleBindingMismatch)},
			{Key: "citedRole", Value: cited},
			{Key: "boundRole", Value: bound},
			{Key: "digest", Value: policy.digestValue()},
			{Key: "detail", Value: "this task was created citing role " + pyvalue.Repr(cited) + " and is being bound as " + pyvalue.StrRepr(bound) + "; " +
				"one of the two is wrong, and re-recording its settings would only hide that"},
		}
	}
	if !policy.Declared {
		return nil
	}
	expectation, ok := policy.expectation(bound)
	if !ok {
		if authorizedByException(settings, bound, policy) {
			return nil
		}
		return contract.OrderedObject{
			{Key: "code", Value: string(contract.RefusalRolePolicyUnconfigured)},
			{Key: "citedRole", Value: cited},
			{Key: "boundRole", Value: bound},
			{Key: "digest", Value: policy.digest},
			{Key: "detail", Value: "this host's execution policy declares no role " + pyvalue.StrRepr(bound) + ", so a task cannot be " +
				"bound to it and checked; declare it before binding"},
		}
	}
	if expectation.Expectation != "pair" {
		return nil
	}
	model, effort := pairOf(settings)
	if allows(expectation, model, effort) {
		return nil
	}
	if authorizedByException(settings, bound, policy) {
		return nil
	}
	return contract.OrderedObject{
		{Key: "code", Value: string(contract.RefusalRoleBindingMismatch)},
		{Key: "citedRole", Value: cited},
		{Key: "boundRole", Value: bound},
		{Key: "recorded", Value: pairObject(model, effort)},
		{Key: "expected", Value: expectedPairs(expectation)},
		{Key: "digest", Value: policy.digest},
		{Key: "detail", Value: "this task's recorded pair is not the pair role " + pyvalue.StrRepr(bound) + " runs on, and it is being " +
			"bound to that role now rather than having drifted afterwards"},
	}
}

// boundRole is rolepolicy.bound_role(_in): the role, "" for None, or contested roles (sorted).
func boundRole(ctx context.Context, s *store.Store, taskID string) (string, []string, error) {
	rows, err := s.Querier(ctx).QueryContext(ctx, "SELECT DISTINCT role FROM scope_bindings WHERE task_id = ?"+
		"   AND status IN (?,?)   AND superseded_by IS NULL", taskID, "active", "paused")
	if err != nil {
		return "", nil, err
	}
	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			_ = rows.Close()
			return "", nil, err
		}
		roles = append(roles, role)
	}
	if err := rows.Close(); err != nil {
		return "", nil, err
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	switch len(roles) {
	case 0:
		return "", nil, nil
	case 1:
		return roles[0], nil, nil
	}
	sort.Strings(roles)
	return "", roles, nil
}

// SettingsFreeRefusalCode is the code a settings-free resume refuses with (the host adapter's
// verifyResume, as bridge_adapter.py's):
// the first finding's code, except that a difference (settings_not_preserved) is renamed
// settings_differ_after_load, because nothing was transmitted that could have made it agree.
func SettingsFreeRefusalCode(findings []contract.OrderedObject) string {
	if len(findings) == 0 {
		return ""
	}
	code := findingText(findings[0], "code")
	if code == SettingsNotPreserved {
		return SettingsDifferAfterLoad
	}
	return code
}
