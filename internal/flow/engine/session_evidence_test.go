package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
)

// CRW-1482 (1209 field trial defect C9): the engine records a flow evidence assignment for every executor step it publishes, carries
// it to the child in the spawn message and the packet, records the receipt the stop hook accepted on the completed row, and removes
// an unclaimed record where its attempt ends. Each case runs the session kit with SessionConfig.EvidenceCwd set and real directories;
// the kit without EvidenceCwd keeps today's messages and rows (Unconfigured).

const evidenceExe = `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`

type evidenceKit struct {
	*sessionKit
	evCwd string
}

// newEvidenceKit is a session kit whose run records flow evidence assignments under a temporary EvidenceCwd, whose directories are
// real (git checkouts with git, whose .gitignore holds ignore), and whose subagents' host cwd is EvidenceCwd.
func newEvidenceKit(t *testing.T, git bool, ignore string) *evidenceKit {
	t.Helper()
	k := newSessionKit(t)
	ev := t.TempDir()
	k.cfg.EvidenceCwd = ev
	k.placer.mu.Lock()
	k.placer.real, k.placer.realGit, k.placer.realIgnore, k.placer.hostCwd = t.TempDir(), git, ignore, ev
	k.placer.mu.Unlock()
	return &evidenceKit{sessionKit: k, evCwd: ev}
}

func (k *evidenceKit) setHostCwd(cwd string) {
	k.placer.mu.Lock()
	k.placer.hostCwd = cwd
	k.placer.mu.Unlock()
}

