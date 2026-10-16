package role

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// The flow-only role keys (CRW-1330, CRW-1316 design section 8; the critic CRW-1434). A flow run resolves its calls against six
// roles; four are the helper roles of Roles() and two exist only for the flow. All six are read from the same roles object of
// the store. Roles() is the CXC oracle's list, which the GUI bundle, the MCP tools and the spawn hooks read, so the two keys are
// not in it: the helper's list and settings answers never show them, and `crw role helper get|set|reset` take them by name.
const (
	ExecutorT1 RoleName = "executor-t1"
	// ReviewerFinal is the retired role key (CRW-1450) that old journal rows, policy files and role stores carry; no active list
	// holds it. A store entry of it is never read into a pair: the snapshot names it with RetiredKeyNotice, get and set of it are
	// refused, and `crw role helper reset reviewer-final` removes it.
	ReviewerFinal RoleName = "reviewer-final"
	// Critic audits the architect's design in a flow run's design loop (CRW-1434).
	Critic RoleName = "critic"
)

// RetiredKeyNotice is what the store's readers and writers say of the retired reviewer-final key (CRW-1450).
// execution.RetiredRoleKeyNotice is the same text.
const RetiredKeyNotice = string(ReviewerFinal) + " is a retired role key and is ignored"

// FlowOnlyRoles is the two flow-only keys.
func FlowOnlyRoles() []RoleName { return []RoleName{ExecutorT1, Critic} }

// FlowRoles is the six roles of a flow run in the order the flow lists them (execution.AgentRoleNames); the critic is last, so
// a table of the five earlier roles keeps its rows.
func FlowRoles() []RoleName {
	return []RoleName{Architect, Executor, ExecutorT1, Reviewer, Explorer, Critic}
}

func flowOnlyRole(r RoleName) bool { return r == ExecutorT1 || r == Critic }

// retiredRole reports whether r is the retired role key.
func retiredRole(r RoleName) bool { return r == ReviewerFinal }

// storeRole reports whether a write or a read by name may address the role: a helper role, a flow-only key, or the retired key,
// which only a reset addresses (setRoleSettings and the helper's get and set refuse it).
func storeRole(r RoleName) bool { return validRole(r) || flowOnlyRole(r) || retiredRole(r) }

// flowCompiledDefaults are the pairs a flow-only key takes when the store holds no usable entry for it (an entry in mode default
// is undecided instead). They are never the executor's or the reviewer's pair, so a tier step to executor-t1 stays a step to
// another pair. The critic's is a gpt pair at xhigh: a model family other than the architect's, which is what the design audit's
// independence is judged by.
var flowCompiledDefaults = map[RoleName][2]string{
	ExecutorT1: {"anthropic/claude-sonnet-5-5", "xhigh"},
	Critic:     {"gpt-6.1-sol", "xhigh"},
}

// FlowRoleState says where a role's pair in a FlowSnapshot came from.
type FlowRoleState string

const (
	// FlowRoleStored: the store's entry names the model (its effort may still be empty, which is undecided).
	FlowRoleStored FlowRoleState = "store"
	// FlowRoleCompiledDefault: a flow-only key the store has no usable entry for; the compiled default applies.
	FlowRoleCompiledDefault FlowRoleState = "compiled_default"
	// FlowRoleUndecided: the role inherits the session's model (mode default, no entry, or an entry that cannot be used);
	// the flow has no pair for it and a call of the role names no model and no effort.
	FlowRoleUndecided FlowRoleState = "undecided"
)

// FlowRole is one role's pair in a FlowSnapshot; Model and Effort are empty when they are undecided.
type FlowRole struct {
	Role   RoleName      `json:"role"`
	Model  string        `json:"model"`
	Effort string        `json:"effort"`
	State  FlowRoleState `json:"state"`
}

// FlowSnapshot is one read of the store as a flow run reads it: the store's path, whether a file is there, the pair of each of
// the six roles and a warning for every role whose pair is not the store's own complete pair. Reading it never fails and never
// writes: a store or a role that cannot be used is a warning and an undecided role (or the compiled default of a flow-only key),
// because a reversible choice is warned and recorded rather than refused (Jun's standing rule).
type FlowSnapshot struct {
	Path     string     `json:"path"`
	Present  bool       `json:"present"`
	Roles    []FlowRole `json:"roles"`
	Warnings []string   `json:"warnings"`
}

// ReadFlowSnapshot reads the store once for a flow run.
func ReadFlowSnapshot(env host.LookupEnv) FlowSnapshot {
	snap := FlowSnapshot{Warnings: []string{}}
	path, err := StorePath(env)
	var raw rawConfig
	if err == nil {
		snap.Path = path
		if _, statErr := os.Lstat(path); statErr == nil {
			snap.Present = true
		}
		raw, err = readRaw(env, path, false)
	}
	if err != nil {
		var unusable *UnusableSettingsError
		if !errors.As(err, &unusable) {
			err = &UnusableSettingsError{Path: snap.Path, Reason: err.Error()}
		}
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("the helper role store cannot be used (%v): every role without a compiled default is undecided", err))
	}
	for _, name := range FlowRoles() {
		role, warning := flowRoleOf(name, raw, err == nil, snap.Path)
		snap.Roles = append(snap.Roles, role)
		if warning != "" {
			snap.Warnings = append(snap.Warnings, warning)
		}
	}
	// An entry of the retired key is never parsed and takes no pair, so it is not in the roles or the digest; it is named once.
	if err == nil {
		if _, present := raw.roles.raw(string(ReviewerFinal)); present {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("the helper role store %s holds %s: %s (crw role helper reset %s removes the entry)", snap.Path, ReviewerFinal, RetiredKeyNotice, ReviewerFinal))
		}
	}
	return snap
}

