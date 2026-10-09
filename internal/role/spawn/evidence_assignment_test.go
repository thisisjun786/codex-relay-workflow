package spawn

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-1115: a bounded worker assigned a managed worktree keeps the parent's native cwd, so its SubagentStop payload names the
// parent's tree. The parent's spawn packet names the assigned tree (CRW-WORKTREE:) or says the packet allows no evidence write
// (CRW-EVIDENCE: none); the spawn hook records that assignment for the parent's session and injects its location into the
// child's packet, and the SubagentStop gate verifies the receipt in the registered tree, bound to the child that first claims it.
// Every case runs the real spawn hook and the real SubagentStop leg against temporary homes and trees.

type assignedRig struct {
	t       *testing.T
	rig     *spawnHookRig
	cwd, wt string
	// packets is what each child was handed: the harness gives the SubagentStop leg that child's own transcript, which starts with
	// its packet.
	packets map[string]string
}

func newAssignedRig(t *testing.T) *assignedRig {
	t.Helper()
	rig := spawnHookNewRig(t, nil, spawnHookCase{})
	home := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(key, home)
	}
	t.Setenv("CRW_PABCD", "")
	r := &assignedRig{t: t, rig: rig, cwd: rig.ws, wt: assignedGitTree(t), packets: map[string]string{}}
	s := state.DefaultState("s1", "")
	s.Phase, s.OrchestrationActive = state.PhaseB, true
	spawnHookMust(t, state.WriteState(r.cwd, s))
	return r
}

// assignedGitTree is a git checkout with one commit, standing in for a managed worktree.
func assignedGitTree(t *testing.T) string {
	t.Helper()
	dir := spawnHookEval(t, t.TempDir())
	assignedGit(t, dir, "init", "-q")
	spawnHookMust(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	assignedGit(t, dir, "add", "a.txt")
	assignedGit(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func assignedGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// spawn runs the spawn hook for one worker packet and returns the message the child receives ("" when the hook allowed the
// spawn untouched) and the raw answer.
func (r *assignedRig) spawn(message string) (string, string) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "spawn_agent", "session_id": "s1", "cwd": r.cwd,
		"tool_input": map[string]any{"agent_type": "worker", "message": message}})
	spawnHookMust(r.t, err)
	out := RunSpawnAttachHook(string(payload), r.rig.env)
	if out == "" || strings.Contains(out, `"deny"`) {
		return "", out
	}
	v, err := pyjson.Loads(strings.TrimSpace(out), pyjson.LoadOptions{})
	spawnHookMust(r.t, err)
	got, _ := v.(pyjson.Object).Get("hookSpecificOutput").(pyjson.Object).Get("updatedInput").(pyjson.Object).Get("message").(string)
	return got, out
}

// deliver hands packet to the child agent: its transcript, as the SubagentStop leg receives it, starts with that packet.
func (r *assignedRig) deliver(agent, packet string) {
	r.t.Helper()
	r.packets[agent] = packet
}

// stop runs the registered SubagentStop leg for a worker of session s1. A child that was handed a packet (deliver) has a
// transcript path in the payload.
func (r *assignedRig) stop(agent, turn, message string) string {
	r.t.Helper()
	payload := map[string]any{"hook_event_name": "SubagentStop", "cwd": r.cwd, "session_id": "s1", "agent_type": "worker",
		"agent_id": agent, "turn_id": turn, "last_assistant_message": message}
	if packet, ok := r.packets[agent]; ok {
		transcript := filepath.Join(r.t.TempDir(), "agent.jsonl")
		line, err := json.Marshal(map[string]any{"role": "user", "text": packet})
		spawnHookMust(r.t, err)
		spawnHookMust(r.t, os.WriteFile(transcript, append(line, '\n'), 0o644))
		payload["agent_transcript_path"] = transcript
	}
	raw, err := json.Marshal(payload)
	spawnHookMust(r.t, err)
	var out, stderr bytes.Buffer
	code := harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-verifying-evidence"}, bytes.NewReader(raw), &out, &stderr, os.LookupEnv, harness.Legs())
	if code != 0 || stderr.Len() != 0 {
		r.t.Fatalf("subagent-stop exit=%d stderr=%s", code, &stderr)
	}
	return out.String()
}

