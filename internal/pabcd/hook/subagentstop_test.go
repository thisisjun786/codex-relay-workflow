package hook_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	_ "unsafe" // go:linkname, for the lock's give-up seam

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Gate cases from CXC v0.2.40 subagent-evidence.test.ts:52-295,355-408,586-612,700-785,818-971.
// Exercise the registered leg so the tests fail by assertion before its handler is ported.
func subagentStopWorkspace(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(key, home)
	}
	t.Setenv("CRW_PABCD", "")
	return t.TempDir()
}

func subagentStopPut(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func subagentStopSeed(t *testing.T, cwd string, phase state.Phase, active bool) {
	t.Helper()
	s := state.DefaultState("s1", "")
	s.Phase, s.OrchestrationActive = phase, active
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
}

func subagentStopRun(t *testing.T, cwd, role, agent, turn, message string, extra map[string]any) string {
	t.Helper()
	p := map[string]any{"hook_event_name": "SubagentStop", "cwd": cwd, "session_id": "s1", "agent_type": role,
		"agent_id": agent, "turn_id": turn, "last_assistant_message": message}
	for key, value := range extra {
		p[key] = value
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := subagentStopInvoke(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func subagentStopInvoke(raw []byte) (string, error) {
	var out, stderr bytes.Buffer
	code := harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-verifying-evidence"},
		bytes.NewReader(raw), &out, &stderr, os.LookupEnv, harness.Legs())
	if code != 0 || stderr.Len() != 0 {
		return out.String(), fmt.Errorf("exit=%d stderr=%s", code, &stderr)
	}
	return out.String(), nil
}

func subagentStopBlock(t *testing.T, out string, attempt int) {
	t.Helper()
	raw, err := os.ReadFile("../evidence/testdata/directives.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct{ Verifier []string }
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(golden.Verifier[attempt-1])
	want := `{"decision":"block","reason":` + strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(quoted)) + `}`
	if out != want {
		t.Fatalf("attempt %d: got %q, want %q", attempt, out, want)
	}
}

func TestSubagentStopRolesAndWorkerArming(t *testing.T) {
	for _, role := range []string{"explorer", "default", "", "worker"} {
		t.Run("unarmed_"+role, func(t *testing.T) {
			cwd := subagentStopWorkspace(t)
			if got := subagentStopRun(t, cwd, role, "a1", "", "", nil); got != "" {
				t.Fatal(got)
			}
			if _, err := os.Stat(filepath.Join(cwd, ".crw")); !os.IsNotExist(err) {
				t.Fatalf("unexpected state: %v", err)
			}
		})
	}
	for _, phase := range []state.Phase{state.PhaseIdle, state.PhaseP, state.PhaseA, state.PhaseB, state.PhaseC} {
		for _, active := range []bool{false, true} {
			t.Run(string(phase)+map[bool]string{false: "_inactive", true: "_active"}[active], func(t *testing.T) {
				cwd := subagentStopWorkspace(t)
				subagentStopSeed(t, cwd, phase, active)
				got := subagentStopRun(t, cwd, "worker", "a1", "", "", nil)
				if active && (phase == state.PhaseB || phase == state.PhaseC) {
					subagentStopBlock(t, got, 1)
				} else if got != "" {
					t.Fatal(got)
				}
			})
		}
	}
	cwd := subagentStopWorkspace(t)
	path := state.StatePath(cwd, "s1")
	subagentStopPut(t, path, "{ corrupt")
	if out := subagentStopRun(t, cwd, "worker", "a1", "", "", nil); out != "" {
		t.Fatal(out)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{ corrupt" {
		t.Fatal("worker changed corrupt state")
	}
}

func TestSubagentStopPolicyAndExecutor(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	subagentStopPut(t, filepath.Join(cwd, "crw.json"), `{"pabcd":{"enabled":false}}`)
	subagentStopSeed(t, cwd, state.PhaseB, true)
	for _, override := range []string{"", "off", "on"} {
		t.Setenv("CRW_PABCD", override)
		for _, role := range []string{"executor", "worker"} {
			out := subagentStopRun(t, cwd, role, role, "", "", nil)
			if override == "on" {
				subagentStopBlock(t, out, 1)
			} else if out != "" {
				t.Fatal(out)
			}
		}
	}
	fresh := t.TempDir()
	t.Setenv("CRW_PABCD", "")
	subagentStopBlock(t, subagentStopRun(t, fresh, "executor", "a1", "", "", nil), 1)
}

func TestSubagentStopBudgetAndLateReceipt(t *testing.T) {
	for _, role := range []string{"executor", "worker"} {
		t.Run(role, func(t *testing.T) {
			cwd := subagentStopWorkspace(t)
			subagentStopSeed(t, cwd, state.PhaseB, true)
			for n := 1; n <= 3; n++ {
				subagentStopBlock(t, subagentStopRun(t, cwd, role, "a1", "t1", "private prose", nil), n)
			}
			for n := 0; n < 4; n++ {
				if out := subagentStopRun(t, cwd, role, "a1", "t1", "", nil); out != "" {
					t.Fatal(out)
				}
			}
			s := state.ReadState(cwd, "s1")
			if len(s.UnverifiedSubagents) != 1 || s.UnverifiedSubagents[0].AgentType != role || !s.UnverifiedSubagents[0].Resolvable {
				t.Fatalf("verdicts: %+v", s.UnverifiedSubagents)
			}
			if evidence.ReadAttempts(cwd, "s1", "a1", "t1") != 3 {
				t.Fatal("terminal budget lost")
			}
			raw, _ := os.ReadFile(state.StatePath(cwd, "s1"))
			if bytes.Contains(raw, []byte("private prose")) {
				t.Fatal("prose persisted")
			}
			subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/late.md"), "verified")
			if out := subagentStopRun(t, cwd, role, "a1", "t1", "EVIDENCE_RECORDED: .crw/evidence/late.md", nil); out != "" {
				t.Fatal(out)
			}
			if len(state.ReadState(cwd, "s1").UnverifiedSubagents) != 0 || evidence.ReadAttempts(cwd, "s1", "a1", "t1") != 0 {
				t.Fatal("late receipt did not resolve")
			}
		})
	}
}

func TestSubagentStopReceiptValidationAndTranscriptSpoofing(t *testing.T) {
	for _, kind := range []string{"outside", "empty", "symlink", "linked_directory", "valid", "context", "exempt", "buried", "readonly"} {
		t.Run(kind, func(t *testing.T) {
			cwd := subagentStopWorkspace(t)
			subagentStopSeed(t, cwd, state.PhaseB, true)
			receipt := filepath.Join(cwd, ".crw/evidence/proof.md")
			subagentStopPut(t, receipt, "proof")
			message := "EVIDENCE_RECORDED: " + receipt
			extra := map[string]any{}
			switch kind {
			case "outside":
				receipt = filepath.Join(cwd, "outside.md")
				subagentStopPut(t, receipt, "proof")
				message = "EVIDENCE_RECORDED: " + receipt
			case "empty":
				subagentStopPut(t, receipt, "")
			case "symlink":
				link := filepath.Join(cwd, ".crw/evidence/link")
				if err := os.Symlink(receipt, link); err != nil {
					t.Fatal(err)
				}
				message = "EVIDENCE_RECORDED: " + link
			case "linked_directory":
				outside := t.TempDir()
				subagentStopPut(t, filepath.Join(outside, "proof"), "proof")
				if err := os.Symlink(outside, filepath.Join(cwd, ".crw/evidence/link")); err != nil {
					t.Fatal(err)
				}
				message = "EVIDENCE_RECORDED: .crw/evidence/link/proof"
			case "context", "exempt", "buried", "readonly":
				text := map[string]string{"context": "Context compacted", "exempt": "[CXC-EVIDENCE-EXEMPT]", "buried": strings.Repeat("x", 30000) + "[CXC-EVIDENCE-EXEMPT]", "readonly": "[REVIEWER read-only]"}[kind]
				transcript := filepath.Join(cwd, "child.jsonl")
				subagentStopPut(t, transcript, text)
				extra["agent_transcript_path"] = transcript
				message = text
			}
			out := subagentStopRun(t, cwd, "worker", "a1", "", ""+message, extra)
			if kind == "valid" {
				if out != "" {
					t.Fatal(out)
				}
			} else {
				subagentStopBlock(t, out, 1)
			}
		})
	}
}

func TestSubagentStopIdentitiesAndNilFields(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	for _, id := range [][2]string{{"a-b", "c"}, {"a", "b-c"}, {"a/b", ""}, {"a-b", ""}, {"a1", "t1"}, {"a1", "t2"}, {"a1", ""}} {
		for n := 1; n <= 3; n++ {
			subagentStopBlock(t, subagentStopRun(t, cwd, "executor", id[0], id[1], "", nil), n)
		}
		if out := subagentStopRun(t, cwd, "executor", id[0], id[1], "", nil); out != "" {
			t.Fatal(out)
		}
	}
	subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/one.md"), "verified")
	subagentStopRun(t, cwd, "executor", "a1", "t1", "EVIDENCE_RECORDED: .crw/evidence/one.md", nil)
	if evidence.ReadAttempts(cwd, "s1", "a1", "t1") != 0 || evidence.ReadAttempts(cwd, "s1", "a1", "t2") != 3 || evidence.ReadAttempts(cwd, "s1", "a1", "") != 3 {
		t.Fatal("receipt cleared other turns")
	}
	fresh := t.TempDir()
	for n := 1; n <= 3; n++ {
		subagentStopBlock(t, subagentStopRun(t, fresh, "executor", "", "", "", map[string]any{"agent_id": nil, "turn_id": nil, "last_assistant_message": nil}), n)
	}
	subagentStopRun(t, fresh, "executor", "", "", "", nil)
	if entries := state.ReadState(fresh, "s1").UnverifiedSubagents; len(entries) != 1 || entries[0].Resolvable {
		t.Fatalf("nil identity: %+v", entries)
	}
}

func TestSubagentStopPersistenceFailuresAndCorruptCounter(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	subagentStopPut(t, filepath.Join(cwd, ".crw/evidence-attempts"), "not a directory")
	if out := subagentStopRun(t, cwd, "executor", "a1", "", "", nil); out != "" {
		t.Fatal(out)
	}
	entries := state.ReadState(cwd, "s1").UnverifiedSubagents
	if len(entries) != 1 || entries[0].Attempts != 0 {
		t.Fatalf("failed write must record old count: %+v", entries)
	}
	for _, bad := range []string{"-1", "2.5", "999999", `"three"`} {
		fresh := t.TempDir()
		subagentStopRun(t, fresh, "executor", "a1", "", "", nil)
		dir := filepath.Join(fresh, ".crw/evidence-attempts")
		names, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name.Name(), ".json") {
				subagentStopPut(t, filepath.Join(dir, name.Name()), `{"attempts":`+bad+`}`)
			}
		}
		if out := subagentStopRun(t, fresh, "executor", "a1", "", "", nil); out != "" {
			t.Fatal(out)
		}
		if len(state.ReadState(fresh, "s1").UnverifiedSubagents) != 1 {
			t.Fatal("corrupt counter did not terminate")
		}
	}
	fresh := t.TempDir()
	for n := 0; n < 3; n++ {
		subagentStopRun(t, fresh, "executor", "a1", "", "", nil)
	}
	path := state.StatePath(fresh, "s1")
	subagentStopPut(t, path, "{ corrupt")
	if out := subagentStopRun(t, fresh, "executor", "a1", "", "", nil); out != "" {
		t.Fatal(out)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{ corrupt" {
		t.Fatal("unreadable state overwritten")
	}
	if verdict := evidence.UnrecordableVerdictStatus(fresh, "s1"); !verdict.Present {
		t.Fatalf("missing marker: %+v", verdict)
	}
}

// Two agents that stop at the same moment each record their terminal verdict under the session lock:
// neither verdict is lost. Each stop runs in a process of its own, as a hook does, and the two are
// released together. The lock's wait budget is the oracle's (about 250 ms, state.WithSessionLock), so a
// holder the host has not scheduled for that long makes the other stop's commit give up before it runs:
// that stop records nothing, raises the corruption sentinel (unverifiedCorrupt) or, when the sentinel's
// lock gives up as well, writes the unrecordable marker. That give-up is a property of the host's load,
// not of the lock, so the case reruns such a stop (CRW-1172). Whether the commit gave up is observed per
// call, never read from the session: the child counts its own acquisition give-ups through the lock's
// give-up seam (state.sessionLockBeforeGiveUp), and a stop's lock calls are the commit's, then the
// sentinel's only if the commit failed. Two give-ups are both tiers giving up; one give-up without a
// marker of this agent's written by this call is the commit giving up and the sentinel recording; one
// give-up with this call's marker is a commit that held the lock and failed while the sentinel gave up;
// none is a commit that held the lock. Only the first two are rerun. A stop that returns without its
// verdict after its commit held the lock (a write that failed, or a verdict another commit overwrote)
// fails at once, whatever the other agent's stops left in the session; so does an error or a blocked
// stop, and the final read still requires both verdicts.
func TestSubagentStopConcurrentTerminalVerdicts(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	agents := []string{"racer-a", "racer-b"}
	for _, agent := range agents {
		for n := 1; n <= 3; n++ {
			subagentStopBlock(t, subagentStopRun(t, cwd, "executor", agent, "", "", nil), n)
		}
	}
	recorded := func(agent string) bool {
		for _, entry := range state.ReadState(cwd, "s1").UnverifiedSubagents {
			if entry.AgentID == agent {
				return true
			}
		}
		return false
	}
	markers := func(agent string) int {
		entries, _ := os.ReadDir(filepath.Join(cwd, crwdir.DirName, evidence.UnrecordableSubdir))
		prefix := state.SanitizeKey("s1") + "-" + state.SanitizeKey(agent) + "-"
		n := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), prefix) {
				n++
			}
		}
		return n
	}
	const rerunLimit = 50
	payloads := make(map[string][]byte, len(agents))
	for _, agent := range agents {
		raw, err := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "cwd": cwd, "session_id": "s1", "agent_type": "executor", "agent_id": agent})
		if err != nil {
			t.Fatal(err)
		}
		payloads[agent] = raw
	}
	first := make(map[string]*subagentStopChild, len(agents))
	for _, agent := range agents {
		child, err := subagentStopSpawn()
		if err != nil {
			t.Fatal(err)
		}
		first[agent] = child
	}
	release := make(chan struct{})
	var wg sync.WaitGroup
	errors := make(chan error, len(agents))
	for _, agent := range agents {
		raw, child := payloads[agent], first[agent]
		wg.Go(func() {
			for attempt := 0; attempt <= rerunLimit; attempt++ {
				if attempt == 0 {
					<-release
				} else {
					var err error
					if child, err = subagentStopSpawn(); err != nil {
						errors <- err
						return
					}
				}
				before := markers(agent)
				result, err := child.stop(raw)
				if err != nil {
					errors <- fmt.Errorf("%s: %w", agent, err)
					return
				}
				if result.Code != 0 || result.Stderr != "" {
					errors <- fmt.Errorf("%s: exit=%d stderr=%s", agent, result.Code, result.Stderr)
					return
				}
				if result.Out != "" {
					errors <- fmt.Errorf("%s: terminal stop blocked: %s", agent, result.Out)
					return
				}
				if recorded(agent) {
					return
				}
				marked := markers(agent) > before
				if commitGaveUp := result.GiveUps == 2 || result.GiveUps == 1 && !marked; !commitGaveUp {
					errors <- fmt.Errorf("%s: the stop returned without its verdict although its commit held the lock (lock give-ups %d, marker written %t): the commit failed or a committed verdict was lost", agent, result.GiveUps, marked)
					return
				}
				t.Logf("%s: stop %d gave up its commit lock (lock give-ups %d, marker written %t); rerun", agent, attempt+1, result.GiveUps, marked)
			}
			errors <- fmt.Errorf("%s: the verdict was not recorded after %d stops whose commit lock gave up", agent, rerunLimit+1)
		})
	}
	close(release)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if entries := state.ReadState(cwd, "s1").UnverifiedSubagents; len(entries) != 2 {
		t.Fatalf("lost verdict: %+v", entries)
	}
}

