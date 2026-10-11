package spawn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// F14 (CRW-1207): the native spawn hook of a thread that a flow run has bound. These tests run the real hook (RunSpawnAttachHook)
// over binding files in a temporary home; the first four write the binding the way the run does, by a turn of the real agent
// backend on the fake App Server host, the others write the files by hand so that a mode, an owner, a link or a cut line can be
// made.

const (
	fbThreadA = "0190a000-0000-7000-8000-00000000000a"
	fbThreadB = "0190a000-0000-7000-8000-00000000000b"
	fbThreadC = "0190a000-0000-7000-8000-00000000000c"
	fbCall1   = "c_000000000001"
	fbCall2   = "c_000000000002"
)

var (
	fbExplorer = bound.Pair{Model: "explorer-model", Effort: "high"}
	fbReviewer = bound.Pair{Model: "reviewer-model", Effort: "xhigh"}
)

// fbRig is a home with a flow-bound directory and the hook's environment.
type fbRig struct {
	t    *testing.T
	hook *spawnHookRig
	home string
	dir  string
}

func newFBRig(t *testing.T) *fbRig {
	t.Helper()
	r := &fbRig{t: t, hook: spawnHookNewRig(t, nil, spawnHookCase{}), home: t.TempDir()}
	t.Setenv("HOME", r.home)
	r.dir = filepath.Join(r.home, ".local", "state", "crw", bound.DirName)
	return r
}

// mkdir makes the flow-bound directory with the mode.
func (r *fbRig) mkdir(mode os.FileMode) {
	r.t.Helper()
	spawnHookMust(r.t, os.MkdirAll(filepath.Dir(r.dir), 0o755))
	spawnHookMust(r.t, os.Mkdir(r.dir, 0o700))
	spawnHookMust(r.t, os.Chmod(r.dir, mode))
}

// write puts raw into the binding file of the thread with the mode.
func (r *fbRig) write(thread, raw string, mode os.FileMode) {
	r.t.Helper()
	if _, err := os.Lstat(r.dir); err != nil {
		r.mkdir(0o700)
	}
	path := filepath.Join(r.dir, thread+bound.FileSuffix)
	spawnHookMust(r.t, os.WriteFile(path, []byte(raw), mode))
	spawnHookMust(r.t, os.Chmod(path, mode))
}

func fbPairJSON(p bound.Pair) string {
	return `{"model":"` + p.Model + `","effort":"` + p.Effort + `"}`
}

// engineLine is the line of a turn of an engine call.
func engineLine(thread, call, role string, allowance int) string {
	return `{"run_id":"run-fb","thread_id":"` + thread + `","kind":"engine","call_id":"` + call + `","role":"` + role + `","nativeAllowance":` +
		strconv.Itoa(allowance) + `,"helperPairs":{"explorer":` + fbPairJSON(fbExplorer) + `},"policyDigest":"sha256:fb"}` + "\n"
}

func orchestratorLine(thread string) string {
	return `{"run_id":"run-fb","thread_id":"` + thread + `","kind":"orchestrator","nativeAllowance":null,"helperPairs":{"explorer":` + fbPairJSON(fbExplorer) +
		`,"reviewer":` + fbPairJSON(fbReviewer) + `},"policyDigest":"sha256:fb"}` + "\n"
}

func closedLine(thread string) string {
	return `{"run_id":"run-fb","thread_id":"` + thread + `","kind":"closed","nativeAllowance":null,"policyDigest":"sha256:fb"}` + "\n"
}

// bindEngine and bindOrchestrator write the files of a safe directory.
func (r *fbRig) bindEngine(thread, call, role string, allowance int) {
	r.t.Helper()
	r.write(thread, engineLine(thread, call, role, allowance), 0o600)
}

func (r *fbRig) bindOrchestrator(thread string) {
	r.t.Helper()
	r.write(thread, orchestratorLine(thread), 0o600)
}

// payload is a root thread's spawn event.
func (r *fbRig) payload(session, toolUse, toolInput string) string {
	id := ""
	if toolUse != "" {
		id = `,"tool_use_id":` + strconv.Quote(toolUse)
	}
	return `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":` + strconv.Quote(session) + `,"cwd":` + strconv.Quote(r.hook.ws) + id +
		`,"tool_input":` + toolInput + `}`
}

// normalize takes out of a hook answer what differs between two rigs that did the same: the nonce of a minted grant and the
// scratch paths.
func (r *fbRig) normalize(out string) string {
	out = strings.ReplaceAll(out, r.hook.skills, "{SKILLS}")
	out = strings.ReplaceAll(out, r.hook.ws, "{WS}")
	return regexp.MustCompile(`[a-f0-9]{64}`).ReplaceAllString(out, "N")
}

func (r *fbRig) run(payload string) string { return RunSpawnAttachHook(payload, r.hook.env) }

// spawn runs the hook on a root spawn of the thread.
func (r *fbRig) spawn(thread, toolUse, toolInput string) string {
	return r.run(r.payload(thread, toolUse, toolInput))
}

// fbDecision parses a hook answer: the permission decision, the deny reason and the updated input.
func fbDecision(t *testing.T, out string) (decision, reason string, updated pyjson.Object) {
	t.Helper()
	if out == "" {
		return "", "", nil
	}
	v, err := pyjson.Loads(out, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers})
	spawnHookMust(t, err)
	hso, _ := v.(pyjson.Object).Get("hookSpecificOutput").(pyjson.Object)
	decision, _ = hso.Get("permissionDecision").(string)
	reason, _ = hso.Get("permissionDecisionReason").(string)
	updated, _ = hso.Get("updatedInput").(pyjson.Object)
	return decision, reason, updated
}