// complete is the parent's update_goal status complete through the goal-complete gate: "" allows it.
func (r *assignedRig) complete() string {
	r.t.Helper()
	raw, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": r.cwd, "tool_name": "update_goal",
		"tool_input": map[string]any{"status": "complete"}})
	spawnHookMust(r.t, err)
	return pabcdhook.GoalGateHandlePreToolUseFailClosed(string(raw), os.LookupEnv, false)
}

func (r *assignedRig) put(path, text string) string {
	r.t.Helper()
	spawnHookMust(r.t, os.MkdirAll(filepath.Dir(path), 0o755))
	spawnHookMust(r.t, os.WriteFile(path, []byte(text), 0o644))
	return path
}

func assignedBlocked(t *testing.T, out string, attempt int) {
	t.Helper()
	if !strings.Contains(out, `"decision":"block"`) || !strings.Contains(out, "This is attempt "+strconv.Itoa(attempt)+" of 3.") {
		t.Fatalf("want block at attempt %d, got %q", attempt, out)
	}
}

var assignedID = regexp.MustCompile(`\[CRW-EVIDENCE-ASSIGNMENT:([A-Za-z0-9]+)\]`)

// The packet carries the assigned tree; the child's packet names the approved evidence location, and a receipt recorded there
// passes the gate although the payload's cwd is the parent's tree. The same receipt failed at attempt 1 before (B1-01 replay).
func TestEvidenceAssignmentTreeReceiptPasses(t *testing.T) {
	for _, form := range []string{"absolute", "relative"} {
		t.Run(form, func(t *testing.T) {
			r := newAssignedRig(t)
			child, raw := r.spawn("TASK: fix it\nCRW-WORKTREE: " + r.wt + "\nSCOPE: a.txt")
			r.deliver("w1", child)
			if !assignedID.MatchString(child) || !strings.Contains(child, filepath.Join(r.wt, ".crw", "evidence")) {
				t.Fatalf("the child's packet does not name the assigned evidence root:\n%s\n%s", child, raw)
			}
			receipt := r.put(filepath.Join(r.wt, ".crw", "evidence", "check.txt"), "go test ./... ok")
			claim := receipt
			if form == "relative" {
				claim = ".crw/evidence/check.txt"
			}
			if out := r.stop("w1", "t1", "Verified assigned worktree.\nEVIDENCE_RECORDED: "+claim); out != "" {
				t.Fatalf("a receipt in the registered tree was refused: %s", out)
			}
			if got := r.complete(); got != "" {
				t.Fatalf("completion denied after a verified receipt: %s", got)
			}
		})
	}
}

// Without the packet marker nothing is registered and the native cwd stays the only root (compatibility).
func TestEvidenceAssignmentAbsentKeepsNativeRoot(t *testing.T) {
	r := newAssignedRig(t)
	child, _ := r.spawn("TASK: fix it in " + r.wt)
	if strings.Contains(child, "CRW-EVIDENCE-ASSIGNMENT") {
		t.Fatalf("an unmarked packet got an assignment:\n%s", child)
	}
	if _, err := os.Stat(filepath.Join(r.cwd, ".crw", "evidence-assignments")); !os.IsNotExist(err) {
		t.Fatalf("an unmarked packet registered an assignment: %v", err)
	}
	receipt := r.put(filepath.Join(r.wt, ".crw", "evidence", "check.txt"), "ok")
	assignedBlocked(t, r.stop("w1", "", "EVIDENCE_RECORDED: "+receipt), 1)
	native := r.put(filepath.Join(r.cwd, ".crw", "evidence", "check.txt"), "ok")
	if out := r.stop("w1", "", "EVIDENCE_RECORDED: "+native); out != "" {
		t.Fatalf("a native receipt was refused: %s", out)
	}
}

