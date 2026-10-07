// Package policystore reads the host's execution policy, checks a proposed change against it, and
// applies one to the file the plugin wiring record names.
//
// Reading and checking never write: the one file the wiring record names is the only source of a
// policy value, a change is applied to an in-memory copy of the document and judged with the
// bridge's own parser, and a caller can learn whether the host would accept it without touching a
// byte of the file. The write path is the one exception, and it is deliberately narrow (Write, in
// policy_write.go): it takes the lock beside the policy file, judges the candidate with the same
// check, backs the original bytes up, replaces the file atomically, and re-registers it through
// the installer so the wiring record and the file never disagree for longer than that operation.
package policystore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/pluginwiring"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
)

// The named states a reading answers with. A state is never collapsed into another: a host with no
// record is not a host whose record is unreadable, and neither is a success with an empty value.
const (
	NotRegistered = "not_registered"
	Registered    = "registered"
	Unreadable    = "unreadable"
)

// The three answers of the applied field: whether the running relay service holds the file's bytes.
const (
	AppliedApplied       = "applied"
	AppliedNeedsAction   = "needs_user_action"
	AppliedUnverifiable  = "unverifiable"
	AppliedActionRestart = "restart the relay service so it loads the new policy"
	// AppliedActionReregister is the repair when the file bytes no longer match the digest the
	// wiring record names: the bridge launcher refuses those bytes, so the record must be brought up
	// to date before a new bridge can start under them.
	AppliedActionReregister = "re-register the execution policy with crw install register-mcp --re-register-policy --execution-policy <file>"
)

// LookupEnv is os.LookupEnv: a test supplies a map through it.
type LookupEnv func(key string) (value string, set bool)

// Located is where the policy file is and what the wiring record says it hashes to, or why neither
// could be established.
type Located struct {
	State            string
	Path             string
	RegisteredDigest string
	Reason           string
}

// Pair is one model and reasoning effort a role may run on. Both halves are compared as exact
// strings: an effort name belongs to the model beside it, so max and xhigh are never substituted
// for one another. On the wire the effort is spelled reasoningEffort, as the policy document
// spells it, and effort is accepted as an alias.
type Pair struct {
	Model, Effort string
	// AutoCompactTokenLimit is the pair's optional model_auto_compact_token_limit, zero when the
	// pair declares none. It is not part of the pair's identity - the pair is its model and effort -
	// but it does belong to the pair, so a change that rebuilds a role's pairs has to carry it
	// across for every pair it keeps.
	AutoCompactTokenLimit int64
	// limit is the same field exactly as a request spelled it, present only when the request named the
	// key. A request that states zero, null, a fraction or a string has stated a value, and the rule
	// the policy file is held to applies to it: the value is carried into the candidate document and
	// refused there. Collapsing it into the absent representation would approve a document whose pair
	// silently kept an older limit, or quietly dropped the invalid one.
	limit        any
	limitPresent bool
	// conflict is why the two spellings of the effort could not be reconciled, or "" when they
	// could. It is unexported because it is not part of the pair: it is how a mismatch reaches the
	// check as a refused change rather than as a decode failure the API would answer as a bad
	// request.
	conflict string
}

// pairWire is a pair as JSON carries it, for writing.
type pairWire struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
	Effort          string `json:"effort,omitempty"`
	// AutoCompactTokenLimit is written only when the pair declares one, so a pair without it reads
	// back the way it was written.
	AutoCompactTokenLimit int64 `json:"autoCompactTokenLimit,omitempty"`
}

// pairRequest is a pair as a request carries it. The two effort spellings are raw so the reader can
// tell a key that was absent from one supplied empty: the contract refuses two spellings that
// disagree, and an empty value beside a non-empty one is a disagreement rather than an absence.
type pairRequest struct {
	Model           string          `json:"model"`
	ReasoningEffort json.RawMessage `json:"reasoningEffort"`
	Effort          json.RawMessage `json:"effort"`
	// AutoCompactTokenLimit is raw so an absent key is distinguishable from a zero one: zero is not
	// a positive integer, and the parser refuses it rather than reading it as "no limit".
	AutoCompactTokenLimit json.RawMessage `json:"autoCompactTokenLimit"`
}