// evidenceRecords are the assignment records under cwd, of any session.
func evidenceRecords(t *testing.T, cwd string) []string {
	t.Helper()
	got, err := filepath.Glob(filepath.Join(cwd, ".crw", evidence.AssignmentsSubdir, "*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

var assignmentBlock = regexp.MustCompile(`\[CRW-EVIDENCE-ASSIGNMENT:([A-Z2-7]+)\]`)

// messageAssignment is the id of the assignment block of a message, "" when it has none.
func messageAssignment(message string) string {
	m := assignmentBlock.FindStringSubmatch(message)
	if m == nil {
		return ""
	}
	return m[1]
}

// plainSpawnMessage is the spawn message of a step as the engine wrote it before CRW-1482.
func plainSpawnMessage(st Step) string {
	return "crw-flow-op: " + st.Op + "\nCRW-ROLE: " + st.Role + "\n\nYour shell starts in a directory that is NOT your workspace. Your workspace is " + st.Workspace +
		": run every command with workdir " + st.Workspace + " and edit only by absolute paths under " + st.Workspace + ".\nRead the task packet " + st.Packet +
		" in full and follow it exactly. You are a leaf subagent: never spawn agents.\nYour final message is exactly the JSON object the packet names, and nothing else."
}

// writeRollout writes the rollout of a subagent whose packet (first UserMessage) is message.
func writeRollout(t *testing.T, agent, message string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), agent+".jsonl")
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": agent}})
	item, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": agent,
		"item": map[string]any{"type": "UserMessage", "content": []any{map[string]any{"type": "text", "text": message}}}}})
	if err := os.WriteFile(path, append(append(append(meta, '\n'), item...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// finalText is the answer a subagent gives to a step.
func finalText(st Step) string {
	packet, _ := os.ReadFile(st.Packet)
	return defaultMessage(st.Message, packetSchema(string(packet)))
}

// claim is the stop hook's judgement of a subagent's stop: the receipt is written at the file its record names under cwd and
// JudgeAssignedReceiptTurn judges it for claimer (the subagent itself, or another agent) and turn.
func claim(t *testing.T, st Step, cwd, claimer, turn string, receipt []byte) (evidence.AssignedVerdict, string) {
	t.Helper()
	id := messageAssignment(st.Message)
	rec, ok := evidence.RecordedAssignment(cwd, sessionOwner, id)
	if !ok {
		t.Errorf("no record %s under %s", id, cwd)
		return evidence.AssignedRefused, ""
	}
	path := evidence.FlowReceiptPath(rec)
	if err := os.WriteFile(path, receipt, 0o600); err != nil {
		t.Errorf("receipt: %v", err)
		return evidence.AssignedRefused, ""
	}
	rollout := writeRollout(t, claimer, st.Message)
	return evidence.JudgeAssignedReceiptTurn(cwd, sessionOwner, claimer, turn, rollout, finalText(st), ""), path
}

// complete ends the hanging turn of agent with the step's answer.
func (k *evidenceKit) complete(agent string, st Step) {
	text := finalText(st)
	k.host.set(agent, func(a *fakeAgent, t *SessionTurn) {
		t.Status, t.FinalText, t.FinalPhase, t.UpdatedAt = "completed", text, "final_answer", time.Now()
		a.Status = "idle"
	})
}

// waitRecord waits until the record of the step's assignment is under cwd.
func waitRecord(t *testing.T, st Step, cwd string) bool {
	id := messageAssignment(st.Message)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := evidence.RecordedAssignment(cwd, sessionOwner, id); ok {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("the record %s never appeared under %s", id, cwd)
	return false
}

// receiptOf is the evidence_receipt of a completed row.
func receiptOf(t *testing.T, row Event) map[string]any {
	t.Helper()
	r, ok := row.Data["evidence_receipt"].(map[string]any)
	if !ok {
		t.Fatalf("the %s row has no evidence_receipt: %v", row.Event, row.Data)
	}
	return r
}

// freezeRecords makes the record directories under cwd read only: the process that died removes nothing of them.
func freezeRecords(t *testing.T, cwd string, frozen bool) {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(cwd, ".crw", evidence.AssignmentsSubdir, "*"))
	mode := os.FileMode(0o755)
	if frozen {
		mode = 0o555
	}
	for _, d := range dirs {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
	}
	if frozen {
		t.Cleanup(func() { freezeRecords(t, cwd, false) })
	}
}

// die runs body in a process that dies after the row stop matches (its record directories frozen at that moment), and readies
// the kit for the process that resumes the run over the same journal.
func (k *evidenceKit) die(t *testing.T, stop func(Event) bool, body string) *MemoryJournal {
	t.Helper()
	sw := &deadSwitch{}
	mem := NewMemoryJournal()
	k.h.cfg.Journal = &dyingJournal{Journal: mem, sw: sw, stop: func(e Event) bool {
		if stop(e) {
			freezeRecords(t, k.evCwd, true)
			return true
		}
		return false
	}}
	k.jr = mem
	k.cfg.Bus = &dyingBus{FileStepBus: k.bus, sw: sw}
	k.cfg.Host = &dyingHost{fakeSessionHost: k.host, sw: sw}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx1, cancel1 := context.WithCancel(ctx)
	sw.mu.Lock()
	sw.cancel = cancel1
	sw.mu.Unlock()
	if _, err := Run(ctx1, k.h.load(body), k.h.cfg); err != nil {
		t.Fatal(err)
	}
	if !sw.isDead() {
		t.Fatalf("the process never died")
	}
	freezeRecords(t, k.evCwd, false)
	k.h.cfg.Journal = mem
	k.cfg.Bus, k.cfg.Host = k.bus, k.host
	k.h.cfg.Resume = &ResumeOptions{}
	k.h.cfg.SpentBefore = RunSpent(mem.Events(), k.h.cfg.RunID).Total
	return mem
}

// setOrch swaps the fake owner's hooks under its lock.
func (k *evidenceKit) setOrch(answer func(st Step, packet string, n int) orchAnswer, onSpawn func(st Step, agent string)) {
	k.orch.mu.Lock()
	k.orch.answer, k.orch.onSpawn = answer, onSpawn
	k.orch.mu.Unlock()
}

func TestSessionEvidence(t *testing.T) {
	// AssignmentPublished: the step of an executor call carries the record the engine wrote under EvidenceCwd before its
	// step_published row: keyed by the owner, rooted at P with the step's op, the receipt directory made in P, and the spawn message
	// today's text with the block and the file appended. The rows are those of before.
	t.Run("AssignmentPublished", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		var mu sync.Mutex
		var rec evidence.Assignment
		var found, dirMade bool
		k.setOrch(nil, func(st Step, _ string) {
			mu.Lock()
			defer mu.Unlock()
			rec, found = evidence.RecordedAssignment(k.evCwd, sessionOwner, messageAssignment(st.Message))
			info, err := os.Lstat(filepath.Join(st.Workspace, ".crw", "evidence"))
			dirMade = err == nil && info.IsDir()
		})
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("result %v", got)
		}
		rows := k.rowsOf("exe")
		if got := eventsOf(rows); got != oneAttempt {
			t.Fatalf("rows %s", got)
		}
		step := rows[1]
		marker := rowStr(step.Data, "marker")
		id := evidence.FlowAssignmentID(sessionOwner, k.h.cfg.RunID, rows[0].CallID, marker)
		if rowStr(step.Data, "evidence_assignment") != id || step.Data["evidence_ignored"] != true {
			t.Errorf("step_published %v, want evidence_assignment %s and evidence_ignored true", step.Data, id)
		}
		spawns := k.orch.spawned()
		if len(spawns) != 1 {
			t.Fatalf("spawns %v", spawns)
		}
		st := spawns[0]
		mu.Lock()
		defer mu.Unlock()
		if !found {
			t.Fatalf("no record %s under the evidence cwd when the step was taken", id)
		}
		real, _ := filepath.EvalSymlinks(st.Workspace)
		if rec.ID != id || rec.SessionID != sessionOwner || rec.Root != real || rec.Op != marker || rec.Origin != evidence.AssignmentOriginFlow || rec.Status != evidence.AssignmentOpen {
			t.Errorf("record %+v (P %s, op %s)", rec, real, marker)
		}
		if !dirMade {
			t.Errorf("P has no real .crw/evidence directory")
		}
		receipt := evidence.FlowReceiptPath(rec)
		if !strings.HasPrefix(st.Message, plainSpawnMessage(st)+"\n\n") || messageAssignment(st.Message) != id || !strings.Contains(st.Message, receipt) {
			t.Errorf("spawn message:\n%s\nwant today's text, then the block of %s and the file %s", st.Message, id, receipt)
		}
		packet, _ := os.ReadFile(st.Packet)
		if !strings.Contains(string(packet), "## Evidence") || !strings.Contains(string(packet), receipt) {
			t.Errorf("the packet names no receipt file:\n%s", packet)
		}
		// the receipt was never written: the completed row says so, and the unclaimed record is gone with the call
		if r := receiptOf(t, rows[4]); r["state"] != "missing" || r["assignment"] != id {
			t.Errorf("evidence_receipt %v", r)
		}
		if got := evidenceRecords(t, k.evCwd); len(got) != 0 {
			t.Errorf("records left after the call ended: %v", got)
		}
	})

	// RecoveredStepReusesAssignment: a step a dead process published is published again with the record it has (one record, its
	// creation time kept), and an adopted subagent goes on with the record, open or already claimed before the restart.
	t.Run("RecoveredStepReusesAssignment", func(t *testing.T) {
		t.Run("republished", func(t *testing.T) {
			k := newEvidenceKit(t, true, ".crw/\n")
			var before evidence.Assignment
			mem := k.die(t, func(e Event) bool {
				if e.Kind == KindCall && e.Event == EvStepPublished {
					before, _ = evidence.RecordedAssignment(k.evCwd, sessionOwner, rowStr(e.Data, "evidence_assignment"))
					return true
				}
				return false
			}, evidenceExe)
			_ = mem
			if before.ID == "" {
				t.Fatalf("no record when the step_published row was written")
			}
			var mu sync.Mutex
			var records []string
			var during evidence.Assignment
			k.setOrch(nil, func(st Step, _ string) {
				mu.Lock()
				defer mu.Unlock()
				records = evidenceRecords(t, k.evCwd)
				during, _ = evidence.RecordedAssignment(k.evCwd, sessionOwner, messageAssignment(st.Message))
			})
			res := k.h.run(evidenceExe)
			if got := value[[]any](t, res); got[0] != "completed" {
				t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			mu.Lock()
			defer mu.Unlock()
			if len(records) != 1 || during.ID != before.ID || during.CreatedAt != before.CreatedAt {
				t.Errorf("records %v, the step's %+v, want the one record %+v", records, during, before)
			}
			if spawns := k.orch.spawned(); len(spawns) != 1 || messageAssignment(spawns[0].Message) != before.ID {
				t.Errorf("spawns %v", spawns)
			}
		})
		for _, claimed := range []bool{false, true} {
			name := "adopted open"
			if claimed {
				name = "adopted claimed"
			}
			t.Run(name, func(t *testing.T) {
				k := newEvidenceKit(t, true, ".crw/\n")
				// the claim is made before the subagent is on the host (the answer hook runs first): a marker search that binds it before the
				// report would otherwise end the process, and freeze its records, before the claim
				k.setOrch(func(st Step, _ string, _ int) orchAnswer {
					if claimed {
						if v, _ := claim(t, st, k.evCwd, "agent-1", "agent-1-turn1", []byte("red then green\n")); v != evidence.AssignedAccepted {
							t.Errorf("the claim before the restart: %v", v)
						}
					}
					return orchAnswer{after: 30 * time.Millisecond}
				}, nil)
				k.die(t, deathAt(EvThreadStarted), evidenceExe)
				k.setOrch(nil, nil)
				res := k.h.run(evidenceExe)
				if got := value[[]any](t, res); got[0] != "completed" {
					t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
				}
				rows := k.rowsOf("exe")
				if got := eventsOf(rows); got != oneAttempt {
					t.Fatalf("rows %s", got)
				}
				if n := len(k.orch.spawned()); n != 1 {
					t.Errorf("%d spawns: the adopted subagent was spawned again", n)
				}
				r := receiptOf(t, rows[4])
				id := rowStr(rows[1].Data, "evidence_assignment")
				want := "missing"
				if claimed {
					want = "accepted"
				}
				if r["state"] != want || r["assignment"] != id {
					t.Errorf("evidence_receipt %v, want %s of %s", r, want, id)
				}
				if claimed && (r["agent_id"] != "agent-1" || r["turn_match"] != true) {
					t.Errorf("evidence_receipt %v", r)
				}
			})
		}
	})

	// QACaptureMints: the QA capture call is an executor call too: it gets its record in its scratch worktree, and its completed row
	// records the receipt although it writes no node worktree.
	t.Run("QACaptureMints", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, func(st Step, agent string) {
			go func() {
				if v, _ := claim(t, st, k.evCwd, agent, agent+"-turn1", []byte("captured\n")); v != evidence.AssignedAccepted {
					t.Errorf("claim %v", v)
				}
				k.complete(agent, st)
			}()
		})
		res := k.h.run(`const qa = await agent.call('capture the surface', { agentType: 'executor', label: 'qa', qa: true, head: 'h-gate', schema: RECEIPT })
return [qa.status]`)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("result %v", got)
		}
		rows := k.rowsOf("qa")
		if got := eventsOf(rows); got != oneAttempt {
			t.Fatalf("rows %s", got)
		}
		st := k.orch.spawned()[0]
		if !strings.Contains(st.Workspace, "/w/call/") || messageAssignment(st.Message) == "" {
			t.Errorf("the QA step %+v carries no assignment in its scratch worktree", st)
		}
		if r := receiptOf(t, rows[4]); r["state"] != "accepted" {
			t.Errorf("evidence_receipt %v", r)
		}
	})

	// RepairPacketNamesSameFile: the repair a resumed process sends to the same subagent names the receipt file of the spawn.
	t.Run("RepairPacketNamesSameFile", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		k.setOrch(func(st Step, _ string, _ int) orchAnswer {
			if st.K == 1 {
				return orchAnswer{text: "this is not JSON"}
			}
			return orchAnswer{}
		}, nil)
		k.die(t, deathAtK(EvStepPublished, 2), evidenceExe)
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
		}
		rows := k.rowsOf("exe")
		steps := rowsNamed(rows, EvStepPublished)
		if len(steps) != 2 || rowStr(steps[1].Data, "action") != StepSendInput {
			t.Fatalf("rows %s", eventsOf(rows))
		}
		spawn := k.orch.spawned()[0]
		file := messageAssignment(spawn.Message) + ".md"
		in := k.orch.sentInput()
		if len(in) != 1 {
			t.Fatalf("inputs %v", in)
		}
		packet, _ := os.ReadFile(in[0].Packet)
		if !strings.Contains(string(packet), "## Evidence") || !strings.Contains(string(packet), filepath.Join(".crw", "evidence", file)) {
			t.Errorf("the repair packet does not name the spawn's receipt %s:\n%s", file, packet)
		}
		if _, ok := steps[1].Data["evidence_assignment"]; ok {
			t.Errorf("the send_input step recorded an assignment of its own: %v", steps[1].Data)
		}
	})

	// HostCwdDiffers: the host runs the subagent in another cwd than the one the run was started in: the record is written there at
	// the bind too, thread_started says where, and the receipt the hook accepts there is the one the completed row records.
	t.Run("HostCwdDiffers", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		host := t.TempDir()
		k.setHostCwd(host)
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, func(st Step, agent string) {
			go func() {
				if !waitRecord(t, st, host) {
					return
				}
				if v, _ := claim(t, st, host, agent, agent+"-turn1", []byte("red then green\n")); v != evidence.AssignedAccepted {
					t.Errorf("claim under the host cwd: %v", v)
				}
				k.complete(agent, st)
			}()
		})
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("result %v", got)
		}
		rows := k.rowsOf("exe")
		if got := eventsOf(rows); got != oneAttempt {
			t.Fatalf("rows %s", got)
		}
		if rowStr(rows[2].Data, "evidence_cwd") != host {
			t.Errorf("thread_started %v, want evidence_cwd %s", rows[2].Data, host)
		}
		if r := receiptOf(t, rows[4]); r["state"] != "accepted" {
			t.Errorf("evidence_receipt %v", r)
		}
		if got := evidenceRecords(t, k.evCwd); len(got) != 0 {
			t.Errorf("the unclaimed copy under the evidence cwd stayed: %v", got)
		}
	})

	// ReceiptRecorded: the receipt the hook accepted (claimed through JudgeAssignedReceiptTurn on a real rollout) is copied to the
	// step bus and recorded on the completed row with its digest, the claiming subagent and turn.
	t.Run("ReceiptRecorded", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		body := []byte("$ go test ./x\nFAIL\n$ go test ./x\nok\n")
		var mu sync.Mutex
		receiptPath := ""
		k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, func(st Step, agent string) {
			go func() {
				v, path := claim(t, st, k.evCwd, agent, agent+"-turn1", body)
				if v != evidence.AssignedAccepted {
					t.Errorf("claim %v", v)
				}
				mu.Lock()
				receiptPath = path
				mu.Unlock()
				k.complete(agent, st)
			}()
		})
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("result %v", got)
		}
		rows := k.rowsOf("exe")
		if got := eventsOf(rows); got != oneAttempt {
			t.Fatalf("rows %s", got)
		}
		r := receiptOf(t, rows[4])
		sum := sha256.Sum256(body)
		mu.Lock()
		defer mu.Unlock()
		id := rowStr(rows[1].Data, "evidence_assignment")
		if r["state"] != "accepted" || r["assignment"] != id || r["path"] != receiptPath || r["sha256"] != hex.EncodeToString(sum[:]) ||
			r["agent_id"] != "agent-1" || r["turn_id"] != "agent-1-turn1" || r["turn_match"] != true || r["ignored"] != true {
			t.Errorf("evidence_receipt %v (receipt %s)", r, receiptPath)
		}
		copyPath, _ := r["copy"].(string)
		if copied, err := os.ReadFile(copyPath); err != nil || string(copied) != string(body) || copyPath != k.bus.FilePath(rows[0].CallID, "evidence-"+id+".md") {
			t.Errorf("the bus copy %s: %q, %v", copyPath, copied, err)
		}
	})

	// ReceiptMissing: a record never claimed is missing; one another agent claimed, or whose receipt changed after the claim, is
	// unverified; none of them is copied.
	t.Run("ReceiptMissing", func(t *testing.T) {
		for _, tc := range []struct{ name, want string }{{"unclaimed", "missing"}, {"another agent", "unverified"}, {"drift", "unverified"}} {
			t.Run(tc.name, func(t *testing.T) {
				k := newEvidenceKit(t, true, ".crw/\n")
				k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }, func(st Step, agent string) {
					go func() {
						switch tc.name {
						case "another agent":
							if v, _ := claim(t, st, k.evCwd, "agent-other", "turn-x", []byte("not mine\n")); v != evidence.AssignedAccepted {
								t.Errorf("claim %v", v)
							}
						case "drift":
							v, path := claim(t, st, k.evCwd, agent, agent+"-turn1", []byte("first\n"))
							if v != evidence.AssignedAccepted {
								t.Errorf("claim %v", v)
							}
							_ = os.WriteFile(path, []byte("rewritten later\n"), 0o600)
						}
						k.complete(agent, st)
					}()
				})
				res := k.h.run(evidenceExe)
				if got := value[[]any](t, res); got[0] != "completed" {
					t.Fatalf("result %v", got)
				}
				rows := k.rowsOf("exe")
				r := receiptOf(t, rows[4])
				if r["state"] != tc.want || r["assignment"] != rowStr(rows[1].Data, "evidence_assignment") || r["detail"] == "" {
					t.Errorf("evidence_receipt %v, want %s", r, tc.want)
				}
				if _, ok := r["copy"]; ok {
					t.Errorf("a receipt that is not accepted was copied: %v", r)
				}
				if got, _ := filepath.Glob(filepath.Join(k.bus.StepsDir(rows[0].CallID), "evidence-*")); len(got) != 0 {
					t.Errorf("copies on the bus: %v", got)
				}
			})
		}
	})

	// NotIgnored: a P whose repository does not ignore .crw/ is recorded as such, and the step is published all the same, with the
	// rows of before.
	t.Run("NotIgnored", func(t *testing.T) {
		k := newEvidenceKit(t, true, "")
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("result %v", got)
		}
		rows := k.rowsOf("exe")
		if got := eventsOf(rows); got != oneAttempt {
			t.Fatalf("rows %s", got)
		}
		if v, ok := rows[1].Data["evidence_ignored"]; !ok || v != false || rowStr(rows[1].Data, "evidence_assignment") == "" {
			t.Errorf("step_published %v, want evidence_ignored false", rows[1].Data)
		}
		if r := receiptOf(t, rows[4]); r["ignored"] != false {
			t.Errorf("evidence_receipt %v, want ignored false", r)
		}
	})

	// IgnoredByTheReceiptName: whether git ignores the receipt is asked of the step's own receipt file <id>.md, so a rule that
	// matches another name in .crw/evidence (here receipt.md) does not make the step's receipt ignored, and one that matches only the
	// receipt's own name does.
	t.Run("IgnoredByTheReceiptName", func(t *testing.T) {
		for _, tc := range []struct {
			name, ignore string
			want         bool
		}{{"another name", ".crw/evidence/receipt.md\n", false}, {"its own name", ".crw/evidence/*.md\n!.crw/evidence/receipt.md\n", true}} {
			t.Run(tc.name, func(t *testing.T) {
				k := newEvidenceKit(t, true, tc.ignore)
				res := k.h.run(evidenceExe)
				if got := value[[]any](t, res); got[0] != "completed" {
					t.Fatalf("result %v", got)
				}
				rows := k.rowsOf("exe")
				if v, ok := rows[1].Data["evidence_ignored"]; !ok || v != tc.want {
					t.Errorf("step_published %v, want evidence_ignored %v", rows[1].Data, tc.want)
				}
			})
		}
	})

	// NotForReadRoles: the read roles get no record, no directory and today's message.
	t.Run("NotForReadRoles", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		res := k.h.run(`const arch = await agent.call('design the change', { agentType: 'architect', label: 'arch' })
const rev = await agent.call('review the head', { agentType: 'reviewer', label: 'rev', head: 'h-review', schema: VERDICT })
const exp = await agent.call('look around', { agentType: 'explorer', label: 'exp' })
return [arch.status, rev.status, exp.status]`)
		if got := value[[]string](t, res); strings.Join(got, ",") != "completed,completed,completed" {
			t.Fatalf("statuses %v", got)
		}
		for _, st := range k.orch.spawned() {
			if st.Message != plainSpawnMessage(st) {
				t.Errorf("message of %s (%s):\n%s", st.Op, st.Role, st.Message)
			}
			if _, err := os.Lstat(filepath.Join(st.Workspace, ".crw")); err == nil {
				t.Errorf("%s: a .crw directory was made in %s", st.Role, st.Workspace)
			}
		}
		for _, label := range []string{"arch", "rev", "exp"} {
			for _, r := range k.rowsOf(label) {
				for _, key := range []string{"evidence_assignment", "evidence_ignored", "evidence_cwd", "evidence_receipt"} {
					if _, ok := r.Data[key]; ok {
						t.Errorf("%s %s has %s: %v", label, r.Event, key, r.Data)
					}
				}
			}
		}
		if got := evidenceRecords(t, k.evCwd); len(got) != 0 {
			t.Errorf("records %v", got)
		}
	})

	// Unconfigured: without EvidenceCwd the engine writes today's message and rows exactly, and makes nothing in P.
	t.Run("Unconfigured", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		k.cfg.EvidenceCwd = ""
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "completed" {
			t.Fatalf("result %v", got)
		}
		rows := k.rowsOf("exe")
		if got := eventsOf(rows); got != oneAttempt {
			t.Fatalf("rows %s", got)
		}
		st := k.orch.spawned()[0]
		if st.Message != plainSpawnMessage(st) {
			t.Errorf("message:\n%s", st.Message)
		}
		packet, _ := os.ReadFile(st.Packet)
		if strings.Contains(string(packet), "## Evidence") {
			t.Errorf("the packet has an evidence section:\n%s", packet)
		}
		for _, r := range rows {
			for _, key := range []string{"evidence_assignment", "evidence_ignored", "evidence_cwd", "evidence_receipt"} {
				if _, ok := r.Data[key]; ok {
					t.Errorf("%s has %s: %v", r.Event, key, r.Data)
				}
			}
		}
		if _, err := os.Lstat(filepath.Join(st.Workspace, ".crw")); err == nil {
			t.Errorf("a .crw directory was made in P")
		}
		if got := evidenceRecords(t, k.evCwd); len(got) != 0 {
			t.Errorf("records %v", got)
		}
	})

	// UnclaimedRemovedOnLoseAndEnd: an unclaimed record is removed where its attempt ends without its subagent (a lost attempt, in
	// the process that lost it or in the one that recovers the lost row) and where the call ends completed or failed; a call held
	// with its step unbound keeps it, and the resumed held call publishes the step with it.
	t.Run("UnclaimedRemovedOnLoseAndEnd", func(t *testing.T) {
		attemptRecords := func(k *evidenceKit, mu *sync.Mutex, seen *[][]string, ids *[]string) func(st Step, _ string) {
			return func(st Step, _ string) {
				mu.Lock()
				defer mu.Unlock()
				*seen = append(*seen, evidenceRecords(t, k.evCwd))
				*ids = append(*ids, messageAssignment(st.Message))
			}
		}
		t.Run("lost", func(t *testing.T) {
			k := newEvidenceKit(t, true, ".crw/\n")
			var mu sync.Mutex
			var seen [][]string
			var ids []string
			k.setOrch(func(st Step, _ string, _ int) orchAnswer {
				if st.Attempt == 1 {
					return orchAnswer{lose: true}
				}
				return orchAnswer{}
			}, attemptRecords(k, &mu, &seen, &ids))
			res := k.h.run(evidenceExe)
			if got := value[[]any](t, res); got[0] != "completed" {
				t.Fatalf("result %v", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 2 || len(seen[1]) != 1 || filepath.Base(seen[1][0]) != ids[1]+".json" || ids[0] == ids[1] {
				t.Errorf("records at each spawn %v (ids %v): the lost attempt's record stayed", seen, ids)
			}
			if got := evidenceRecords(t, k.evCwd); len(got) != 0 {
				t.Errorf("records left after the call completed: %v", got)
			}
		})
		t.Run("lost row recovered", func(t *testing.T) {
			k := newEvidenceKit(t, true, ".crw/\n")
			var lostRec evidence.Assignment
			k.setOrch(func(st Step, _ string, _ int) orchAnswer { return orchAnswer{lose: true} }, func(st Step, _ string) {
				lostRec, _ = evidence.RecordedAssignment(k.evCwd, sessionOwner, messageAssignment(st.Message))
			})
			k.die(t, deathAt(EvSessionAttemptLost), evidenceExe)
			if _, ok := evidence.RecordedAssignment(k.evCwd, sessionOwner, lostRec.ID); !ok {
				t.Fatalf("the dead process left no record of the lost attempt")
			}
			var mu sync.Mutex
			var seen [][]string
			var ids []string
			k.setOrch(nil, attemptRecords(k, &mu, &seen, &ids))
			res := k.h.run(evidenceExe)
			if got := value[[]any](t, res); got[0] != "completed" {
				t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 || len(seen[0]) != 1 || ids[0] == lostRec.ID {
				t.Errorf("records at the next attempt's spawn %v (ids %v, lost %s)", seen, ids, lostRec.ID)
			}
		})
		t.Run("failed", func(t *testing.T) {
			k := newEvidenceKit(t, true, ".crw/\n")
			k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{fail: "contextWindowExceeded"} }, nil)
			res := k.h.run(evidenceExe)
			if got := value[[]any](t, res); got[0] != "null_model" {
				t.Fatalf("result %v", got)
			}
			if got := evidenceRecords(t, k.evCwd); len(got) != 0 {
				t.Errorf("records left after the call failed: %v", got)
			}
		})
		t.Run("held", func(t *testing.T) {
			k := newEvidenceKit(t, true, ".crw/\n")
			k.setOrch(func(Step, string, int) orchAnswer { return orchAnswer{skip: true} }, nil)
			ctl := &Control{}
			k.h.cfg.Control = ctl
			go func() {
				for i := 0; i < 5000; i++ {
					if len(rowsNamed(k.h.journal.Events(), EvStepPublished)) > 0 {
						ctl.Pause("operator", "test")
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
			res := k.h.run(evidenceExe)
			if got := value[[]any](t, res); got[0] != "held" {
				t.Fatalf("result %v", got)
			}
			kept := evidenceRecords(t, k.evCwd)
			id := rowStr(rowsNamed(k.rowsOf("exe"), EvStepPublished)[0].Data, "evidence_assignment")
			if len(kept) != 1 || filepath.Base(kept[0]) != id+".json" {
				t.Fatalf("records of the held call %v, want %s", kept, id)
			}
			before, _ := evidence.RecordedAssignment(k.evCwd, sessionOwner, id)
			var mu sync.Mutex
			var during evidence.Assignment
			k.setOrch(nil, func(st Step, _ string) {
				mu.Lock()
				defer mu.Unlock()
				during, _ = evidence.RecordedAssignment(k.evCwd, sessionOwner, messageAssignment(st.Message))
			})
			k.h.cfg.Control = &Control{}
			k.h.cfg.Resume = &ResumeOptions{}
			res = k.h.run(evidenceExe)
			if got := value[[]any](t, res); got[0] != "completed" {
				t.Fatalf("resumed %v: rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			mu.Lock()
			defer mu.Unlock()
			if during.ID != id || during.CreatedAt != before.CreatedAt {
				t.Errorf("the resumed step's record %+v, want the held call's %+v", during, before)
			}
		})
	})

	// AssignmentWriteFailureHolds: a record that cannot be written holds the call before anything is published or spawned.
	t.Run("AssignmentWriteFailureHolds", func(t *testing.T) {
		k := newEvidenceKit(t, true, ".crw/\n")
		if err := os.Symlink(t.TempDir(), filepath.Join(k.evCwd, ".crw")); err != nil {
			t.Fatal(err)
		}
		res := k.h.run(evidenceExe)
		if got := value[[]any](t, res); got[0] != "held" || got[1] != ReasonEvidenceAssignmentFailed {
			t.Fatalf("result %v", got)
		}
		rows := k.rowsOf("exe")
		if got := eventsOf(rows); got != "prepared,held" {
			t.Errorf("rows %s", got)
		}
		if held := rows[len(rows)-1]; rowStr(held.Data, "input_state") != "not_delivered" || rowStr(held.Data, "detail") == "" {
			t.Errorf("held %v", held.Data)
		}
		if n := len(k.orch.spawned()); n != 0 {
			t.Errorf("%d spawns", n)
		}
		// the host cwd the subagent runs in cannot hold the record: the hook would find no contract for it, so it is never bound but
		// closed, and the call is held for the same reason
		t.Run("host cwd", func(t *testing.T) {
			k := newEvidenceKit(t, true, ".crw/\n")
			host := t.TempDir()
			if err := os.Symlink(t.TempDir(), filepath.Join(host, ".crw")); err != nil {
				t.Fatal(err)
			}
			k.setHostCwd(host)
			res := k.h.run(evidenceExe)
			if got := value[[]any](t, res); got[0] != "held" || got[1] != ReasonEvidenceAssignmentFailed {
				t.Fatalf("result %v", got)
			}
			rows := k.rowsOf("exe")
			if got := eventsOf(rows); got != "prepared,step_published,held" {
				t.Errorf("rows %s", got)
			}
			if held := rows[len(rows)-1]; !strings.Contains(rowStr(held.Data, "detail"), host) {
				t.Errorf("held %v does not name the host cwd %s", held.Data, host)
			}
			waitFor(t, "the unbound subagent closed", func() bool { return len(k.orch.closedAgents()) == 1 && k.orch.closedAgents()[0] == "agent-1" })
		})
	})
}