// Receipts that the registration does not cover are refused: another tree, a second actor, a receipt older than the
// dispatch, a tree whose history was replaced, a linked evidence directory, a receipt link and a root replaced after dispatch.
func TestEvidenceAssignmentRefusals(t *testing.T) {
	cases := []string{"unrelated_tree", "foreign_actor", "stale_receipt", "foreign_head", "linked_evidence_dir", "linked_crw_dir", "receipt_symlink", "replaced_root", "root_symlink_swap", "dotdot_escape"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			r := newAssignedRig(t)
			var receipt string
			if kind == "stale_receipt" {
				receipt = r.put(filepath.Join(r.wt, ".crw", "evidence", "check.txt"), "old")
				past := time.Now().Add(-time.Hour)
				spawnHookMust(t, os.Chtimes(receipt, past, past))
			}
			child, raw := r.spawn("TASK: fix it\nCRW-WORKTREE: " + r.wt)
			r.deliver("w1", child)
			r.deliver("w2", "TASK: an unrelated job")
			if !assignedID.MatchString(child) {
				t.Fatalf("no assignment:\n%s", raw)
			}
			if receipt == "" {
				receipt = r.put(filepath.Join(r.wt, ".crw", "evidence", "check.txt"), "ok")
			}
			agent := "w1"
			switch kind {
			case "unrelated_tree":
				receipt = r.put(filepath.Join(assignedGitTree(t), ".crw", "evidence", "check.txt"), "ok")
			case "foreign_actor":
				if out := r.stop("w1", "", "EVIDENCE_RECORDED: "+receipt); out != "" {
					t.Fatalf("the first actor was refused: %s", out)
				}
				agent = "w2"
			case "foreign_head":
				orphan := filepath.Join(r.wt, "b.txt")
				assignedGit(t, r.wt, "checkout", "-q", "--orphan", "other")
				r.put(orphan, "b")
				assignedGit(t, r.wt, "add", "b.txt")
				assignedGit(t, r.wt, "commit", "-q", "-m", "unrelated history")
			case "linked_evidence_dir", "linked_crw_dir":
				outside := t.TempDir()
				r.put(filepath.Join(outside, "evidence", "check.txt"), "ok")
				r.put(filepath.Join(outside, "check.txt"), "ok")
				spawnHookMust(t, os.RemoveAll(filepath.Join(r.wt, ".crw")))
				if kind == "linked_crw_dir" {
					spawnHookMust(t, os.Symlink(outside, filepath.Join(r.wt, ".crw")))
				} else {
					spawnHookMust(t, os.MkdirAll(filepath.Join(r.wt, ".crw"), 0o755))
					spawnHookMust(t, os.Symlink(outside, filepath.Join(r.wt, ".crw", "evidence")))
				}
			case "receipt_symlink":
				link := filepath.Join(r.wt, ".crw", "evidence", "link.txt")
				spawnHookMust(t, os.Symlink(receipt, link))
				receipt = link
			case "replaced_root":
				spawnHookMust(t, os.RemoveAll(r.wt))
				spawnHookMust(t, os.MkdirAll(r.wt, 0o755))
				r.put(receipt, "ok")
			case "root_symlink_swap":
				moved := r.wt + "-moved"
				spawnHookMust(t, os.Rename(r.wt, moved))
				spawnHookMust(t, os.Symlink(moved, r.wt))
			case "dotdot_escape":
				r.put(filepath.Join(r.wt, "escape.txt"), "ok")
				receipt = filepath.Join(r.wt, ".crw", "evidence", "..", "..", "escape.txt")
			}
			assignedBlocked(t, r.stop(agent, "", "EVIDENCE_RECORDED: "+receipt), 1)
		})
	}
}

