package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CodeSchemaInvalid         = "schema_invalid"
	CodeTaskInvalid           = "task_invalid"
	CodeDigestMismatch        = "digest_mismatch"
	CodeIdentityMismatch      = "identity_mismatch"
	CodeHostMismatch          = "host_mismatch"
	CodeCapabilityUnsupported = "capability_unsupported"
	CodeArtifactInvalid       = "artifact_invalid"
	CodeResultInvalid         = "result_invalid"
	CodeStaleAssignment       = "stale_assignment"
	CodeResultConflict        = "result_conflict"
)

type ValidationError struct {
	Code, Field, Detail string
}

func (e *ValidationError) Error() string {
	return e.Code + ": " + e.Field + ": " + e.Detail
}

func errInvalid(code, field, detail string) *ValidationError {
	return &ValidationError{Code: code, Field: field, Detail: detail}
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isHex(s string, sizes ...int) bool {
	for _, n := range sizes {
		if len(s) == n {
			for _, c := range s {
				if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
					return false
				}
			}
			return true
		}
	}
	return false
}

func validDigest(s string) bool { return isHex(s, 64) }

func validOID(s string) bool { return isHex(s, 40, 64) }

func validID(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	if strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func validRef(s string) bool {
	if s == "" || s == "@" || s[0] == '-' || s[0] == '.' || s[0] == '/' || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "/") {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.Contains(s, "//") {
		return false
	}
	for _, c := range s {
		if c <= ' ' || c == 0x7f || strings.ContainsRune(`~^:?*[\`, c) {
			return false
		}
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return false
		}
	}
	return true
}

func validArtifactPath(p string) bool {
	if len(p) == 0 || len(p) > 4096 || !fs.ValidPath(p) || p == "." {
		return false
	}
	if strings.ContainsRune(p, '\\') || strings.IndexByte(p, 0) >= 0 {
		return false
	}
	return path.Base(p) != ".executor.lock"
}

func checkRegionPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, '\\') || strings.IndexByte(p, 0) >= 0 {
		return "", fmt.Errorf("unsafe path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("unsafe path %q", p)
		}
	}
	return path.Clean(p), nil
}

func normalizeTask(t Task) (Task, error) {
	regions := make([]Region, len(t.Regions))
	for i, r := range t.Regions {
		n := r
		if n.Repository == "" {
			n.Repository = t.Repository.ID
		}
		if n.Kind == "" {
			n.Kind = "file"
		}
		if n.Change == "" {
			n.Change = "edit"
		}
		p := n.Path
		if n.Kind == "tree" {
			p = strings.TrimSuffix(p, "/")
		}
		p, err := checkRegionPath(p)
		if err != nil {
			return t, errInvalid(CodeTaskInvalid, "regions", err.Error())
		}
		n.Path = p
		regions[i] = n
	}
	sort.Slice(regions, func(i, j int) bool {
		a, b := regions[i], regions[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Key < b.Key
	})
	artifacts := make([]Artifact, len(t.Input.Artifacts))
	copy(artifacts, t.Input.Artifacts)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	checks := make([]Check, len(t.Checks))
	for i, c := range t.Checks {
		n := c
		n.Args = append([]string(nil), c.Args...)
		checks[i] = n
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].ID < checks[j].ID })
	t.Regions, t.Input.Artifacts, t.Checks = regions, artifacts, checks
	return t, nil
}

func validateArtifact(a Artifact, field string) error {
	if !validArtifactPath(a.Path) {
		return errInvalid(CodeArtifactInvalid, field+".path", fmt.Sprintf("unsafe path %q", a.Path))
	}
	if !validDigest(a.SHA256) {
		return errInvalid(CodeArtifactInvalid, field+".sha256", "not a 64-hex digest")
	}
	if a.Size < 0 {
		return errInvalid(CodeArtifactInvalid, field+".size", "negative size")
	}
	return nil
}

func validateTask(t Task) error {
	if t.Schema != TaskSchema {
		return errInvalid(CodeSchemaInvalid, "schema", fmt.Sprintf("want %s, got %q", TaskSchema, t.Schema))
	}
	id := t.Identity
	for field, v := range map[string]string{"task_id": id.TaskID, "execution_id": id.ExecutionID, "plan": id.Plan, "node": id.Node, "revision": id.Revision} {
		if !validID(v) {
			return errInvalid(CodeTaskInvalid, "identity."+field, fmt.Sprintf("invalid id %q", v))
		}
	}
	if id.Attempt <= 0 {
		return errInvalid(CodeTaskInvalid, "identity.attempt", "not positive")
	}
	if id.AssignmentGeneration <= 0 {
		return errInvalid(CodeTaskInvalid, "identity.assignment_generation", "not positive")
	}
	if !validDigest(id.CriteriaDigest) {
		return errInvalid(CodeTaskInvalid, "identity.criteria_digest", "not a 64-hex digest")
	}
	repo := t.Repository
	if !validID(repo.Forge) || !validID(repo.ID) {
		return errInvalid(CodeTaskInvalid, "repository", "forge and id must be registered ids")
	}
	if !validOID(repo.BaseCommit) {
		return errInvalid(CodeTaskInvalid, "repository.base_commit", "not a full object id")
	}
	if !validRef(repo.TargetRef) {
		return errInvalid(CodeTaskInvalid, "repository.target_ref", fmt.Sprintf("invalid ref %q", repo.TargetRef))
	}
	if !validRef(repo.TaskBranch) {
		return errInvalid(CodeTaskInvalid, "repository.task_branch", fmt.Sprintf("invalid ref %q", repo.TaskBranch))
	}
	if strings.TrimPrefix(repo.TaskBranch, "refs/heads/") == strings.TrimPrefix(repo.TargetRef, "refs/heads/") {
		return errInvalid(CodeTaskInvalid, "repository.task_branch", "the task branch is the target")
	}
	switch t.WorkKind {
	case "implementation":
		if len(t.Regions) == 0 || len(t.Checks) == 0 {
			return errInvalid(CodeTaskInvalid, "work_kind", "an implementation task needs regions and checks")
		}
	case "design":
		if len(t.Checks) == 0 && strings.TrimSpace(t.NotApplicable) == "" {
			return errInvalid(CodeTaskInvalid, "not_applicable", "a design task without checks needs an approved reason")
		}
	default:
		return errInvalid(CodeTaskInvalid, "work_kind", fmt.Sprintf("unknown work kind %q", t.WorkKind))
	}
	seen := map[string]bool{}
	for i, r := range t.Regions {
		f := fmt.Sprintf("regions[%d]", i)
		if r.Repository != repo.ID {
			return errInvalid(CodeTaskInvalid, f+".repository", "a region of another repository")
		}
		if r.Path == "." && r.Kind != "tree" {
			return errInvalid(CodeTaskInvalid, f+".path", "the whole tree is not a file or symbol")
		}
		switch r.Kind {
		case "tree", "file":
			if r.Key != "" {
				return errInvalid(CodeTaskInvalid, f+".key", "a non-symbol region names no key")
			}
		case "symbol":
			if r.Key == "" {
				return errInvalid(CodeTaskInvalid, f+".key", "a symbol region needs a key")
			}
		default:
			return errInvalid(CodeTaskInvalid, f+".kind", fmt.Sprintf("unknown kind %q", r.Kind))
		}
		switch r.Change {
		case "edit", "rename", "delete":
		default:
			return errInvalid(CodeTaskInvalid, f+".change", fmt.Sprintf("unknown change %q", r.Change))
		}
		switch r.Grade {
		case "", "independent", "local", "exclusive":
		case "mechanical":
			if strings.TrimSpace(r.Rule) == "" {
				return errInvalid(CodeTaskInvalid, f+".rule", "a mechanical region needs its rule")
			}
		default:
			return errInvalid(CodeTaskInvalid, f+".grade", fmt.Sprintf("unknown grade %q", r.Grade))
		}
		key := r.Path + "\x00" + r.Kind + "\x00" + r.Key
		if seen[key] {
			return errInvalid(CodeTaskInvalid, f, "duplicate region "+r.Path)
		}
		seen[key] = true
	}
	ex := t.Executor
	if ex.Kind != Codex && ex.Kind != Devin && ex.Kind != ClaudeCode {
		return errInvalid(CodeTaskInvalid, "executor.kind", fmt.Sprintf("unknown kind %q", ex.Kind))
	}
	if !validID(ex.HostID) || !validID(ex.Incarnation) {
		return errInvalid(CodeTaskInvalid, "executor", "host id and incarnation are required")
	}
	if !validID(ex.CapabilityRevision) {
		return errInvalid(CodeTaskInvalid, "executor.capability_revision", "required")
	}
	if !validDigest(ex.LaunchProfileDigest) || !validDigest(ex.ToolchainProfileDigest) {
		return errInvalid(CodeTaskInvalid, "executor", "launch and toolchain profile digests are required")
	}
	if strings.TrimSpace(t.Requested.Model) == "" || strings.TrimSpace(t.Requested.Effort) == "" {
		return errInvalid(CodeTaskInvalid, "requested", "model and effort are required")
	}
	if t.Input.Kind != "prompt" {
		return errInvalid(CodeTaskInvalid, "input.kind", fmt.Sprintf("unknown input kind %q", t.Input.Kind))
	}
	if t.Input.Prompt == "" {
		return errInvalid(CodeTaskInvalid, "input.prompt", "empty prompt")
	}
	if t.Input.BriefDigest != Digest([]byte(t.Input.Prompt)) {
		return errInvalid(CodeDigestMismatch, "input.brief_digest", "does not hash the prompt")
	}
	seen = map[string]bool{}
	for i, art := range t.Input.Artifacts {
		f := fmt.Sprintf("input.artifacts[%d]", i)
		if err := validateArtifact(art, f); err != nil {
			return err
		}
		if seen[art.Path] {
			return errInvalid(CodeArtifactInvalid, f, "duplicate artifact "+art.Path)
		}
		seen[art.Path] = true
	}
	seen = map[string]bool{}
	for i, c := range t.Checks {
		f := fmt.Sprintf("checks[%d]", i)
		if !validID(c.ID) {
			return errInvalid(CodeTaskInvalid, f+".id", "empty check id")
		}
		if seen[c.ID] {
			return errInvalid(CodeTaskInvalid, f+".id", "duplicate check "+c.ID)
		}
		seen[c.ID] = true
		if len(c.Args) == 0 || c.Args[0] == "" {
			return errInvalid(CodeTaskInvalid, f+".args", "empty command arguments")
		}
		for _, a := range c.Args {
			if strings.IndexByte(a, 0) >= 0 {
				return errInvalid(CodeTaskInvalid, f+".args", "an argument holds NUL")
			}
		}
		if c.ExpectedExitCode < 0 || c.ExpectedExitCode > 255 {
			return errInvalid(CodeTaskInvalid, f+".expected_exit_code", "not an exit code")
		}
		if c.Resources.CPU <= 0 || c.Resources.MemoryBytes < 0 {
			return errInvalid(CodeTaskInvalid, f+".resources", "cpu must be positive and memory non-negative")
		}
	}
	return nil
}

func Freeze(task Task) (Admission, error) {
	if err := validateStrings(reflect.ValueOf(task)); err != nil {
		return Admission{}, err
	}
	n, err := normalizeTask(task)
	if err != nil {
		return Admission{}, err
	}
	if err := validateTask(n); err != nil {
		return Admission{}, err
	}
	raw, err := json.Marshal(n)
	if err != nil {
		return Admission{}, err
	}
	return Admission{Task: n, TaskDigest: Digest(raw)}, nil
}

func ValidateAdmission(a Admission) error {
	re, err := Freeze(a.Task)
	if err != nil {
		return err
	}
	if !validDigest(a.TaskDigest) {
		return errInvalid(CodeDigestMismatch, "task_digest", "not a 64-hex digest")
	}
	if re.TaskDigest != a.TaskDigest {
		return errInvalid(CodeDigestMismatch, "task_digest", "the admitted task does not hash to its digest")
	}
	return nil
}

func ValidateGrant(a Admission, g Grant, now time.Time) error {
	if err := ValidateAdmission(a); err != nil {
		return err
	}
	if g.ExecutionID != a.Task.Identity.ExecutionID {
		return errInvalid(CodeIdentityMismatch, "grant.execution_id", "the grant is of another execution")
	}
	if g.AssignmentGeneration != a.Task.Identity.AssignmentGeneration {
		return errInvalid(CodeIdentityMismatch, "grant.assignment_generation", "the grant is of another generation")
	}
	if !validID(g.Owner) || g.OwnerEpoch <= 0 {
		return errInvalid(CodeTaskInvalid, "grant", "owner and a positive epoch are required")
	}
	if !g.ExpiresAt.After(now) {
		return errInvalid(CodeTaskInvalid, "grant.expires_at", "the grant is not live")
	}
	return nil
}

func ValidateHeartbeat(a Admission, h Heartbeat) error {
	if err := ValidateAdmission(a); err != nil {
		return err
	}
	id, ex := a.Task.Identity, a.Task.Executor
	if h.ExecutionID != id.ExecutionID || h.AssignmentGeneration != id.AssignmentGeneration {
		return errInvalid(CodeIdentityMismatch, "heartbeat", "the heartbeat is of another execution or generation")
	}
	if h.HostID != ex.HostID || h.Incarnation != ex.Incarnation {
		return errInvalid(CodeHostMismatch, "heartbeat", "the heartbeat is of another host or incarnation")
	}
	if h.ObservedAt.IsZero() {
		return errInvalid(CodeTaskInvalid, "heartbeat.observed_at", "no observation time")
	}
	return nil
}

func validObservation(o Observation, field string) error {
	if (o.Value != "") == (o.UnknownReason != "") {
		return errInvalid(CodeResultInvalid, field, "an observation is exactly a value or an unknown reason")
	}
	return nil
}

func NewResult(a Admission, outcome string, p Payload, observed Provenance) (Result, error) {
	if err := ValidateAdmission(a); err != nil {
		return Result{}, err
	}
	p.Checks = append([]CheckResult{}, p.Checks...)
	sort.Slice(p.Checks, func(i, j int) bool { return p.Checks[i].ID < p.Checks[j].ID })
	p.Artifacts = append([]Artifact{}, p.Artifacts...)
	sort.Slice(p.Artifacts, func(i, j int) bool { return p.Artifacts[i].Path < p.Artifacts[j].Path })
	if p.Candidate != nil {
		c := *p.Candidate
		p.Candidate = &c
	}
	observed.Tools = append([]Tool{}, observed.Tools...)
	sort.Slice(observed.Tools, func(i, j int) bool { return observed.Tools[i].Name < observed.Tools[j].Name })
	observed.NativeTurns = append([]string{}, observed.NativeTurns...)
	r := Result{Schema: ResultSchema, Identity: a.Task.Identity, TaskDigest: a.TaskDigest, Executor: a.Task.Executor,
		Outcome: outcome, Provenance: observed, Payload: p}
	peer := Peer{HostID: a.Task.Executor.HostID, Incarnation: a.Task.Executor.Incarnation}
	if err := ValidateResult(a, r, peer); err != nil {
		return Result{}, err
	}
	return r, nil
}

func ValidateResult(a Admission, r Result, peer Peer) error {
	if err := ValidateAdmission(a); err != nil {
		return err
	}
	if err := validateStrings(reflect.ValueOf(r)); err != nil {
		return errInvalid(CodeResultInvalid, "result", err.(*ValidationError).Detail)
	}
	if r.Schema != ResultSchema {
		return errInvalid(CodeSchemaInvalid, "schema", fmt.Sprintf("want %s, got %q", ResultSchema, r.Schema))
	}
	if r.Identity != a.Task.Identity || r.Executor != a.Task.Executor {
		return errInvalid(CodeIdentityMismatch, "identity", "the result does not carry the admitted identity and binding")
	}
	if r.TaskDigest != a.TaskDigest {
		return errInvalid(CodeDigestMismatch, "task_digest", "the result is of another task")
	}
	if !validID(peer.HostID) || !validID(peer.Incarnation) || peer.HostID != a.Task.Executor.HostID || peer.Incarnation != a.Task.Executor.Incarnation {
		return errInvalid(CodeHostMismatch, "peer", "the delivery is not from the admitted host and incarnation")
	}
	switch r.Outcome {
	case "completed", "failed", "cancelled", "incomplete":
	default:
		return errInvalid(CodeResultInvalid, "outcome", fmt.Sprintf("unknown outcome %q", r.Outcome))
	}
	approved := map[string]Check{}
	for _, c := range a.Task.Checks {
		approved[c.ID] = c
	}
	if c := r.Payload.Candidate; c != nil {
		if c.Branch != a.Task.Repository.TaskBranch {
			return errInvalid(CodeResultInvalid, "payload.candidate.branch", "not the admitted branch")
		}
		if !validOID(c.Head) || !validOID(c.Tree) {
			return errInvalid(CodeResultInvalid, "payload.candidate", "head and tree must be full object ids")
		}
	}
	designNA := a.Task.WorkKind == "design" && len(a.Task.Checks) == 0 && strings.TrimSpace(a.Task.NotApplicable) != ""
	seen := map[string]bool{}
	for i, cr := range r.Payload.Checks {
		f := fmt.Sprintf("payload.checks[%d]", i)
		c, ok := approved[cr.ID]
		if !ok {
			return errInvalid(CodeResultInvalid, f+".id", "the check "+cr.ID+" was not approved")
		}
		if seen[cr.ID] {
			return errInvalid(CodeResultInvalid, f+".id", "duplicate check "+cr.ID)
		}
		seen[cr.ID] = true
		switch cr.Outcome {
		case "passed":
			if cr.ExitCode != c.ExpectedExitCode {
				return errInvalid(CodeResultInvalid, f+".exit_code", "a passed check did not exit as expected")
			}
		case "failed", "not_run":
		default:
			return errInvalid(CodeResultInvalid, f+".outcome", fmt.Sprintf("unknown check outcome %q", cr.Outcome))
		}
	}
	if r.Outcome == "completed" {
		if r.Payload.Candidate == nil && !designNA {
			return errInvalid(CodeResultInvalid, "payload.candidate", "a completed result has no candidate")
		}
		for id, c := range approved {
			found := false
			for _, cr := range r.Payload.Checks {
				if cr.ID == id && cr.Outcome == "passed" && cr.ExitCode == c.ExpectedExitCode {
					found = true
				}
			}
			if !found {
				return errInvalid(CodeResultInvalid, "payload.checks", "the approved check "+id+" did not pass as expected")
			}
		}
	}
	pv := r.Provenance
	if pv.Provider != a.Task.Executor.Kind {
		return errInvalid(CodeResultInvalid, "provenance.provider", "not the bound executor kind")
	}
	if err := validObservation(pv.NativeSession, "provenance.native_session"); err != nil {
		return err
	}
	if err := validObservation(pv.Model, "provenance.model"); err != nil {
		return err
	}
	if err := validObservation(pv.Effort, "provenance.effort"); err != nil {
		return err
	}
	if err := validObservation(pv.CPUMethod, "provenance.cpu_method"); err != nil {
		return err
	}
	if len(pv.NativeTurns) > 0 && pv.NativeSession.Value == "" {
		return errInvalid(CodeResultInvalid, "provenance.native_turns", "turns without a native session")
	}
	seen = map[string]bool{}
	for i, tn := range pv.NativeTurns {
		if tn == "" || seen[tn] {
			return errInvalid(CodeResultInvalid, fmt.Sprintf("provenance.native_turns[%d]", i), "empty or duplicate native turn")
		}
		seen[tn] = true
	}
	seen = map[string]bool{}
	for i, tl := range pv.Tools {
		f := fmt.Sprintf("provenance.tools[%d]", i)
		if tl.Name == "" || seen[tl.Name] {
			return errInvalid(CodeResultInvalid, f+".name", "empty or duplicate tool")
		}
		seen[tl.Name] = true
		if tl.Version == "" {
			return errInvalid(CodeResultInvalid, f+".version", "a tool without a version")
		}
		if tl.SHA256 != "" && !validDigest(tl.SHA256) {
			return errInvalid(CodeResultInvalid, f+".sha256", "not a 64-hex digest")
		}
	}
	seen = map[string]bool{}
	for i, art := range r.Payload.Artifacts {
		f := fmt.Sprintf("payload.artifacts[%d]", i)
		if err := validateArtifact(art, f); err != nil {
			return err
		}
		if seen[art.Path] {
			return errInvalid(CodeArtifactInvalid, f, "duplicate artifact "+art.Path)
		}
		seen[art.Path] = true
	}
	return nil
}

func EncodeTask(t Task) ([]byte, error) {
	a, err := Freeze(t)
	if err != nil {
		return nil, err
	}
	return json.Marshal(a.Task)
}

func DecodeTask(raw []byte, limits Limits) (Admission, error) {
	var t Task
	if err := decodeStrict(raw, limits, &t); err != nil {
		return Admission{}, err
	}
	return Freeze(t)
}

func EncodeResult(r Result) ([]byte, error) {
	return json.Marshal(r)
}

func DecodeResult(a Admission, raw []byte, peer Peer, limits Limits) (Result, error) {
	var r Result
	if err := decodeStrict(raw, limits, &r); err != nil {
		return Result{}, err
	}
	if err := ValidateResult(a, r, peer); err != nil {
		return Result{}, err
	}
	return r, nil
}

func checkLimits(l Limits) error {
	if l.EnvelopeBytes <= 0 || l.EnvelopeBytes == math.MaxInt64 || l.ArtifactBytes <= 0 || l.ArtifactBytes == math.MaxInt64 || l.TotalArtifactBytes <= 0 || l.ArtifactCount <= 0 {
		return errInvalid(CodeSchemaInvalid, "limits", "the limits must be positive and bounded")
	}
	return nil
}

func decodeStrict(raw []byte, limits Limits, v any) error {
	if err := checkLimits(limits); err != nil {
		return err
	}
	if int64(len(raw)) > limits.EnvelopeBytes {
		return errInvalid(CodeSchemaInvalid, "bytes", "the envelope is over the byte limit")
	}
	if !utf8.Valid(raw) {
		return errInvalid(CodeSchemaInvalid, "bytes", "the envelope is not UTF-8")
	}
	if err := checkJSONEscapes(raw); err != nil {
		return err
	}
	if _, generic := v.(*any); !generic {
		if err := validateShape(raw, reflect.TypeOf(v).Elem()); err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errInvalid(CodeSchemaInvalid, "bytes", "the envelope is not the schema: "+err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return errInvalid(CodeSchemaInvalid, "bytes", "trailing data after the document")
	}
	return checkDuplicateKeys(raw)
}

func hex4(b []byte) (int, bool) {
	v := 0
	for _, c := range b {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v += int(c - '0')
		case c >= 'a' && c <= 'f':
			v += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v += int(c-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}

func checkJSONEscapes(raw []byte) error {
	in := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !in {
			if c == '"' {
				in = true
			}
			continue
		}
		if c == '"' {
			in = false
			continue
		}
		if c != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			break
		}
		v, ok := hex4(raw[i+1 : i+5])
		if !ok {
			continue
		}
		i += 4
		switch {
		case v >= 0xD800 && v <= 0xDBFF:
			if i+6 < len(raw) && raw[i+1] == '\\' && raw[i+2] == 'u' {
				if lo, ok := hex4(raw[i+3 : i+7]); ok && lo >= 0xDC00 && lo <= 0xDFFF {
					i += 6
					continue
				}
			}
			return errInvalid(CodeSchemaInvalid, "bytes", "an unpaired high surrogate escape")
		case v >= 0xDC00 && v <= 0xDFFF:
			return errInvalid(CodeSchemaInvalid, "bytes", "an unpaired low surrogate escape")
		}
	}
	return nil
}

func checkDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	type jframe struct {
		object    bool
		expectKey bool
		keys      map[string]bool
	}
	var stack []jframe
	valueDone := func() {
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].expectKey = true
		}
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errInvalid(CodeSchemaInvalid, "bytes", "invalid JSON: "+err.Error())
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				valueDone()
				stack = append(stack, jframe{object: true, expectKey: true, keys: map[string]bool{}})
			case '[':
				valueDone()
				stack = append(stack, jframe{})
			case '}', ']':
				if len(stack) == 0 {
					return errInvalid(CodeSchemaInvalid, "bytes", "unbalanced JSON")
				}
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		}
		if len(stack) == 0 {
			continue
		}
		top := &stack[len(stack)-1]
		if !top.object {
			continue
		}
		if top.expectKey {
			key, _ := tok.(string)
			if top.keys[key] {
				return errInvalid(CodeSchemaInvalid, "bytes", "duplicate key "+strconv.Quote(key))
			}
			top.keys[key] = true
			top.expectKey = false
		} else {
			top.expectKey = true
		}
	}
}

func validateStrings(v reflect.Value) error {
	switch v.Kind() {
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return errInvalid(CodeTaskInvalid, "string", "a field holds invalid UTF-8")
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath == "" {
				if err := validateStrings(v.Field(i)); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := validateStrings(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return validateStrings(v.Elem())
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if err := validateStrings(it.Key()); err != nil {
				return err
			}
			if err := validateStrings(it.Value()); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateShape(raw json.RawMessage, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errInvalid(CodeSchemaInvalid, typ.Name(), "null is not a value")
	}
	switch typ.Kind() {
	case reflect.Struct:
		if typ == reflect.TypeOf(time.Time{}) {
			return nil
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return errInvalid(CodeSchemaInvalid, typ.Name(), "not an object")
		}
		required := map[string]reflect.Type{}
		optional := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.PkgPath != "" {
				continue
			}
			tag := f.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if strings.Contains(tag, "omitempty") {
				optional[name] = f.Type
			} else {
				required[name] = f.Type
			}
		}
		for k, v := range m {
			ft, ok := required[k]
			if !ok {
				ft, ok = optional[k]
			}
			if !ok {
				return errInvalid(CodeSchemaInvalid, typ.Name(), "unknown field "+strconv.Quote(k))
			}
			if err := validateShape(v, ft); err != nil {
				return err
			}
		}
		for k := range required {
			if _, ok := m[k]; !ok {
				return errInvalid(CodeSchemaInvalid, typ.Name(), "missing field "+strconv.Quote(k))
			}
		}
		return nil
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return errInvalid(CodeSchemaInvalid, typ.Name(), "not an array")
		}
		for _, e := range arr {
			if err := validateShape(e, typ.Elem()); err != nil {
				return err
			}
		}
		return nil
	}
	return nil
}