// UnmarshalJSON reads a pair from either spelling of its effort. A pair that names neither is read
// with an empty effort, which the parser then refuses, rather than silently matching another pair.
// A pair that names both with different values records the disagreement instead of silently
// keeping one of them.
func (p *Pair) UnmarshalJSON(raw []byte) error {
	var wire pairRequest
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	reasoningEffort, reasoningPresent, err := effortField(wire.ReasoningEffort)
	if err != nil {
		return err
	}
	effort, effortPresent, err := effortField(wire.Effort)
	if err != nil {
		return err
	}
	p.Model = wire.Model
	p.Effort, p.conflict = effortAlias(reasoningEffort, reasoningPresent, effort, effortPresent)
	if len(wire.AutoCompactTokenLimit) > 0 {
		value, err := pyjson.Loads(string(wire.AutoCompactTokenLimit), pyjson.LoadOptions{Constants: true, Numbers: pyjson.SpelledNumbers})
		if err != nil {
			return err
		}
		p.limit, p.limitPresent = value, true
		if spelled, ok := value.(json.Number); ok {
			if parsed, err := strconv.ParseInt(string(spelled), 10, 64); err == nil {
				p.AutoCompactTokenLimit = parsed
			}
		}
	}
	return nil
}

// effortField is one spelling of an effort as a request carried it: its value when the key was
// present, whether the key was present at all, and why a value that is not a string could not be
// read. The presence flag is what separates an absent key from one supplied empty.
func effortField(raw json.RawMessage) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, err
	}
	return value, true, nil
}

// effortAlias resolves the two spellings an effort may be given in: the policy document's
// reasoningEffort and the shorter effort the check contract uses. It is the one place the alias is
// resolved, so Pair and Change cannot disagree about it. A request that supplies both spellings
// with different values is ambiguous, and the returned reason says so rather than silently picking
// one; an explicitly empty value is a supplied value, so it disagrees with a non-empty one.
func effortAlias(reasoningEffort string, reasoningPresent bool, effort string, effortPresent bool) (string, string) {
	if reasoningPresent && effortPresent && reasoningEffort != effort {
		return "", "reasoningEffort " + strconv.Quote(reasoningEffort) + " and effort " + strconv.Quote(effort) + " disagree; name one"
	}
	if reasoningPresent && reasoningEffort != "" {
		return reasoningEffort, ""
	}
	return effort, ""
}

// MarshalJSON writes a pair as the policy document spells it.
func (p Pair) MarshalJSON() ([]byte, error) {
	if p.limitPresent {
		return json.Marshal(struct {
			Model                 string `json:"model"`
			ReasoningEffort       string `json:"reasoningEffort"`
			AutoCompactTokenLimit any    `json:"autoCompactTokenLimit"`
		}{p.Model, p.Effort, p.limit})
	}
	return json.Marshal(pairWire{Model: p.Model, ReasoningEffort: p.Effort, AutoCompactTokenLimit: p.AutoCompactTokenLimit})
}

// RoleView is one declared role: its pairs, or the record expectation of a supervisor. The JSON
// keys are the document spellings, so the response reads the way the policy file does.
type RoleView struct {
	Name        string `json:"name"`
	Expectation string `json:"expectation"`
	Pairs       []Pair `json:"pairs"`
}

// AllowedView is one allowlist entry.
type AllowedView struct {
	Model   string   `json:"model"`
	Efforts []string `json:"efforts"`
}

// ExceptionView is one declared exception.
type ExceptionView struct {
	ID     string   `json:"id"`
	Role   string   `json:"role,omitempty"`
	Model  string   `json:"model"`
	Effort string   `json:"reasoningEffort"`
	CWD    []string `json:"cwd"`
}

