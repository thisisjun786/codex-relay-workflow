package hook_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
)

// CRW-1482 (1209 field trial, defect C9). A flow run's executor is spawned by the owner thread of a session run and works in the call
// worktree the engine made for it. Its SubagentStop payload carries the owner's session id and the owner's cwd, its transcript's own
// first UserMessage is the engine's spawn message, and its final message is the RECEIPT JSON alone. The hook found no contract for it
// and looked for a receipt under the owner's native .crw/evidence, where the child never writes: three blocks spent the budget, then
// a tombstone. The engine now records a flow assignment for the dispatch (root = the call worktree, op = the step's crw-flow-op
// marker) and appends its block to the spawn message; the hook judges the one fixed receipt file of that contract without a marker
// line after the JSON, and its block reason names that file.

const (
	flowStopOwner = "01a13c08-0c03-7951-afc5-57656aca48bc"
	flowStopRun   = "run-7b534f998cfc"
	flowStopCall  = "c_5cff7231980e"
	flowStopOp    = "c_5cff7231980e/t1-a1"
	flowStopAgent = "019a2b3c-4d5e-7f60-8123-456789abcdef"
	flowStopTurn  = "019a2b3c-4d5e-7f60-8123-4567890abc01"
	flowStopJSON  = `{"status":"done","summary":"implemented the parser fix","tests":["go test ./internal/x/"]}`
)

// flowStopRig is the owner's cwd with pabcd on and the call worktree of one executor step.
type flowStopRig struct {
	t          *testing.T
	owner, run string
}

func newFlowStopRig(t *testing.T) *flowStopRig {
	t.Helper()
	owner := subagentStopWorkspace(t)
	t.Setenv("CRW_PABCD", "on")
	return &flowStopRig{t: t, owner: owner, run: flowStopWorktree(t)}
}