// subagentStopLockGiveUp is state.sessionLockBeforeGiveUp, the seam that runs on every session-lock
// acquisition that gives up, immediately before the give-up is returned. Only the child process of
// TestSubagentStopConcurrentTerminalVerdicts sets it; production and every other test leave it nil.
//
//go:linkname subagentStopLockGiveUp github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state.sessionLockBeforeGiveUp
var subagentStopLockGiveUp func()

const subagentStopChildEnv = "CRW_SUBAGENT_STOP_CHILD"

// subagentStopChildResult is what one stop in a child process answered and how many of its session-lock
// acquisitions gave up.
type subagentStopChildResult struct {
	Code    int    `json:"code"`
	Out     string `json:"out"`
	Stderr  string `json:"stderr"`
	GiveUps int    `json:"giveUps"`
}

// TestSubagentStopTerminalStopProcess is the child process of TestSubagentStopConcurrentTerminalVerdicts:
// it says ready, waits for its payload on stdin, runs one subagent-stop through the registered leg and
// prints the result, its own lock give-ups included, as one JSON line.
func TestSubagentStopTerminalStopProcess(t *testing.T) {
	if os.Getenv(subagentStopChildEnv) == "" {
		t.Skip("child process of TestSubagentStopConcurrentTerminalVerdicts")
	}
	var giveUps atomic.Int32
	subagentStopLockGiveUp = func() { giveUps.Add(1) }
	if _, err := os.Stdout.WriteString("ready\n"); err != nil {
		os.Exit(70)
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(70)
	}
	var out, stderr bytes.Buffer
	code := harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-verifying-evidence"},
		bytes.NewReader(raw), &out, &stderr, os.LookupEnv, harness.Legs())
	line, err := json.Marshal(subagentStopChildResult{Code: code, Out: out.String(), Stderr: stderr.String(), GiveUps: int(giveUps.Load())})
	if err != nil {
		os.Exit(70)
	}
	if _, err := os.Stdout.Write(append(line, '\n')); err != nil {
		os.Exit(70)
	}
	os.Exit(0)
}

