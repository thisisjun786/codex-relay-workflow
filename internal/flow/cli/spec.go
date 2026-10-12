package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

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