func (r *fbRig) expectAllowed(out string) pyjson.Object {
	r.t.Helper()
	decision, reason, updated := fbDecision(r.t, out)
	if decision != "allow" || updated == nil {
		r.t.Fatalf("want an allow with an updatedInput, got %q (%s)", out, reason)
	}
	return updated
}

func (r *fbRig) expectRefused(out, code string) string {
	r.t.Helper()
	decision, reason, _ := fbDecision(r.t, out)
	if decision != "deny" || !strings.Contains(reason, code) {
		r.t.Fatalf("want a deny naming %s, got %q", code, out)
	}
	return reason
}

func fbPairOf(updated pyjson.Object) bound.Pair {
	model, _ := updated.Get("model").(string)
	effort, _ := updated.Get("reasoning_effort").(string)
	return bound.Pair{Model: model, Effort: effort}
}

func (r *fbRig) decisions(thread string) []bound.Decision {
	r.t.Helper()
	d, err := bound.ReadDecisions(r.dir, thread)
	spawnHookMust(r.t, err)
	return d
}

func (r *fbRig) claimCount(thread string) int {
	entries, err := os.ReadDir(filepath.Join(r.dir, bound.ClaimsDir, thread))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") && !strings.HasSuffix(e.Name(), ".answer") {
			n++
		}
	}
	return n
}

func (r *fbRig) grantFiles() int { return spawnHookCount(r.hook.tmp) }

// fbHost is the real agent backend on the fake host: its turns write the bindings the hook reads.
type fbHost struct {
	t      *testing.T
	srv    *fakehost.Server
	policy *engine.Policy
	runID  string
	cwd    string
}

func newFBHost(t *testing.T) *fbHost {
	t.Helper()
	h := &fbHost{t: t, srv: fakehost.Start(t), policy: engine.FakePolicy(), runID: "run-fb-host", cwd: t.TempDir()}
	h.srv.Threads().ThreadIDs(func(n int) string { return fmt.Sprintf("0190c000-0000-7000-8000-%012x", n) })
	return h
}

// backend is a backend of a run process, connected to the host.
func (h *fbHost) backend() *agent.Backend {
	h.t.Helper()
	threads := agent.NewThreads()
	client := appserver.New(h.srv.SocketPath, appserver.DefaultBounds, agent.ClientOption(threads))
	h.t.Cleanup(func() { _ = client.Close() })
	b, err := agent.NewHost(agent.Config{Client: client, Threads: threads, Bound: &agent.BoundConfig{RunID: h.runID, Policy: h.policy}})
	spawnHookMust(h.t, err)
	return b
}

// start makes the thread of an engine call.
func (h *fbHost) start(b *agent.Backend, role, call string) string {
	h.t.Helper()
	spec := h.policy.Roles[role]
	info, err := b.StartThread(context.Background(), engine.ThreadSpec{CallID: call, Role: role, Pair: spec.Pair, Sandbox: spec.Sandbox, Cwd: h.cwd})
	spawnHookMust(h.t, err)
	return info.ID
}

// turn runs turn k of a call on the thread, which writes the call's binding line before the host gets the turn.
func (h *fbHost) turn(b *agent.Backend, thread, role, call string, k int) {
	h.t.Helper()
	op := fmt.Sprintf("%s/t%d", call, k)
	_, err := b.RunTurn(context.Background(), engine.TurnSpec{CallID: call, Op: op, K: k, ThreadID: thread, Role: role, Pair: h.policy.Roles[role].Pair, Input: "crw-flow-op: " + op + "\n\nwork"})
	spawnHookMust(h.t, err)
}

// boundThread is a thread with its first turn run: a flow-bound engine thread.
func (h *fbHost) boundThread(role, call string) (*agent.Backend, string) {
	h.t.Helper()
	b := h.backend()
	thread := h.start(b, role, call)
	h.turn(b, thread, role, call, 1)
	return b, thread
}

const fbPolicyExplorerModel = "claude-fake-haiku"

func fbRolePair(h *fbHost, role string) bound.Pair {
	p := h.policy.Roles[role].Pair
	return bound.Pair{Model: p.Model, Effort: p.Effort}
}

// A helper that names no pair does not inherit its parent's: it gets the explorer pair of the run's policy.
func TestFlowBoundHelperInheritsNoParentPair(t *testing.T) {
	r := newFBRig(t)
	h := newFBHost(t)
	_, thread := h.boundThread("executor", fbCall1)
	out := r.spawn(thread, "tool-1", `{"agent_type":"explorer","message":"TASK: where is the parser?"}`)
	updated := r.expectAllowed(out)
	want := fbRolePair(h, "explorer")
	if got := fbPairOf(updated); got != want || want.Model != fbPolicyExplorerModel {
		t.Fatalf("the helper gets %+v, want the explorer pair %+v", got, want)
	}
	if updated.Get("message") != "TASK: where is the parser?" || updated.Get("agent_type") != "explorer" {
		t.Fatalf("the hook changed more than the pair: %v", updated)
	}
	d := r.decisions(thread)
	if len(d) != 1 || d[0].Decision != bound.DecisionAllowed || d[0].CallID != fbCall1 || d[0].ToolUseID != "tool-1" || d[0].RunID != h.runID ||
		d[0].RequestedModel != "" || d[0].RequestedEffort != "" || d[0].AppliedModel != want.Model || d[0].AppliedEffort != want.Effort || d[0].ThreadKind != bound.KindEngine {
		t.Fatalf("ledger %+v: the request is the parent's (nothing sent), the applied pair the role's", d)
	}
}