// A packet that allows no evidence write (one file only, read-only) keeps its scope: the child names its assignment, is released
// at once without spending retries, and leaves a scope-conflict verdict the parent must verify. The parent's own verification
// then resolves it (pass) or, without a valid receipt, leaves completion denied (fail).
func TestEvidenceAssignmentScopeConflict(t *testing.T) {
	for _, outcome := range []string{"parent_pass", "parent_fail"} {
		t.Run(outcome, func(t *testing.T) {
			r := newAssignedRig(t)
			child, raw := r.spawn("TASK: edit only a.txt\nCRW-WORKTREE: " + r.wt + "\nCRW-EVIDENCE: none\nSCOPE: a.txt only, read nothing else")
			r.deliver("w1", child)
			m := assignedID.FindStringSubmatch(child)
			if m == nil || !strings.Contains(child, "EVIDENCE_SCOPE_CONFLICT: "+m[1]) {
				t.Fatalf("the child's packet does not carry the scope-conflict contract:\n%s\n%s", child, raw)
			}
			if strings.Contains(child, filepath.Join(r.wt, ".crw", "evidence")) {
				t.Fatalf("a no-write packet was told to write evidence:\n%s", child)
			}
			if out := r.stop("w1", "t1", "Edited a.txt; I may not write a receipt.\nEVIDENCE_SCOPE_CONFLICT: "+m[1]); out != "" {
				t.Fatalf("scope conflict was blocked: %s", out)
			}
			if _, err := os.Stat(filepath.Join(r.wt, ".crw")); !os.IsNotExist(err) {
				t.Fatalf("the scope-conflict path wrote into the child's tree: %v", err)
			}
			entries := state.ReadState(r.cwd, "s1").UnverifiedSubagents
			if len(entries) != 1 || entries[0].AgentID != "w1" || entries[0].TurnID != "t1" || !entries[0].Resolvable || entries[0].Attempts != 0 {
				t.Fatalf("scope conflict did not leave one resolvable verdict: %+v", entries)
			}
			if r.complete() == "" {
				t.Fatal("completion allowed over an unverified scope conflict")
			}
			// A second child cannot borrow the same assignment.
			assignedBlocked(t, r.stop("w2", "", "EVIDENCE_SCOPE_CONFLICT: "+m[1]), 1)
			receipt := filepath.Join(r.cwd, ".crw", "evidence", "parent-check.txt")
			if outcome == "parent_pass" {
				r.put(receipt, "parent re-ran go test: ok")
			}
			_, code := cli.RunEvidenceCLI(cli.EvidenceResolveArgs{Verb: "resolve", SessionID: "s1", AgentID: "w1", Receipt: receipt, Cwd: r.cwd})
			if outcome == "parent_pass" {
				if code != 0 || r.complete() != "" {
					t.Fatalf("parent verification did not resolve the scope conflict: code=%d", code)
				}
			} else if code == 0 || r.complete() == "" {
				t.Fatalf("a failed parent verification released completion: code=%d", code)
			}
		})
	}
	// A forged or unknown assignment id is no exit: the gate blocks as before.
	r := newAssignedRig(t)
	assignedBlocked(t, r.stop("w1", "", "EVIDENCE_SCOPE_CONFLICT: AAAAAAAAAAAAAAAAAAAAAAAAAA"), 1)
}

// Registering an assignment writes only the parent's evidence-assignment record: no relay registration, no dispatch ledger
// entry, nothing in the child's tree. An explorer stays outside the gate.
func TestEvidenceAssignmentNativeHelperStaysUnregistered(t *testing.T) {
	r := newAssignedRig(t)
	before := assignedFiles(t, r.rig.dir)
	if child, _ := r.spawn("TASK: fix it\nCRW-WORKTREE: " + r.wt); !assignedID.MatchString(child) {
		t.Fatal("no assignment")
	}
	var added []string
	for _, f := range assignedFiles(t, r.rig.dir) {
		if !slices.Contains(before, f) {
			added = append(added, f)
		}
	}
	if len(added) != 1 || !strings.HasPrefix(added[0], filepath.Join("ws", ".crw", "evidence-assignments")+string(filepath.Separator)) {
		t.Fatalf("registration wrote more than its record: %v", added)
	}
	home, _ := os.LookupEnv("CRW_HOME")
	if files := assignedFiles(t, home); len(files) != 0 {
		t.Fatalf("registration touched the CRW home: %v", files)
	}
	if wt := assignedFiles(t, r.wt); slices.ContainsFunc(wt, func(f string) bool { return strings.HasPrefix(f, ".crw") }) {
		t.Fatalf("registration wrote into the child's tree: %v", wt)
	}
	raw, err := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "cwd": r.cwd, "session_id": "s1", "agent_type": "explorer", "agent_id": "e1"})
	spawnHookMust(t, err)
	var out, stderr bytes.Buffer
	harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-verifying-evidence"}, bytes.NewReader(raw), &out, &stderr, os.LookupEnv, harness.Legs())
	if out.Len() != 0 {
		t.Fatalf("explorer gated: %s", &out)
	}
}

func assignedFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			if !strings.HasSuffix(rel, ".lock") {
				files = append(files, rel)
			}
		}
		return nil
	})
	return files
}

// An assignment request the gate could not honour is refused, never guessed; a packet that already carries an assignment block
// (the hook running over its own output) is not registered twice.
func TestEvidenceAssignmentRequestRefusalsAndIdempotence(t *testing.T) {
	r := newAssignedRig(t)
	other := assignedGitTree(t)
	for name, message := range map[string]string{
		"relative":   "TASK\nCRW-WORKTREE: relative/tree",
		"missing":    "TASK\nCRW-WORKTREE: " + filepath.Join(r.wt, "absent"),
		"two_trees":  "TASK\nCRW-WORKTREE: " + r.wt + "\nCRW-WORKTREE: " + other,
		"bad_mode":   "TASK\nCRW-WORKTREE: " + r.wt + "\nCRW-EVIDENCE: maybe",
		"empty_tree": "TASK\nCRW-WORKTREE:",
	} {
		if _, out := r.spawn(message); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "evidence assignment") {
			t.Errorf("%s: want a deny, got %q", name, out)
		}
	}
	if _, err := os.Stat(filepath.Join(r.cwd, ".crw", "evidence-assignments")); !os.IsNotExist(err) {
		t.Fatalf("a refused request left a record: %v", err)
	}
	child, _ := r.spawn("TASK\nCRW-WORKTREE: " + r.wt)
	again, _ := r.spawn(child) // "" is the hook allowing the guarded packet untouched
	if again != "" && strings.Count(again, EvidenceAssignmentMarker) != 1 {
		t.Fatalf("a second pass added another assignment:\n%s", again)
	}
	records, _ := filepath.Glob(filepath.Join(r.cwd, ".crw", "evidence-assignments", "*", "*.json"))
	if len(records) != 1 {
		t.Fatalf("records: %v", records)
	}
}

// CRW-1115 verification round 1. The child that stops is tied to the dispatch by its own transcript, which holds the packet the
// spawn hook injected: an actor whose packet did not carry the assignment cannot claim it, whoever submits first.
func TestEvidenceAssignmentForeignActorCannotClaimFirst(t *testing.T) {
	r := newAssignedRig(t)
	child, _ := r.spawn("TASK: fix it\nCRW-WORKTREE: " + r.wt)
	r.deliver("w1", child)
	r.deliver("unrelated-worker", "TASK: something else entirely")
	receipt := r.put(filepath.Join(r.wt, ".crw", "evidence", "check.txt"), "ok")
	assignedBlocked(t, r.stop("unrelated-worker", "foreign-turn", "EVIDENCE_RECORDED: "+receipt), 1)
	if out := r.stop("w1", "t1", "EVIDENCE_RECORDED: "+receipt); out != "" {
		t.Fatalf("the assigned actor was refused after a foreign attempt: %s", out)
	}
	// An actor that cites the assignment id without holding the packet is no better off.
	m := assignedID.FindStringSubmatch(child)
	assignedBlocked(t, r.stop("unrelated-worker", "foreign-turn-2", "EVIDENCE_ASSIGNMENT: "+m[1]+"\nEVIDENCE_RECORDED: "+receipt), 1)
}

