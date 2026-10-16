package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// CRW-1482 (1209 field trial, defect C9). A flow run's executor works in the call worktree the engine made for it, while the stop
// hook looked for its receipt under the owner's native cwd and found nothing there. The engine now records a tree assignment of
// origin flow for each executor dispatch (the trusted writer, as the spawn hook is in CRW-1115), names one fixed receipt file in
// the spawn message, and the hook accepts that file without a marker line after the RECEIPT JSON. The claim records the turn and
// the receipt's digest, so the acceptance path can read back exactly the bytes the hook accepted.

const (
	flowTestOwner = "01a13c08-0c03-7951-afc5-57656aca48bc"
	flowTestRun   = "run-7b534f998cfc"
	flowTestCall  = "c_5cff7231980e"
	flowTestOp    = "c_5cff7231980e/t1-a1"
	flowTestJSON  = `{"status":"done","summary":"implemented","commands":["go test ./..."]}`
)

type flowTestRig struct {
	t         *testing.T
	cwd, tree string
}

func newFlowTestRig(t *testing.T) *flowTestRig {
	t.Helper()
	return &flowTestRig{t: t, cwd: t.TempDir(), tree: flowTestTree(t)}
}

// flowTestTree is a call worktree with its real .crw/evidence directory, by its real path.
func flowTestTree(t *testing.T) string {
	t.Helper()
	tree := t.TempDir()
	assignMust(t, os.MkdirAll(filepath.Join(tree, ".crw", "evidence"), 0o755))
	real, err := filepath.EvalSymlinks(tree)
	assignMust(t, err)
	return real
}

// mint records the flow assignment of op made at created, as the engine does at publication.
func (r *flowTestRig) mint(op string, created time.Time) Assignment {
	r.t.Helper()
	a, err := NewFlowAssignment(flowTestOwner, FlowAssignmentID(flowTestOwner, flowTestRun, flowTestCall, op), r.tree, op, created)
	assignMust(r.t, err)
	assignMust(r.t, a.PersistNew(r.cwd))
	return a
}

func (r *flowTestRig) recordPath(id string) string {
	return filepath.Join(r.cwd, ".crw", AssignmentsSubdir, sessionRecordDir(flowTestOwner), id+".json")
}

// unchanged fails t unless the record id still holds exactly the bytes raw.
func (r *flowTestRig) unchanged(t *testing.T, id string, raw []byte) {
	t.Helper()
	if now, err := os.ReadFile(r.recordPath(id)); err != nil || string(now) != string(raw) {
		t.Fatalf("the record changed (%v):\n%s\nwant\n%s", err, now, raw)
	}
}

func (r *flowTestRig) record(id string) Assignment {
	r.t.Helper()
	a, ok := readAssignment(r.recordPath(id), id)
	if !ok {
		r.t.Fatalf("the record %s cannot be read", id)
	}
	return a
}

// flowSpawnText is the engine's spawn message for op with the evidence block appended.
func flowSpawnText(op, id, receipt string) string {
	return "crw-flow-op: " + op + "\nCRW-ROLE: executor\n\n" +
		"Your shell starts in a directory that is NOT your workspace. Read the packet. " +
		"Your final message is exactly the JSON object the packet names, and nothing else.\n\n" +
		AssignmentMarker + ":" + id + "] Write your evidence receipt to " + receipt + " before your final message; no marker line is needed after the JSON."
}

func (r *flowTestRig) transcript(packet string) string {
	r.t.Helper()
	return (&assignTestRig{t: r.t}).transcript(rolloutMeta("child-thread"), rolloutUserMessage("# AGENTS.md instructions"), rolloutUserItem("child-thread", packet))
}

func flowWrite(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	assignMust(t, os.MkdirAll(filepath.Dir(path), 0o755))
	assignMust(t, os.WriteFile(path, []byte(body), 0o644))
	if !mtime.IsZero() {
		assignMust(t, os.Chtimes(path, mtime, mtime))
	}
}

func flowDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func TestFlowAssignment(t *testing.T) {
	t.Run("FixedReceiptAccepted", func(t *testing.T) {
		r := newFlowTestRig(t)
		a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		receipt := FlowReceiptPath(a)
		if want := filepath.Join(r.tree, ".crw", "evidence", a.ID+".md"); receipt != want {
			t.Fatalf("FlowReceiptPath = %s, want %s", receipt, want)
		}
		if rec := r.record(a.ID); rec.Version != 2 || rec.Origin != AssignmentOriginFlow || rec.Op != flowTestOp || rec.Root != r.tree ||
			rec.Mode != AssignTree || rec.Status != AssignmentOpen || rec.SessionID != flowTestOwner {
			t.Fatalf("the minted record: %+v", rec)
		}
		body := "$ go test ./x\nFAIL (red before)\n$ go test ./x\nok (green after)\n"
		flowWrite(t, receipt, body, time.Time{})
		tr := r.transcript(flowSpawnText(flowTestOp, a.ID, receipt))
		if c, ok := FlowContractOf(r.cwd, flowTestOwner, "agent-1", tr, flowTestJSON); !ok || c.ID != a.ID {
			t.Fatalf("FlowContractOf = %+v, %v", c, ok)
		}
		// The final message is the JSON alone: no EVIDENCE_RECORDED line, and the receipt argument is "".
		if got := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-1", tr, flowTestJSON, ""); got != AssignedAccepted {
			t.Fatalf("the fixed receipt judged %v", got)
		}
		rec := r.record(a.ID)
		if rec.Status != AssignmentClaimed || rec.AgentID != "agent-1" || rec.TurnID != "turn-1" || rec.ReceiptDigest != flowDigest(body) || rec.Version != 2 {
			t.Fatalf("the claimed record: %+v", rec)
		}
		got, ok := AcceptedReceipt(r.cwd, flowTestOwner, a.ID)
		if !ok || got.Path != receipt || got.Digest != flowDigest(body) || got.AgentID != "agent-1" || got.TurnID != "turn-1" || string(got.Bytes) != body {
			t.Fatalf("AcceptedReceipt = %+v, %v", got, ok)
		}
		// An explicit marker that names exactly the fixed file, absolute or relative to the tree, is the same receipt.
		for _, claim := range []string{receipt, filepath.Join(".crw", "evidence", a.ID+".md")} {
			if got := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-1", tr, flowTestJSON+"\nEVIDENCE_RECORDED: "+claim, claim); got != AssignedAccepted {
				t.Fatalf("the explicit fixed path %s judged %v", claim, got)
			}
		}
		// The signature without a turn delegates.
		if got := JudgeAssignedReceipt(r.cwd, flowTestOwner, "agent-1", tr, flowTestJSON, ""); got != AssignedAccepted {
			t.Fatalf("JudgeAssignedReceipt judged %v", got)
		}
	})

	t.Run("Refusals", func(t *testing.T) {
		type refusal struct {
			agent   string
			packet  func(a Assignment) string // the dispatch packet; "" means no transcript
			message func(a Assignment) string
			receipt func(a Assignment) string
			setup   func(t *testing.T, r *flowTestRig, a Assignment)
		}
		own := func(a Assignment) string { return flowSpawnText(a.Op, a.ID, FlowReceiptPath(a)) }
		jsonOnly := func(Assignment) string { return flowTestJSON }
		none := func(Assignment) string { return "" }
		fixed := func(t *testing.T, r *flowTestRig, a Assignment) {
			flowWrite(t, FlowReceiptPath(a), "checked", time.Time{})
		}
		cases := map[string]refusal{
			"no receipt": {packet: own, message: jsonOnly, receipt: none},
			"receipt only in the native cwd": {packet: own, message: jsonOnly, receipt: none, setup: func(t *testing.T, r *flowTestRig, a Assignment) {
				flowWrite(t, filepath.Join(r.cwd, ".crw", "evidence", a.ID+".md"), "forged", time.Time{})
			}},
			"explicit native receipt": {packet: own,
				message: func(a Assignment) string { return flowTestJSON + "\nEVIDENCE_RECORDED: native" },
				receipt: func(a Assignment) string { return "NATIVE" },
				setup: func(t *testing.T, r *flowTestRig, a Assignment) {
					flowWrite(t, filepath.Join(r.cwd, ".crw", "evidence", a.ID+".md"), "forged", time.Time{})
				}},
			"receipt in another call's worktree": {packet: own, message: jsonOnly, receipt: func(a Assignment) string { return "OTHER" }},
			"explicit marker naming another file under the root": {packet: own, message: jsonOnly,
				receipt: func(a Assignment) string { return filepath.Join(a.Root, ".crw", "evidence", "other.md") },
				setup: func(t *testing.T, r *flowTestRig, a Assignment) {
					fixed(t, r, a)
					flowWrite(t, filepath.Join(a.Root, ".crw", "evidence", "other.md"), "other", time.Time{})
				}},
			"receipt older than its dispatch": {packet: own, message: jsonOnly, receipt: none, setup: func(t *testing.T, r *flowTestRig, a Assignment) {
				created, err := time.Parse(time.RFC3339Nano, a.CreatedAt)
				assignMust(t, err)
				flowWrite(t, FlowReceiptPath(a), "stale", created.Add(-time.Second))
			}},
			"symlinked evidence directory": {packet: own, message: jsonOnly, receipt: none, setup: func(t *testing.T, r *flowTestRig, a Assignment) {
				elsewhere := t.TempDir()
				flowWrite(t, filepath.Join(elsewhere, a.ID+".md"), "linked", time.Time{})
				dir := filepath.Join(a.Root, ".crw", "evidence")
				assignMust(t, os.RemoveAll(dir))
				assignMust(t, os.Symlink(elsewhere, dir))
			}},
			"symlinked receipt": {packet: own, message: jsonOnly, receipt: none, setup: func(t *testing.T, r *flowTestRig, a Assignment) {
				target := filepath.Join(t.TempDir(), "target.md")
				flowWrite(t, target, "linked", time.Time{})
				assignMust(t, os.Symlink(target, FlowReceiptPath(a)))
			}},
			"the block under another op": {packet: func(a Assignment) string { return flowSpawnText("c_other00000/t1-a1", a.ID, FlowReceiptPath(a)) },
				message: jsonOnly, receipt: none, setup: fixed},
			"the block without an op line": {packet: func(a Assignment) string { return strings.SplitN(own(a), "\n", 2)[1] },
				message: jsonOnly, receipt: none, setup: fixed},
			"no packet shown, the id cited": {packet: none,
				message: func(a Assignment) string { return flowTestJSON + "\n" + AssignmentCitation + " " + a.ID },
				receipt: none, setup: fixed},
			"no agent id": {agent: "-", packet: own, message: jsonOnly, receipt: none, setup: fixed},
		}
		for name, c := range cases {
			t.Run(name, func(t *testing.T) {
				r := newFlowTestRig(t)
				a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
				if c.setup != nil {
					c.setup(t, r, a)
				}
				receipt := c.receipt(a)
				switch receipt {
				case "NATIVE":
					receipt = filepath.Join(r.cwd, ".crw", "evidence", a.ID+".md")
				case "OTHER":
					other := flowTestTree(t)
					receipt = filepath.Join(other, ".crw", "evidence", a.ID+".md")
					flowWrite(t, receipt, "other call", time.Time{})
				}
				tr := ""
				if p := c.packet(a); p != "" {
					tr = r.transcript(p)
				}
				agent := "agent-1"
				if c.agent == "-" {
					agent = ""
				}
				if got := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, agent, "turn-1", tr, c.message(a), receipt); got != AssignedRefused {
					t.Fatalf("judged %v, want refused", got)
				}
				if rec := r.record(a.ID); rec.AgentID != "" || rec.Status != AssignmentOpen || rec.TurnID != "" || rec.ReceiptDigest != "" {
					t.Fatalf("a refused receipt changed the record: %+v", rec)
				}
				if got, ok := AcceptedReceipt(r.cwd, flowTestOwner, a.ID); ok {
					t.Fatalf("AcceptedReceipt of a refused receipt: %+v", got)
				}
			})
		}

		t.Run("a second agent after the claim", func(t *testing.T) {
			r := newFlowTestRig(t)
			a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
			flowWrite(t, FlowReceiptPath(a), "checked", time.Time{})
			tr := r.transcript(own(a))
			if got := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-1", tr, flowTestJSON, ""); got != AssignedAccepted {
				t.Fatalf("the first agent judged %v", got)
			}
			if got := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-2", "turn-9", tr, flowTestJSON, ""); got != AssignedRefused {
				t.Fatalf("a second agent judged %v", got)
			}
			if rec := r.record(a.ID); rec.AgentID != "agent-1" || rec.TurnID != "turn-1" {
				t.Fatalf("the second agent changed the claim: %+v", rec)
			}
		})

		t.Run("records of the wrong version", func(t *testing.T) {
			for name, damage := range map[string]func(a *Assignment){
				"v1 with origin":           func(a *Assignment) { a.Version = 1 },
				"v1 with an op":            func(a *Assignment) { a.Version, a.Origin = 1, "" },
				"v1 with a digest":         func(a *Assignment) { a.Version, a.Origin, a.Op, a.ReceiptDigest = 1, "", "", flowDigest("x") },
				"v2 without origin":        func(a *Assignment) { a.Origin = "" },
				"v2 without an op":         func(a *Assignment) { a.Op = "" },
				"an unknown origin":        func(a *Assignment) { a.Origin = "spawn" },
				"v3":                       func(a *Assignment) { a.Version = 3 },
				"flow record of mode none": func(a *Assignment) { a.Mode = AssignNone },
			} {
				t.Run(name, func(t *testing.T) {
					r := newFlowTestRig(t)
					a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
					flowWrite(t, FlowReceiptPath(a), "checked", time.Time{})
					b := a
					damage(&b)
					assignMust(t, writeRecord(r.recordPath(a.ID), b))
					if _, ok := readAssignment(r.recordPath(a.ID), a.ID); ok {
						t.Fatalf("the record %+v was read", b)
					}
					if got := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-1", r.transcript(own(a)), flowTestJSON, ""); got != AssignedRefused {
						t.Fatalf("judged %v", got)
					}
				})
			}
		})
	})

	t.Run("RepairReclaim", func(t *testing.T) {
		r := newFlowTestRig(t)
		a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		receipt := FlowReceiptPath(a)
		tr := r.transcript(flowSpawnText(flowTestOp, a.ID, receipt))
		flowWrite(t, receipt, "first turn", time.Time{})
		if got := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-1", tr, flowTestJSON, ""); got != AssignedAccepted {
			t.Fatalf("turn 1 judged %v", got)
		}
		flowWrite(t, receipt, "repair turn: red before, green after", time.Time{})
		if got := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-2", tr, flowTestJSON, ""); got != AssignedAccepted {
			t.Fatalf("the repair turn judged %v", got)
		}
		if rec := r.record(a.ID); rec.TurnID != "turn-2" || rec.ReceiptDigest != flowDigest("repair turn: red before, green after") || rec.AgentID != "agent-1" {
			t.Fatalf("the repair claim: %+v", rec)
		}
		if got, ok := AcceptedReceipt(r.cwd, flowTestOwner, a.ID); !ok || got.TurnID != "turn-2" || string(got.Bytes) != "repair turn: red before, green after" {
			t.Fatalf("AcceptedReceipt after the repair = %+v, %v", got, ok)
		}
	})

	t.Run("AcceptedReceiptDrift", func(t *testing.T) {
		claimed := func(t *testing.T) (*flowTestRig, Assignment) {
			r := newFlowTestRig(t)
			a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
			flowWrite(t, FlowReceiptPath(a), "accepted bytes", time.Time{})
			if got := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-1", r.transcript(flowSpawnText(flowTestOp, a.ID, FlowReceiptPath(a))), flowTestJSON, ""); got != AssignedAccepted {
				t.Fatalf("judged %v", got)
			}
			if _, ok := AcceptedReceipt(r.cwd, flowTestOwner, a.ID); !ok {
				t.Fatal("the accepted receipt is not returned")
			}
			return r, a
		}
		for name, drift := range map[string]func(t *testing.T, r *flowTestRig, a Assignment){
			"bytes changed": func(t *testing.T, r *flowTestRig, a Assignment) {
				flowWrite(t, FlowReceiptPath(a), "edited later", time.Time{})
			},
			"file removed": func(t *testing.T, r *flowTestRig, a Assignment) { assignMust(t, os.Remove(FlowReceiptPath(a))) },
			"file replaced by a link to the same bytes": func(t *testing.T, r *flowTestRig, a Assignment) {
				target := filepath.Join(t.TempDir(), "same.md")
				flowWrite(t, target, "accepted bytes", time.Time{})
				assignMust(t, os.Remove(FlowReceiptPath(a)))
				assignMust(t, os.Symlink(target, FlowReceiptPath(a)))
			},
			"digest edited in the record": func(t *testing.T, r *flowTestRig, a Assignment) {
				rec := r.record(a.ID)
				rec.ReceiptDigest = flowDigest("something else")
				assignMust(t, writeRecord(r.recordPath(a.ID), rec))
			},
		} {
			t.Run(name, func(t *testing.T) {
				r, a := claimed(t)
				drift(t, r, a)
				if got, ok := AcceptedReceipt(r.cwd, flowTestOwner, a.ID); ok {
					t.Fatalf("a drifted receipt was returned: %+v", got)
				}
			})
		}
		// Unclaimed, unknown, another session, and a claimed native record are not accepted flow receipts either.
		r := newFlowTestRig(t)
		a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		flowWrite(t, FlowReceiptPath(a), "never claimed", time.Time{})
		if _, ok := AcceptedReceipt(r.cwd, flowTestOwner, a.ID); ok {
			t.Fatal("an unclaimed record gave a receipt")
		}
		if _, ok := AcceptedReceipt(r.cwd, "another-owner", a.ID); ok {
			t.Fatal("another session's record gave a receipt")
		}
		if _, ok := AcceptedReceipt(r.cwd, flowTestOwner, "AAAAAAAAAAAAAAAAAAAAAAAAAA"); ok {
			t.Fatal("an unknown id gave a receipt")
		}
		n := newAssignTestRig(t)
		native := n.assign(time.Now().Add(-time.Minute))
		receipt := n.receipt("check.txt", time.Time{})
		packet := n.transcript(rolloutMeta("child"), rolloutUserItem("child", assignBlock(native.ID)))
		if got := JudgeAssignedReceipt(n.cwd, assignTestSession, "w1", packet, "EVIDENCE_RECORDED: "+receipt, receipt); got != AssignedAccepted {
			t.Fatalf("the native receipt judged %v", got)
		}
		if _, ok := AcceptedReceipt(n.cwd, assignTestSession, native.ID); ok {
			t.Fatal("a claimed native record gave a flow receipt")
		}
	})

	t.Run("DeterministicID", func(t *testing.T) {
		id := FlowAssignmentID(flowTestOwner, flowTestRun, flowTestCall, flowTestOp)
		if !validAssignmentID(id) {
			t.Fatalf("the id %q has not the shape of an assignment id", id)
		}
		if again := FlowAssignmentID(flowTestOwner, flowTestRun, flowTestCall, flowTestOp); again != id {
			t.Fatalf("the same inputs gave %s and %s", id, again)
		}
		for _, other := range [][4]string{
			{"other-owner", flowTestRun, flowTestCall, flowTestOp},
			{flowTestOwner, "run-other", flowTestCall, flowTestOp},
			{flowTestOwner, flowTestRun, "c_other", flowTestOp},
			{flowTestOwner, flowTestRun, flowTestCall, "c_5cff7231980e/t1-a2"},
			{flowTestOwner + "\x00" + flowTestRun, "", flowTestCall, flowTestOp},
		} {
			if FlowAssignmentID(other[0], other[1], other[2], other[3]) == id {
				t.Fatalf("%q gave the same id", other)
			}
		}
		r := newFlowTestRig(t)
		a := r.mint(flowTestOp, time.Now())
		if a.ID != id {
			t.Fatalf("the minted id %s, want %s", a.ID, id)
		}
		b, err := NewFlowAssignment(flowTestOwner, id, r.tree, flowTestOp, time.Now().Add(time.Second))
		assignMust(t, err)
		if err := b.PersistNew(r.cwd); !errors.Is(err, ErrAssignmentExists) {
			t.Fatalf("PersistNew twice = %v", err)
		}
		if rec := r.record(id); rec.CreatedAt != a.CreatedAt {
			t.Fatalf("the second PersistNew replaced the record: %+v", rec)
		}
		for name, mk := range map[string]func() (Assignment, error){
			"no session": func() (Assignment, error) { return NewFlowAssignment("", id, r.tree, flowTestOp, time.Now()) },
			"bad id": func() (Assignment, error) {
				return NewFlowAssignment(flowTestOwner, "../x", r.tree, flowTestOp, time.Now())
			},
			"no op": func() (Assignment, error) { return NewFlowAssignment(flowTestOwner, id, r.tree, "", time.Now()) },
			"op on two lines": func() (Assignment, error) {
				return NewFlowAssignment(flowTestOwner, id, r.tree, flowTestOp+"\nx", time.Now())
			},
			"relative root": func() (Assignment, error) { return NewFlowAssignment(flowTestOwner, id, "wt", flowTestOp, time.Now()) },
			"missing root": func() (Assignment, error) {
				return NewFlowAssignment(flowTestOwner, id, filepath.Join(r.tree, "nope"), flowTestOp, time.Now())
			},
		} {
			if _, err := mk(); err == nil {
				t.Errorf("NewFlowAssignment with %s succeeded", name)
			}
		}
	})

	t.Run("ReuseOpenOrClaimed", func(t *testing.T) {
		r := newFlowTestRig(t)
		id := FlowAssignmentID(flowTestOwner, flowTestRun, flowTestCall, flowTestOp)
		fresh, err := NewFlowAssignment(flowTestOwner, id, r.tree, flowTestOp, time.Now().Add(-time.Minute))
		assignMust(t, err)
		got, err := EnsureFlowAssignment(r.cwd, fresh)
		if err != nil || got != fresh {
			t.Fatalf("the first EnsureFlowAssignment = %+v, %v", got, err)
		}
		later, err := NewFlowAssignment(flowTestOwner, id, r.tree, flowTestOp, time.Now())
		assignMust(t, err)
		if got, err := EnsureFlowAssignment(r.cwd, later); err != nil || got != fresh {
			t.Fatalf("reuse of the open record = %+v, %v; want %+v", got, err, fresh)
		}
		flowWrite(t, FlowReceiptPath(fresh), "checked", time.Time{})
		if v := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-1", r.transcript(flowSpawnText(flowTestOp, id, FlowReceiptPath(fresh))), flowTestJSON, ""); v != AssignedAccepted {
			t.Fatalf("judged %v", v)
		}
		got, err = EnsureFlowAssignment(r.cwd, later)
		if err != nil || got.Status != AssignmentClaimed || got.AgentID != "agent-1" || got.CreatedAt != fresh.CreatedAt {
			t.Fatalf("reuse of the claimed record = %+v, %v", got, err)
		}
		otherRoot, err := NewFlowAssignment(flowTestOwner, id, flowTestTree(t), flowTestOp, time.Now())
		assignMust(t, err)
		if _, err := EnsureFlowAssignment(r.cwd, otherRoot); err == nil {
			t.Fatal("a record of another root was reused")
		}
		otherOp, err := NewFlowAssignment(flowTestOwner, id, r.tree, "c_5cff7231980e/t1-a2", time.Now())
		assignMust(t, err)
		if _, err := EnsureFlowAssignment(r.cwd, otherOp); err == nil {
			t.Fatal("a record of another op was reused")
		}
		// A record that cannot be read is no record to reuse.
		assignMust(t, os.WriteFile(r.recordPath(id), []byte("{damaged"), 0o644))
		if _, err := EnsureFlowAssignment(r.cwd, later); err == nil {
			t.Fatal("a damaged record was reused")
		}
		native, err := NewAssignment(flowTestOwner, r.tree, AssignTree, time.Now())
		assignMust(t, err)
		if _, err := EnsureFlowAssignment(r.cwd, native); err == nil {
			t.Fatal("EnsureFlowAssignment took a native assignment")
		}
	})

	// CRW-1496: an open, unclaimed record is reused only while it is still the contract a claim would accept for the tree now.
	t.Run("RecreatedWorktreeReplacesOpenRecord", func(t *testing.T) {
		r := newFlowTestRig(t)
		old := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		if got, err := EnsureFlowAssignment(r.cwd, old); err != nil || got != old {
			t.Fatalf("Ensure of the minted record = %+v, %v", got, err)
		}
		flowRecreateTree(t, r.tree)
		fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
		assignMust(t, err)
		got, err := EnsureFlowAssignment(r.cwd, fresh)
		if err != nil {
			t.Fatalf("Ensure after the recreate: %v", err)
		}
		if got.RootIno == old.RootIno || got.RootIno != fresh.RootIno || got.CreatedAt != fresh.CreatedAt {
			t.Fatalf("Ensure after the recreate returned %+v; want the new tree's record %+v (old %+v)", got, fresh, old)
		}
		if rec := r.record(old.ID); rec != got {
			t.Fatalf("the file holds %+v, Ensure returned %+v", rec, got)
		}
		flowAcceptIn(t, r, got, "agent-1")
	})

	t.Run("RecreatedWorktreeSameClock", func(t *testing.T) {
		created := time.Now().Add(-time.Minute)
		for name, now := range map[string]time.Time{"same clock": created, "earlier clock": created.Add(-time.Second)} {
			t.Run(name, func(t *testing.T) {
				r := newFlowTestRig(t)
				old := r.mint(flowTestOp, created)
				flowRecreateTree(t, r.tree)
				fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, now)
				assignMust(t, err)
				got, err := EnsureFlowAssignment(r.cwd, fresh)
				if err != nil {
					t.Fatalf("Ensure after the recreate: %v", err)
				}
				want := created.UTC().Add(time.Nanosecond).Format(time.RFC3339Nano)
				if got.RootIno == old.RootIno || got.CreatedAt != want {
					t.Fatalf("Ensure returned %+v; want the new inode and CreatedAt %s", got, want)
				}
				if rec := r.record(old.ID); rec != got {
					t.Fatalf("the file holds %+v, Ensure returned %+v", rec, got)
				}
				flowAcceptIn(t, r, got, "agent-1")
			})
		}
	})

	t.Run("RewoundHeadReplacesOpenRecord", func(t *testing.T) {
		r := newFlowTestRig(t)
		flowGitRepo(t, r.tree)
		first := flowGitCommit(t, r.tree, "first")
		second := flowGitCommit(t, r.tree, "second")
		old := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		if old.Head != second {
			t.Fatalf("the minted record's head %q, want %q", old.Head, second)
		}
		flowGit(t, r.tree, "reset", "-q", "--hard", first)
		fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
		assignMust(t, err)
		got, err := EnsureFlowAssignment(r.cwd, fresh)
		if err != nil {
			t.Fatalf("Ensure after the rewind: %v", err)
		}
		if got.Head != first || got.RootIno != old.RootIno || got.CreatedAt != fresh.CreatedAt {
			t.Fatalf("Ensure after the rewind returned %+v; want head %s at the same inode", got, first)
		}
		if rec := r.record(old.ID); rec != got {
			t.Fatalf("the file holds %+v, Ensure returned %+v", rec, got)
		}
		flowAcceptIn(t, r, got, "agent-1")
	})

	t.Run("ClaimedRecordKeptAfterRecreate", func(t *testing.T) {
		r := newFlowTestRig(t)
		old := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		flowAcceptIn(t, r, old, "agent-1")
		claimed := r.record(old.ID)
		before, err := os.ReadFile(r.recordPath(old.ID))
		assignMust(t, err)
		flowRecreateTree(t, r.tree)
		fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
		assignMust(t, err)
		got, err := EnsureFlowAssignment(r.cwd, fresh)
		if err != nil || got != claimed {
			t.Fatalf("Ensure of the claimed record after the recreate = %+v, %v; want %+v", got, err, claimed)
		}
		after, err := os.ReadFile(r.recordPath(old.ID))
		assignMust(t, err)
		if string(after) != string(before) {
			t.Fatalf("the claimed record was rewritten:\n%s\nwant\n%s", after, before)
		}
	})

	t.Run("SameTreeReuseUnchanged", func(t *testing.T) {
		r := newFlowTestRig(t)
		flowGitRepo(t, r.tree)
		flowGitCommit(t, r.tree, "first")
		old := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		path := r.recordPath(old.ID)
		past := time.Now().Add(-time.Hour).Truncate(time.Second)
		assignMust(t, os.Chtimes(path, past, past))
		before, err := os.ReadFile(path)
		assignMust(t, err)
		flowGitCommit(t, r.tree, "second") // HEAD advances to a descendant of the recorded head
		fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
		assignMust(t, err)
		got, err := EnsureFlowAssignment(r.cwd, fresh)
		if err != nil || got != old {
			t.Fatalf("Ensure of the same tree = %+v, %v; want %+v", got, err, old)
		}
		after, err := os.ReadFile(path)
		assignMust(t, err)
		info, err := os.Stat(path)
		assignMust(t, err)
		if string(after) != string(before) || !info.ModTime().Equal(past) {
			t.Fatalf("the record was rewritten (mtime %v, want %v):\n%s", info.ModTime(), past, after)
		}
	})

	// replaceOpen decides on the record it reads again under the lock, not on the one the caller read before.
	t.Run("ReplaceOpenRace", func(t *testing.T) {
		stale := func(t *testing.T) (*flowTestRig, Assignment, Assignment) {
			r := newFlowTestRig(t)
			old := r.mint(flowTestOp, time.Now().Add(-time.Minute))
			flowRecreateTree(t, r.tree)
			fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
			assignMust(t, err)
			return r, old, fresh
		}
		t.Run("claimed after the caller's read", func(t *testing.T) {
			r, old, fresh := stale(t)
			// The child claimed the old contract before the tree went: the claim stands whatever the tree is now.
			flowWrite(t, FlowReceiptPath(old), "checked", time.Time{})
			assignMust(t, writeRecord(r.recordPath(old.ID), func() Assignment {
				c := old
				c.Status, c.AgentID, c.TurnID, c.ReceiptDigest = AssignmentClaimed, "agent-1", "turn-1", flowDigest("checked")
				return c
			}()))
			claimed := r.record(old.ID)
			before, err := os.ReadFile(r.recordPath(old.ID))
			assignMust(t, err)
			got, err := fresh.replaceOpen(r.cwd)
			if err != nil || got != claimed {
				t.Fatalf("replaceOpen over a claimed record = %+v, %v; want %+v", got, err, claimed)
			}
			after, err := os.ReadFile(r.recordPath(old.ID))
			assignMust(t, err)
			if string(after) != string(before) {
				t.Fatalf("the claimed record was rewritten:\n%s", after)
			}
		})
		t.Run("open and stale", func(t *testing.T) {
			r, old, fresh := stale(t)
			got, err := fresh.replaceOpen(r.cwd)
			if err != nil || got != fresh || got == old {
				t.Fatalf("replaceOpen over a stale open record = %+v, %v; want %+v", got, err, fresh)
			}
			if rec := r.record(old.ID); rec != fresh {
				t.Fatalf("the file holds %+v, want %+v", rec, fresh)
			}
		})
		t.Run("refreshed by another writer", func(t *testing.T) {
			r, old, fresh := stale(t)
			assignMust(t, writeRecord(r.recordPath(old.ID), fresh))
			mine, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now().Add(time.Second))
			assignMust(t, err)
			got, err := mine.replaceOpen(r.cwd)
			if err != nil || got != fresh {
				t.Fatalf("replaceOpen over a record another writer refreshed = %+v, %v; want %+v", got, err, fresh)
			}
			if rec := r.record(old.ID); rec != fresh {
				t.Fatalf("the file holds %+v, want %+v", rec, fresh)
			}
		})
		t.Run("removed", func(t *testing.T) {
			r, old, fresh := stale(t)
			assignMust(t, os.Remove(r.recordPath(old.ID)))
			if got, err := fresh.replaceOpen(r.cwd); err == nil {
				t.Fatalf("replaceOpen brought a released record back: %+v", got)
			}
			if _, err := os.Lstat(r.recordPath(old.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("replaceOpen wrote a removed record: %v", err)
			}
		})
		t.Run("another contract", func(t *testing.T) {
			r, old, fresh := stale(t)
			other := old
			other.Op = "c_5cff7231980e/t1-a2"
			assignMust(t, writeRecord(r.recordPath(old.ID), other))
			if got, err := fresh.replaceOpen(r.cwd); err == nil {
				t.Fatalf("replaceOpen replaced a record of another op: %+v", got)
			}
			if rec := r.record(old.ID); rec != other {
				t.Fatalf("the file holds %+v, want %+v", rec, other)
			}
		})
	})

	// The claim decides on the record it holds under the lock. The record may be replaced between the judgement and the claim (the
	// tree recreated and the record placed again, replaceOpen): the claim then checks the receipt against the new contract as the
	// judgement would have, its tree, HEAD and CreatedAt included, so a receipt older than the record it is accepted against is
	// refused (CRW-1496).
	t.Run("ReplacedBetweenCheckAndClaim", func(t *testing.T) {
		body := "$ go test ./x\nok\n"
		race := func(t *testing.T, receiptAt func(fresh Assignment) time.Time) (*flowTestRig, Assignment, Assignment, AssignedVerdict) {
			t.Helper()
			r := newFlowTestRig(t)
			old := r.mint(flowTestOp, time.Now().Add(-2*time.Minute))
			receipt := FlowReceiptPath(old)
			flowWrite(t, receipt, body, time.Now().Add(-time.Minute))
			tr := r.transcript(flowSpawnText(old.Op, old.ID, receipt))
			var fresh Assignment
			beforeClaim = func() {
				beforeClaim = nil
				flowRecreateTree(t, r.tree)
				next, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
				assignMust(t, err)
				got, err := EnsureFlowAssignment(r.cwd, next)
				if err != nil || got.RootIno == old.RootIno || got != next {
					t.Fatalf("the record placed again after the recreate = %+v, %v; want %+v", got, err, next)
				}
				fresh = got
				flowWrite(t, FlowReceiptPath(fresh), body, receiptAt(fresh))
			}
			t.Cleanup(func() { beforeClaim = nil })
			v := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-1", tr, flowTestJSON, "")
			if beforeClaim != nil {
				t.Fatal("the claim ran without the record replaced before it")
			}
			return r, old, fresh, v
		}
		t.Run("older receipt refused", func(t *testing.T) {
			// the old receipt is put back in the new tree with its modification time kept: it predates the new record
			r, old, fresh, v := race(t, func(Assignment) time.Time { return time.Now().Add(-time.Minute) })
			if v != AssignedRefused {
				t.Fatalf("a receipt older than the record it is claimed against judged %v", v)
			}
			if rec := r.record(old.ID); rec != fresh {
				t.Fatalf("the record after the refused claim: %+v, want the new open record %+v", rec, fresh)
			}
			if got, ok := AcceptedReceipt(r.cwd, flowTestOwner, old.ID); ok {
				t.Fatalf("AcceptedReceipt = %+v after a refused claim", got)
			}
		})
		t.Run("receipt of the new contract accepted", func(t *testing.T) {
			r, old, fresh, v := race(t, func(fresh Assignment) time.Time {
				created, err := time.Parse(time.RFC3339Nano, fresh.CreatedAt)
				assignMust(t, err)
				return created.Add(time.Second)
			})
			if v != AssignedAccepted {
				t.Fatalf("a receipt of the record held at the claim judged %v", v)
			}
			rec := r.record(old.ID)
			if rec.Status != AssignmentClaimed || rec.RootIno != fresh.RootIno || rec.CreatedAt != fresh.CreatedAt || rec.ReceiptDigest != flowDigest(body) {
				t.Fatalf("the claimed record %+v, want the new contract %+v claimed", rec, fresh)
			}
			if got, ok := AcceptedReceipt(r.cwd, flowTestOwner, old.ID); !ok || string(got.Bytes) != body {
				t.Fatalf("AcceptedReceipt = %+v, %v", got, ok)
			}
		})
		t.Run("accepted receipt older than its record", func(t *testing.T) {
			// AcceptedReceipt reads the receipt with the claim's rule: a file older than the record's CreatedAt is no accepted receipt,
			// whatever digest it has
			r := newFlowTestRig(t)
			a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
			flowAcceptIn(t, r, a, "agent-1")
			flowWrite(t, FlowReceiptPath(a), "$ go test ./x\nok\n", time.Now().Add(-time.Hour))
			if got, ok := AcceptedReceipt(r.cwd, flowTestOwner, a.ID); ok {
				t.Fatalf("AcceptedReceipt took a receipt older than its record: %+v", got)
			}
		})
	})

	// A git that fails is no answer (CRW-1496): neither the HEAD of a new record nor the ancestry check of a recorded one is read from
	// it. NewFlowAssignment, EnsureFlowAssignment and replaceOpen return the error, so the caller holds the call, and no record loses
	// its recorded HEAD.
	t.Run("GitFailureIsAnError", func(t *testing.T) {
		setup := func(t *testing.T) (*flowTestRig, Assignment, []byte) {
			t.Helper()
			r := newFlowTestRig(t)
			flowGitRepo(t, r.tree)
			flowGitCommit(t, r.tree, "first")
			old := r.mint(flowTestOp, time.Now().Add(-time.Minute))
			if old.Head == "" {
				t.Fatal("the minted record has no head")
			}
			raw, err := os.ReadFile(r.recordPath(old.ID))
			assignMust(t, err)
			return r, old, raw
		}
		// failGit makes the next n git runs of the package fail as a git that timed out (n < 0: every run), and counts them.
		failGit := func(t *testing.T, n int) *int {
			t.Helper()
			failed := new(int)
			real := probeGit
			probeGit = func(dir string, o source.ProbeOptions, args ...string) ([]byte, error) {
				if n < 0 || *failed < n {
					*failed++
					return nil, errors.New("git timed out")
				}
				return real(dir, o, args...)
			}
			t.Cleanup(func() { probeGit = real })
			return failed
		}
		t.Run("new record", func(t *testing.T) {
			r, old, _ := setup(t)
			failGit(t, 1)
			if a, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now()); err == nil {
				t.Fatalf("NewFlowAssignment with git failing = %+v, want an error (never an empty head)", a)
			}
		})
		t.Run("ensure fails once", func(t *testing.T) {
			r, old, raw := setup(t)
			fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
			assignMust(t, err)
			failed := failGit(t, 1)
			if got, err := EnsureFlowAssignment(r.cwd, fresh); err == nil {
				t.Fatalf("EnsureFlowAssignment with git failing once = %+v, want the error", got)
			}
			if *failed != 1 {
				t.Fatalf("%d git runs failed, want 1", *failed)
			}
			r.unchanged(t, old.ID, raw)
			// git answers again: the record is the contract still, and is kept
			if got, err := EnsureFlowAssignment(r.cwd, fresh); err != nil || got != old {
				t.Fatalf("EnsureFlowAssignment once git answers = %+v, %v; want %+v", got, err, old)
			}
			r.unchanged(t, old.ID, raw)
		})
		t.Run("ensure during an outage", func(t *testing.T) {
			// the record NewFlowAssignment made before the fix while git failed: no head
			r, old, raw := setup(t)
			headless := old
			headless.Head, headless.CreatedAt = "", time.Now().UTC().Format(time.RFC3339Nano)
			failGit(t, -1)
			if got, err := EnsureFlowAssignment(r.cwd, headless); err == nil {
				t.Fatalf("EnsureFlowAssignment with git failing = %+v, want the error", got)
			}
			r.unchanged(t, old.ID, raw)
			if _, err := headless.replaceOpen(r.cwd); err == nil {
				t.Fatal("replaceOpen with git failing replaced the record")
			}
			r.unchanged(t, old.ID, raw)
		})
		t.Run("no head over a head", func(t *testing.T) {
			// the tree is no checkout any more (same inode): git answers that it has no HEAD, the record no longer matches, and its
			// replacement would carry no head, so the record is not replaced
			r, old, raw := setup(t)
			assignMust(t, os.RemoveAll(filepath.Join(r.tree, ".git")))
			fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
			assignMust(t, err)
			if fresh.Head != "" {
				t.Fatalf("the head of a tree that is no checkout: %q", fresh.Head)
			}
			if got, err := EnsureFlowAssignment(r.cwd, fresh); err == nil {
				t.Fatalf("EnsureFlowAssignment replaced a record with a head by one without: %+v", got)
			}
			r.unchanged(t, old.ID, raw)
		})
	})

	// A commit whose object is there but cannot be read (its bytes hash to another id) is no answer either (CRW-1561): git reads it
	// as no commit, but it is neither the commit a rewritten history no longer has nor the HEAD of a repository with no commit yet.
	// Neither the HEAD of a new record nor the ancestry of a recorded one is read from it.
	t.Run("CorruptRecordedHeadIsAnError", func(t *testing.T) {
		r := newFlowTestRig(t)
		flowGitRepo(t, r.tree)
		first := flowGitCommit(t, r.tree, "first")
		old := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		if old.Head != first {
			t.Fatalf("the minted record's head %q, want %q", old.Head, first)
		}
		raw, err := os.ReadFile(r.recordPath(old.ID))
		assignMust(t, err)
		second := flowGitCommit(t, r.tree, "second")
		flowCorruptObject(t, r.tree, first, second)
		fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
		assignMust(t, err)
		if fresh.Head != second {
			t.Fatalf("the new record's head %q, want %q", fresh.Head, second)
		}
		if got, err := EnsureFlowAssignment(r.cwd, fresh); err == nil {
			t.Fatalf("EnsureFlowAssignment over a recorded head that cannot be read = %+v, want the error", got)
		}
		r.unchanged(t, old.ID, raw)
		if rec := r.record(old.ID); rec.Head != first {
			t.Fatalf("the record's head %q, want %q", rec.Head, first)
		}
	})

	t.Run("CorruptHeadIsAnError", func(t *testing.T) {
		t.Run("new record", func(t *testing.T) {
			r := newFlowTestRig(t)
			flowGitRepo(t, r.tree)
			first := flowGitCommit(t, r.tree, "first")
			second := flowGitCommit(t, r.tree, "second")
			flowCorruptObject(t, r.tree, second, first)
			id := FlowAssignmentID(flowTestOwner, flowTestRun, flowTestCall, flowTestOp)
			if a, err := NewFlowAssignment(flowTestOwner, id, r.tree, flowTestOp, time.Now()); err == nil {
				got, eerr := EnsureFlowAssignment(r.cwd, a)
				t.Fatalf("NewFlowAssignment at a HEAD that cannot be read = %+v (EnsureFlowAssignment then: %+v, %v), want the error", a, got, eerr)
			}
			if _, err := os.Lstat(r.recordPath(id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a record was written for a HEAD that cannot be read (%v)", err)
			}
		})
		t.Run("recorded contract", func(t *testing.T) {
			r := newFlowTestRig(t)
			flowGitRepo(t, r.tree)
			flowGitCommit(t, r.tree, "first")
			old := r.mint(flowTestOp, time.Now().Add(-time.Minute))
			raw, err := os.ReadFile(r.recordPath(old.ID))
			assignMust(t, err)
			second := flowGitCommit(t, r.tree, "second")
			flowCorruptObject(t, r.tree, second, old.Head)
			if got, err := EnsureFlowAssignment(r.cwd, old); err == nil {
				t.Fatalf("EnsureFlowAssignment at a HEAD that cannot be read = %+v, want the error", got)
			}
			r.unchanged(t, old.ID, raw)
		})
	})

	// The two no-commit answers that stay answers: a recorded head this history does not have (rewritten, its object gone) no longer
	// matches and the open record is replaced, and a repository with no commit yet records no head.
	t.Run("AbsentRecordedHeadReplacesOpenRecord", func(t *testing.T) {
		r := newFlowTestRig(t)
		flowGitRepo(t, r.tree)
		first := flowGitCommit(t, r.tree, "first")
		old := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		branch := flowGit(t, r.tree, "symbolic-ref", "-q", "HEAD")
		flowGit(t, r.tree, "checkout", "-q", "--orphan", "rewritten")
		rewritten := flowGitCommit(t, r.tree, "rewritten")
		flowGit(t, r.tree, "update-ref", "-d", branch)
		assignMust(t, os.Remove(flowObjectPath(r.tree, first)))
		fresh, err := NewFlowAssignment(flowTestOwner, old.ID, r.tree, flowTestOp, time.Now())
		assignMust(t, err)
		got, err := EnsureFlowAssignment(r.cwd, fresh)
		if err != nil {
			t.Fatalf("Ensure after the history was rewritten: %v", err)
		}
		if got.Head != rewritten || got.CreatedAt != fresh.CreatedAt {
			t.Fatalf("Ensure after the history was rewritten returned %+v; want head %s", got, rewritten)
		}
		if rec := r.record(old.ID); rec != got {
			t.Fatalf("the file holds %+v, Ensure returned %+v", rec, got)
		}
	})

	t.Run("UnbornHeadRecordsNoHead", func(t *testing.T) {
		r := newFlowTestRig(t)
		flowGitRepo(t, r.tree)
		a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
		if a.Head != "" {
			t.Fatalf("the head of a repository with no commit: %q", a.Head)
		}
		if got, err := EnsureFlowAssignment(r.cwd, a); err != nil || got != a {
			t.Fatalf("Ensure of the minted record = %+v, %v; want %+v", got, err, a)
		}
	})

	t.Run("ReceiptIgnored", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("no git")
		}
		repo := func(t *testing.T, ignore string) string {
			dir := flowTestTree(t)
			cmd := exec.Command("git", "init", "-q", dir)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			if ignore != "" {
				flowWrite(t, filepath.Join(dir, ".gitignore"), ignore, time.Time{})
			}
			return dir
		}
		const id = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		at := func(root string) Assignment { return Assignment{Root: root, ID: id} }
		// the receipt's own name is asked: a rule for another name in .crw/evidence decides nothing for it
		if ignored, err := ReceiptIgnored(at(repo(t, ".crw/evidence/receipt.md\n"))); err != nil || ignored {
			t.Fatalf("a rule that matches only another name: %v, %v", ignored, err)
		}
		if ignored, err := ReceiptIgnored(at(repo(t, ".crw/evidence/*.md\n!.crw/evidence/receipt.md\n"))); err != nil || !ignored {
			t.Fatalf("a rule that matches the receipt's own name: %v, %v", ignored, err)
		}
		if ignored, err := ReceiptIgnored(at(repo(t, "bin/\n.crw/\n"))); err != nil || !ignored {
			t.Fatalf("a repo that ignores .crw/: %v, %v", ignored, err)
		}
		if ignored, err := ReceiptIgnored(at(repo(t, "bin/\n"))); err != nil || ignored {
			t.Fatalf("a repo that does not ignore .crw/: %v, %v", ignored, err)
		}
		if ignored, err := ReceiptIgnored(at(repo(t, ""))); err != nil || ignored {
			t.Fatalf("a repo without .gitignore: %v, %v", ignored, err)
		}
		if ignored, err := ReceiptIgnored(at(flowTestTree(t))); err == nil {
			t.Fatalf("outside git: %v without an error", ignored)
		}
	})

	t.Run("NativeUnchanged", func(t *testing.T) {
		// A native record keeps the version 1 bytes: no flow key appears.
		n := newAssignTestRig(t)
		a := n.assign(time.Now().Add(-time.Minute))
		raw, err := os.ReadFile(n.recordPath(a.ID))
		assignMust(t, err)
		var keys map[string]json.RawMessage
		assignMust(t, json.Unmarshal(raw, &keys))
		want := []string{"agentId", "createdAt", "head", "id", "mode", "root", "rootDev", "rootIno", "sessionId", "status", "turnId", "version"}
		var got []string
		for k := range keys {
			got = append(got, k)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) || string(keys["version"]) != "1" {
			t.Fatalf("the native record's keys %v (version %s), want %v (version 1)", got, keys["version"], want)
		}
		// A native contract still needs an explicit receipt, and its claim records no turn and no digest.
		receipt := n.receipt("check.txt", time.Time{})
		packet := n.transcript(rolloutMeta("child"), rolloutUserItem("child", assignBlock(a.ID)))
		if _, ok := FlowContractOf(n.cwd, assignTestSession, "w1", packet, ""); ok {
			t.Fatal("a native contract is a flow contract")
		}
		if got := JudgeAssignedReceiptTurn(n.cwd, assignTestSession, "w1", "turn-1", packet, "done", ""); got != AssignedRefused {
			t.Fatalf("a native contract without a receipt judged %v", got)
		}
		if got := JudgeAssignedReceiptTurn(n.cwd, assignTestSession, "w1", "turn-1", packet, "EVIDENCE_RECORDED: "+receipt, receipt); got != AssignedAccepted {
			t.Fatalf("the native receipt judged %v", got)
		}
		rec, ok := readAssignment(n.recordPath(a.ID), a.ID)
		claimed := a
		claimed.Status, claimed.AgentID = AssignmentClaimed, "w1"
		if !ok || rec != claimed {
			t.Fatalf("the native claim wrote %+v, want %+v", rec, claimed)
		}
		after, err := os.ReadFile(n.recordPath(a.ID))
		assignMust(t, err)
		if strings.Contains(string(after), "origin") || strings.Contains(string(after), "receiptDigest") || strings.Contains(string(after), `"op"`) {
			t.Fatalf("the native claim wrote flow keys: %s", after)
		}
		// A child without a contract is still NoContract, with or without a turn.
		if got := JudgeAssignedReceiptTurn(t.TempDir(), assignTestSession, "w1", "turn-1", "", "", ""); got != NoContract {
			t.Fatalf("no contract judged %v", got)
		}
		// The generic directives are the oracle's texts, and the flow directive is pinned by its own golden entries.
		golden := loadDirectives(t)
		for i, want := range golden.Verifier {
			if got := VerifierDirective(i + 1); got != want {
				t.Errorf("VerifierDirective(%d) changed: %q", i+1, got)
			}
		}
		var flow struct {
			VerifierFor struct {
				Receipt string
				Texts   []string
			}
		}
		raw, err = os.ReadFile("testdata/directives.golden.json")
		assignMust(t, err)
		assignMust(t, json.Unmarshal(raw, &flow))
		if flow.VerifierFor.Receipt == "" || len(flow.VerifierFor.Texts) != MaxAttempts {
			t.Fatalf("the golden has no flow directive entries: %+v", flow.VerifierFor)
		}
		for i, want := range flow.VerifierFor.Texts {
			got := VerifierDirectiveFor(i+1, flow.VerifierFor.Receipt)
			if got != want {
				t.Errorf("VerifierDirectiveFor(%d) = %q, want %q", i+1, got, want)
			}
			if !strings.Contains(got, flow.VerifierFor.Receipt) || got == VerifierDirective(i+1) {
				t.Errorf("VerifierDirectiveFor(%d) does not name the receipt file: %q", i+1, got)
			}
		}
	})
}