// flowRoleOf is one role of the snapshot and its warning ("" for the store's own complete pair).
func flowRoleOf(name RoleName, raw rawConfig, usable bool, path string) (FlowRole, string) {
	role := FlowRole{Role: name, State: FlowRoleUndecided}
	var cfg RoleConfig
	var reason string
	present := false
	if usable {
		var value json.RawMessage
		if value, present = raw.roles.raw(string(name)); present {
			cfg, reason = parseRole(value)
		}
	}
	setHint := fmt.Sprintf("set it with crw role helper set %s --mode model --model <id> --effort <level>", name)
	if _, flowOnly := flowCompiledDefaults[name]; flowOnly {
		setHint += fmt.Sprintf(", or crw role helper reset %s for the compiled default", name)
	}
	// A flow-only key takes its compiled default only when the store holds no usable entry for it; an entry in mode default is
	// the operator's decision to leave it undecided, as for a helper role.
	if def, ok := flowCompiledDefaults[name]; ok && (!present || reason != "") {
		role.Model, role.Effort, role.State = def[0], def[1], FlowRoleCompiledDefault
		why := "has no " + string(name) + " entry"
		switch {
		case !usable:
			why = "cannot be used"
		case reason != "":
			why = "cannot use its " + string(name) + " entry (" + reason + ")"
		}
		return role, fmt.Sprintf("the helper role store %s %s: the flow uses the compiled default %s at %s for %s (%s)", path, why, def[0], def[1], name, setHint)
	}
	switch {
	case !usable:
		return role, "" // the store's own warning says why
	case reason != "":
		return role, fmt.Sprintf("role %s in %s cannot be used (%s): its pair is undecided (%s)", name, path, reason, setHint)
	case !present || cfg.Mode != ModeModel:
		return role, fmt.Sprintf("role %s is in mode default in %s: it inherits the session's model, so its pair is undecided and a call of the role names no model or effort (%s)", name, path, setHint)
	}
	role.Model, role.State = *cfg.Model, FlowRoleStored
	if cfg.Effort == nil {
		return role, fmt.Sprintf("role %s names model %s and no effort in %s: its effort is undecided (%s)", name, role.Model, path, setHint)
	}
	role.Effort = string(*cfg.Effort)
	return role, ""
}

// Role is the snapshot's entry for a role.
func (s FlowSnapshot) Role(name RoleName) (FlowRole, bool) {
	for _, r := range s.Roles {
		if r.Role == name {
			return r, true
		}
	}
	return FlowRole{}, false
}

// Digest is the canonical digest of the six (model, effort) pairs: SHA-256 of the compact JSON array of [role, model, effort]
// in FlowRoles order, undecided values empty. Nothing else of the store (prompt overrides, fallbacks, member order, other
// members) is in it, so a run's pinned digest changes exactly when a pair it runs on does.
func (s FlowSnapshot) Digest() string {
	rows := make([][3]string, 0, len(s.Roles))
	for _, name := range FlowRoles() {
		r, _ := s.Role(name)
		rows = append(rows, [3]string{string(name), r.Model, r.Effort})
	}
	data, _ := json.Marshal(rows)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// flowRoleView is what `crw role helper get|set|reset` answers for a flow-only key: the role's settings as the store holds them,
// or the compiled default the flow uses when the store holds none, with the source named as the flow reads it (store,
// compiled_default, or undecided for an entry in mode default).
func flowRoleView(env host.LookupEnv, name RoleName) (object, error) {
	path, err := StorePath(env)
	if err != nil {
		return nil, &UnusableSettingsError{Reason: err.Error()}
	}
	raw, err := readRaw(env, path, false)
	if err != nil {
		return nil, err
	}
	cfg, source := RoleConfig{}, FlowRoleStored
	value, present := raw.roles.raw(string(name))
	if present {
		var reason string
		if cfg, reason = parseRole(value); reason != "" {
			return nil, &UnusableSettingsError{Path: path, Role: name, Reason: reason}
		}
		if cfg.Mode != ModeModel {
			source = FlowRoleUndecided // mode default: the flow has no pair for the role (a reset brings back the compiled default)
		}
	} else {
		def := flowCompiledDefaults[name]
		model, effort := def[0], EffortName(def[1])
		cfg.Mode, cfg.Model, cfg.Effort, source = ModeModel, &model, &effort, FlowRoleCompiledDefault
	}
	return object{{"mode", cfg.Mode}, {"model", cfg.Model}, {"effort", cfg.Effort}, {"promptOverride", cfg.PromptOverride}, {"fallback", cfg.Fallback}, {"source", source}}, nil
}
