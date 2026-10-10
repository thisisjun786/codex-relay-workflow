package evidence

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-1115 verification round 2. The contract of a child is read from the packet it was dispatched with, which in a Codex rollout
// is the first user message item of the child's own thread; a marker anywhere else in the transcript (a tool's output, the child's
// own words, a packet the child itself sent to an agent it spawned, a forked parent history) names no contract of this child. A
// child whose packet names a contract that cannot be read is refused rather than judged by the native root, a receipt older than
// its dispatch is refused at the precision the dispatch time was recorded with, and a transcript path that names a FIFO is refused
// without blocking the gate.

const assignTestSession = "s1"

type assignTestRig struct {
	t         *testing.T
	cwd, tree string
}

func newAssignTestRig(t *testing.T) *assignTestRig {
	t.Helper()
	r := &assignTestRig{t: t, cwd: t.TempDir(), tree: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(r.tree, ".crw", "evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(r.tree); err == nil {
		r.tree = real
	}
	return r
}

// assign records a tree assignment made at created.
func (r *assignTestRig) assign(created time.Time) Assignment {
	r.t.Helper()
	a, err := NewAssignment(assignTestSession, r.tree, AssignTree, created)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := a.Persist(r.cwd); err != nil {
		r.t.Fatal(err)
	}
	return a
}

// receipt writes a non-empty receipt under the tree's evidence directory, with mtime when it is not zero.
func (r *assignTestRig) receipt(name string, mtime time.Time) string {
	r.t.Helper()
	path := filepath.Join(r.tree, ".crw", "evidence", name)
	if err := os.WriteFile(path, []byte("checked"), 0o644); err != nil {
		r.t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			r.t.Fatal(err)
		}
	}
	return path
}

func (r *assignTestRig) recordPath(id string) string {
	return filepath.Join(r.cwd, ".crw", AssignmentsSubdir, sessionRecordDir(assignTestSession), id+".json")
}

// The lines of a Codex rollout as the harness writes them.
func rolloutMeta(thread string) any {
	return map[string]any{"type": "session_meta", "payload": map[string]any{"id": thread, "session_id": "parent"}}
}

func rolloutUserItem(thread, text string) any {
	return map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": thread, "turn_id": "turn",
		"item": map[string]any{"type": "UserMessage", "id": "u", "content": []any{map[string]any{"type": "text", "text": text}}}}}
}

func rolloutUserMessage(text string) any {
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}}}}
}

func rolloutToolOutput(text string) any {
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "c",
		"output": []any{map[string]any{"type": "input_text", "text": text}}}}
}

func rolloutAgentItem(thread, text string) any {
	return map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": thread, "turn_id": "turn",
		"item": map[string]any{"type": "AgentMessage", "id": "m", "content": []any{map[string]any{"type": "Text", "text": text}}}}}
}

func rolloutSpawnCall(text string) any {
	input, _ := json.Marshal(map[string]any{"agent_type": "worker", "message": text})
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "spawn_agent", "call_id": "s",
		"arguments": string(input)}}
}