// flowRecreateTree removes the tree at path and makes it again there with a new inode, as a worktree that was removed and
// established again: the new directory is made before the old one goes, so the two can never share an inode.
func flowRecreateTree(t *testing.T, tree string) {
	t.Helper()
	_, _, oldIno, err := treeIdentity(tree)
	assignMust(t, err)
	fresh := tree + ".recreated"
	assignMust(t, os.MkdirAll(filepath.Join(fresh, ".crw", "evidence"), 0o755))
	assignMust(t, os.RemoveAll(tree))
	assignMust(t, os.Rename(fresh, tree))
	if _, _, ino, err := treeIdentity(tree); err != nil || ino == oldIno {
		t.Fatalf("the recreated tree has inode %d (%v), the old one %d", ino, err, oldIno)
	}
}

// flowAcceptIn writes a receipt at the fixed file of a, in the tree as it is now, and checks that agent's claim of it is accepted
// and is the accepted receipt of the record.
func flowAcceptIn(t *testing.T, r *flowTestRig, a Assignment, agent string) {
	t.Helper()
	receipt := FlowReceiptPath(a)
	body := "$ go test ./x\nok\n"
	flowWrite(t, receipt, body, time.Time{})
	tr := r.transcript(flowSpawnText(a.Op, a.ID, receipt))
	if v := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, agent, "turn-1", tr, flowTestJSON, ""); v != AssignedAccepted {
		t.Fatalf("the receipt %s judged %v", receipt, v)
	}
	if got, ok := AcceptedReceipt(r.cwd, flowTestOwner, a.ID); !ok || got.Path != receipt || string(got.Bytes) != body {
		t.Fatalf("AcceptedReceipt = %+v, %v", got, ok)
	}
}

func flowGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// flowGitRepo makes dir a git checkout, or skips the test when there is no git.
func flowGitRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	flowGit(t, dir, "init", "-q")
}

// flowObjectPath is the loose object file of id in the checkout dir.
func flowObjectPath(dir, id string) string {
	return filepath.Join(dir, ".git", "objects", id[:2], id[2:])
}

// flowCorruptObject overwrites the loose object of id in dir with the bytes of donor's: the file is there and inflates to a commit,
// but one whose bytes hash to donor, so git finds id and cannot read it as a commit.
func flowCorruptObject(t *testing.T, dir, id, donor string) {
	t.Helper()
	body, err := os.ReadFile(flowObjectPath(dir, donor))
	assignMust(t, err)
	path := flowObjectPath(dir, id)
	assignMust(t, os.Chmod(path, 0o644)) // loose objects are read-only
	assignMust(t, os.WriteFile(path, body, 0o444))
}

// flowGitCommit records an empty commit in dir and returns its id.
func flowGitCommit(t *testing.T, dir, msg string) string {
	t.Helper()
	flowGit(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return flowGit(t, dir, "rev-parse", "HEAD")
}

// TestFlowClaimRechecksHeadAfterJudgement (CRW-1512): a record that changed after the receipt's
// judgement — a HEAD moved to a non-ancestor — is refused at the claim: the locked re-check judges
// the record it actually writes, not the copy the judgement read.
func TestFlowClaimRechecksHeadAfterJudgement(t *testing.T) {
	r := newFlowTestRig(t)
	flowGitRepo(t, r.tree)
	head := flowGitCommit(t, r.tree, "current history")
	tree := flowGit(t, r.tree, "rev-parse", "HEAD^{tree}")
	unrelated := flowGit(t, r.tree, "commit-tree", tree, "-m", "unrelated history") // no -p: another root
	cmd := exec.Command("git", "-C", r.tree, "merge-base", "--is-ancestor", unrelated, head)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("fixture head must be a real non-ancestor: %v", err)
	}
	a := r.mint(flowTestOp, time.Now().Add(-time.Minute))
	if a.Head != head {
		t.Fatalf("recorded HEAD %q, want %q", a.Head, head)
	}
	receipt := FlowReceiptPath(a)
	flowWrite(t, receipt, "$ go test ./x\nok\n", time.Now())
	tr := r.transcript(flowSpawnText(a.Op, a.ID, receipt))
	changed := a
	changed.Head = unrelated
	var raw []byte
	reached := false
	beforeClaim = func() {
		beforeClaim = nil
		reached = true
		assignMust(t, writeRecord(r.recordPath(a.ID), changed))
		var err error
		raw, err = os.ReadFile(r.recordPath(a.ID))
		assignMust(t, err)
	}
	t.Cleanup(func() { beforeClaim = nil })
	verdict := JudgeAssignedReceiptTurn(r.cwd, flowTestOwner, "agent-1", "turn-1", tr, flowTestJSON, "")
	if !reached {
		t.Fatal("initial valid judgement never reached the pre-claim seam")
	}
	if verdict != AssignedRefused {
		t.Errorf("claim of non-ancestor HEAD judged %v, want refused", verdict)
	}
	r.unchanged(t, a.ID, raw)
	if _, ok := AcceptedReceipt(r.cwd, flowTestOwner, a.ID); ok {
		t.Error("refused claim has an accepted receipt")
	}
}