// A pair the caller names (low effort, another model) is replaced by the role pair, and the ledger keeps what the caller asked
// next to what the hook applied.
func TestFlowBoundExplicitLowReplacedByRolePair(t *testing.T) {
	r := newFBRig(t)
	h := newFBHost(t)
	_, thread := h.boundThread("executor", fbCall1)
	out := r.spawn(thread, "tool-1", `{"agent_type":"explorer","message":"TASK: q","model":"someone-elses","reasoning_effort":"low","fork_context":false}`)
	updated := r.expectAllowed(out)
	want := fbRolePair(h, "explorer")
	if got := fbPairOf(updated); got != want {
		t.Fatalf("the helper gets %+v, want %+v", got, want)
	}
	d := r.decisions(thread)
	if len(d) != 1 || d[0].RequestedModel != "someone-elses" || d[0].RequestedEffort != "low" || d[0].AppliedModel != want.Model || d[0].AppliedEffort != want.Effort {
		t.Fatalf("ledger %+v: requested, applied and (later) observed are three values", d)
	}
}

// On an engine thread the role is the engine call's, not the message's: a CRW-ROLE header does not choose another pair.
func TestFlowBoundEngineThreadIgnoresTheRoleHeader(t *testing.T) {
	r := newFBRig(t)
	r.bindEngine(fbThreadA, fbCall1, "reviewer", 1)
	updated := r.expectAllowed(r.spawn(fbThreadA, "tool-1", `{"agent_type":"explorer","message":"CRW-ROLE: reviewer\nTASK: review it"}`))
	if got := fbPairOf(updated); got != fbExplorer {
		t.Fatalf("pair %+v, want the explorer pair %+v", got, fbExplorer)
	}
}

// A full-history fork keeps the parent's model and effort, so its pair cannot be set: it is refused with the way out, recorded,
// and it costs the call nothing.
func TestFlowBoundFullHistoryForkRefusedWithGuidance(t *testing.T) {
	r := newFBRig(t)
	h := newFBHost(t)
	_, thread := h.boundThread("executor", fbCall1)
	for i, input := range []string{
		`{"agent_type":"explorer","message":"TASK: q","fork_context":true}`,
		`{"agent_type":"explorer","task_name":"t","fork_turns":"all","message":"TASK: q"}`,
		`{"agent_type":"explorer","task_name":"t","message":"TASK: q"}`,
	} {
		reason := r.expectRefused(r.spawn(thread, fmt.Sprintf("fork-%d", i), input), bound.ReasonFullHistoryFork)
		if !strings.Contains(reason, "new explorer") || !strings.Contains(reason, "question") {
			t.Errorf("input %d: the refusal gives no guidance: %q", i, reason)
		}
	}
	if r.claimCount(thread) != 0 {
		t.Fatal("a refused fork used the call's allowance")
	}
	d := r.decisions(thread)
	if len(d) != 3 || d[0].Decision != bound.DecisionRefused || d[0].Reason != bound.ReasonFullHistoryFork || d[2].ToolUseID != "fork-2" {
		t.Fatalf("ledger %+v", d)
	}
	r.expectAllowed(r.spawn(thread, "fresh", `{"agent_type":"explorer","message":"TASK: q","fork_context":false}`))
}

// A resumed run keeps the role pair: a new turn of the thread (another call, or the same call again after a restart) writes its
// own line and the hook reads the last one.
func TestFlowBoundResumeKeepsRolePair(t *testing.T) {
	r := newFBRig(t)
	h := newFBHost(t)
	b1, thread := h.boundThread("executor", fbCall1)
	want := fbRolePair(h, "explorer")
	if got := fbPairOf(r.expectAllowed(r.spawn(thread, "tool-1", `{"agent_type":"explorer","message":"TASK: one","reasoning_effort":"low"}`))); got != want {
		t.Fatalf("before the resume: %+v", got)
	}
	// the run process is gone; its successor resumes the run and sends the same call's turn again on a new connection
	b2 := h.backend()
	h.turn(b2, thread, "executor", fbCall1, 2)
	if r.expectRefused(r.spawn(thread, "tool-2", `{"agent_type":"explorer","message":"TASK: two"}`), bound.ReasonNativeLimitRefused) == "" {
		t.Fatal("no reason")
	}
	// the thread goes on with a new call: a new line, a new allowance, the same role pair
	h.turn(b2, thread, "executor", fbCall2, 1)
	if got := fbPairOf(r.expectAllowed(r.spawn(thread, "tool-3", `{"agent_type":"explorer","message":"TASK: three","model":"x","reasoning_effort":"low"}`))); got != want {
		t.Fatalf("after the resume: %+v", got)
	}
	_ = b1
	if d := r.decisions(thread); len(d) != 3 || d[0].CallID != fbCall1 || d[1].Reason != bound.ReasonNativeLimitRefused || d[2].CallID != fbCall2 {
		t.Fatalf("ledger %+v", d)
	}
}

