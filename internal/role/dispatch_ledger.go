package role

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// This file ports fallback-dispatch.ts:1-234 (CXC v0.2.40). Ordered records keep
// foreign members, as the role store does. The existing global store replaces
// the project layer; OS/JSON errors have Go text. The assignment additionally
// refuses a linked .crw or an unrelated git root. Status/Action retain raw JSON:
// upstream validates String(status), then compares its original value strictly.
// Managed spawn and created-report issuance checks belong to the next slice.
const dispatchMaxInput = 64 * 1024

// DispatchCandidate is a primary or first fallback; nil inherits the session.
type DispatchCandidate struct {
	Model  *string     `json:"model"`
	Effort *EffortName `json:"effort"`
	raw    object
}

func (c DispatchCandidate) MarshalJSON() ([]byte, error) {
	if c.raw != nil {
		return c.raw.MarshalJSON()
	}
	type wire DispatchCandidate
	return Stringify(wire(c), "")
}

type DispatchTaskFailure struct {
	Kind     string `json:"kind"`
	Evidence string `json:"evidence"`
}

type DispatchAttempt struct {
	ID             string               `json:"id"`
	Candidate      DispatchCandidate    `json:"candidate"`
	Claimed        bool                 `json:"claimed"`
	AgentID        *string              `json:"agentId"`
	ObservedModel  *string              `json:"observedModel"`
	Code           *string              `json:"code"`
	TaskFailure    *DispatchTaskFailure `json:"taskFailure"`
	Status         any                  `json:"status"`
	Reconciliation *string              `json:"reconciliation"`
	SpawnIssued    bool                 `json:"spawnIssued"`
	ToolUseID      *string              `json:"toolUseId"`
	raw            object
}

func (a DispatchAttempt) MarshalJSON() ([]byte, error) {
	if a.raw == nil {
		type wire DispatchAttempt
		return Stringify(wire(a), "")
	}
	o := slices.Clone(a.raw)
	for _, m := range []member{{"claimed", a.Claimed}, {"agentId", a.AgentID}, {"observedModel", a.ObservedModel}, {"code", a.Code}, {"status", a.Status}, {"reconciliation", a.Reconciliation}, {"taskFailure", a.TaskFailure}} {
		o.set(m.key, m.value)
	}
	return o.MarshalJSON()
}

type Dispatch struct {
	Version    int                 `json:"version"`
	SessionID  string              `json:"sessionId"`
	ID         string              `json:"id"`
	Role       RoleName            `json:"role"`
	Candidates []DispatchCandidate `json:"candidates"`
	Attempts   []DispatchAttempt   `json:"attempts"`
	Status     any                 `json:"status"`
	raw        object
}

func (d Dispatch) MarshalJSON() ([]byte, error) {
	if d.raw == nil {
		type wire Dispatch
		return Stringify(wire(d), "")
	}
	o := slices.Clone(d.raw)
	o.set("attempts", d.Attempts)
	o.set("status", d.Status)
	return o.MarshalJSON()
}

type DispatchResult struct {
	Action                    any                `json:"action"`
	DispatchID                string             `json:"dispatchId"`
	AttemptID                 string             `json:"attemptId"`
	IndependentReviewRequired bool               `json:"independentReviewRequired"`
	Attempts                  []DispatchAttempt  `json:"attempts"`
	Reason                    string             `json:"reason,omitempty"`
	Candidate                 *DispatchCandidate `json:"candidate,omitempty"`
	Marker                    string             `json:"marker,omitempty"`
}

