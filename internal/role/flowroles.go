package role

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// The flow-only role keys (CRW-1330, CRW-1316 design section 8; the critic CRW-1434; the unit check CRW-1589). A flow run
// resolves its calls against six agent roles; four are the helper roles of Roles() and two exist only for the flow. The unit
// check is a seventh key of the same roles object: the pair the shipped unit check's review tool runs on — a tool run, not an
// agent call, so nothing spawns it. All seven are read from the same roles object of the store. Roles() is the CXC oracle's
// list, which the GUI bundle, the MCP tools and the spawn hooks read, so the three keys are not in it: the helper's list and
// settings answers never show them, and `crw role helper get|set|reset` take them by name.
const (
	ExecutorT1 RoleName = "executor-t1"
	// ReviewerFinal is the retired role key (CRW-1450) that old journal rows, policy files and role stores carry; no active list
	// holds it. A store entry of it is never read into a pair: the snapshot names it with RetiredKeyNotice, get and set of it are
	// refused, and `crw role helper reset reviewer-final` removes it.
	ReviewerFinal RoleName = "reviewer-final"
	// Critic audits the architect's design in a flow run's design loop (CRW-1434). A store that declares none gives it the
	// reviewer's pair (CRW-1589).
	Critic RoleName = "critic"
	// UnitCheck is the unit check's tool key (CRW-1589): the model and effort the shipped Codex review runs on, recorded in the
	// snapshot and the run's journal like every role's pair.
	UnitCheck RoleName = "unit-check"
)

// The environment overrides of the unit check's pair (CRW-1589): a non-blank value replaces the store's field in the
// snapshot, is named in a warning and recorded in the run's started row.
const (
	UnitCheckModelEnv  = "CRW_UNIT_CHECK_MODEL"
	UnitCheckEffortEnv = "CRW_UNIT_CHECK_EFFORT"
	// UnitCheckDefaultModel and UnitCheckDefaultEffort are the unit check's pair when the store holds no usable unit-check
	// entry (and the effort when a stored model names none): the pair the shipped unit check ran before the key existed.
	UnitCheckDefaultModel  = "gpt-6-astra"
	UnitCheckDefaultEffort = "high"
)

// RetiredKeyNotice is what the store's readers and writers say of the retired reviewer-final key (CRW-1450).
// execution.RetiredRoleKeyNotice is the same text.
const RetiredKeyNotice = string(ReviewerFinal) + " is a retired role key and is ignored"

// FlowOnlyRoles is the three flow-only keys.
func FlowOnlyRoles() []RoleName { return []RoleName{ExecutorT1, Critic, UnitCheck} }

// FlowRoles is the seven roles of a flow run in the order the flow lists them: the six agent roles
// (execution.AgentRoleNames) and then the unit check's tool key, so a table of the six earlier rows keeps them.
func FlowRoles() []RoleName {
	return []RoleName{Architect, Executor, ExecutorT1, Reviewer, Explorer, Critic, UnitCheck}
}

func flowOnlyRole(r RoleName) bool { return r == ExecutorT1 || r == Critic || r == UnitCheck }

// retiredRole reports whether r is the retired role key.
func retiredRole(r RoleName) bool { return r == ReviewerFinal }

// storeRole reports whether a write or a read by name may address the role: a helper role, a flow-only key, or the retired key,
// which only a reset addresses (setRoleSettings and the helper's get and set refuse it).
func storeRole(r RoleName) bool { return validRole(r) || flowOnlyRole(r) || retiredRole(r) }

// flowCompiledDefaults are the pairs a flow-only key takes when the store holds no usable entry for it (an executor-t1 entry
// in mode default is undecided instead; the unit check, a tool, never inherits a session's pair, so its mode default entry
// takes the default too). The executor-t1 default is never the executor's or the reviewer's pair, so a tier step to
// executor-t1 stays a step to another pair. The critic has none: a store without a critic entry inherits the reviewer's pair.
var flowCompiledDefaults = map[RoleName][2]string{
	ExecutorT1: {"anthropic/claude-sonnet-5-5", "xhigh"},
	UnitCheck:  {UnitCheckDefaultModel, UnitCheckDefaultEffort},
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
	// FlowRoleReviewer: a store with no critic entry gives the critic the reviewer's pair (CRW-1589).
	FlowRoleReviewer FlowRoleState = "reviewer"
	// FlowRoleEnvironment: the unit check's pair holds an environment override (CRW_UNIT_CHECK_MODEL/_EFFORT, CRW-1589).
	FlowRoleEnvironment FlowRoleState = "environment"
)

// FlowRole is one role's pair in a FlowSnapshot; Model and Effort are empty when they are undecided.
type FlowRole struct {
	Role   RoleName      `json:"role"`
	Model  string        `json:"model"`
	Effort string        `json:"effort"`
	State  FlowRoleState `json:"state"`
}