// Reading is what the policy file declares, or the named state that says why it could not be read.
type Reading struct {
	State            string
	Path             string
	Digest           string
	RegisteredDigest string
	Reason           string
	Roles            []RoleView
	Allowed          []AllowedView
	Exceptions       []ExceptionView
}

// codexHome is <CODEX_HOME> when set and non-empty, else <HOME>/.codex.
func codexHome(env LookupEnv) (string, bool) {
	if value, set := env("CODEX_HOME"); set && value != "" {
		return value, true
	}
	home, set := env("HOME")
	if !set || home == "" {
		return "", false
	}
	return filepath.Join(home, ".codex"), true
}

// Locate resolves the policy file the plugin wiring record names. The record is the only place the
// path and its digest come from, so a host with no record is not_registered rather than empty.
func Locate(env LookupEnv) Located {
	home, ok := codexHome(env)
	if !ok {
		return Located{State: Unreadable, Reason: "neither CODEX_HOME nor HOME names a Codex home"}
	}
	record := filepath.Join(home, pluginwiring.RecordName)
	found := reading.ReadJSON(record, "the bridge MCP record", nil, nil)
	switch {
	case found.State == reading.Absent:
		return Located{State: NotRegistered, Reason: "no record at " + record}
	case !found.OK():
		return Located{State: Unreadable, Reason: found.Detail}
	}
	document, ok := found.Value.(pyjson.Object)
	if !ok {
		return Located{State: Unreadable, Reason: "the record at " + record + " is not an object"}
	}
	read := pluginwiring.ReadBridgeRecord(document)
	if read.Version != 2 {
		return Located{State: Unreadable, Reason: "the record at " + record + " is version " + pyjson.Dumps(read.VersionValue, pyjson.Options{}) + ", and this reader uses a version 2 record the bridge launcher starts"}
	}
	if read.Owner != "plugin" {
		return Located{State: Unreadable, Reason: "the record at " + record + " names " + pyjson.Dumps(read.Owner, pyjson.Options{}) + " as the owner, and the packaged launcher starts only a plugin-owned record"}
	}
	if read.ServerName != nil && read.ServerName != "codex-thread-bridge" {
		return Located{State: Unreadable, Reason: "the record at " + record + " names another server, and the packaged launcher starts only the declared one"}
	}
	if !read.IsString || !strings.HasPrefix(read.Executable, "/") {
		return Located{State: Unreadable, Reason: "the record at " + record + " does not name bridgeExecutable as an absolute path"}
	}
	if !read.ArgsOK {
		return Located{State: Unreadable, Reason: "the record at " + record + " does not list args as strings"}
	}
	if !read.HasPolicy {
		return Located{State: NotRegistered, Reason: "the record at " + record + " names no execution policy"}
	}
	reference := pluginwiring.ReadPolicyReference(read.Policy)
	if !reference.Shaped {
		return Located{State: Unreadable, Reason: "the record at " + record + " does not name the execution policy as an object with exactly digest and path"}
	}
	if !reference.File.OK() {
		return Located{State: Unreadable, Reason: "the record at " + record + " does not name an absolute policy path"}
	}
	if !reference.DigestOK {
		return Located{State: Unreadable, Reason: "the record at " + record + " does not name the policy digest as 64 lowercase hexadecimal characters"}
	}
	return Located{State: Registered, Path: reference.File.Text, RegisteredDigest: reference.Digest}
}

// Read reads the policy file the location names and projects its declared values. It writes nothing.
func Read(located Located) Reading {
	reading := Reading{State: located.State, Path: located.Path, RegisteredDigest: located.RegisteredDigest, Reason: located.Reason}
	if located.State != Registered {
		return reading
	}
	raw, err := readRegular(located.Path)
	if err != nil {
		reading.State, reading.Reason = Unreadable, err.Error()
		return reading
	}
	sum := sha256.Sum256(raw)
	reading.Digest = hex.EncodeToString(sum[:])
	policy, err := execution.FromBytes(raw, located.Path)
	if err != nil {
		reading.State, reading.Reason = Unreadable, err.Error()
		return reading
	}
	document, err := decode(raw)
	if err != nil {
		reading.State, reading.Reason = Unreadable, err.Error()
		return reading
	}
	reading.Roles = projectRoles(policy)
	reading.Allowed = projectAllowed(document)
	reading.Exceptions = projectExceptions(document)
	return reading
}

