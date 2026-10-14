// Package bound is the file contract between a flow run and the native spawn hook (F14, CRW-1207): which Codex threads
// belong to a run, and with what.
//
// A run writes one append-only line per engine turn (and an opening and a closing line for the thread that orchestrates the
// run) to ~/.local/state/crw/flow-bound/<thread_id>.jsonl before the turn is sent. The spawn hook reads the file of the
// thread in whose name a native spawn is made (Lookup) and decides from its last line: no directory, a thread that has no
// file, or a closed last line is an ordinary thread (Standalone); an unsafe directory or a file that cannot be trusted is
// Unavailable: the hook cannot tell whose thread it is, and decides the spawn as an ordinary thread's with a warning (CRW-1348:
// no blanket refusal); anything else binds the thread to the run (Engine, Orchestrator). The hook never
// opens the relay store and never reads the policy file: the helper pairs travel in the line.
//
// The location is computed here, once, from os.UserHomeDir and not from XDG_STATE_HOME, so that the run and the Codex host,
// whose XDG settings may differ, cannot end up with two directories. The directory is outside the workspace-write range of
// an engine thread (its own worktree), so an executor cannot delete a binding or a claim. A binding file that vanished is
// not told from an ordinary thread by the hook; the run's import (internal/flow/ops) catches that as unbound_spawn.
//
// The package imports only the standard library: both the run and the hook read and write these files.
package bound

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Names of the files and directories under the flow-bound directory.
const (
	// DirName is the directory under ~/.local/state/crw.
	DirName = "flow-bound"
	// FileSuffix ends the binding file of a thread.
	FileSuffix = ".jsonl"
	// ClaimsDir holds claims/<thread_id>/<call_id>, the per-call claim of a native helper, and <call_id>.answer beside it.
	ClaimsDir = "claims"
	// DecisionsDir holds decisions/<thread_id>.jsonl, the hook's record of its decisions (the dispatch ledger of F14).
	DecisionsDir = "decisions"
)

// Kinds of a binding line.
const (
	KindEngine       = "engine"
	KindOrchestrator = "orchestrator"
	KindClosed       = "closed"
)

// The helper pair keys of a line.
const (
	HelperExplorer = "explorer"
	HelperReviewer = "reviewer"
)

// Pair is a model and a reasoning effort.
type Pair struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// Line is one line of a binding file.
type Line struct {
	RunID    string `json:"run_id"`
	ThreadID string `json:"thread_id"`
	Kind     string `json:"kind"`
	// CallID and Role are those of the engine call the turn belongs to; empty on an orchestrator or a closed line.
	CallID string `json:"call_id,omitempty"`
	Role   string `json:"role,omitempty"`
	// NativeAllowance is how many native helpers the call may spawn (0 or 1); nil on an orchestrator or a closed line.
	NativeAllowance *int `json:"nativeAllowance"`
	// HelperPairs are the pairs the hook applies to an allowed helper: explorer for an engine line, from the pinned policy of the
	// run; for an orchestrator line the pairs of the six roles (OrchestratorRoles) the role subagents of a session run are spawned
	// with.
	HelperPairs  map[string]Pair `json:"helperPairs,omitempty"`
	PolicyDigest string          `json:"policyDigest"`
}

var (
	threadIDPattern = regexp.MustCompile(`^[0-9a-f][0-9a-f-]{0,63}$`)
	callIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

// ValidThreadID reports whether s has the form of a thread id: lower-case hexadecimal digits and '-', starting with a digit
// (so that it is never a path component with a meaning of its own).
func ValidThreadID(s string) bool { return threadIDPattern.MatchString(s) }

// ValidCallID reports whether s can name a claim file: letters, digits, '_' and '-'.
func ValidCallID(s string) bool { return callIDPattern.MatchString(s) }

// Dir is the flow-bound directory of the current user: <home>/.local/state/crw/flow-bound, the home being os.UserHomeDir.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("bound: the home directory %q is not absolute", home)
	}
	return filepath.Join(home, ".local", "state", "crw", DirName), nil
}

// Allowance returns the engine line's allowance, 0 for a line that has none.
func (l Line) Allowance() int {
	if l.NativeAllowance == nil {
		return 0
	}
	return *l.NativeAllowance
}

// Validate checks the line as the file of thread would hold it.
func (l Line) Validate(thread string) error {
	switch {
	case l.RunID == "":
		return errors.New("the line names no run")
	case !ValidThreadID(thread) || l.ThreadID != thread:
		return fmt.Errorf("the line names thread %q, the file is that of %q", l.ThreadID, thread)
	}
	pair := func(key string) error {
		p, ok := l.HelperPairs[key]
		if !ok || p.Model == "" || p.Effort == "" {
			return fmt.Errorf("the line has no %s helper pair", key)
		}
		return nil
	}
	switch l.Kind {
	case KindEngine:
		switch {
		case !ValidCallID(l.CallID):
			return fmt.Errorf("the call id %q cannot name a claim", l.CallID)
		case l.Role == "":
			return errors.New("the line names no role")
		case l.NativeAllowance == nil || *l.NativeAllowance < 0 || *l.NativeAllowance > 1:
			return errors.New("nativeAllowance is not 0 or 1")
		}
		return pair(HelperExplorer)
	case KindOrchestrator:
		if l.NativeAllowance != nil {
			return errors.New("an orchestrator line has no nativeAllowance")
		}
		// the pair of each of the six roles the hook overwrites a role subagent's spawn with (CRW-1316 §9); an undecided role of the
		// helper role store (CRW-1330) has the empty pair, and its spawns keep the host's default
		for _, role := range OrchestratorRoles {
			p, ok := l.HelperPairs[role]
			switch {
			case !ok:
				return fmt.Errorf("the line has no %s helper pair", role)
			case (p.Model == "") != (p.Effort == ""):
				return fmt.Errorf("the %s pair of the line names a model or an effort without the other", role)
			}
		}
		return nil
	case KindClosed:
		return nil
	}
	return fmt.Errorf("unknown kind %q", l.Kind)
}

// Encode is the line as it is written: one line of JSON ended by a line feed.
func (l Line) Encode() ([]byte, error) {
	raw, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// decodeLine reads one line strictly: one JSON object, no trailing data.
func decodeLine(raw []byte) (Line, error) {
	var l Line
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&l); err != nil {
		return Line{}, err
	}
	if dec.More() {
		return Line{}, errors.New("trailing data after the line")
	}
	return l, nil
}