// A directory or file under the flow-bound directory that cannot be trusted refuses the spawn: it never falls back to the
// ordinary path, which would let a bound thread spawn at will.
func TestFlowBoundLookupUnavailableDenies(t *testing.T) {
	good := engineLine(fbThreadA, fbCall1, "executor", 1)
	input := `{"agent_type":"explorer","message":"TASK: q"}`
	cases := []struct {
		name   string
		detail string
		setup  func(r *fbRig)
	}{
		{"directory 0755", bound.ReasonUnsafeDir, func(r *fbRig) { r.mkdir(0o755); r.write(fbThreadA, good, 0o600) }},
		{"directory 0770", bound.ReasonUnsafeDir, func(r *fbRig) { r.mkdir(0o770); r.write(fbThreadA, good, 0o600) }},
		{"directory of another owner", bound.ReasonUnsafeDir, func(r *fbRig) {
			r.write(fbThreadA, good, 0o600)
			uid := bound.UID
			bound.UID = func() int { return os.Getuid() + 1 }
			r.t.Cleanup(func() { bound.UID = uid })
		}},
		{"directory is a symlink", bound.ReasonUnsafeDir, func(r *fbRig) {
			elsewhere := filepath.Join(r.home, "elsewhere")
			spawnHookMust(r.t, os.Mkdir(elsewhere, 0o700))
			spawnHookMust(r.t, os.WriteFile(filepath.Join(elsewhere, fbThreadA+bound.FileSuffix), []byte(good), 0o600))
			spawnHookMust(r.t, os.MkdirAll(filepath.Dir(r.dir), 0o755))
			spawnHookMust(r.t, os.Symlink(elsewhere, r.dir))
		}},
		{"directory is a file", bound.ReasonUnsafeDir, func(r *fbRig) {
			spawnHookMust(r.t, os.MkdirAll(filepath.Dir(r.dir), 0o755))
			spawnHookMust(r.t, os.WriteFile(r.dir, nil, 0o600))
		}},
		{"file is a symlink", bound.ReasonUnsafeFile, func(r *fbRig) {
			r.mkdir(0o700)
			target := filepath.Join(r.home, "real.jsonl")
			spawnHookMust(r.t, os.WriteFile(target, []byte(good), 0o600))
			spawnHookMust(r.t, os.Symlink(target, filepath.Join(r.dir, fbThreadA+bound.FileSuffix)))
		}},
		{"file 0644", bound.ReasonUnsafeFile, func(r *fbRig) { r.write(fbThreadA, good, 0o644) }},
		{"file 0660", bound.ReasonUnsafeFile, func(r *fbRig) { r.write(fbThreadA, good, 0o660) }},
		{"file without read permission", "", func(r *fbRig) { r.write(fbThreadA, good, 0o000) }},
		{"file is a directory", bound.ReasonUnsafeFile, func(r *fbRig) {
			r.mkdir(0o700)
			spawnHookMust(r.t, os.Mkdir(filepath.Join(r.dir, fbThreadA+bound.FileSuffix), 0o700))
		}},
		{"last line cut short", bound.ReasonTruncated, func(r *fbRig) { r.write(fbThreadA, good+good[:len(good)/2], 0o600) }},
		{"empty file", bound.ReasonTruncated, func(r *fbRig) { r.write(fbThreadA, "", 0o600) }},
		{"last line is not JSON", bound.ReasonInvalid, func(r *fbRig) { r.write(fbThreadA, good+"not json\n", 0o600) }},
		{"last line of another thread", bound.ReasonInvalid, func(r *fbRig) { r.write(fbThreadA, engineLine(fbThreadB, fbCall1, "executor", 1), 0o600) }},
		{"last line without a helper pair", bound.ReasonInvalid, func(r *fbRig) {
			r.write(fbThreadA, `{"run_id":"r","thread_id":"`+fbThreadA+`","kind":"engine","call_id":"`+fbCall1+`","role":"executor","nativeAllowance":1,"helperPairs":{},"policyDigest":"d"}`+"\n", 0o600)
		}},
		{"last line of an unknown kind", bound.ReasonInvalid, func(r *fbRig) {
			r.write(fbThreadA, strings.Replace(good, `"kind":"engine"`, `"kind":"nested"`, 1), 0o600)
		}},
		{"allowance 2", bound.ReasonInvalid, func(r *fbRig) {
			r.write(fbThreadA, strings.Replace(good, `"nativeAllowance":1`, `"nativeAllowance":2`, 1), 0o600)
		}},
		{"call id that cannot name a claim", bound.ReasonInvalid, func(r *fbRig) { r.write(fbThreadA, engineLine(fbThreadA, "../escape", "executor", 1), 0o600) }},
	}
	standalone := ""
	{
		r := newFBRig(t)
		standalone = r.spawn(fbThreadA, "tool-1", input)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newFBRig(t)
			c.setup(r)
			// every kind of spawn is refused: an explorer, a worker and one without a tool use id
			for _, in := range []string{input, `{"agent_type":"worker","message":"go"}`, `{"message":"go"}`} {
				for _, tool := range []string{"tool-1", ""} {
					out := r.spawn(fbThreadA, tool, in)
					reason := r.expectRefused(out, bound.ReasonFlowBoundUnavailable)
					if c.detail != "" && !strings.Contains(reason, c.detail) {
						t.Errorf("the refusal does not name %s: %q", c.detail, reason)
					}
					if out == standalone {
						t.Fatalf("the spawn fell through to the ordinary path")
					}
				}
			}
			// a session that is not a thread id does not consult the directory: it stays an ordinary one
			if out := r.spawn("not-a-thread-id", "tool-9", input); strings.Contains(out, bound.ReasonFlowBoundUnavailable) {
				t.Errorf("a session id of another shape was refused: %q", out)
			}
		})
	}
}

