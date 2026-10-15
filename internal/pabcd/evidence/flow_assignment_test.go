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
		if ignored, err := ReceiptIgnored(repo(t, "bin/\n.crw/\n")); err != nil || !ignored {
			t.Fatalf("a repo that ignores .crw/: %v, %v", ignored, err)
		}
		if ignored, err := ReceiptIgnored(repo(t, "bin/\n")); err != nil || ignored {
			t.Fatalf("a repo that does not ignore .crw/: %v, %v", ignored, err)
		}
		if ignored, err := ReceiptIgnored(repo(t, "")); err != nil || ignored {
			t.Fatalf("a repo without .gitignore: %v, %v", ignored, err)
		}
		if ignored, err := ReceiptIgnored(flowTestTree(t)); err == nil {
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
