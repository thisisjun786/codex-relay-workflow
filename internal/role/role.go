// Package role is the helper role configuration store: the per-role settings (mode, model, reasoning effort, prompt override and a
// first fallback) of the four helper roles, in one JSON file at CRW_HOME/subagents.json. It is the Go form of CXC v0.2.40
// subagent-config/src/store.ts (the store, lines 1-303, then the spawn resolution and the project trust token) and settings-api.ts
// under the CRW names of contract/schema/cxc/name-substitution.json; the CLI, the catalog, the MCP tools and the hooks are later
// issues, and nothing here registers a hook, tool or command.
//
// The store is the global layer only (decision 7), so the oracle's project layer is not ported (I1): STATE_DIR, storePath,
// scopedPath, the trust warning and writeConfig (its only target was the project file, and nothing called it). Other differences:
// I2 ParseScope takes an optional scope that defaults to "global" and accepts nothing else. I3 The legacy
// <CODEX_HOME>/codexclaw/subagents.json is not read. I5 A patch holds typed values; a request member of the wrong JSON type is kept
// raw and refused by Validate in the oracle's order with the oracle's message ("promptOverride must be a string or null", "fallback must
// be an object or null", and String() of the value in the mode and effort messages, which throws for an object that has a toString
// member), and the settings API reads its members by exact name, as JavaScript does. I6 Members this package does not own are carried
// as the file wrote them (a number keeps its form, an escape its spelling, integer-like keys are not moved first; U+2028 and U+2029
// print literally, as JSON.stringify prints them), and a stored role value that is not an object is replaced. I7 The text of a JSON
// syntax or IO error is Go's, not V8's or libuv's. I8 A lone surrogate in a role's string reads as U+FFFD, and so does each byte of an
// ill-formed UTF-8 sequence (Node writes one per maximal sequence); a key holding U+FFFD is identified by its literal text, so keys
// that differ only by lone surrogates stay apart, and "\ufffd", a literal U+FFFD key and two spellings of one lone surrogate (one key
// each in JavaScript) stay two. I9 A
// store nested past 10,000 levels is refused by encoding/json: it reads as malformed and a write is refused with the file untouched;
// the oracle's JSON.stringify also refuses a write when an unknown member is nested a few thousand levels, which the port makes.
// I10 SetRole and ResetRole hold an exclusive lock file beside the store (lockStore), where the oracle publishes with no lock and two
// concurrent writers lose an update (a data-loss defect, fixed): a writer that meets the lock waits on the schedule of the session state
// lock and is then refused; a lock left by a dead process refuses writes until it is removed by hand (no stale-lock breaker); the
// first write creates the store's directory before it takes the lock, so a refused first write, or a ResetRole of a missing store, can
// leave an empty 0700 directory. I11 ResolveSpawnConfig of a role the oracle does not know is the refusal "unknown role" (the oracle
// throws a V8 TypeError from an unguarded lookup); it takes no working directory and has no trust warning (no project layer). I12
// IsTrackedProjectConfig and ProjectConfigTrustToken keep their commands and token over <cwd>/.crw/subagents.json, which nothing reads
// any more; git output that is not valid UTF-8 gives no token.
package role