// A thread no run registered is an ordinary thread, whether there is no flow-bound directory, the directory holds only other
// threads' files, or the session id is not a thread id: the answers are byte for byte the same.
func TestFlowBoundUnregisteredThreadStandalone(t *testing.T) {
	inputs := []string{
		`{"agent_type":"explorer","message":"TASK: q"}`,
		`{"agent_type":"worker","message":"implement it","reasoning_effort":"low"}`,
		`{"message":"please review this diff"}`,
		`{"agent_type":"explorer","task_name":"t","fork_turns":"none","message":"look"}`,
		`{"agent_type":"explorer","message":"CRW-SUBSPAWN-ALLOWED coordinate"}`,
	}
	run := func(populate bool, session string) []string {
		r := newFBRig(t)
		if populate {
			r.bindEngine(fbThreadB, fbCall1, "executor", 1) // another thread's binding in a safe directory
			r.bindOrchestrator(fbThreadC)
		}
		var out []string
		for i, in := range inputs {
			out = append(out, r.normalize(r.spawn(session, fmt.Sprintf("tool-%d", i), in)))
		}
		return out
	}
	for _, session := range []string{fbThreadA, "rec-s1", "", "ABCDEF01-0000-7000-8000-00000000000A", "../" + fbThreadB} {
		bare, populated := run(false, session), run(true, session)
		for i := range bare {
			if bare[i] != populated[i] {
				t.Errorf("session %q input %d: the answers differ\n no directory: %q\n populated:    %q", session, i, bare[i], populated[i])
			}
		}
		for i, got := range bare {
			if strings.Contains(got, "flow_bound") || strings.Contains(got, "native_") {
				t.Errorf("session %q input %d: a flow answer for an unregistered thread: %q", session, i, got)
			}
		}
	}
}

// The call has one native helper. Another spawn (another tool use id, or the same id with another input) is over the limit, and a
// spawn that cannot show its tool use id cannot be told from a redelivery, so it is refused as unavailable.
func TestFlowBoundSecondSpawnRefused(t *testing.T) {
	r := newFBRig(t)
	r.bindEngine(fbThreadA, fbCall1, "executor", 1)
	first := `{"agent_type":"explorer","message":"TASK: one"}`
	r.expectAllowed(r.spawn(fbThreadA, "tool-1", first))
	r.expectRefused(r.spawn(fbThreadA, "tool-2", `{"agent_type":"explorer","message":"TASK: two"}`), bound.ReasonNativeLimitRefused)
	r.expectRefused(r.spawn(fbThreadA, "tool-2", first), bound.ReasonNativeLimitRefused)
	r.expectRefused(r.spawn(fbThreadA, "tool-1", `{"agent_type":"explorer","message":"TASK: one, differently"}`), bound.ReasonNativeLimitRefused)
	r.expectRefused(r.spawn(fbThreadA, "", `{"agent_type":"explorer","message":"TASK: no id"}`), bound.ReasonFlowBoundUnavailable)
	r.expectRefused(r.spawn(fbThreadA, "", first), bound.ReasonFlowBoundUnavailable)
	// without a claim either: a fresh call, no tool use id
	r.bindEngine(fbThreadB, fbCall2, "executor", 1)
	r.expectRefused(r.spawn(fbThreadB, "", first), bound.ReasonFlowBoundUnavailable)
	if r.claimCount(fbThreadB) != 0 {
		t.Fatal("a refused spawn made a claim")
	}
	if r.claimCount(fbThreadA) != 1 {
		t.Fatalf("%d claims for one call", r.claimCount(fbThreadA))
	}
}

// The same event delivered again gets its stored answer, and so does the answer's updatedInput applied again; the call still has
// one claim, and the orchestrator's pair injection is stable too.
func TestFlowBoundSameEventRedeliveryReplays(t *testing.T) {
	r := newFBRig(t)
	r.bindEngine(fbThreadA, fbCall1, "executor", 1)
	r.bindOrchestrator(fbThreadB)
	for _, c := range []struct {
		name, thread, input string
		claims              int
	}{
		{"engine thread", fbThreadA, `{"agent_type":"explorer","message":"TASK: one","reasoning_effort":"low"}`, 1},
		{"orchestrator thread", fbThreadB, `{"agent_type":"explorer","message":"CRW-ROLE: reviewer\nTASK: one","model":"x"}`, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			first := r.spawn(c.thread, "tool-1", c.input)
			updated := r.expectAllowed(first)
			again := r.spawn(c.thread, "tool-1", c.input)
			if again != first {
				t.Fatalf("the original input delivered again:\n got %q\nwant %q", again, first)
			}
			reapplied := r.spawn(c.thread, "tool-1", spawnHookRouteStringify(updated))
			second := r.expectAllowed(reapplied)
			if !reflect.DeepEqual(second, updated) {
				t.Fatalf("the answer's updatedInput applied again changed the input:\n got %v\nwant %v", second, updated)
			}
			third := r.expectAllowed(r.spawn(c.thread, "tool-1", spawnHookRouteStringify(second)))
			if !reflect.DeepEqual(third, updated) {
				t.Fatalf("a third application changed the input: %v", third)
			}
			if got := r.claimCount(c.thread); got != c.claims {
				t.Fatalf("%d claims, want %d", got, c.claims)
			}
			if d := r.decisions(c.thread); len(d) != 1 || d[0].Decision != bound.DecisionAllowed {
				t.Fatalf("a redelivery made ledger entries: %+v", d)
			}
		})
	}
}

