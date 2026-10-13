package bound

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// The hook's record of its decisions for the native spawns of bound threads: decisions/<thread_id>.jsonl, one line per decision,
// appended by the hook and read by the run, which imports it (internal/flow/ops) with the turn's own items. It is this feature's
// dispatch ledger: the managed fallback dispatch record of internal/role has an attempt per dispatch and nowhere to put a
// refusal, a pair the caller sent or a pair the hook applied.

// Decisions of the hook.
const (
	DecisionAllowed = "allowed"
	DecisionRefused = "refused"
)

// Reasons of a refusal (the closed set F14 adds to the refusal list of the spawn policy).
const (
	ReasonNativeTypeRefused    = "native_type_refused"
	ReasonNativeLimitRefused   = "native_limit_refused"
	ReasonFlowBoundUnavailable = "flow_bound_unavailable"
	ReasonFullHistoryFork      = "full_history_fork_refused"
)

// Decision is one line of a ledger.
type Decision struct {
	RunID    string `json:"run_id"`
	ThreadID string `json:"thread_id"`
	// ThreadKind is the kind of the binding line the decision was made under (engine or orchestrator).
	ThreadKind string `json:"thread_kind"`
	// CallID is the engine call (empty for an orchestrator thread).
	CallID    string `json:"call_id,omitempty"`
	ToolUseID string `json:"tool_use_id"`
	Decision  string `json:"decision"`
	// Reason is the refusal reason of a refused spawn, empty for an allowed one.
	Reason string `json:"reason,omitempty"`
	// HelperRole is the helper pair the hook chose (explorer, reviewer); empty when it chose none.
	HelperRole string `json:"helper_role,omitempty"`
	// RequestedModel and RequestedEffort are what the caller sent in tool_input; empty is a value the caller left to the
	// default, which is the parent's (inherited).
	RequestedModel  string `json:"requested_model"`
	RequestedEffort string `json:"requested_effort"`
	// AppliedModel and AppliedEffort are the pair the hook put in the spawn it allowed.
	AppliedModel  string `json:"applied_model"`
	AppliedEffort string `json:"applied_effort"`
	// Detail is the guidance or detail the refusal gave.
	Detail string `json:"detail,omitempty"`
	// Op is the operation the spawn message names on its first line (`crw-flow-op: <call>/t<k>-a<n>`), empty when the
	// message names none or the hook did not record it. A session run joins it with the owner turn's collabAgentToolCall
	// items to find the subagent of a published step (internal/flow/sessionhost FindByMarker).
	Op string `json:"op,omitempty"`
}

// maxLedger bounds a ledger that is read.
const maxLedger = 8 << 20