// flowStopWorktree is a call worktree as the allocator makes it: a git checkout with a commit, by its real path, with the real
// .crw/evidence directory the engine creates at publication.
func flowStopWorktree(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err == nil {
		for _, args := range [][]string{
			{"init", "-q"},
			{"-c", "user.name=crw-test", "-c", "user.email=crw-test@example.invalid", "commit", "-q", "--allow-empty", "-m", "base"},
		} {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".crw", "evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mint records the flow assignment of the call's op in tree under the owner's cwd, as the engine does at the step's publication.
func (r *flowStopRig) mint(call, op, tree string) evidence.Assignment {
	r.t.Helper()
	a, err := evidence.NewFlowAssignment(flowStopOwner, evidence.FlowAssignmentID(flowStopOwner, flowStopRun, call, op), tree, op, time.Now().Add(-time.Second))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := a.PersistNew(r.owner); err != nil {
		r.t.Fatal(err)
	}
	return a
}

// spawnText is the engine's spawn message of an executor step with the evidence block appended.
func flowStopSpawnText(op, tree string, a evidence.Assignment) string {
	return "crw-flow-op: " + op + "\nCRW-ROLE: executor\n\n" +
		"Your shell starts in a directory that is NOT your workspace. Your workspace is " + tree + ": run every command with workdir " + tree +
		" and edit only by absolute paths under " + tree + ".\nRead the task packet /bus/" + flowStopCall + "/packet-t1-a1.md in full and follow it exactly. " +
		"You are a leaf subagent: never spawn agents.\nYour final message is exactly the JSON object the packet names, and nothing else.\n\n" +
		evidence.AssignmentMarker + ":" + a.ID + "] Before your final message, write your evidence receipt to " + evidence.FlowReceiptPath(a) +
		"; no EVIDENCE_RECORDED line is needed after the JSON."
}

// rollout writes the child's Codex rollout: its own session_meta, the developer instructions and the spawn message as the first
// UserMessage item of its own thread.
func (r *flowStopRig) rollout(packet string) string {
	r.t.Helper()
	lines := []any{
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": flowStopAgent, "session_id": flowStopOwner}},
		map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "# AGENTS.md instructions"}}}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": flowStopAgent, "turn_id": flowStopTurn,
			"item": map[string]any{"type": "UserMessage", "id": "u1", "content": []any{map[string]any{"type": "text", "text": packet}}}}},
	}
	var b strings.Builder
	for _, line := range lines {
		raw, err := json.Marshal(line)
		if err != nil {
			r.t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	path := filepath.Join(r.t.TempDir(), "rollout-"+flowStopAgent+".jsonl")
	subagentStopPut(r.t, path, b.String())
	return path
}

// stop sends the SubagentStop payload the host sends for the executor: the owner's cwd and session, the child's agent and turn,
// its rollout and its final message, through the registered leg.
func (r *flowStopRig) stop(transcript, message string) string {
	r.t.Helper()
	p := map[string]any{"hook_event_name": "SubagentStop", "cwd": r.owner, "session_id": flowStopOwner, "agent_type": "executor",
		"agent_id": flowStopAgent, "turn_id": flowStopTurn, "last_assistant_message": message, "agent_transcript_path": transcript}
	raw, err := json.Marshal(p)
	if err != nil {
		r.t.Fatal(err)
	}
	out, err := subagentStopInvoke(raw)
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func (r *flowStopRig) attempts() evidence.Counter {
	return evidence.ReadCounter(r.owner, flowStopOwner, flowStopAgent, flowStopTurn)
}

func (r *flowStopRig) item(message string) evidence.Payload {
	return evidence.Payload{AgentType: "executor", AgentID: flowStopAgent, TurnID: flowStopTurn, LastAssistantMessage: message}
}

// flowStopReason is the reason of a block, which must be a block.
func flowStopReason(t *testing.T, out string) string {
	t.Helper()
	var v struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Decision != "block" {
		t.Fatalf("want a block, got %q", out)
	}
	return v.Reason
}

func flowStopDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func TestSubagentStopFlowExecutor(t *testing.T) {
	t.Run("ReceiptInRunWorktree", func(t *testing.T) {
		r := newFlowStopRig(t)
		a := r.mint(flowStopCall, flowStopOp, r.run)
		tr := r.rollout(flowStopSpawnText(flowStopOp, r.run, a))
		body := "$ go test ./internal/x/ -run TestParser\nFAIL (red before)\n$ go test ./internal/x/\nok (green after)\n"
		subagentStopPut(t, evidence.FlowReceiptPath(a), body)
		// The final message is the RECEIPT JSON alone, as the packet asks: no EVIDENCE_RECORDED line after it.
		if out := r.stop(tr, flowStopJSON); out != "" {
			t.Fatalf("the receipt in the run worktree was not accepted: %q", out)
		}
		if c := r.attempts(); c.Attempts != 0 || c.Spent() {
			t.Fatalf("attempts after the release: %+v", c)
		}
		if evidence.HasTombstone(r.owner, flowStopOwner, r.item(flowStopJSON)) {
			t.Fatal("a released child has a tombstone")
		}
		rec, ok := evidence.RecordedAssignment(r.owner, flowStopOwner, a.ID)
		if !ok || rec.Status != evidence.AssignmentClaimed || rec.AgentID != flowStopAgent || rec.TurnID != flowStopTurn || rec.ReceiptDigest != flowStopDigest(body) {
			t.Fatalf("the claimed record: %+v (%v)", rec, ok)
		}
		// The acceptance path reads back exactly the bytes the hook accepted.
		if got, ok := evidence.AcceptedReceipt(r.owner, flowStopOwner, a.ID); !ok || string(got.Bytes) != body || got.TurnID != flowStopTurn || got.AgentID != flowStopAgent {
			t.Fatalf("AcceptedReceipt = %+v, %v", got, ok)
		}
		// Nothing was looked for or written in the owner's native evidence directory.
		if _, err := os.Stat(filepath.Join(r.owner, ".crw", "evidence")); !os.IsNotExist(err) {
			t.Fatalf("the owner's native evidence directory: %v", err)
		}
	})

	t.Run("ReceiptWrittenAfterABlock", func(t *testing.T) {
		r := newFlowStopRig(t)
		a := r.mint(flowStopCall, flowStopOp, r.run)
		tr := r.rollout(flowStopSpawnText(flowStopOp, r.run, a))
		flowStopReason(t, r.stop(tr, flowStopJSON))
		if c := r.attempts(); c.Attempts != 1 {
			t.Fatalf("attempts after the first block: %+v", c)
		}
		subagentStopPut(t, evidence.FlowReceiptPath(a), "ran the checks")
		if out := r.stop(tr, flowStopJSON); out != "" {
			t.Fatalf("the receipt written after the block was not accepted: %q", out)
		}
		if c := r.attempts(); c.Attempts != 0 {
			t.Fatalf("the release left attempts: %+v", c)
		}
	})

	t.Run("ExplicitMarkerNamingTheFixedFile", func(t *testing.T) {
		r := newFlowStopRig(t)
		a := r.mint(flowStopCall, flowStopOp, r.run)
		tr := r.rollout(flowStopSpawnText(flowStopOp, r.run, a))
		subagentStopPut(t, evidence.FlowReceiptPath(a), "ran the checks")
		if out := r.stop(tr, flowStopJSON+"\nEVIDENCE_RECORDED: "+evidence.FlowReceiptPath(a)); out != "" {
			t.Fatalf("a marker naming the fixed file was not accepted: %q", out)
		}
		// the marked path records the turn too
		if rec, ok := evidence.RecordedAssignment(r.owner, flowStopOwner, a.ID); !ok || rec.TurnID != flowStopTurn || rec.ReceiptDigest != flowStopDigest("ran the checks") {
			t.Fatalf("the claimed record: %+v (%v)", rec, ok)
		}
	})

	t.Run("Blocked", func(t *testing.T) {
		t.Run("no receipt names the absolute file", func(t *testing.T) {
			r := newFlowStopRig(t)
			a := r.mint(flowStopCall, flowStopOp, r.run)
			tr := r.rollout(flowStopSpawnText(flowStopOp, r.run, a))
			reason := flowStopReason(t, r.stop(tr, flowStopJSON))
			if want := evidence.VerifierDirectiveFor(1, evidence.FlowReceiptPath(a)); reason != want {
				t.Fatalf("reason %q, want %q", reason, want)
			}
			if !strings.Contains(reason, filepath.Join(r.run, ".crw", "evidence", a.ID+".md")) {
				t.Fatalf("the reason names no absolute receipt file: %q", reason)
			}
			if c := r.attempts(); c.Attempts != 1 {
				t.Fatalf("attempts: %+v", c)
			}
		})

		t.Run("forged receipt in the owner's native evidence", func(t *testing.T) {
			r := newFlowStopRig(t)
			a := r.mint(flowStopCall, flowStopOp, r.run)
			tr := r.rollout(flowStopSpawnText(flowStopOp, r.run, a))
			forged := filepath.Join(r.owner, ".crw", "evidence", a.ID+".md")
			subagentStopPut(t, forged, "forged")
			// The native root would take this receipt for a child with no contract; this child has one, so it does not count.
			if !evidence.HasValidReceipt(r.owner, forged) {
				t.Fatal("the forged receipt is not one the native root would take")
			}
			for i, message := range []string{flowStopJSON, flowStopJSON + "\nEVIDENCE_RECORDED: " + forged, flowStopJSON + "\nEVIDENCE_RECORDED: .crw/evidence/" + a.ID + ".md"} {
				reason := flowStopReason(t, r.stop(tr, message))
				if want := evidence.VerifierDirectiveFor(i+1, evidence.FlowReceiptPath(a)); reason != want {
					t.Fatalf("stop %d: reason %q, want %q", i+1, reason, want)
				}
			}
			if rec, ok := evidence.RecordedAssignment(r.owner, flowStopOwner, a.ID); !ok || rec.Status != evidence.AssignmentOpen || rec.AgentID != "" {
				t.Fatalf("a refused receipt claimed the record: %+v (%v)", rec, ok)
			}
		})

		t.Run("receipt in another call's worktree", func(t *testing.T) {
			r := newFlowStopRig(t)
			a := r.mint(flowStopCall, flowStopOp, r.run)
			otherTree := flowStopWorktree(t)
			b := r.mint("c_000000000bbb", "c_000000000bbb/t1-a1", otherTree)
			subagentStopPut(t, evidence.FlowReceiptPath(b), "the other call's receipt")
			subagentStopPut(t, filepath.Join(otherTree, ".crw", "evidence", a.ID+".md"), "this id in the other tree")
			tr := r.rollout(flowStopSpawnText(flowStopOp, r.run, a))
			for i, message := range []string{
				flowStopJSON,
				flowStopJSON + "\nEVIDENCE_RECORDED: " + evidence.FlowReceiptPath(b),
				flowStopJSON + "\nEVIDENCE_RECORDED: " + filepath.Join(otherTree, ".crw", "evidence", a.ID+".md"),
			} {
				flowStopReason(t, r.stop(tr, message))
				if c := r.attempts(); c.Attempts != i+1 {
					t.Fatalf("stop %d: attempts %+v", i+1, c)
				}
			}
			for _, id := range []string{a.ID, b.ID} {
				if rec, ok := evidence.RecordedAssignment(r.owner, flowStopOwner, id); !ok || rec.Status != evidence.AssignmentOpen {
					t.Fatalf("the record %s after refused stops: %+v (%v)", id, rec, ok)
				}
			}
		})

		t.Run("the budget ends in a tombstone as today", func(t *testing.T) {
			r := newFlowStopRig(t)
			a := r.mint(flowStopCall, flowStopOp, r.run)
			tr := r.rollout(flowStopSpawnText(flowStopOp, r.run, a))
			for attempt := 1; attempt <= evidence.MaxAttempts; attempt++ {
				reason := flowStopReason(t, r.stop(tr, flowStopJSON))
				if want := evidence.VerifierDirectiveFor(attempt, evidence.FlowReceiptPath(a)); reason != want {
					t.Fatalf("attempt %d: reason %q, want %q", attempt, reason, want)
				}
				if c := r.attempts(); c.Attempts != attempt {
					t.Fatalf("attempt %d: counter %+v", attempt, c)
				}
			}
			if out := r.stop(tr, flowStopJSON); out != "" {
				t.Fatalf("the spent budget still blocks: %q", out)
			}
			if !evidence.HasTombstone(r.owner, flowStopOwner, r.item(flowStopJSON)) {
				t.Fatal("no tombstone after the budget")
			}
			if c := r.attempts(); !c.Spent() {
				t.Fatalf("the counter after the budget: %+v", c)
			}
			if rec, ok := evidence.RecordedAssignment(r.owner, flowStopOwner, a.ID); !ok || rec.Status != evidence.AssignmentOpen {
				t.Fatalf("the tombstone claimed the record: %+v (%v)", rec, ok)
			}
		})

		t.Run("a packet without the block keeps the generic reason", func(t *testing.T) {
			r := newFlowStopRig(t)
			r.mint(flowStopCall, flowStopOp, r.run)
			packet := flowStopSpawnText(flowStopOp, r.run, evidence.Assignment{})
			packet = packet[:strings.Index(packet, evidence.AssignmentMarker)]
			reason := flowStopReason(t, r.stop(r.rollout(packet), flowStopJSON))
			if want := evidence.VerifierDirective(1); reason != want {
				t.Fatalf("reason %q, want the generic %q", reason, want)
			}
		})

		t.Run("the block under another op", func(t *testing.T) {
			r := newFlowStopRig(t)
			a := r.mint(flowStopCall, flowStopOp, r.run)
			subagentStopPut(t, evidence.FlowReceiptPath(a), "ran the checks")
			reason := flowStopReason(t, r.stop(r.rollout(flowStopSpawnText("c_000000000ccc/t1-a1", r.run, a)), flowStopJSON))
			// the child is not the dispatch of the contract's op, so it has no flow contract to be told about either
			if want := evidence.VerifierDirective(1); reason != want {
				t.Fatalf("reason %q, want the generic %q", reason, want)
			}
		})
	})
}