// FlowSnapshot is one read of the store as a flow run reads it: the store's path, whether a file is there, the pair of each of
// the seven roles and a warning for every role whose pair is not the store's own complete pair. Reading it never fails and never
// writes: a store or a role that cannot be used is a warning and an undecided role (or the compiled default of a flow-only key),
// because a reversible choice is warned and recorded rather than refused (Jun's standing rule).
type FlowSnapshot struct {
	Path     string     `json:"path"`
	Present  bool       `json:"present"`
	Roles    []FlowRole `json:"roles"`
	Warnings []string   `json:"warnings"`
	// Overrides are the environment values applied to the unit check's row (CRW-1589): the variable's name is the key and the
	// normalized effective value is the value. Absent when no override applied.
	Overrides map[string]string `json:"overrides,omitempty"`
}

// ReadFlowSnapshot reads the store once for a flow run.
func ReadFlowSnapshot(env host.LookupEnv) FlowSnapshot {
	if env == nil {
		env = os.LookupEnv
	}
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
	// The unit check's environment overrides apply per field to the resolved row (CRW-1589); an empty or blank value is none.
	for i := range snap.Roles {
		if snap.Roles[i].Role != UnitCheck {
			continue
		}
		for _, item := range []struct {
			key string
			dst *string
		}{
			{UnitCheckModelEnv, &snap.Roles[i].Model}, {UnitCheckEffortEnv, &snap.Roles[i].Effort},
		} {
			if value, _ := env(item.key); strings.TrimSpace(value) != "" {
				value = strings.TrimSpace(value)
				*item.dst = value
				snap.Roles[i].State = FlowRoleEnvironment
				if snap.Overrides == nil {
					snap.Overrides = map[string]string{}
				}
				snap.Overrides[item.key] = value
				snap.Warnings = append(snap.Warnings, fmt.Sprintf("unit-check uses environment override %s=%s", item.key, value))
			}
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
	// A store with no critic entry gives the critic the reviewer's pair (CRW-1589), including its undecided parts.
	if name == Critic && !present {
		reviewer, _ := flowRoleOf(Reviewer, raw, usable, path)
		role.Model, role.Effort, role.State = reviewer.Model, reviewer.Effort, FlowRoleReviewer
		if role.Model == "" {
			role.State = FlowRoleUndecided
		}
		return role, fmt.Sprintf("the helper role store %s has no critic entry: critic inherits the reviewer pair (%s at %s)", path, role.Model, role.Effort)
	}
	// A flow-only key takes its compiled default when the store holds no usable entry for it; an entry in mode default is
	// the operator's decision to leave it undecided — except the unit check's: a tool never inherits a session's pair, so
	// its mode default entry takes the tool default (CRW-1589).
	if def, ok := flowCompiledDefaults[name]; ok && (!present || reason != "" || name == UnitCheck && cfg.Mode != ModeModel) {
		role.Model, role.Effort, role.State = def[0], def[1], FlowRoleCompiledDefault
		why := "has no " + string(name) + " entry"
		switch {
		case !usable:
			why = "cannot be used"
		case reason != "":
			why = "cannot use its " + string(name) + " entry (" + reason + ")"
		case name == UnitCheck && present:
			why = "has unit-check in mode default"
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
	if name == UnitCheck {
		// the unit check's argv and the digest read the same value: the model is recorded trimmed
		role.Model = strings.TrimSpace(role.Model)
	}
	if cfg.Effort == nil {
		if name == UnitCheck {
			role.Effort = UnitCheckDefaultEffort
			return role, fmt.Sprintf("role %s names model %s and no effort in %s: the unit check uses the default effort %s (%s)", name, role.Model, path, UnitCheckDefaultEffort, setHint)
		}
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

// Digest is the canonical digest of the seven (model, effort) pairs: SHA-256 of the compact JSON array of [role, model, effort]
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
	} else if name == Critic {
		// a store with no critic entry shows the reviewer pair the flow inherits (CRW-1589)
		reviewer, _ := flowRoleOf(Reviewer, raw, true, path)
		if source = FlowRoleReviewer; reviewer.Model == "" {
			source, cfg.Mode = FlowRoleUndecided, ModeDefault
		} else {
			model := reviewer.Model
			cfg.Mode, cfg.Model = ModeModel, &model
			if reviewer.Effort != "" {
				effort := EffortName(reviewer.Effort)
				cfg.Effort = &effort
			}
		}
	} else {
		def := flowCompiledDefaults[name]
		model, effort := def[0], EffortName(def[1])
		cfg.Mode, cfg.Model, cfg.Effort, source = ModeModel, &model, &effort, FlowRoleCompiledDefault
	}
	return object{{"mode", cfg.Mode}, {"model", cfg.Model}, {"effort", cfg.Effort}, {"promptOverride", cfg.PromptOverride}, {"fallback", cfg.Fallback}, {"source", source}}, nil
}