// Two deliveries of one event at once get the same allowing answer; of two different spawns at once, one is allowed.
func TestFlowBoundSameEventConcurrentDelivery(t *testing.T) {
	r := newFBRig(t)
	input := `{"agent_type":"explorer","message":"TASK: one"}`
	t.Run("same event", func(t *testing.T) {
		for round := range 10 {
			thread := fmt.Sprintf("0190d000-0000-7000-8000-%012x", round)
			r.bindEngine(thread, fbCall1, "executor", 1)
			answers := make([]string, 6)
			var wg sync.WaitGroup
			for i := range answers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					answers[i] = r.spawn(thread, "tool-1", input)
				}()
			}
			wg.Wait()
			for i, a := range answers {
				if a != answers[0] {
					t.Fatalf("round %d: delivery %d differs:\n%q\n%q", round, i, a, answers[0])
				}
			}
			r.expectAllowed(answers[0])
			if r.claimCount(thread) != 1 {
				t.Fatalf("%d claims", r.claimCount(thread))
			}
		}
	})
	t.Run("different spawns", func(t *testing.T) {
		for round := range 10 {
			thread := fmt.Sprintf("0190e000-0000-7000-8000-%012x", round)
			r.bindEngine(thread, fbCall1, "executor", 1)
			answers := make([]string, 4)
			var wg sync.WaitGroup
			for i := range answers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					answers[i] = r.spawn(thread, fmt.Sprintf("tool-%d", i), input)
				}()
			}
			wg.Wait()
			allowed, refused := 0, 0
			for _, a := range answers {
				switch decision, reason, _ := fbDecision(t, a); {
				case decision == "allow":
					allowed++
				case strings.Contains(reason, bound.ReasonNativeLimitRefused):
					refused++
				}
			}
			if allowed != 1 || refused != len(answers)-1 {
				t.Fatalf("round %d: %d allowed, %d refused of %q", round, allowed, refused, answers)
			}
		}
	})
}

// A hook that stopped after the claim and before it answered leaves a claim and no answer file: the same event delivered again
// gets the answer an uninterrupted run gives and stores it, and any other spawn is over the limit.
func TestFlowBoundClaimWithoutAnswerRecovers(t *testing.T) {
	r := newFBRig(t)
	input := `{"agent_type":"explorer","message":"TASK: one","reasoning_effort":"low"}`
	r.bindEngine(fbThreadB, fbCall1, "executor", 1)
	want := r.spawn(fbThreadB, "tool-1", input) // an uninterrupted run
	r.expectAllowed(want)
	r.bindEngine(fbThreadA, fbCall1, "executor", 1)
	// the claim exists, the answer does not
	canonical := spawnHookRouteStringify(spawnHookMustLoad(t, input))
	created, err := bound.CreateClaim(r.dir, fbThreadA, fbCall1, bound.Claim{ToolUseID: "tool-1", Input: canonical})
	if err != nil || !created {
		t.Fatalf("claim: %v %v", created, err)
	}
	if _, present, _ := bound.ReadAnswer(r.dir, fbThreadA, fbCall1); present {
		t.Fatal("an answer before the hook ran")
	}
	r.expectRefused(r.spawn(fbThreadA, "tool-2", `{"agent_type":"explorer","message":"TASK: two"}`), bound.ReasonNativeLimitRefused)
	if got := r.spawn(fbThreadA, "tool-1", input); got != want {
		t.Fatalf("the recovered answer\n got %q\nwant %q", got, want)
	}
	if stored, present, err := bound.ReadAnswer(r.dir, fbThreadA, fbCall1); err != nil || !present || stored != want {
		t.Fatalf("stored answer %q %v %v", stored, present, err)
	}
	// the stored answer's updatedInput is the same event too
	if got := r.spawn(fbThreadA, "tool-1", spawnHookRouteStringify(fbUpdatedOf(t, want))); got != want {
		t.Fatalf("the updatedInput after the recovery: %q", got)
	}
	// and again with the answer lost once more, delivered as the answer's updatedInput
	spawnHookMust(t, os.Remove(filepath.Join(r.dir, bound.ClaimsDir, fbThreadA, bound.AnswerFile(fbCall1))))
	if got := r.spawn(fbThreadA, "tool-1", spawnHookRouteStringify(fbUpdatedOf(t, want))); got != want {
		t.Fatalf("the updatedInput with no stored answer: %q", got)
	}
}

func spawnHookMustLoad(t *testing.T, raw string) pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(raw, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers})
	spawnHookMust(t, err)
	return v.(pyjson.Object)
}

func fbUpdatedOf(t *testing.T, answer string) pyjson.Object {
	t.Helper()
	_, _, updated := fbDecision(t, answer)
	if updated == nil {
		t.Fatalf("no updatedInput in %q", answer)
	}
	return updated
}