// AppendDecision adds d to the ledger of its thread under dir, once: a decision of the same tool use that says the same (the
// same decision and reason), or a line identical to one already there when d has no tool use id, is not written again, so a
// delivery of the same event repeats nothing. The ledger and its directory are made (mode 0700 and 0600) when they
// are not there; the directory, the file and the lock around the append must be the current user's.
func AppendDecision(dir string, d Decision) error {
	if !ValidThreadID(d.ThreadID) {
		return unavailable(ReasonLedger, "%q is not a thread id", d.ThreadID)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	top, err := openDir(dir)
	if err != nil {
		return err
	}
	defer top.Close()
	sub, err := openSub(top, DecisionsDir, true)
	if err != nil {
		return err
	}
	defer sub.Close()
	name := d.ThreadID + FileSuffix
	f, err := sub.OpenFile(name, os.O_RDWR|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return unavailable(ReasonLedger, "%s: %v", name, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return unavailable(ReasonLedger, "%s: %v", name, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	info, err := f.Stat()
	if err != nil {
		return unavailable(ReasonLedger, "%s: %v", name, err)
	}
	if info.Mode().Perm() != 0o600 && info.Size() == 0 {
		_ = f.Chmod(0o600) // a restrictive umask may have taken bits off a file made just now
		info, _ = f.Stat()
	}
	if bad := trusted(info, name, false, 0o600); bad != nil {
		return bad
	}
	if info.Size() > maxLedger {
		return unavailable(ReasonLedger, "%s is larger than %d bytes", name, maxLedger)
	}
	existing, err := io.ReadAll(io.NewSectionReader(f, 0, info.Size()))
	if err != nil {
		return unavailable(ReasonLedger, "%s: %v", name, err)
	}
	// A line is committed when its line feed is written: the hook answers only after the whole line and its line feed are written
	// and flushed, so what follows the last line feed is the cut end of a write that did not finish and was never answered. It is
	// taken off before the next decision goes in. A committed line that is no decision is a ledger the run cannot import: the
	// writer stops there instead of adding an allow it could not be read with.
	end := bytes.LastIndexByte(existing, '\n') + 1
	lines, err := decisionLines(name, existing[:end])
	if err != nil {
		return err
	}
	flush := func() error {
		if err := f.Sync(); err != nil {
			return unavailable(ReasonLedger, "%s: %v", name, err)
		}
		return syncRoot(sub, "entry:"+name) // the ledger's own entry in the decisions directory
	}
	for i, line := range bytes.SplitAfter(existing[:end], []byte("\n")) {
		if bytes.Equal(line, raw) || d.ToolUseID != "" && lines[i] != nil && lines[i].ToolUseID == d.ToolUseID && lines[i].Decision == d.Decision && lines[i].Reason == d.Reason {
			return flush() // the decision is there; a flush that failed when it was written may not have reached the disk
		}
	}
	if end < len(existing) {
		if err := f.Truncate(int64(end)); err != nil {
			return unavailable(ReasonLedger, "%s: the cut tail cannot be taken off: %v", name, err)
		}
	}
	// the ledger's entry in the decisions directory is flushed before the line goes in, so a refused flush leaves no decision behind
	if err := syncRoot(sub, "entry:"+name); err != nil {
		return err
	}
	if n, err := f.Write(raw); err != nil || n != len(raw) {
		if err == nil {
			err = errors.New("short write")
		}
		return unavailable(ReasonLedger, "%s: %v", name, err)
	}
	if err := f.Sync(); err != nil {
		return unavailable(ReasonLedger, "%s: %v", name, err)
	}
	return nil
}

// decisionLines parses the committed lines of a ledger (data ends in a line feed or is empty). The result has one element for each
// piece bytes.SplitAfter gives, nil for a blank one; a line that is no decision is an error.
func decisionLines(name string, data []byte) ([]*Decision, error) {
	pieces := bytes.SplitAfter(data, []byte("\n"))
	out := make([]*Decision, len(pieces))
	for i, line := range pieces {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var d Decision
		dec := json.NewDecoder(bytes.NewReader(line))
		if err := dec.Decode(&d); err != nil || dec.More() {
			return nil, unavailable(ReasonLedger, "%s: line %d is not a decision", name, i+1)
		}
		out[i] = &d
	}
	return out, nil
}

// ReadDecisions reads the ledger of thread under dir, in the order written. A thread with no ledger has no decisions. A committed
// line (one that ends in a line feed) that does not parse is an error: the run does not import a ledger it cannot read whole; the
// end of a write that was cut short has no line feed and is left out.
func ReadDecisions(dir, thread string) ([]Decision, error) {
	if !ValidThreadID(thread) {
		return nil, unavailable(ReasonLedger, "%q is not a thread id", thread)
	}
	exists, err := CheckDir(dir)
	if err != nil || !exists {
		return nil, err
	}
	top, err := openDir(dir)
	if err != nil {
		return nil, err
	}
	defer top.Close()
	sub, err := openSub(top, DecisionsDir, false)
	if err != nil {
		if isAbsentSub(err) {
			return nil, nil
		}
		return nil, err
	}
	defer sub.Close()
	name := thread + FileSuffix
	f, err := sub.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, unavailable(ReasonLedger, "%s: %v", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, unavailable(ReasonLedger, "%s: %v", name, err)
	}
	if bad := trusted(info, name, false, 0o600); bad != nil {
		return nil, bad
	}
	if info.Size() > maxLedger {
		return nil, unavailable(ReasonLedger, "%s is larger than %d bytes", name, maxLedger)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxLedger+1))
	if err != nil {
		return nil, unavailable(ReasonLedger, "%s: %v", name, err)
	}
	// the cut end of a write that did not finish (after the last line feed) is no decision: see AppendDecision
	lines, err := decisionLines(name, data[:bytes.LastIndexByte(data, '\n')+1])
	if err != nil {
		return nil, err
	}
	var out []Decision
	for _, d := range lines {
		if d != nil {
			out = append(out, *d)
		}
	}
	return out, nil
}

// Describe is a one-line description of a decision for messages.
func (d Decision) Describe() string {
	if d.Decision == DecisionRefused {
		return fmt.Sprintf("tool use %s refused (%s)", d.ToolUseID, d.Reason)
	}
	return fmt.Sprintf("tool use %s allowed with %s/%s", d.ToolUseID, d.AppliedModel, d.AppliedEffort)
}
