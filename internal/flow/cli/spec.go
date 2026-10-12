package cli

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
)

// SpecSchema names the run spec.
const SpecSchema = "crw-flow-run/1"

// Spec is what the process of a run needs to start, and to start again: the workflow, its arguments, the owner thread, the budget
// and the host. `crw flow start` writes it before it starts the process, so a run that died is resumed from its journal and this
// file, with nothing the first caller held in its head. It holds no secret.
type Spec struct {
	Schema   string `json:"schema"`
	RunID    string `json:"run_id"`
	Workflow string `json:"workflow"`
	// Script is the absolute path of the workflow file; empty for a built-in workflow.
	Script      string          `json:"script,omitempty"`
	Args        json.RawMessage `json:"args"`
	Thread      string          `json:"thread"`
	TokenBudget int64           `json:"token_budget,omitempty"`
	Pool        int             `json:"pool,omitempty"`
	Repo        string          `json:"repo,omitempty"`
	Socket      string          `json:"socket,omitempty"`
	// Deadline is an RFC 3339 time at which the run pauses (a night batch's deadline).
	Deadline string `json:"deadline,omitempty"`
	// Decision is the decision record a night batch was started from.
	Decision string `json:"decision,omitempty"`
	// Forced names the preflight facts that failed and were started over with --force.
	Forced    []string `json:"forced,omitempty"`
	CreatedAt string   `json:"created_at"`
}

// NewRunID makes the id of a new run: "run-" and twelve hex digits.
func NewRunID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "run-" + hex.EncodeToString(b[:]), nil
}

func specPath(state, run string) string { return filepath.Join(RunDir(state, run), "run.json") }