func (r *assignTestRig) transcript(lines ...any) string {
	r.t.Helper()
	var b strings.Builder
	for _, line := range lines {
		raw, err := json.Marshal(line)
		if err != nil {
			r.t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	path := filepath.Join(r.t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return path
}

func assignBlock(id string) string {
	return AssignmentMarker + ":" + id + "] Record your evidence receipt under the tree."
}

func TestAssignmentMarkerOutsideTheOwnPacketNamesNoContract(t *testing.T) {
	unrelated := "[CRW-SUBAGENT-SCOPE] guard\n\nTASK: something else entirely"
	for name, lines := range map[string]func(id string) []any{
		"tool output": func(id string) []any {
			return []any{rolloutMeta("child"), rolloutUserMessage(unrelated), rolloutUserItem("child", unrelated), rolloutToolOutput(assignBlock(id))}
		},
		"own words": func(id string) []any {
			return []any{rolloutMeta("child"), rolloutUserItem("child", unrelated), rolloutAgentItem("child", assignBlock(id))}
		},
		"spawned packet": func(id string) []any {
			return []any{rolloutMeta("child"), rolloutUserItem("child", unrelated), rolloutSpawnCall(assignBlock(id))}
		},
		"forked history": func(id string) []any {
			return []any{rolloutMeta("child"), rolloutUserItem("parent-thread", assignBlock(id)), rolloutUserItem("child", unrelated)}
		},
		"later user input": func(id string) []any {
			return []any{rolloutMeta("child"), rolloutUserItem("child", unrelated), rolloutUserItem("child", assignBlock(id))}
		},
		"no user item": func(id string) []any {
			return []any{rolloutMeta("child"), rolloutUserMessage(assignBlock(id)), rolloutToolOutput(assignBlock(id))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newAssignTestRig(t)
			a := r.assign(time.Now().Add(-time.Minute))
			receipt := r.receipt("check.txt", time.Time{})
			if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "foreign", r.transcript(lines(a.ID)...), "EVIDENCE_RECORDED: "+receipt, receipt); got == AssignedAccepted {
				t.Fatalf("an actor whose own packet holds no marker claimed assignment %s", a.ID)
			}
			if got, _ := readAssignment(r.recordPath(a.ID), a.ID); got.AgentID != "" {
				t.Fatalf("the assignment was claimed by %q", got.AgentID)
			}
		})
	}
	r := newAssignTestRig(t)
	a := r.assign(time.Now().Add(-time.Minute))
	receipt := r.receipt("check.txt", time.Time{})
	// The child whose own packet holds the marker is judged by it, whatever else its transcript shows afterwards.
	other := r.assign(time.Now().Add(-time.Minute))
	own := r.transcript(rolloutMeta("child"), rolloutUserMessage("# AGENTS.md instructions"), rolloutUserItem("child", "guard\n\n"+assignBlock(a.ID)+"\n\nTASK"),
		rolloutToolOutput(assignBlock(other.ID)), rolloutSpawnCall(assignBlock(other.ID)))
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", own, "EVIDENCE_RECORDED: "+receipt, receipt); got != AssignedAccepted {
		t.Fatalf("the dispatched child was not accepted: %v", got)
	}
	if got, _ := readAssignment(r.recordPath(a.ID), a.ID); got.AgentID != "w1" {
		t.Fatalf("the child's own assignment is held by %q", got.AgentID)
	}
	if got, _ := readAssignment(r.recordPath(other.ID), other.ID); got.AgentID != "" {
		t.Fatalf("a marker after the packet was claimed by %q", got.AgentID)
	}
}

func TestAssignmentNamedButUnreadableIsRefused(t *testing.T) {
	for _, damage := range []string{"removed", "corrupt", "other session", "directory removed", "directory unreadable"} {
		t.Run(damage, func(t *testing.T) {
			r := newAssignTestRig(t)
			a := r.assign(time.Now().Add(-time.Minute))
			receipt := r.receipt("check.txt", time.Time{})
			native := filepath.Join(r.cwd, ".crw", "evidence", "native.txt")
			if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(native, []byte("native"), 0o644); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(r.recordPath(a.ID))
			switch damage {
			case "removed":
				err := os.Remove(r.recordPath(a.ID))
				assignMust(t, err)
			case "corrupt":
				assignMust(t, os.WriteFile(r.recordPath(a.ID), []byte("{not json"), 0o644))
			case "other session":
				b := a
				b.SessionID = "s2"
				assignMust(t, writeRecord(r.recordPath(a.ID), b))
			case "directory removed":
				assignMust(t, os.RemoveAll(dir))
			case "directory unreadable":
				if os.Geteuid() == 0 {
					t.Skip("root reads any directory")
				}
				assignMust(t, os.Chmod(dir, 0))
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			}
			packet := r.transcript(rolloutMeta("child"), rolloutUserItem("child", assignBlock(a.ID)))
			for _, claim := range []string{receipt, native} {
				if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", packet, "EVIDENCE_RECORDED: "+claim, claim); got != AssignedRefused {
					t.Fatalf("a contract named by the packet but %s judged %v for %s", damage, got, claim)
				}
			}
			// Without a transcript, a cited id stands for the contract the same way.
			if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", "", AssignmentCitation+" "+a.ID+"\nEVIDENCE_RECORDED: "+native, native); got != AssignedRefused {
				t.Fatalf("a cited contract that is %s judged %v", damage, got)
			}
		})
	}
	// A packet without a marker is a dispatch without a contract: the native root decides.
	r := newAssignTestRig(t)
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", r.transcript(rolloutMeta("child"), rolloutUserItem("child", "TASK")), "", "x"); got != NoContract {
		t.Fatalf("a dispatch without a contract judged %v", got)
	}
}

func assignMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAssignmentReceiptOlderThanItsDispatchInTheSameSecond(t *testing.T) {
	r := newAssignTestRig(t)
	second := time.Now().Add(-time.Minute).Truncate(time.Second)
	a := r.assign(second.Add(900 * time.Millisecond))
	packet := r.transcript(rolloutMeta("child"), rolloutUserItem("child", assignBlock(a.ID)))
	stale := r.receipt("stale.txt", second.Add(100*time.Millisecond))
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", packet, "EVIDENCE_RECORDED: "+stale, stale); got != AssignedRefused {
		t.Fatalf("a receipt 800ms older than its dispatch judged %v", got)
	}
	fresh := r.receipt("fresh.txt", second.Add(950*time.Millisecond))
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", packet, "EVIDENCE_RECORDED: "+fresh, fresh); got != AssignedAccepted {
		t.Fatalf("a receipt written after its dispatch judged %v", got)
	}
}

