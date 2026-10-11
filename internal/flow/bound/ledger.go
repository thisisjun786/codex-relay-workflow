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
	for _, line := range bytes.SplitAfter(existing, []byte("\n")) {
		if bytes.Equal(line, raw) {
			return nil
		}
		var seen Decision
		if d.ToolUseID != "" && json.Unmarshal(line, &seen) == nil && seen.ToolUseID == d.ToolUseID && seen.Decision == d.Decision && seen.Reason == d.Reason {
			return nil
		}
	}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		raw = append([]byte{'\n'}, raw...)
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

// ReadDecisions reads the ledger of thread under dir, in the order written. A thread with no ledger has no decisions. A line
// that does not parse is an error: the run does not import a ledger it cannot read whole.
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
	var out []Decision
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var d Decision
		dec := json.NewDecoder(bytes.NewReader(line))
		if err := dec.Decode(&d); err != nil || dec.More() {
			return nil, unavailable(ReasonLedger, "%s: line %d is not a decision", name, i+1)
		}
		out = append(out, d)
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