// An engine thread's helper is an explorer: any other agent_type, or none, is refused whatever it asks for, and the refusal
// costs the call nothing.
func TestFlowBoundNonExplorerTypeRefused(t *testing.T) {
	r := newFBRig(t)
	r.bindEngine(fbThreadA, fbCall1, "executor", 1)
	for i, input := range []string{
		`{"agent_type":"worker","message":"TASK: q"}`,
		`{"agent_type":"executor","message":"TASK: q"}`,
		`{"agent_type":"default","message":"TASK: q"}`,
		`{"agent_type":"architect","message":"TASK: q"}`,
		`{"agent_type":"reviewer","message":"TASK: q"}`,
		`{"message":"TASK: q"}`,
		`{"agent_type":null,"message":"TASK: q"}`,
		`{"agent_type":5,"message":"TASK: q"}`,
		`{"agent_type":"Explorer","message":"TASK: q"}`,
		`{"agent_type":"explorer ","message":"TASK: q"}`,
		`{"agent_type":"worker","message":"CRW-SUBSPAWN-ALLOWED TASK: q"}`,
	} {
		for _, tool := range []string{fmt.Sprintf("tool-%d", i), ""} {
			r.expectRefused(r.spawn(fbThreadA, tool, input), bound.ReasonNativeTypeRefused)
		}
	}
	if r.claimCount(fbThreadA) != 0 {
		t.Fatal("a refused type used the allowance")
	}
	r.expectRefused(r.run(`{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"`+fbThreadA+`","tool_use_id":"x","tool_input":"nope"}`), bound.ReasonNativeTypeRefused)
	r.expectAllowed(r.spawn(fbThreadA, "tool-ok", `{"agent_type":"explorer","message":"TASK: q"}`))
	for _, d := range r.decisions(fbThreadA) {
		if d.Decision == bound.DecisionRefused && d.Reason != bound.ReasonNativeTypeRefused {
			t.Errorf("ledger entry %+v", d)
		}
	}
}

// A call that may spawn nothing (an explorer call, the QA capture call) has its first spawn refused.
func TestFlowBoundZeroAllowanceRefused(t *testing.T) {
	for _, c := range []struct{ name, role string }{{"explorer call", "explorer"}, {"SP-QA-1 call", "executor"}} {
		t.Run(c.name, func(t *testing.T) {
			r := newFBRig(t)
			r.bindEngine(fbThreadA, fbCall1, c.role, 0)
			r.expectRefused(r.spawn(fbThreadA, "tool-1", `{"agent_type":"explorer","message":"TASK: q"}`), bound.ReasonNativeLimitRefused)
			r.expectRefused(r.spawn(fbThreadA, "tool-1", `{"agent_type":"explorer","message":"TASK: q"}`), bound.ReasonNativeLimitRefused)
			if r.claimCount(fbThreadA) != 0 {
				t.Fatal("a claim for a call that may spawn nothing")
			}
		})
	}
}

