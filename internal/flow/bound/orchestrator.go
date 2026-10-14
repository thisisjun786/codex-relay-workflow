package bound

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"strings"
	"syscall"
)

// The orchestrator line of a session run (CRW-1316 design §9). The run's owner thread, the project orchestrator, spawns the role
// subagents of the run's steps; its line carries the pair of every role the hook overwrites a spawn with. The line lives as long
// as the run: BindOrchestrator writes it when the run starts or resumes, CloseOrchestrator closes it when the run ends. A thread
// holds the line of one live run, a run whose process holds its run lock (the caller tells, Live); the open line of a run whose
// process is gone is closed before another run's is written, and a second start while the first run lives is not bound.

// OrchestratorRoles are the six roles whose pairs an orchestrator line carries, in the order the flow's role table lists them.
var OrchestratorRoles = []string{"architect", "executor", "executor-t1", "reviewer", "reviewer-final", "explorer"}

// AgentTypeOf is the spawn_agent agent_type of a role: executor-t1 spawns an executor, reviewer-final a reviewer, every other
// role its own name.
func AgentTypeOf(role string) string {
	switch role {
	case "executor-t1":
		return "executor"
	case "reviewer-final":
		return "reviewer"
	}
	return role
}

// RoleOfAgentType is the role an agent_type names on its own: worker is executor; architect, executor, explorer and reviewer are
// themselves; any other value names none ("").
func RoleOfAgentType(agentType string) string {
	switch agentType {
	case "worker", "executor":
		return "executor"
	case "architect", "explorer", "reviewer":
		return agentType
	}
	return ""
}

// IsOrchestratorRole reports whether role is one of the six.
func IsOrchestratorRole(role string) bool {
	for _, r := range OrchestratorRoles {
		if r == role {
			return true
		}
	}
	return false
}

// MarkerPrefix starts the first line of a step's spawn message: `crw-flow-op: <call>/t<k>-a<n>`.
const MarkerPrefix = "crw-flow-op: "

var opPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}/t[1-9][0-9]*(-a[1-9][0-9]*)?$`)

// MarkerOp is the operation the first line of a spawn message names (`crw-flow-op: <call>/t<k>-a<n>`), "" when the first line is
// no such marker. The hook records it in the ledger, and the run joins it with the owner turn's spawn items.
func MarkerOp(message string) string {
	first, _, _ := strings.Cut(message, "\n")
	op, ok := strings.CutPrefix(strings.TrimRight(first, "\r \t"), MarkerPrefix)
	if !ok || !opPattern.MatchString(strings.TrimSpace(op)) {
		return ""
	}
	return strings.TrimSpace(op)
}

// OrchestratorBinding is the binding of a run's owner thread.
type OrchestratorBinding struct {
	// Writer appends the lines; nil is a writer over Dir().
	Writer *Writer
	// RunID is the run, Thread its owner thread, Digest the digest of the role pairs the run pinned (the line's policyDigest).
	RunID, Thread, Digest string
	// Pairs are the six role pairs.
	Pairs map[string]Pair
	// Live reports whether the process of a run holds its run lock; nil takes every other run for one without a process.
	Live func(run string) bool
	// KeepRunLine leaves a thread whose last line is the run's own open line as it is, whatever its pairs (Already): the binding is
	// written only when it is missing. `crw flow next` binds so after it resumed the run, because the resumed process binds the line
	// with the pairs it runs on, and next must not write other pairs over them (CRW-1372).
	KeepRunLine bool
}

// BindResult is what BindOrchestrator did.
type BindResult struct {
	// Bound: a line of the run was written. Already: the thread's last line is the run's open line with the same pairs (with any
	// pairs under KeepRunLine).
	Bound, Already bool
	// Closed is the run whose open line was closed first (its process is gone); Holder the live run the thread stays bound to,
	// in which case nothing was written.
	Closed, Holder string
}

// BindOrchestrator writes the orchestrator line of a run to its owner thread's file, unless the thread's last line is already
// the run's (with the same pairs and digest) or the open line of another run that lives. The open line of a run without a process
// is closed first. A thread whose file cannot be read or written is an error: the caller warns and goes on (the binding is what
// the hook decides by, and a run without one still runs).
func BindOrchestrator(o OrchestratorBinding) (BindResult, error) {
	if !ValidThreadID(o.Thread) {
		return BindResult{}, fmt.Errorf("bound: %q is not a thread id, so no flow-bound line can name it", o.Thread)
	}
	if o.RunID == "" {
		return BindResult{}, errors.New("bound: the orchestrator binding needs the run id")
	}
	w := o.Writer
	// the read of the last line and the append are one step under a lock of the directory: two near-simultaneous starts of runs of
	// one thread cannot both read no live line and both bind (CRW-1372); the second reads the first's line
	unlock, err := lockBindings(w)
	if err != nil {
		return BindResult{}, err
	}
	defer unlock()
	line := Line{RunID: o.RunID, ThreadID: o.Thread, Kind: KindOrchestrator, HelperPairs: maps.Clone(o.Pairs), PolicyDigest: o.Digest}
	if err := line.Validate(o.Thread); err != nil {
		return BindResult{}, fmt.Errorf("bound: %w", err)
	}
	last, present, err := lastLine(w, o.Thread)
	var u *UnavailableError
	if err != nil && errors.As(err, &u) && (u.Reason == ReasonInvalid || u.Reason == ReasonTruncated) {
		// a last line that is cut or no line of today's form (the two-pair line of the App Server path) binds nothing the hook
		// can read: the run's line goes after it
		err, present = nil, false
	}
	if err != nil {
		return BindResult{}, err
	}
	var res BindResult
	if present && last.Kind != KindClosed {
		switch {
		case last.RunID == o.RunID && last.Kind == KindOrchestrator && last.PolicyDigest == o.Digest && maps.Equal(last.HelperPairs, line.HelperPairs):
			return BindResult{Already: true}, nil
		case last.RunID == o.RunID && last.Kind == KindOrchestrator && o.KeepRunLine:
			return BindResult{Already: true}, nil
		case last.RunID == o.RunID:
			// the run's own line with other pairs (a resume that re-pinned them): written again below
		case o.Live != nil && o.Live(last.RunID):
			return BindResult{Holder: last.RunID}, nil
		default:
			if err := w.Append(Line{RunID: last.RunID, ThreadID: o.Thread, Kind: KindClosed, PolicyDigest: last.PolicyDigest}); err != nil {
				return BindResult{}, err
			}
			res.Closed = last.RunID
		}
	}
	if err := w.Append(line); err != nil {
		return res, err
	}
	res.Bound = true
	return res, nil
}

// lockBindings takes the exclusive lock of the flow-bound directory (made when it is not there) and returns its release.
func lockBindings(w *Writer) (func(), error) {
	dir, err := w.dir()
	if err != nil {
		return nil, err
	}
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("bound: the lock of %s: %w", dir, err)
	}
	return func() {
		_ = syscall.Flock(int(d.Fd()), syscall.LOCK_UN)
		_ = d.Close()
	}, nil
}

// CloseOrchestrator appends the closed line to the owner thread's file when its last line is the run's open line; closed says
// whether it did. A thread whose last line is another run's, or closed, is left as it is.
func CloseOrchestrator(w *Writer, runID, thread string) (closed bool, err error) {
	if !ValidThreadID(thread) {
		return false, nil
	}
	last, present, err := lastLine(w, thread)
	if err != nil || !present || last.Kind != KindOrchestrator || last.RunID != runID {
		return false, err
	}
	if err := w.Append(Line{RunID: runID, ThreadID: thread, Kind: KindClosed, PolicyDigest: last.PolicyDigest}); err != nil {
		return false, err
	}
	return true, nil
}

// OrchestratorLine reads the last line of a thread's file: present is false when there is none (no directory, no file).
func OrchestratorLine(w *Writer, thread string) (Line, bool, error) { return lastLine(w, thread) }

func lastLine(w *Writer, thread string) (Line, bool, error) {
	dir, err := w.dir()
	if err != nil {
		return Line{}, false, err
	}
	exists, err := CheckDir(dir)
	if err != nil || !exists {
		return Line{}, false, err
	}
	return ReadLast(dir, thread)
}