// projectRoles reads each declared role's pairs and expectation through the parser's own accessors,
// so the projection cannot disagree with what the bridge enforces.
func projectRoles(policy execution.Policy) []RoleView {
	order := policy.RoleOrder()
	if len(order) == 0 {
		for _, name := range []string{execution.Supervisor, execution.Parent, execution.Child} {
			if _, declared := policy.Role(name); declared {
				order = append(order, name)
			}
		}
	}
	roles := make([]RoleView, 0, len(order))
	for _, name := range order {
		role, declared := policy.Role(name)
		if !declared {
			continue
		}
		view := RoleView{Name: name, Expectation: role.Expectation}
		for _, pair := range role.Pairs {
			limit := int64(0)
			if pair.AutoCompactTokenLimit != nil {
				limit = *pair.AutoCompactTokenLimit
			}
			view.Pairs = append(view.Pairs, Pair{Model: pair.Model, Effort: pair.Effort, AutoCompactTokenLimit: limit})
		}
		roles = append(roles, view)
	}
	return roles
}

// projectAllowed reads the allowlist in the order the file declares it. The parser keeps the list
// unexported, so it is projected from the decoded document rather than re-implemented.
func projectAllowed(document pyjson.Object) []AllowedView {
	entries, _ := document.Get("allowed").([]any)
	allowed := make([]AllowedView, 0, len(entries))
	for _, item := range entries {
		entry, ok := item.(pyjson.Object)
		if !ok {
			continue
		}
		model, _ := entry.Get("model").(string)
		view := AllowedView{Model: model}
		if efforts, ok := entry.Get("efforts").([]any); ok {
			for _, effort := range efforts {
				if name, ok := effort.(string); ok {
					view.Efforts = append(view.Efforts, name)
				}
			}
		}
		slices.Sort(view.Efforts)
		allowed = append(allowed, view)
	}
	return allowed
}

// projectExceptions reads the declared exceptions in document order.
func projectExceptions(document pyjson.Object) []ExceptionView {
	section, ok := document.Get("exceptions").(pyjson.Object)
	if !ok {
		return nil
	}
	exceptions := make([]ExceptionView, 0, len(section))
	for _, field := range section {
		entry, ok := field.Value.(pyjson.Object)
		if !ok {
			continue
		}
		view := ExceptionView{ID: field.Key}
		view.Role, _ = entry.Get("role").(string)
		view.Model, _ = entry.Get("model").(string)
		view.Effort, _ = entry.Get("reasoningEffort").(string)
		if roots, ok := entry.Get("cwd").([]any); ok {
			for _, root := range roots {
				if text, ok := root.(string); ok {
					view.CWD = append(view.CWD, text)
				}
			}
		}
		exceptions = append(exceptions, view)
	}
	return exceptions
}

// readRegular reads a regular file's bytes, judged on the descriptor.
func readRegular(path string) ([]byte, error) {
	encoded, err := encodedPath(path)
	if err != nil {
		return nil, err
	}
	file, err := reading.OpenRegular(encoded)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// decode parses a policy document into ordered objects, as the bridge's own decoder reads values.
func decode(raw []byte) (pyjson.Object, error) {
	text, err := pyjson.DecodeBytes(raw)
	if err != nil {
		return nil, err
	}
	value, err := pyjson.Loads(text, pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.SpelledNumbers, Unique: true, Deep: true})
	if err != nil {
		return nil, err
	}
	document, ok := value.(pyjson.Object)
	if !ok {
		return nil, errors.New("the execution policy must be a JSON object")
	}
	return document, nil
}

// Mode is the file's mode: allowlist when it declares an allowed list, presence_only otherwise.
func (r Reading) Mode() string {
	if len(r.Allowed) == 0 {
		return "presence_only"
	}
	return "allowlist"
}