func TestAssignmentTranscriptFIFOIsRefusedWithoutBlocking(t *testing.T) {
	r := newAssignTestRig(t)
	a := r.assign(time.Now().Add(-time.Minute))
	receipt := r.receipt("check.txt", time.Time{})
	fifo := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan AssignedVerdict, 1)
	go func() {
		done <- JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", fifo, "EVIDENCE_RECORDED: "+receipt, receipt)
	}()
	select {
	case got := <-done:
		if got == AssignedAccepted {
			t.Fatalf("a FIFO transcript gave a contract")
		}
	case <-time.After(3 * time.Second):
		// Release the blocked open so the goroutine ends, then fail.
		if f, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
			_ = f.Close()
		}
		<-done
		t.Fatal("the gate blocked opening a FIFO transcript")
	}
	_ = a
}

// CRW-1106 post-evaluation round: an actor without an agent id claims nothing, yet the contract its packet names stands.
func TestAssignmentMissingAgentIDIsRefusedWhenThePacketNamesAContract(t *testing.T) {
	r := newAssignTestRig(t)
	a := r.assign(time.Now().Add(-time.Minute))
	receipt := r.receipt("check.txt", time.Time{})
	packet := r.transcript(rolloutMeta("child"), rolloutUserItem("child", assignBlock(a.ID)))
	for _, claim := range []string{receipt, filepath.Join(r.cwd, ".crw", "evidence", "native.txt")} {
		if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "", packet, "EVIDENCE_RECORDED: "+claim, claim); got != AssignedRefused {
			t.Fatalf("an actor without an id judged %v for %s", got, claim)
		}
	}
	if got, _ := readAssignment(r.recordPath(a.ID), a.ID); got.AgentID != "" || got.Status != AssignmentOpen {
		t.Fatalf("an actor without an id changed the record: %+v", got)
	}
	plain := r.transcript(rolloutMeta("child"), rolloutUserItem("child", "TASK"))
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "", plain, "", "x"); got != NoContract {
		t.Fatalf("a packet without a contract and without an id judged %v", got)
	}
}

// CRW-1115 post-evaluation round: a child tied to no dispatch is refused while an unclaimed assignment of the session exists, and an
// actor that holds a claimed one keeps it.
func TestAssignmentUntiedChildIsRefusedWhileAnAssignmentIsOpen(t *testing.T) {
	r := newAssignTestRig(t)
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", "", "", "x"); got != NoContract {
		t.Fatalf("no assignment recorded: judged %v", got)
	}
	a := r.assign(time.Now().Add(-time.Minute))
	receipt := r.receipt("check.txt", time.Time{})
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", "", "", receipt); got != AssignedRefused {
		t.Fatalf("an untied child with an open assignment judged %v", got)
	}
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", "", AssignmentCitation+" "+a.ID, receipt); got != AssignedAccepted {
		t.Fatalf("the cited assignment judged %v", got)
	}
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", "", "", receipt); got != AssignedAccepted {
		t.Fatalf("the actor that claimed it lost it: %v", got)
	}
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w2", "", "", receipt); got != NoContract {
		t.Fatalf("every assignment claimed, an untied child judged %v", got)
	}
}

// CRW-1115 verification round 3: a record whose directory cannot be synced after its rename is published but not known to be durable;
// Persist fails, the spawn hook refuses the spawn, and nothing of that refused spawn may stay open, or every later child of the
// session that is tied to no dispatch would be refused for an assignment no child was ever made for.
func TestAssignmentPersistFailureAfterThePublicationLeavesNoOpenRecord(t *testing.T) {
	r := newAssignTestRig(t)
	a, err := NewAssignment(assignTestSession, r.tree, AssignTree, time.Now())
	assignMust(t, err)
	oldDir := syncDirectory
	t.Cleanup(func() { syncDirectory = oldDir })
	syncDirectory = func(dir string) error { return errors.New("injected directory sync failure") }
	if err := a.Persist(r.cwd); err == nil {
		t.Fatal("a record whose directory could not be synced was reported durable")
	}
	syncDirectory = oldDir
	if _, err := os.Lstat(r.recordPath(a.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused spawn's record was left behind: %v", err)
	}
	native := filepath.Join(r.cwd, ".crw", "evidence", "native.txt")
	if got := JudgeAssignedReceipt(r.cwd, assignTestSession, "w1", "", "EVIDENCE_RECORDED: "+native, native); got != NoContract {
		t.Fatalf("an unrelated child after the refused spawn judged %v, want the native root", got)
	}
}

// PersistNew creates a record and never replaces one: a later registration of the same id leaves the claimed record as it is.
func TestPersistNewDoesNotReplaceARecord(t *testing.T) {
	r := newAssignTestRig(t)
	a, err := NewAssignment(assignTestSession, r.tree, AssignTree, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.PersistNew(r.cwd); err != nil {
		t.Fatal(err)
	}
	claimed := a
	claimed.Status, claimed.AgentID = AssignmentClaimed, "worker"
	if err := claimed.Persist(r.cwd); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(r.recordPath(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.PersistNew(r.cwd); !errors.Is(err, ErrAssignmentExists) {
		t.Fatalf("PersistNew over a record = %v", err)
	}
	if after, _ := os.ReadFile(r.recordPath(a.ID)); string(after) != string(before) {
		t.Fatalf("the record changed:\n%s\n%s", before, after)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(r.recordPath(a.ID)), "*.tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}