import (
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

const StoreFile = "subagents.json"

type RoleName string

const (
	Explorer  RoleName = "explorer"
	Reviewer  RoleName = "reviewer"
	Executor  RoleName = "executor"
	Architect RoleName = "architect"
)

func Roles() []RoleName { return []RoleName{Explorer, Reviewer, Executor, Architect} }

func validRole(r RoleName) bool {
	switch r {
	case Explorer, Reviewer, Executor, Architect:
		return true
	}
	return false
}

// RoleMode is the main Codex model (default) or the model the role names.
type RoleMode string

const (
	ModeDefault RoleMode = "default"
	ModeModel   RoleMode = "model"
)

// EffortName is a spawn reasoning effort in the host's wire values; no effort inherits the parent session's.
type EffortName string

const (
	EffortLow    EffortName = "low"
	EffortMedium EffortName = "medium"
	EffortHigh   EffortName = "high"
	EffortXHigh  EffortName = "xhigh"
)

func Efforts() []EffortName { return []EffortName{EffortLow, EffortMedium, EffortHigh, EffortXHigh} }

func validEffort(e EffortName) bool {
	switch e {
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh:
		return true
	}
	return false
}

// RoleFallback is the first fallback after a role's primary candidate.
type RoleFallback struct {
	Model  string      `json:"model"`
	Effort *EffortName `json:"effort"`
}

// RoleConfig is one role's settings; a nil pointer is the oracle's null.
type RoleConfig struct {
	Mode           RoleMode      `json:"mode"`
	Model          *string       `json:"model"`
	Effort         *EffortName   `json:"effort"`
	PromptOverride *string       `json:"promptOverride"`
	Fallback       *RoleFallback `json:"fallback"`
}

// RoleMap holds a value per role and encodes them in Roles order. Encode with Stringify: encoding/json would escape HTML.
type RoleMap[T any] map[RoleName]T

func (m RoleMap[T]) MarshalJSON() ([]byte, error) {
	var o object
	for _, r := range Roles() {
		if v, ok := m[r]; ok {
			o = append(o, member{string(r), v})
		}
	}
	return o.MarshalJSON()
}

type Config struct {
	Roles RoleMap[RoleConfig] `json:"roles"`
}

// ConfigScope is the layer a settings call addresses; only the global layer exists (I1).
type ConfigScope string

const ScopeGlobal ConfigScope = "global"

// ConfigSource is where a role's setting came from: the store, or the original session (no override).
type ConfigSource string

const (
	SourceGlobal  ConfigSource = "global"
	SourceSession ConfigSource = "session"
)

// Settings is the effective settings with each role's source (SubagentSettings).
type Settings struct {
	Roles     RoleMap[RoleConfig]   `json:"roles"`
	Scope     ConfigScope           `json:"scope"`
	Sources   RoleMap[ConfigSource] `json:"sources"`
	Overrides RoleMap[bool]         `json:"overrides"`
}

// ParseScope is configScope: nil is the default scope, and every value but "global" is refused (I2).
func ParseScope(value *string) (ConfigScope, error) {
	if value == nil || *value == string(ScopeGlobal) {
		return ScopeGlobal, nil
	}
	return "", fmt.Errorf("invalid scope \"%s\"", *value)
}

// DefaultRole is a role with no override: the main model, the session's effort, no prompt, no fallback.
func DefaultRole() RoleConfig { return RoleConfig{Mode: ModeDefault} }

func DefaultConfig() Config {
	roles := RoleMap[RoleConfig]{}
	for _, r := range Roles() {
		roles[r] = DefaultRole()
	}
	return Config{Roles: roles}
}

// reconstructRole normalises one persisted role field by field, never failing: a value that is not an object is the default role,
// and a model mode without a usable model is the default mode (the effort, prompt and fallback survive).
func reconstructRole(raw json.RawMessage) RoleConfig {
	fields, ok := members(raw)
	if !ok {
		return DefaultRole()
	}
	role := RoleConfig{Mode: ModeDefault, Effort: effortOf(fields["effort"]), Fallback: reconstructFallback(fields["fallback"])}
	if prompt, ok := stringOf(fields["promptOverride"]); ok {
		role.PromptOverride = &prompt
	}
	if mode, _ := stringOf(fields["mode"]); mode == string(ModeModel) {
		if model, ok := stringOf(fields["model"]); ok && model != "" {
			role.Mode, role.Model = ModeModel, &model
		}
	}
	return role
}

// reconstructFallback keeps a fallback whose model is not blank after a JavaScript trim (and stays untrimmed).
func reconstructFallback(raw json.RawMessage) *RoleFallback {
	fields, ok := members(raw)
	if !ok {
		return nil
	}
	if model, ok := stringOf(fields["model"]); ok && text.Trim(model) != "" {
		return &RoleFallback{Model: model, Effort: effortOf(fields["effort"])}
	}
	return nil
}

// members is a JSON object's members by key, the last of a repeated key winning; ok is false for anything but an object or null.
func members(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	err := json.Unmarshal(raw, &m)
	return m, err == nil
}

func stringOf(raw json.RawMessage) (s string, ok bool) {
	ok = len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil
	return s, ok
}

func effortOf(raw json.RawMessage) *EffortName {
	if s, ok := stringOf(raw); ok && validEffort(EffortName(s)) {
		e := EffortName(s)
		return &e
	}
	return nil
}