// Where the harness gives no transcript the id the child cites stands in for it; the first actor to cite it still claims it, and
// no other actor can take it afterwards.
func TestEvidenceAssignmentCitationWithoutTranscript(t *testing.T) {
	r := newAssignedRig(t)
	child, _ := r.spawn("TASK: fix it\nCRW-WORKTREE: " + r.wt)
	m := assignedID.FindStringSubmatch(child)
	if !strings.Contains(child, "EVIDENCE_ASSIGNMENT: "+m[1]) {
		t.Fatalf("the packet does not tell the child to cite its assignment:\n%s", child)
	}
	receipt := r.put(filepath.Join(r.wt, ".crw", "evidence", "check.txt"), "ok")
	assignedBlocked(t, r.stop("w1", "t1", "EVIDENCE_RECORDED: "+receipt), 1) // no id, no transcript: no contract, and not native
	if out := r.stop("w1", "t1", "EVIDENCE_ASSIGNMENT: "+m[1]+"\nEVIDENCE_RECORDED: "+receipt); out != "" {
		t.Fatalf("a cited assignment was refused: %s", out)
	}
	assignedBlocked(t, r.stop("w2", "t2", "EVIDENCE_ASSIGNMENT: "+m[1]+"\nEVIDENCE_RECORDED: "+receipt), 1)
}

// The same tree is assigned again in one session: each dispatch is its own contract, so a finished first dispatch is no
// candidate for the second worker, and two dispatches open at once do not collide either.
func TestEvidenceAssignmentSameTreeSequentialAndConcurrent(t *testing.T) {
	r := newAssignedRig(t)
	child1, _ := r.spawn("TASK: first\nCRW-WORKTREE: " + r.wt)
	r.deliver("w1", child1)
	first := r.put(filepath.Join(r.wt, ".crw", "evidence", "first.txt"), "first ok")
	if out := r.stop("w1", "t1", "EVIDENCE_RECORDED: "+first); out != "" {
		t.Fatalf("first dispatch refused: %s", out)
	}
	time.Sleep(1100 * time.Millisecond) // receipt times are compared in whole seconds
	assignedGit(t, r.wt, "commit", "-q", "--allow-empty", "-m", "work of the first worker")
	child2, _ := r.spawn("TASK: second\nCRW-WORKTREE: " + r.wt)
	child3, _ := r.spawn("TASK: third, at once\nCRW-WORKTREE: " + r.wt)
	r.deliver("w2", child2)
	r.deliver("w3", child3)
	// The first dispatch's receipt predates the second dispatch.
	assignedBlocked(t, r.stop("w2", "t2", "EVIDENCE_RECORDED: "+first), 1)
	second := r.put(filepath.Join(r.wt, ".crw", "evidence", "second.txt"), "second ok")
	third := r.put(filepath.Join(r.wt, ".crw", "evidence", "third.txt"), "third ok")
	if out := r.stop("w2", "t2", "EVIDENCE_RECORDED: "+second); out != "" {
		t.Fatalf("second dispatch refused: %s", out)
	}
	if out := r.stop("w3", "t3", "EVIDENCE_RECORDED: "+third); out != "" {
		t.Fatalf("a concurrent dispatch of the same tree refused: %s", out)
	}
	// A worker holds exactly one contract: it cannot pass on another dispatch's.
	assignedBlocked(t, r.stop("w4", "t4", "EVIDENCE_RECORDED: "+third), 1)
}

// A child with a recorded contract is judged by it alone: a receipt in the parent's native tree, old or its own, is no
// substitute for the assigned tree (tree mode) and no way around the scope conflict (none mode). A dispatch without a contract
// keeps the native root.
func TestEvidenceAssignmentContractExcludesNativeReceipt(t *testing.T) {
	for _, mode := range []string{"tree", "none"} {
		t.Run(mode, func(t *testing.T) {
			r := newAssignedRig(t)
			packet := "TASK: fix it\nCRW-WORKTREE: " + r.wt
			if mode == "none" {
				packet += "\nCRW-EVIDENCE: none"
			}
			child, _ := r.spawn(packet)
			r.deliver("w1", child)
			r.deliver("free", "TASK: a dispatch with no packet lines")
			native := r.put(filepath.Join(r.cwd, ".crw", "evidence", "old-unrelated-check.txt"), "an unrelated earlier check")
			assignedBlocked(t, r.stop("w1", "t1", "EVIDENCE_RECORDED: "+native), 1)
			if out := r.stop("free", "t9", "EVIDENCE_RECORDED: "+native); out != "" {
				t.Fatalf("a dispatch without a contract lost the native root: %s", out)
			}
		})
	}
}