// subagentStopChild is a started child process that has said it is ready and waits for its payload.
type subagentStopChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func subagentStopSpawn() (*subagentStopChild, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestSubagentStopTerminalStopProcess$")
	cmd.Env = append(os.Environ(), subagentStopChildEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	child := &subagentStopChild{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: &bytes.Buffer{}}
	cmd.Stderr = child.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if line, err := child.stdout.ReadString('\n'); err != nil || line != "ready\n" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("child not ready: %q %v; stderr %s", line, err, child.stderr)
	}
	return child, nil
}

// stop hands the child its payload and returns the one result line it prints.
func (c *subagentStopChild) stop(raw []byte) (subagentStopChildResult, error) {
	var result subagentStopChildResult
	_, werr := c.stdin.Write(raw)
	cerr := c.stdin.Close()
	line, rerr := c.stdout.ReadString('\n')
	if err := c.cmd.Wait(); err != nil || werr != nil || cerr != nil || rerr != nil {
		return result, fmt.Errorf("child failed: wait=%v write=%v close=%v read=%v; stdout %q stderr %s", err, werr, cerr, rerr, line, c.stderr)
	}
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		return result, fmt.Errorf("child result %q: %w", line, err)
	}
	return result, nil
}

func TestSubagentStopInternalErrorAndDirectPolicy(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	p := hook.SubagentStopPayload{Cwd: cwd, SessionID: "s1", AgentType: "executor", AgentID: "a1"}
	if out := hook.RunSubagentStopGate(p, func(string) string { return "off" }); out != "" {
		t.Fatal(out)
	}
	if evidence.ReadAttempts(cwd, "s1", "a1", "") != 0 {
		t.Fatal("disabled direct gate spent an attempt")
	}
	if out := hook.RunSubagentStopGate(p, func(string) string { panic("environment read failed") }); out != "" {
		t.Fatal(out)
	}
	entries := state.ReadState(cwd, "s1").UnverifiedSubagents
	if len(entries) != 1 || entries[0].Attempts != 3 || evidence.ReadAttempts(cwd, "s1", "a1", "") != 0 {
		t.Fatalf("internal-error verdict: %+v", entries)
	}
	// The nested catch also releases when no record can be written (oracle :526-528).
	p.Cwd = "\x00"
	if out := hook.RunSubagentStopGate(p, nil); out != "" {
		t.Fatal(out)
	}
}