// CRW-SUBSPAWN-ALLOWED mints no grant on a bound thread, engine or orchestrator, and a subagent of a bound session is a leaf.
func TestFlowBoundRecursionTokenNotMinted(t *testing.T) {
	r := newFBRig(t)
	r.bindEngine(fbThreadA, fbCall1, "executor", 1)
	r.bindOrchestrator(fbThreadB)
	for i, c := range []struct{ thread, input string }{
		{fbThreadA, `{"agent_type":"explorer","message":"CRW-SUBSPAWN-ALLOWED coordinate one helper"}`},
		{fbThreadA, `{"agent_type":"worker","message":"CRW-SUBSPAWN-ALLOWED coordinate one helper"}`},
		{fbThreadB, `{"agent_type":"explorer","message":"CRW-SUBSPAWN-ALLOWED coordinate one helper"}`},
		{fbThreadB, `{"agent_type":"explorer","task_name":"t","fork_turns":"none","message":"CRW-SUBSPAWN-ALLOWED coordinate"}`},
		{fbThreadB, `{"agent_type":"explorer","items":[{"type":"text","text":"CRW-SUBSPAWN-ALLOWED coordinate"}]}`},
	} {
		out := r.spawn(c.thread, fmt.Sprintf("tool-%d", i), c.input)
		if strings.Contains(out, "CRW-SUBSPAWN-GRANT") || strings.Contains(out, "COORDINATOR") || strings.Contains(out, "coordinator") {
			t.Errorf("%s: a grant or a coordinator guard: %q", c.input, out)
		}
		if n := r.grantFiles(); n != 0 {
			t.Fatalf("%s: %d grant files", c.input, n)
		}
	}
	// a subagent of the bound session (a child that spawns) is denied as any leaf
	child := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"` + fbThreadA + `","agent_id":"child","agent_type":"explorer","cwd":` +
		strconv.Quote(r.hook.ws) + `,"tool_use_id":"child-1","tool_input":{"agent_type":"explorer","message":"TASK: q"}}`
	if _, reason, _ := fbDecision(t, r.run(child)); !strings.Contains(reason, "LEAF-TOPOLOGY-01") {
		t.Errorf("a subagent's spawn on a bound session: %q", reason)
	}
}

// On the orchestrator thread of an open run only an explorer helper is allowed, and its role is the CRW-ROLE line of the message
// header: reviewer gets the reviewer pair, explorer or no header the explorer pair, architect is refused. Keywords never choose.
// A closed run's thread is an ordinary one.
func TestOrchestratorHelperTypeMapping(t *testing.T) {
	r := newFBRig(t)
	r.bindOrchestrator(fbThreadA)
	cases := []struct {
		name, input string
		want        bound.Pair
		refuse      string
	}{
		{"reviewer header", `{"agent_type":"explorer","message":"CRW-ROLE: reviewer\nTASK: look at the diff"}`, fbReviewer, ""},
		{"reviewer header after other header lines", `{"agent_type":"explorer","message":"scope: x\nCRW-ROLE: reviewer\nTASK: look"}`, fbReviewer, ""},
		{"reviewer header in items", `{"agent_type":"explorer","items":[{"type":"text","text":"CRW-ROLE: reviewer\nTASK: look"}]}`, fbReviewer, ""},
		{"reviewer header in a v2 spawn", `{"agent_type":"explorer","task_name":"t","fork_turns":"none","message":"CRW-ROLE: reviewer\nTASK: look"}`, fbReviewer, ""},
		{"review keywords, no header", `{"agent_type":"explorer","message":"review this diff"}`, fbExplorer, ""},
		{"review keywords after TASK", `{"agent_type":"explorer","message":"TASK: review this diff\nCRW-ROLE: reviewer"}`, fbExplorer, ""},
		{"explorer header", `{"agent_type":"explorer","message":"CRW-ROLE: explorer\nTASK: review this diff"}`, fbExplorer, ""},
		{"no header", `{"agent_type":"explorer","message":"TASK: where is it"}`, fbExplorer, ""},
		{"architect header", `{"agent_type":"explorer","message":"CRW-ROLE: architect\nTASK: design it"}`, bound.Pair{}, bound.ReasonNativeTypeRefused},
		{"worker", `{"agent_type":"worker","message":"CRW-ROLE: reviewer\nTASK: x"}`, bound.Pair{}, bound.ReasonNativeTypeRefused},
		{"reviewer agent type", `{"agent_type":"reviewer","message":"TASK: x"}`, bound.Pair{}, bound.ReasonNativeTypeRefused},
		{"no agent type", `{"message":"TASK: x"}`, bound.Pair{}, bound.ReasonNativeTypeRefused},
		{"explorer with a model and effort of its own", `{"agent_type":"explorer","message":"CRW-ROLE: reviewer\nTASK: x","model":"m","reasoning_effort":"low"}`, fbReviewer, ""},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := r.spawn(fbThreadA, fmt.Sprintf("tool-%d", i), c.input)
			if c.refuse != "" {
				reason := r.expectRefused(out, c.refuse)
				if !strings.Contains(reason, "crw flow") {
					t.Errorf("the refusal does not send the work to run(crw flow): %q", reason)
				}
				return
			}
			if got := fbPairOf(r.expectAllowed(out)); got != c.want {
				t.Fatalf("pair %+v, want %+v", got, c.want)
			}
		})
	}
	// no count limit beyond the host's: every allowed case above was a spawn of the same thread
	if r.claimCount(fbThreadA) != 0 {
		t.Fatal("the orchestrator's helpers were claimed")
	}
	// after the closed line the thread is an ordinary one: the answer is what a thread no run registered gets
	other := newFBRig(t)
	want := other.spawn(fbThreadA, "tool-x", `{"agent_type":"worker","message":"CRW-ROLE: reviewer\nTASK: x"}`)
	r.write(fbThreadA, orchestratorLine(fbThreadA)+closedLine(fbThreadA), 0o600)
	got := r.spawn(fbThreadA, "tool-x", `{"agent_type":"worker","message":"CRW-ROLE: reviewer\nTASK: x"}`)
	if got != want || strings.Contains(got, "native_type_refused") {
		t.Fatalf("after the closed line:\n got %q\nwant %q", got, want)
	}
}

// The ordinary path is what it was: the recorded oracle steps give the same answers with no flow-bound directory and with a
// safe directory that holds only other threads' files, and InferRole keeps its order (the header, then the explorer type, then the
// review keywords).
func TestStandaloneHookUnchanged(t *testing.T) {
	fixture := spawnHookReadFixture(t)
	tid := "0190f000-0000-7000-8000-0000000000ff"
	grant := regexp.MustCompile(`\[CRW-SUBSPAWN-GRANT:([a-f0-9]{64})\]`)
	replay := func(t *testing.T, c spawnHookCase, populate bool) []string {
		r := newFBRig(t)
		rig := spawnHookNewRig(t, fixture.Skills, c)
		r.hook = rig
		if populate {
			r.bindEngine(fbThreadB, fbCall1, "executor", 1)
			r.bindOrchestrator(fbThreadC)
		}
		var outs []string
		for _, step := range c.Steps {
			input := rig.expand(spawnHookLoad(t, step.Input)).(pyjson.Object)
			if _, ok := input.Get("session_id").(string); ok {
				input = input.Set("session_id", tid) // a thread id no run registered
			}
			out := RunSpawnAttachHook(spawnHookRouteStringify(input), rig.env)
			for _, m := range grant.FindAllStringSubmatch(out, -1) {
				rig.nonces = append(rig.nonces, m[1])
			}
			outs = append(outs, r.normalize(out))
		}
		return outs
	}
	steps := 0
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			bare, populated := replay(t, c, false), replay(t, c, true)
			steps += len(bare)
			if !reflect.DeepEqual(bare, populated) {
				t.Fatalf("answers differ with a populated flow-bound directory:\n%q\n%q", bare, populated)
			}
		})
	}
	if steps == 0 {
		t.Fatal("no steps replayed")
	}
	for _, c := range []struct {
		agentType any
		message   string
		want      string
	}{
		{"explorer", "review this diff", "explorer"},
		{"explorer", "CRW-ROLE: reviewer\nTASK: x", "reviewer"},
		{nil, "review this diff", "reviewer"},
		{nil, "TASK: where is it", "explorer"},
		{"worker", "CRW-ROLE: reviewer\nTASK: x", "executor"},
		{"explorer", "CRW-ROLE: architect\nTASK: x", "architect"},
	} {
		if got := InferRole(c.agentType, c.message); string(got) != c.want {
			t.Errorf("InferRole(%v, %q) = %s, want %s", c.agentType, c.message, got, c.want)
		}
	}
}