// WriteSpec stores a spec, once: a run's spec is never replaced, because a resume must run the script and the arguments the run
// started with.
func WriteSpec(state string, spec Spec) error {
	if !validRunID(spec.RunID) {
		return fmt.Errorf("%q is not a run id", spec.RunID)
	}
	if err := os.MkdirAll(RunDir(state, spec.RunID), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(specPath(state, spec.RunID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ErrNoSpec is a run with no spec file: it was not started by `crw flow start`.
var ErrNoSpec = errors.New("the run has no spec file (it was not started by crw flow start)")

// ReadSpec reads a run's spec.
func ReadSpec(state, run string) (Spec, error) {
	if !validRunID(run) {
		return Spec{}, fmt.Errorf("%q is not a run id", run)
	}
	raw, err := os.ReadFile(specPath(state, run))
	if errors.Is(err, fs.ErrNotExist) {
		return Spec{}, fmt.Errorf("%w: %s", ErrNoSpec, run)
	}
	if err != nil {
		return Spec{}, err
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return Spec{}, fmt.Errorf("the spec of run %s: %w", run, err)
	}
	if spec.Schema != SpecSchema || spec.RunID != run {
		return Spec{}, fmt.Errorf("the spec of run %s is %s of run %q", run, spec.Schema, spec.RunID)
	}
	return spec, nil
}

// Source is a loaded workflow: its program and the loader of the workflows it nests.
type Source struct {
	Program   *script.Program
	Workflows func(name, scriptPath string) (*script.Program, error)
}

// Load loads the workflow a spec names: a built-in workflow from the binary, or the script file it names. A script file nests
// the workflows of its own directory only; a built-in workflow nests the built-in ones.
func (s Spec) Load() (Source, error) {
	if s.Script == "" {
		prog, err := workflows.Load(s.Workflow)
		if err != nil {
			return Source{}, err
		}
		return Source{Program: prog, Workflows: func(name, scriptPath string) (*script.Program, error) {
			return loadFrom(workflows.FS, name, scriptPath)
		}}, nil
	}
	text, err := os.ReadFile(s.Script)
	if err != nil {
		return Source{}, err
	}
	root, err := os.OpenRoot(filepath.Dir(s.Script))
	if err != nil {
		return Source{}, err
	}
	dir := root.FS()
	prog, err := script.Load(script.Source{Name: filepath.Base(s.Script), Text: text, Dir: dir})
	if err != nil {
		_ = root.Close()
		return Source{}, err
	}
	return Source{Program: prog, Workflows: func(name, scriptPath string) (*script.Program, error) {
		return loadFrom(dir, name, scriptPath)
	}}, nil
}

// loadFrom resolves workflow(name) to <name>.js and workflow({scriptPath}) to that relative path, inside dir.
func loadFrom(dir fs.FS, name, scriptPath string) (*script.Program, error) {
	rel := scriptPath
	if rel == "" {
		rel = name
		if !strings.HasSuffix(rel, ".js") {
			rel += ".js"
		}
	}
	rel = path.Clean(rel)
	if !fs.ValidPath(rel) {
		return nil, fmt.Errorf("%q is outside the workflows directory", rel)
	}
	text, err := fs.ReadFile(dir, rel)
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(dir, path.Dir(rel))
	if err != nil {
		return nil, err
	}
	return script.Load(script.Source{Name: rel, Text: text, Dir: sub})
}

// HandoffRef names the crw-plan handoff (crw-flow-compile-input/1) a run reads its compiled milestone from (CRW-1277 a): the file and
// the sha256 of its bytes. The handoff is the one that produced the plan's current revision; `crw flow start --handoff FILE` copies its
// bytes into the run's directory (<state>/flow/<run>/handoff.json) and the run's arguments name that copy, so a resume reads the bytes
// the run started with and never the live file.
type HandoffRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// HandoffFile is the name of a run's pinned copy of its handoff.
const HandoffFile = "handoff.json"

// sha256Hex is the sha256 of bytes in lower-case hex.
func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ReadHandoff reads a handoff a reference names and checks that its bytes are the ones the reference fixed; the handoff is decoded
// strictly (compile.Decode).
func ReadHandoff(ref HandoffRef) ([]byte, compile.Input, error) {
	if !filepath.IsAbs(ref.Path) || !sha256Pattern.MatchString(ref.SHA256) {
		return nil, compile.Input{}, fmt.Errorf("the handoff reference %+v names no absolute file and sha256", ref)
	}
	raw, err := os.ReadFile(ref.Path)
	if err != nil {
		return nil, compile.Input{}, fmt.Errorf("the handoff cannot be read: %w", err)
	}
	if got := sha256Hex(raw); got != ref.SHA256 {
		return nil, compile.Input{}, fmt.Errorf("the handoff %s changed: its sha256 is %s and %s was fixed", ref.Path, got, ref.SHA256)
	}
	in, err := DecodeHandoff(raw)
	return raw, in, err
}

// DecodeHandoff decodes the bytes of a handoff strictly.
func DecodeHandoff(raw []byte) (compile.Input, error) {
	in, err := compile.Decode(raw)
	if err != nil {
		return compile.Input{}, fmt.Errorf("the handoff is not a %s document: %w", compile.InputSchema, err)
	}
	if in.Schema != compile.InputSchema {
		return compile.Input{}, fmt.Errorf("the handoff's schema is %q, not %s", in.Schema, compile.InputSchema)
	}
	return in, nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// HandoffProject is the project of a handoff whose plan is plan (its plan_id, else its key).
func HandoffProject(in compile.Input, plan string) (compile.Project, bool) {
	for _, p := range in.Projects {
		id := p.PlanID
		if id == "" {
			id = p.Key
		}
		if id == plan {
			return p, true
		}
	}
	return compile.Project{}, false
}

// HandoffMilestone is the entry of a milestone in a project of a handoff.
func HandoffMilestone(p compile.Project, milestone string) (compile.Milestone, bool) {
	for _, m := range p.Milestones {
		if m.Key == milestone {
			return m, true
		}
	}
	return compile.Milestone{}, false
}

// CheckHandoff says why a handoff cannot be a run's compiled milestone ("" when it can): the project of the plan and the entry of the
// milestone have to be in it.
func CheckHandoff(in compile.Input, plan, milestone string) string {
	p, ok := HandoffProject(in, plan)
	if !ok {
		return fmt.Sprintf("the handoff holds no project of plan %q", plan)
	}
	if milestone == "" {
		return ""
	}
	m, ok := HandoffMilestone(p, milestone)
	if !ok {
		return fmt.Sprintf("the handoff names no entry of milestone %q in project %s (milestones[] of crw-flow-compile-input/1)", milestone, p.Key)
	}
	return EntryProblem(p, m)
}

// EntryProblem says why the entry of a milestone is not one the compiler would have compiled ("" when it is): the rules of
// compile.MilestoneEntryRefusals (surfaces of the closed set, a work grade C1 to C5, a title, criteria and commands that are not blank),
// which the run applies again to the handoff it pinned.
func EntryProblem(p compile.Project, m compile.Milestone) string {
	refusals := compile.MilestoneEntryRefusals(p.Key, "milestones", m)
	if len(refusals) == 0 {
		return ""
	}
	parts := make([]string, len(refusals))
	for i, r := range refusals {
		parts[i] = r.Path + ": " + r.Detail
	}
	return fmt.Sprintf("the entry of milestone %s in project %s is not one the compiler accepts (%s)", m.Key, p.Key, strings.Join(parts, "; "))
}

// PinHandoff writes the bytes of a handoff into the run's directory, once, and answers the reference the run's arguments carry.
func PinHandoff(state, run string, raw []byte) (HandoffRef, error) {
	if !validRunID(run) {
		return HandoffRef{}, fmt.Errorf("%q is not a run id", run)
	}
	if err := os.MkdirAll(RunDir(state, run), 0o700); err != nil {
		return HandoffRef{}, err
	}
	path := filepath.Join(RunDir(state, run), HandoffFile)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return HandoffRef{}, err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return HandoffRef{}, err
	}
	if err := f.Close(); err != nil {
		return HandoffRef{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return HandoffRef{}, err
	}
	return HandoffRef{Path: abs, SHA256: sha256Hex(raw)}, nil
}

// HandoffOf is the handoff a run's arguments name ("handoff").
func HandoffOf(args json.RawMessage) (HandoffRef, bool) {
	var a struct {
		Handoff *HandoffRef `json:"handoff"`
	}
	if json.Unmarshal(args, &a) != nil || a.Handoff == nil {
		return HandoffRef{}, false
	}
	return *a.Handoff, true
}

// withHandoff is a run's arguments with the handoff reference added as their last member. The arguments are an object (or none),
// and do not name a handoff of their own.
func withHandoff(args json.RawMessage, ref HandoffRef) (json.RawMessage, error) {
	member, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return compactJSON(append(append([]byte(`{"handoff":`), member...), '}')), nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(trimmed, &obj) != nil {
		return nil, errors.New("the workflow's args are not a JSON object, so they cannot name the handoff")
	}
	if _, ok := obj["handoff"]; ok {
		return nil, errors.New("the args name a handoff of their own: the run's handoff is given with --handoff")
	}
	out := append([]byte{}, trimmed[:len(trimmed)-1]...)
	if len(obj) > 0 {
		out = append(out, ',')
	}
	out = append(append(append(out, `"handoff":`...), member...), '}')
	return compactJSON(out), nil
}

// milestoneArg is the milestone a run's arguments name ("milestone"), or "".
func milestoneArg(args json.RawMessage) string {
	var a struct {
		Milestone string `json:"milestone"`
	}
	_ = json.Unmarshal(args, &a)
	return a.Milestone
}