func TestSubagentStopOmittedFieldsAndObserverIsolation(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "session_id": "s1", "cwd": cwd, "agent_type": "executor"})
	var out, stderr bytes.Buffer
	args := []string{"subagent-stop", "--leg", "subagent-stop-verifying-evidence"}
	if code := harness.Hook(context.Background(), args, strings.NewReader(string(raw)), &out, &stderr, os.LookupEnv, harness.Legs()); code != 0 {
		t.Fatal(code)
	}
	subagentStopBlock(t, out.String(), 1)
	// CRW-564 wired the observer's own row. The two stay separate registrations: the gate's leg answered above without touching the
	// observer's state, and the observer leaves executor and worker exits to the gate (reviewobserver_test.go, TestReviewObserverRoles).
	for _, leg := range harness.Legs() {
		if leg.ID == "subagent-stop-observing-review" && leg.Handle == nil {
			t.Fatal("observer row has no handler")
		}
	}
}

func TestSubagentStopReceiptRecoveryIOFailures(t *testing.T) {
	for _, failure := range []string{"held_lock", "counter_delete"} {
		t.Run(failure, func(t *testing.T) {
			cwd := subagentStopWorkspace(t)
			for n := 0; n < 4; n++ {
				subagentStopRun(t, cwd, "executor", "a1", "t1", "", nil)
			}
			subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/proof.md"), "verified")
			before, _ := os.ReadFile(state.StatePath(cwd, "s1"))
			if failure == "held_lock" {
				subagentStopPut(t, state.StatePath(cwd, "s1")+".lock", "held")
			} else {
				if os.Geteuid() == 0 {
					t.Skip("counter deletion permissions require a non-root process")
				}
				dir := filepath.Join(cwd, ".crw/evidence-attempts")
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0o700); err != nil {
						t.Error(err)
					}
				})
			}
			if out := subagentStopRun(t, cwd, "executor", "a1", "t1", "EVIDENCE_RECORDED: .crw/evidence/proof.md", nil); out != "" {
				t.Fatal(out)
			}
			if failure == "held_lock" {
				after, _ := os.ReadFile(state.StatePath(cwd, "s1"))
				if !bytes.Equal(before, after) || len(state.ReadState(cwd, "s1").UnverifiedSubagents) != 1 {
					t.Fatal("failed resolution lost verdict")
				}
			} else if !evidence.HasSpentBudget(cwd, "s1") || evidence.ReadAttempts(cwd, "s1", "a1", "t1") != 3 {
				t.Fatal("failed clear lost budget")
			}
		})
	}
}
