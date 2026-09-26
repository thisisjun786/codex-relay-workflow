package registry

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
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

type roleExpectation struct{ Expectation, Model, Effort string }

// ResolveRolePolicy is rolepolicy._resolve over one environment.
func ResolveRolePolicy(env map[string]string) RolePolicy {
	configured := strings.TrimSpace(env[execution.EnvPolicy])
	if configured == "" {
		return RolePolicy{Detail: execution.EnvPolicy + " is not set in this process, so no role policy can be read",
			PublicDetail: "execution policy environment is not configured in this process"}
	}
	policy, err := execution.FromFile(configured)
	if err != nil {
		return RolePolicy{Detail: err.Error(), PublicDetail: "configured execution policy is unreadable or invalid"}
	}
	summary := policy.Summary()
	roles, _ := summary["roles"].(map[string]any)
	if len(roles) == 0 {
		return RolePolicy{Detail: "this host's execution policy declares no roles", PublicDetail: "execution policy declares no roles"}
	}
	digest, _ := summary["digest"].(string)
	return RolePolicy{Declared: true, policy: policy, digest: digest}
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
	names := make([]string, 0, len(roles))
	for name := range roles {
		names = append(names, name)
	}
	sort.Strings(names)
	ordered := contract.OrderedObject{}
	for _, name := range names {
		entry, _ := roles[name].(map[string]any)
		ordered = append(ordered, contract.Field{Key: name, Value: contract.OrderedObject{{Key: "role", Value: entry["role"]},
			{Key: "expectation", Value: entry["expectation"]}, {Key: "model", Value: entry["model"]}, {Key: "reasoningEffort", Value: entry["reasoningEffort"]}}})
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

func (p RolePolicy) expectation(role string) (roleExpectation, bool) {
	if !p.Declared || role == "" {
		return roleExpectation{}, false
	}
	roles, _ := p.policy.Summary()["roles"].(map[string]any)
	entry, ok := roles[role].(map[string]any)
	if !ok {
		return roleExpectation{}, false
	}
	out := roleExpectation{}
	out.Expectation, _ = entry["expectation"].(string)
	out.Model, _ = entry["model"].(string)
	out.Effort, _ = entry["reasoningEffort"].(string)
	return out, true
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
	model, _ := getField(settings, "model")
	effort, _ := getField(settings, "reasoningEffort")
	return model, effort
}

// citedRole and citedException are rolepolicy.cited_role / cited_exception.
func citedRole(settings contract.OrderedObject) any {
	v, _ := getField(settings, "citedRole")
	return v
}
func citedException(settings contract.OrderedObject) any {
	v, _ := getField(settings, "citedException")
	return v
}

func authorizedByException(settings contract.OrderedObject, role string, policy RolePolicy) bool {
	model, effort := pairOf(settings)
	cwd, _ := getField(settings, "cwd")
	return policy.exceptionCovers(citedException(settings), role, model, effort, cwd)
}

// declaredPairFor is rolepolicy.declared_pair_for; ok=false is None.
func declaredPairFor(role string, policy RolePolicy) (string, string, bool) {
	expectation, ok := policy.expectation(role)
	if !ok || expectation.Expectation != "pair" {
		return "", "", false
	}
	return expectation.Model, expectation.Effort, true
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
		{Key: "detail", Value: "this record cites exception " + pyRepr(name) + ", and this host's execution policy does not " +
			"authorize that id for role " + pyStr(role) + " with this pair and directory. Record what " +
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
			{Key: "recovery", Value: "declare role " + pyStr(role) + " in this host's execution policy"},
		}
	}
	if expectation.Expectation != "pair" {
		return nil
	}
	model, effort := pairOf(settings)
	if model == expectation.Model && effort == expectation.Effort {
		return nil
	}
	if authorizedByException(settings, role, policy) {
		return nil
	}
	return contract.OrderedObject{
		{Key: "code", Value: string(contract.RefusalSettingsRecordStaleForRole)},
		{Key: "role", Value: role},
		{Key: "recorded", Value: pairObject(model, effort)},
		{Key: "expected", Value: pairObject(expectation.Model, expectation.Effort)},
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
			{Key: "detail", Value: "this task was created citing role " + pyRepr(cited) + " and is being bound as " + pyStr(bound) + "; " +
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
			{Key: "detail", Value: "this host's execution policy declares no role " + pyStr(bound) + ", so a task cannot be " +
				"bound to it and checked; declare it before binding"},
		}
	}
	if expectation.Expectation != "pair" {
		return nil
	}
	model, effort := pairOf(settings)
	if model == expectation.Model && effort == expectation.Effort {
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
		{Key: "expected", Value: pairObject(expectation.Model, expectation.Effort)},
		{Key: "digest", Value: policy.digest},
		{Key: "detail", Value: "this task's recorded pair is not the pair role " + pyStr(bound) + " runs on, and it is being " +
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

// taskIDOf is rolepolicy.task_id_of: whatever names this record in a message (its cwd).
func taskIDOf(settings contract.OrderedObject) string {
	cwd, _ := getField(settings, "cwd")
	if !truthyValue(cwd) {
		return pyStr("this recipient")
	}
	return pyRepr(cwd)
}

// CheckUnloadedTransmission is rolepolicy.check_unloaded_transmission: the refusal for
// transmitting a pair policy did not derive to a thread not yet loaded, or nil. Provenance, not
// resemblance: a cited exception is never derived, even when it equals the role's pair.
func CheckUnloadedTransmission(settings contract.OrderedObject, role string, policy RolePolicy, runtimeStatus string) error {
	if runtimeStatus != "notLoaded" || !policy.Declared {
		return nil
	}
	expectation, ok := policy.expectation(role)
	model, effort := pairOf(settings)
	if citedException(settings) == nil && ok && expectation.Expectation == "pair" && model == any(expectation.Model) && effort == any(expectation.Effort) {
		return nil
	}
	return refuse(contract.RefusalUnverifiedPairForUnloadedThread, "%s is bound as %s, the host reports it as notLoaded, and "+
		"the pair this send would transmit was not derived from a declared role pair (policy "+
		"%s). Nothing was sent and no turn was started: a resume may apply what it "+
		"transmits to a thread the host has to load first, which would restore a pair the user "+
		"may have changed. Send once the host has the thread loaded, or read its current "+
		"settings and re-record the authorization from that reading.", taskIDOf(settings), pyStr(role), policy.digest)
}

// SettingsFreeRefusalCode is the code a settings-free resume refuses with (bridge_adapter.py):
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