func dispatchIs(v any, s string) bool { got, ok := v.(string); return ok && got == s }
func dispatchID(value any, field string) (string, error) {
	s, ok := value.(string)
	if !ok || len(s) < 1 || len(s) > 96 {
		return "", fmt.Errorf("invalid %s", field)
	}
	for i, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '_' || c == '-')) {
			return "", fmt.Errorf("invalid %s", field)
		}
	}
	return s, nil
}
func dispatchRecord(v any) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil, errors.New("expected a JSON object")
	}
	return m, nil
}
func dispatchSmallText(v any, field string) (string, error) {
	s, ok := v.(string)
	if !ok || text.Trim(s) == "" || len(utf16.Encode([]rune(s))) > 2000 {
		return "", fmt.Errorf("invalid %s", field)
	}
	return text.Trim(s), nil
}
func dispatchTaskFailure(v any) (*DispatchTaskFailure, error) {
	m, err := dispatchRecord(v)
	if err != nil {
		return nil, err
	}
	for k := range m {
		if k != "kind" && k != "evidence" {
			return nil, errors.New("invalid taskFailure key")
		}
	}
	kind, ok := m["kind"].(string)
	if !ok || kind != "stagnation" && kind != "unusable_output" {
		return nil, errors.New("invalid taskFailure kind")
	}
	evidence, err := dispatchSmallText(m["evidence"], "taskFailure evidence")
	return &DispatchTaskFailure{kind, evidence}, err
}
func dispatchRoot(cwd string) (string, error) {
	root := cwd
	if out, err := git(cwd, "rev-parse", "--show-toplevel"); err == nil {
		root = text.Trim(string(out))
	}
	root, err := realpath(root)
	if err != nil {
		return "", err
	}
	canonical, err := realpath(cwd)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, canonical)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("dispatch root must contain the workspace")
	}
	return root, nil
}
func dispatchDirectory(cwd, session string) (string, error) {
	dir, err := crwdir.EnsureDir(cwd)
	if err != nil {
		return "", err
	}
	for i, part := range []string{"", "dispatches", session} {
		if i > 0 {
			dir = filepath.Join(dir, part)
		}
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) && i > 0 {
			err = os.Mkdir(dir, 0o700)
		} else if err == nil && (!info.IsDir() || info.Mode()&fs.ModeSymlink != 0) {
			return "", errors.New("dispatch directory must not be a symlink")
		}
		if err != nil {
			return "", err
		}
	}
	return dir, nil
}
func dispatchRaw(o object, key string) json.RawMessage { raw, _ := o.raw(key); return raw }
func dispatchValue(raw json.RawMessage) (any, error) {
	var v any
	err := json.Unmarshal(raw, &v)
	return v, err
}
func dispatchObject(raw json.RawMessage) (object, error) {
	o, err := parseObject(raw)
	if errors.Is(err, errNotObject) {
		return nil, errors.New("expected a JSON object")
	}
	return o, err
}
func dispatchNullable(raw json.RawMessage, field string) (*string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	s, ok := stringOf(raw)
	if !ok {
		return nil, fmt.Errorf("invalid attempt %s", field)
	}
	return &s, nil
}
func dispatchCandidate(raw json.RawMessage) (DispatchCandidate, error) {
	o, err := dispatchObject(raw)
	if err != nil {
		return DispatchCandidate{}, err
	}
	c := DispatchCandidate{raw: o}
	model := dispatchRaw(o, "model")
	if string(model) != "null" {
		s, ok := stringOf(model)
		if !ok || text.Trim(s) == "" {
			return c, errors.New("invalid stored model")
		}
		c.Model = &s
	}
	effort := dispatchRaw(o, "effort")
	if string(effort) != "null" {
		s, ok := stringOf(effort)
		if !ok || !validEffort(EffortName(s)) {
			return c, errors.New("invalid stored effort")
		}
		e := EffortName(s)
		c.Effort = &e
	}
	return c, nil
}
func dispatchEqual[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func dispatchStatus(raw json.RawMessage, allowed ...string) (any, error) {
	s, err := jsString(raw)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(allowed, s) {
		return nil, errNotObject
	}
	return dispatchValue(raw)
}
func dispatchRead(path, session, id string) (Dispatch, error) {
	d := Dispatch{}
	info, err := os.Lstat(path)
	if err != nil {
		return d, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return d, errors.New("dispatch state must not be a symlink")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	o, err := dispatchObject(data)
	if err != nil {
		return d, err
	}
	d.raw = o
	version, _ := dispatchValue(dispatchRaw(o, "version"))
	sid, _ := stringOf(dispatchRaw(o, "sessionId"))
	did, _ := stringOf(dispatchRaw(o, "id"))
	role, _ := stringOf(dispatchRaw(o, "role"))
	if version != float64(1) || sid != session || did != id || !validRole(RoleName(role)) {
		return d, errors.New("invalid dispatch identity")
	}
	d.Version, d.SessionID, d.ID, d.Role = 1, sid, did, RoleName(role)
	var candidates []json.RawMessage
	if json.Unmarshal(dispatchRaw(o, "candidates"), &candidates) != nil || len(candidates) < 1 || len(candidates) > 2 {
		return d, errors.New("invalid candidates")
	}
	for _, raw := range candidates {
		c, err := dispatchCandidate(raw)
		if err != nil {
			return d, err
		}
		d.Candidates = append(d.Candidates, c)
	}
	var attempts []json.RawMessage
	if json.Unmarshal(dispatchRaw(o, "attempts"), &attempts) != nil || len(attempts) < 1 || len(attempts) > len(candidates) {
		return d, errors.New("invalid attempts")
	}
	if d.Status, err = dispatchStatus(dispatchRaw(o, "status"), "active", "stopped", "complete", "main-direct"); err != nil {
		if errors.Is(err, errNotObject) {
			return d, errors.New("invalid dispatch status")
		}
		return d, err
	}
	for i, raw := range attempts {
		a := DispatchAttempt{}
		if a.raw, err = dispatchObject(raw); err != nil {
			return d, err
		}
		idValue, _ := dispatchValue(dispatchRaw(a.raw, "id"))
		if a.ID, err = dispatchID(idValue, "attemptId"); err != nil {
			return d, err
		}
		if a.Candidate, err = dispatchCandidate(dispatchRaw(a.raw, "candidate")); err != nil {
			return d, err
		}
		claimed := dispatchRaw(a.raw, "claimed")
		if !dispatchEqual(a.Candidate.Model, d.Candidates[i].Model) || !dispatchEqual(a.Candidate.Effort, d.Candidates[i].Effort) || string(claimed) != "true" && string(claimed) != "false" {
			return d, errors.New("invalid attempt candidate")
		}
		a.Claimed = string(claimed) == "true"
		spawn := dispatchRaw(a.raw, "spawnIssued")
		if string(spawn) != "true" && string(spawn) != "false" {
			return d, errors.New("invalid spawn issuance")
		}
		a.SpawnIssued = string(spawn) == "true"
		if a.Status, err = dispatchStatus(dispatchRaw(a.raw, "status"), "ready", "claimed", "running", "reconcile", "failed", "complete"); err != nil {
			if errors.Is(err, errNotObject) {
				return d, errors.New("invalid attempt status")
			}
			return d, err
		}
		for _, field := range []struct {
			key string
			dst **string
		}{{"agentId", &a.AgentID}, {"observedModel", &a.ObservedModel}, {"code", &a.Code}, {"reconciliation", &a.Reconciliation}, {"toolUseId", &a.ToolUseID}} {
			if *field.dst, err = dispatchNullable(dispatchRaw(a.raw, field.key), field.key); err != nil {
				return d, err
			}
		}
		tf := dispatchRaw(a.raw, "taskFailure")
		if tf != nil && string(tf) != "null" {
			v, err := dispatchValue(tf)
			if err != nil {
				return d, err
			}
			if a.TaskFailure, err = dispatchTaskFailure(v); err != nil {
				return d, err
			}
		}
		// Appending the absent legacy member happens in memory even on status; status never saves.
		a.raw.set("taskFailure", a.TaskFailure)
		d.Attempts = append(d.Attempts, a)
	}
	return d, nil
}
func dispatchUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func dispatchAttempt(c DispatchCandidate) DispatchAttempt {
	return DispatchAttempt{ID: dispatchUUID(), Candidate: c, Status: "ready"}
}
func dispatchResult(d *Dispatch, action any, reason string) DispatchResult {
	a := d.Attempts[len(d.Attempts)-1]
	if action == nil {
		action = d.Status
		if dispatchIs(d.Status, "active") {
			action = "reconcile"
			if dispatchIs(a.Status, "ready") {
				action = "ready"
			} else if dispatchIs(a.Status, "running") {
				action = "wait"
			}
		} else if dispatchIs(d.Status, "stopped") {
			action = "stop"
		}
	}
	return DispatchResult{Action: action, DispatchID: d.ID, AttemptID: a.ID, IndependentReviewRequired: d.Role == Reviewer, Attempts: d.Attempts, Reason: reason}
}
func dispatchSave(path string, d *Dispatch, rename func(string, string) error) (err error) {
	temp := path + "." + dispatchUUID() + ".tmp"
	data, err := Stringify(d, "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if e := os.Remove(temp); e != nil && !errors.Is(e, fs.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	_, writeErr := f.Write(append(data, '\n'))
	if err = errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return rename(temp, path)
}

// RunDispatch selects an attempt but never calls a provider. Reports are caller
// observations, not authenticated receipts. env nil uses the process environment.
func RunDispatch(cwd string, input any, env host.LookupEnv) (DispatchResult, error) {
	return dispatchRun(cwd, input, env, crwdir.Rename)
}
func dispatchRun(cwd string, input any, env host.LookupEnv, rename func(string, string) error) (out DispatchResult, err error) {
	if env == nil {
		env = os.LookupEnv
	}
	if cwd, err = dispatchRoot(cwd); err != nil {
		return out, err
	}
	b, err := dispatchRecord(input)
	if err != nil {
		return out, err
	}
	encoded, err := Stringify(b, "")
	if err != nil {
		return out, err
	}
	if len(utf16.Encode([]rune(string(encoded)))) > dispatchMaxInput {
		return out, errors.New("dispatch input exceeds 64 KiB")
	}
	session, err := dispatchID(b["sessionId"], "sessionId")
	if err != nil {
		return out, err
	}
	if native, _ := env("CODEX_THREAD_ID"); native != "" && native != session {
		return out, errors.New("sessionId must match the native main session")
	}
	id, err := dispatchID(b["dispatchId"], "dispatchId")
	if err != nil {
		return out, err
	}
	dir, err := dispatchDirectory(cwd, session)
	if err != nil {
		return out, err
	}
	path := filepath.Join(dir, id+".json")
	lock := path + ".lock"
	if err = os.Mkdir(lock, 0o700); err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(lock)) }()
	if dispatchIs(b["action"], "start") {
		if info, e := os.Lstat(path); e == nil {
			if info.Mode()&fs.ModeSymlink != 0 {
				return out, errors.New("dispatch state must not be a symlink")
			}
			return out, errors.New("dispatch already exists; use status, never replay start")
		} else if !errors.Is(e, fs.ErrNotExist) {
			return out, e
		}
		role, ok := b["role"].(string)
		if !ok || !validRole(RoleName(role)) {
			return out, errors.New("invalid role")
		}
		cfg, err := ReadConfig(env)
		if err != nil {
			return out, err
		}
		r := cfg.Roles[RoleName(role)]
		c := DispatchCandidate{Effort: r.Effort}
		if r.Mode == ModeModel {
			c.Model = r.Model
		}
		candidates := []DispatchCandidate{c}
		if r.Fallback != nil && (c.Model == nil || r.Fallback.Model != *c.Model) {
			model := r.Fallback.Model
			candidates = append(candidates, DispatchCandidate{Model: &model, Effort: r.Fallback.Effort})
		}
		d := Dispatch{Version: 1, SessionID: session, ID: id, Role: RoleName(role), Candidates: candidates, Attempts: []DispatchAttempt{dispatchAttempt(c)}, Status: "active"}
		if err = dispatchSave(path, &d, rename); err != nil {
			return out, err
		}
		return dispatchResult(&d, nil, ""), nil
	}
	d, err := dispatchRead(path, session, id)
	if err != nil {
		return out, err
	}
	if dispatchIs(b["action"], "status") {
		return dispatchResult(&d, nil, ""), nil
	}
	a := &d.Attempts[len(d.Attempts)-1]
	if !dispatchIs(b["attemptId"], a.ID) {
		return out, errors.New("stale or missing attemptId; inspect status")
	}
	if !dispatchIs(d.Status, "active") {
		return dispatchResult(&d, nil, ""), nil
	}
	if dispatchIs(b["action"], "claim") {
		if a.Claimed || !dispatchIs(a.Status, "ready") {
			return dispatchResult(&d, "reconcile", "attempt already claimed; do not spawn again"), nil
		}
		a.Claimed, a.Status = true, "claimed"
		if err = dispatchSave(path, &d, rename); err != nil {
			return out, err
		}
		out = dispatchResult(&d, "spawn", "")
		out.Candidate = &a.Candidate
		out.Marker = "[CRW-DISPATCH:" + id + ":" + a.ID + "]"
		return out, nil
	}
	if !dispatchIs(b["action"], "report") {
		return out, errors.New("action must be start, claim, report or status")
	}
	if !a.Claimed {
		return out, errors.New("claim the attempt before reporting an outcome")
	}
	out, err = dispatchReport(&d, b)
	if err != nil {
		return out, err
	}
	err = dispatchSave(path, &d, rename)
	return out, err
}
func dispatchReport(d *Dispatch, b map[string]any) (DispatchResult, error) {
	a := &d.Attempts[len(d.Attempts)-1]
	none := DispatchResult{}
	if dispatchIs(b["outcome"], "created") {
		id, err := dispatchID(b["agentId"], "agentId")
		if err != nil {
			return none, err
		}
		if a.AgentID != nil && *a.AgentID != id {
			return none, errors.New("agentId changed")
		}
		a.AgentID, a.Status = &id, "running"
		if v, present := b["observedModel"]; present {
			s, err := dispatchSmallText(v, "observedModel")
			if err != nil {
				return none, err
			}
			a.ObservedModel = &s
		}
		return dispatchResult(d, "wait", ""), nil
	}
	if dispatchIs(b["outcome"], "complete") {
		if a.AgentID == nil || *a.AgentID == "" || !dispatchIs(b["agentId"], *a.AgentID) {
			return none, errors.New("complete requires the recorded agentId")
		}
		a.Status, d.Status = "complete", "complete"
		return dispatchResult(d, nil, ""), nil
	}
	if dispatchIs(b["outcome"], "task_failed") {
		return dispatchTaskFailed(d, b)
	}
	unavailable := dispatchIs(b["outcome"], "unavailable")
	if !unavailable && !dispatchIs(b["outcome"], "failed") {
		return none, errors.New("invalid report outcome")
	}
	failure := FailureDecision{}
	if !unavailable {
		failure = decodeDispatchFailure(b["error"])
	}
	a.Code = failure.Code
	if r, handled := dispatchProviderDecision(d, failure); handled {
		return r, nil
	}
	if !dispatchIs(b["executionState"], "not_created") && !dispatchIs(b["executionState"], "stopped") {
		a.Status = "reconcile"
		return dispatchResult(d, "reconcile", "confirm whether a child exists and stop it before handoff"), nil
	}
	if a.AgentID != nil && *a.AgentID != "" && (!dispatchIs(b["executionState"], "stopped") || !dispatchIs(b["agentId"], *a.AgentID)) {
		return none, errors.New("recorded child must be stopped and identified")
	}
	s, err := dispatchSmallText(b["reconciliation"], "reconciliation evidence")
	if err != nil {
		return none, err
	}
	a.Reconciliation = &s
	if unavailable {
		if a.AgentID != nil && *a.AgentID != "" || !dispatchIs(b["executionState"], "not_created") {
			return none, errors.New("unavailable requires confirmed no child")
		}
		a.Status, d.Status = "failed", "main-direct"
		return dispatchResult(d, nil, ""), nil
	}
	if dispatchIs(b["executionState"], "stopped") && (a.AgentID == nil || *a.AgentID == "") {
		return none, errors.New("record created agent before stopped handoff")
	}
	return dispatchHandoff(d), nil
}
func dispatchProviderDecision(d *Dispatch, f FailureDecision) (DispatchResult, bool) {
	if f.Action == "stop" {
		d.Status = "stopped"
		return dispatchResult(d, "stop", "failure does not permit model fallback"), true
	}
	if f.Action == "unknown" {
		d.Attempts[len(d.Attempts)-1].Status = "reconcile"
		return dispatchResult(d, "reconcile", "error is unclassified; obtain structured OCX evidence, do not guess a code"), true
	}
	return DispatchResult{}, false
}
func dispatchHandoff(d *Dispatch) DispatchResult {
	d.Attempts[len(d.Attempts)-1].Status = "failed"
	if len(d.Attempts) == len(d.Candidates) {
		d.Status = "main-direct"
	} else {
		d.Attempts = append(d.Attempts, dispatchAttempt(d.Candidates[len(d.Attempts)]))
	}
	return dispatchResult(d, nil, "")
}
func dispatchTaskFailed(d *Dispatch, b map[string]any) (DispatchResult, error) {
	a := &d.Attempts[len(d.Attempts)-1]
	none := DispatchResult{}
	if v, present := b["error"]; present {
		failure := decodeDispatchFailure(v)
		a.Code = failure.Code
		if r, handled := dispatchProviderDecision(d, failure); handled {
			return r, nil
		}
		return none, errors.New("fallback-eligible provider error must report outcome failed, not task_failed")
	}
	failure, err := dispatchTaskFailure(b["taskFailure"])
	if err != nil {
		return none, err
	}
	if dispatchIs(b["executionState"], "not_created") {
		return none, errors.New("task failure requires a recorded stopped child")
	}
	if !dispatchIs(b["executionState"], "stopped") {
		a.Status = "reconcile"
		return dispatchResult(d, "reconcile", "confirm the child is stopped before task handoff"), nil
	}
	if a.AgentID == nil || *a.AgentID == "" {
		return none, errors.New("record created agent before stopped handoff")
	}
	if !dispatchIs(b["agentId"], *a.AgentID) {
		return none, errors.New("recorded child must be stopped and identified")
	}
	s, err := dispatchSmallText(b["reconciliation"], "reconciliation evidence")
	if err != nil {
		return none, err
	}
	a.Reconciliation = &s
	a.Code = nil
	a.TaskFailure = failure
	return dispatchHandoff(d), nil
}
