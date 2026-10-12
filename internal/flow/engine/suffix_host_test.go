package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// These tests run the engine on the App Server backend over the fake host, with temporary git repositories: the suffix of a
// resume rebuilds files, forks threads and keeps old worktrees, and only real git and real thread requests show that.

const header = "export const meta = { name: 't' }\n"

// isolate points every git command of the test at its own home and no global or system configuration.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lineageOf(m map[string]any) int {
	switch n := m["lineage"].(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// hookJournal is the in-memory journal with a hook that sees every row before it is appended, and a switch that drops every
// later row: the process that wrote them is dead.
type hookJournal struct {
	*engine.MemoryJournal
	mu     sync.Mutex
	before func(engine.Event)
	after  func(engine.Event)
	frozen bool
}

func (j *hookJournal) Append(e engine.Event) engine.Event {
	j.mu.Lock()
	before, after, frozen := j.before, j.after, j.frozen
	j.mu.Unlock()
	if before != nil {
		before(e)
	}
	if frozen {
		return e
	}
	out := j.MemoryJournal.Append(e)
	if after != nil {
		after(out)
	}
	return out
}

func (j *hookJournal) freeze() {
	j.mu.Lock()
	j.frozen = true
	j.mu.Unlock()
}

func (j *hookJournal) thaw() {
	j.mu.Lock()
	j.frozen = false
	j.before, j.after = nil, nil
	j.mu.Unlock()
}

func (j *hookJournal) onBefore(fn func(engine.Event)) {
	j.mu.Lock()
	j.before = fn
	j.mu.Unlock()
}

func (j *hookJournal) onAfter(fn func(engine.Event)) {
	j.mu.Lock()
	j.after = fn
	j.mu.Unlock()
}

// env is a fake host, a temporary repository and the backend of one run process on both.
type env struct {
	t       *testing.T
	host    *fakehost.Server
	threads *fakehost.Threads
	client  *appserver.Client
	backend *agent.Backend
	owned   *agent.Threads
	alloc   *agent.Allocator
	journal *hookJournal
	policy  *engine.Policy
	root    string
	repo    string
	base    string
	state   string
	runID   string
	budget  int64
	// cancel ends the run that is running (the process is killed).
	cancel context.CancelFunc
	alive  func(owner string) bool
	owner  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	isolate(t)
	e := &env{t: t, runID: "run-f07", policy: engine.FakePolicy(), journal: &hookJournal{MemoryJournal: engine.NewMemoryJournal()}, owner: "proc-1"}
	e.root = t.TempDir()
	e.repo = filepath.Join(e.root, "repo")
	writeFile(t, filepath.Join(e.repo, "a.txt"), "one\n")
	git(t, e.repo, "init", "-q", "-b", "main")
	git(t, e.repo, "add", "-A")
	git(t, e.repo, "commit", "-q", "-m", "base")
	e.base = git(t, e.repo, "rev-parse", "HEAD")
	e.state = filepath.Join(e.root, "state")
	e.startHost()
	e.spawn(e.owner)
	return e
}

func (e *env) startHost() {
	e.host = fakehost.Start(e.t)
	e.threads = e.host.Threads()
	e.threads.OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: "done"} })
}

// spawn starts a run process: a client on the host and a backend on it, with the allocator of the run.
func (e *env) spawn(owner string) {
	e.t.Helper()
	if e.client != nil {
		_ = e.client.Close()
	}
	prev := e.owner
	e.owner = owner
	e.alive = func(o string) bool { return o == owner && o != prev }
	e.owned = agent.NewThreads()
	e.client = appserver.New(e.host.SocketPath, appserver.DefaultBounds, agent.ClientOption(e.owned))
	e.t.Cleanup(func() { _ = e.client.Close() })
	var err error
	e.alloc, err = agent.NewAllocator(agent.AllocatorConfig{Repo: e.repo, StateDir: e.state, RunID: e.runID, Base: func(string) (string, error) { return e.base, nil }})
	if err != nil {
		e.t.Fatal(err)
	}
	e.backend, err = agent.New(agent.Config{Client: e.client, Allocator: e.alloc, Threads: e.owned, PollEvery: 25 * time.Millisecond, InterruptWait: 5 * time.Second,
		Owner: owner, OwnerAlive: e.alive})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) wt(node string, attempt, lineage int) string {
	return filepath.Join(e.state, "flow", e.runID, "wt", node, fmt.Sprintf("a%d-l%d", attempt, lineage))
}

func (e *env) config(resume bool) engine.Config {
	cfg := engine.Config{Backend: e.backend, Policy: e.policy, Journal: e.journal, RunID: e.runID, TokenBudget: e.budget,
		Ops: map[string]engine.Op{"commit": commitOp{e}, "hold": holdOp{}}}
	if resume {
		cfg.Resume = &engine.ResumeOptions{Wait: func(context.Context, time.Duration) error { return nil }, Grace: time.Millisecond, Skew: time.Hour, OpenWait: 10 * time.Millisecond, Poll: time.Millisecond}
	}
	return cfg
}

func (e *env) exec(resume bool, body string) *engine.Result {
	e.t.Helper()
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte(header + body)})
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	e.cancel = cancel
	defer cancel()
	res, err := engine.Run(ctx, prog, e.config(resume))
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// execAsync runs the script in a goroutine, the way a process that is later abandoned runs: it is not cancelled, and is ended
// with the test (an abandoned process that is cancelled would interrupt the turn it left running).
func (e *env) execAsync(resume bool, body string) {
	e.t.Helper()
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte(header + body)})
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	cfg := e.config(resume)
	go func() {
		defer close(done)
		_, _ = engine.Run(ctx, prog, cfg)
	}()
	e.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	})
}

func (e *env) run(body string) *engine.Result    { e.t.Helper(); return e.exec(false, body) }
func (e *env) resume(body string) *engine.Result { e.t.Helper(); return e.exec(true, body) }

// killWhen ends the process when a row is appended that the function accepts: the row is the last one written.
func (e *env) killWhen(fn func(engine.Event) bool) {
	e.journal.onAfter(func(ev engine.Event) {
		if fn(ev) {
			e.kill()
		}
	})
}

// kill ends the process the way a crash does: it writes no more rows, its connection to the host is gone (so it can send and
// interrupt nothing), and its run ends. What it left on the host keeps running.
func (e *env) kill() {
	e.journal.freeze()
	_ = e.client.Close()
	e.cancel()
}

// newProcess is the resume after a kill: the journal accepts rows again and a new backend serves it.
func (e *env) newProcess(owner string) {
	e.t.Helper()
	e.journal.thaw()
	e.spawn(owner)
}

func value[T any](t *testing.T, res *engine.Result) T {
	t.Helper()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	var v T
	if err := json.Unmarshal(res.Value, &v); err != nil {
		t.Fatalf("value %s: %v", res.Value, err)
	}
	return v
}

type request struct {
	Method string
	Params map[string]any
}

func (e *env) requests(methods ...string) []request {
	var out []request
	for _, r := range e.host.Requests() {
		if len(methods) > 0 && !slices.Contains(methods, r.Method) {
			continue
		}
		var params map[string]any
		_ = json.Unmarshal(r.Params, &params)
		out = append(out, request{Method: r.Method, Params: params})
	}
	return out
}

// rows are the rows of the call labelled label (every attempt), in order.
func (e *env) rows(label string) []engine.Event {
	var out []engine.Event
	for _, ev := range e.journal.Events() {
		if ev.Kind == engine.KindCall && ev.Label == label {
			out = append(out, ev)
		}
	}
	return out
}

func countOf(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

func kinds(rows []engine.Event) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Event
	}
	return out
}

func (e *env) flowRows(event string) []engine.Event {
	var out []engine.Event
	for _, ev := range e.journal.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == event {
			out = append(out, ev)
		}
	}
	return out
}

// lastRow is the newest row of a call with the event name.
func (e *env) lastRow(label, event string) engine.Event {
	e.t.Helper()
	rows := e.rows(label)
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Event == event {
			return rows[i]
		}
	}
	e.t.Fatalf("call %q has no %s row: %v", label, event, kinds(rows))
	return engine.Event{}
}

// threadCwd is the working directory a thread was started or forked in, as the journal records it.
func (e *env) threadCwd(thread string) string {
	for _, ev := range e.journal.Events() {
		if ev.Kind == engine.KindCall && (ev.Event == engine.EvThreadStarted || ev.Event == engine.EvForkStarted) && str(ev.Data, "thread_id") == thread {
			return str(ev.Data, "cwd")
		}
	}
	return ""
}

// snapshot is the working snapshot of a worktree.
func (e *env) snapshot(path string) string {
	e.t.Helper()
	s, err := e.alloc.Snapshot(context.Background(), path)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// actions writes a file in the directory of the thread whose turn starts, named by the first word of the prompt that follows
// the marker line - the stand-in for an executor that edits files. A prompt that contains "FAIL" ends the turn failed
// (serverOverloaded) after the edit.
func (e *env) edits(extra func(fakehost.TurnRequest) *fakehost.TurnPlan) {
	e.threads.OnTurn(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if extra != nil {
			if plan := extra(r); plan != nil {
				return *plan
			}
		}
		if cwd := e.threadCwd(r.ThreadID); cwd != "" && e.isExecutorThread(cwd) {
			if word := firstWord(r.Text); word != "" {
				writeFile(e.t, filepath.Join(cwd, word+".txt"), r.Text)
			}
		}
		return fakehost.TurnPlan{Message: "done"}
	})
}

func (e *env) isExecutorThread(cwd string) bool {
	return strings.Contains(cwd, string(filepath.Separator)+"wt"+string(filepath.Separator))
}

// firstWord is the first word of a turn's prompt, after the operation marker line.
func firstWord(text string) string {
	_, rest, ok := strings.Cut(text, "\n\n")
	if !ok {
		rest = text
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(strings.Trim(fields[0], ":,."))
}

// commitOp is crw.commit over real git: it commits the pending tree of the call it names in the node's current worktree.
type commitOp struct{ e *env }

func currentWorktree(events []engine.Event, node string) string {
	path, seq := "", 0
	for _, ev := range events {
		switch {
		case ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && str(ev.Data, "node") == node:
			if m, ok := ev.Data["allocation"].(map[string]any); ok && str(m, "path") != "" && (str(m, "kind") == "fresh" || str(m, "kind") == "restore") && ev.Seq > seq {
				path, seq = str(m, "path"), ev.Seq
			}
		case ev.Kind == engine.KindFlow && ev.Event == engine.EvWorktreeRestored && str(ev.Data, "node") == node && ev.Seq > seq:
			path, seq = str(ev.Data, "path"), ev.Seq
		}
	}
	return path
}

func (o commitOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var args []string
	_ = json.Unmarshal(req.Args, &args)
	if len(args) < 2 {
		return engine.OpResult{}, fmt.Errorf("crw.commit takes a node and a call")
	}
	node, call := args[0], args[1]
	events := req.Journal.Events()
	wt := currentWorktree(events, node)
	tree := ""
	for _, ev := range events {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.CallID == call && ev.Event == engine.EvCompleted {
			tree = str(ev.Data, "pending_tree")
		}
	}
	if wt == "" || tree == "" {
		return engine.OpResult{}, fmt.Errorf("no worktree (%q) or pending tree (%q) for %s", wt, tree, call)
	}
	g := ops.ExecGit("git")
	msg := "crw commit " + req.RequestID
	if last, err := g(ctx, wt, nil, "log", "-1", "--format=%B"); err == nil && strings.Contains(last, req.RequestID) {
		head, _ := ops.Head(ctx, g, wt)
		parent, _ := g(ctx, wt, nil, "rev-parse", head+"^")
		res, _ := json.Marshal(map[string]any{"commit": head})
		return engine.OpResult{Result: res, Data: map[string]any{"node": node, "commit": head, "parent": parent, "tree": tree}}, nil
	}
	head, err := ops.Head(ctx, g, wt)
	if err != nil {
		return engine.OpResult{}, err
	}
	made, err := ops.Commit(ctx, g, wt, ops.CommitRequest{Tree: tree, Parent: head, Message: msg})
	if err != nil {
		return engine.OpResult{}, err
	}
	res, _ := json.Marshal(map[string]any{"commit": made.Commit})
	return engine.OpResult{Result: res, Data: map[string]any{"node": node, "commit": made.Commit, "parent": made.Parent, "tree": made.Tree}}, nil
}

func (o commitOp) Proven(ctx context.Context, events []engine.Event, row engine.Event) (bool, string, error) {
	wt := currentWorktree(events, str(row.Data, "node"))
	if wt == "" {
		return false, "the node has no worktree", nil
	}
	g := ops.ExecGit("git")
	head, err := ops.Head(ctx, g, wt)
	if err != nil {
		return false, err.Error(), nil
	}
	anc, err := ops.IsAncestor(ctx, g, wt, str(row.Data, "commit"), head)
	if err != nil || !anc {
		return false, "the commit is not on the worktree's history", nil
	}
	return true, "", nil
}

// holdOp is crw.hold: a record, with no effect to prove.
type holdOp struct{}

func (holdOp) Run(context.Context, engine.OpRequest) (engine.OpResult, error) {
	return engine.OpResult{Result: json.RawMessage(`{"held":true}`)}, nil
}
func (holdOp) Proven(context.Context, []engine.Event, engine.Event) (bool, string, error) {
	return true, "", nil
}

// ---- the tests ----

const buildScript = `const b = await agent.call('build the thing', { agentType: 'executor', node: 'n1', label: 'build' })
`

// H1 commit, then fix t2 and fix t3 each committed. Editing the prompt of t2 and resuming restores the boundary after the H1
// commit: the new worktree is at H1 with the files of H1, the old worktree's three commits and files are kept on a ref, and the
// executor runs t2 on a fork of the build thread at its t1 turn, in the new worktree.
func TestEditFirstFixAfterTwoFixesRestoresBoundary(t *testing.T) {
	e := newEnv(t)
	e.edits(nil)
	body := func(fix2 string) string {
		return buildScript + `
const c1 = await crw.commit('n1', b.callId)
const f2 = await agent.call('` + fix2 + `', { resume: b, label: 'fix2' })
const c2 = await crw.commit('n1', f2.callId)
const f3 = await agent.call('fixthree again', { resume: f2, label: 'fix3' })
const c3 = await crw.commit('n1', f3.callId)
return [b.status, f2.status, f3.status, c1.commit, c2.commit, c3.commit]`
	}
	first := value[[]string](t, e.run(body("fixtwo the parser")))
	h1, h2, h3 := first[3], first[4], first[5]
	old := e.wt("n1", 1, 0)
	if head := git(t, old, "rev-parse", "HEAD"); head != h3 {
		t.Fatalf("the first run left HEAD at %s, want %s", head, h3)
	}
	buildThread := ""
	for _, ev := range e.rows("build") {
		if ev.Event == engine.EvThreadStarted {
			buildThread = str(ev.Data, "thread_id")
		}
	}
	buildTurn := str(e.lastRow("build", engine.EvTurnStarted).Data, "turn_id")
	oldSnapshot := e.snapshot(old)

	// the resumed process edits the prompt of the first fix; what the executor sees when its turn starts is recorded
	var atFix2 struct{ head, status, files string }
	e.newProcess("proc-2")
	e.edits(func(r fakehost.TurnRequest) *fakehost.TurnPlan {
		if strings.Contains(r.Text, "fixtwo edited") {
			cwd := e.threadCwd(r.ThreadID)
			atFix2.head = git(t, cwd, "rev-parse", "HEAD")
			atFix2.status = git(t, cwd, "status", "--porcelain")
			atFix2.files = git(t, cwd, "ls-files", "-o", "-c", "--exclude-standard")
		}
		return nil
	})
	res := e.resume(body("fixtwo edited"))
	if got := value[[]string](t, res); got[0] != "completed" || got[1] != "completed" || got[2] != "completed" {
		t.Fatalf("%v", got)
	}
	restored := e.wt("n1", 2, 1)
	if atFix2.head != h1 || atFix2.status != "" || atFix2.files != "a.txt\nbuild.txt" {
		t.Errorf("when fix 2 started its worktree was at %s with status %q and files %q, want HEAD %s, clean, a.txt and build.txt (the files of the H1 commit)", atFix2.head, atFix2.status, atFix2.files, h1)
	}
	row := e.flowRows(engine.EvWorktreeRestored)
	if len(row) != 1 || row[0].Data["path"] != restored || row[0].Data["C"] != h1 || row[0].Data["P"] != nil || row[0].Data["node"] != "n1" || lineageOf(row[0].Data) != 1 {
		t.Fatalf("worktree_restored rows %+v", row)
	}
	if digest := str(row[0].Data, "snapshot_digest"); digest != git(t, e.repo, "rev-parse", h1+"^{tree}") {
		t.Errorf("the verified digest %s is not the tree of %s", digest, h1)
	}
	// the fork: the build thread at its t1 turn, into the restored worktree, with no history hydration
	forks := e.requests("thread/fork")
	if len(forks) != 1 {
		t.Fatalf("%d thread/fork requests", len(forks))
	}
	fork := forks[0].Params
	if fork["threadId"] != buildThread || fork["lastTurnId"] != buildTurn || fork["cwd"] != restored || fork["excludeTurns"] != true || fork["ephemeral"] != false {
		t.Errorf("thread/fork %v", fork)
	}
	forkThread := ""
	for _, ev := range e.rows("fix2") {
		if ev.Event == engine.EvForkStarted {
			forkThread = str(ev.Data, "thread_id")
		}
	}
	view, ok := e.threads.Thread(forkThread)
	if !ok || view.ForkedFromID != buildThread || view.Cwd != restored || len(view.Turns) < 2 || view.Turns[0].ID != buildTurn {
		t.Errorf("the fork on the host: %+v", view)
	}
	if len(view.Turns) >= 2 && !strings.Contains(view.Turns[1].Text, "fixtwo edited") {
		t.Errorf("the turn the executor ran on the fork is %q, want fix 2", view.Turns[1].Text)
	}
	// the old worktree was kept, as it was, on a ref of the superseded lineage, and was not written to again
	ref := engine.SupersededRef(e.runID, 1, "root", "n1")
	if git(t, e.repo, "rev-parse", ref+"^{tree}") != oldSnapshot || git(t, e.repo, "rev-parse", ref+"^") != h3 {
		t.Errorf("the superseded ref %s does not keep the old worktree's HEAD %s and snapshot %s", ref, h3, oldSnapshot)
	}
	if got := e.snapshot(old); got != oldSnapshot || git(t, old, "rev-parse", "HEAD") != h3 {
		t.Errorf("the old worktree was written to: snapshot %s HEAD %s", got, git(t, old, "rev-parse", "HEAD"))
	}
	// the three commits were made again from the boundary, in the restored worktree
	if head := git(t, restored, "rev-parse", "HEAD"); head == h3 || head == h2 || git(t, restored, "rev-parse", "HEAD~2") != h1 {
		t.Errorf("the restored worktree's history is not H1 and two new commits: HEAD %s", head)
	}
	// the build and its commit were replayed: one build turn only
	if n := len(e.requests("thread/start")); n != 1 {
		t.Errorf("%d thread/start requests: the build was run again", n)
	}
}

// A run that ended after the build and before the next continuation, or whose continuation's prompt changed, resumes with
// thread/fork{lastTurnId: through_turn_id, cwd: the restored worktree}, even though the original thread's last turn already is
// the through turn: the original thread gets no turn, the old worktree is untouched, and the file the continuation writes is in
// the restored worktree.
func TestSuffixAtBoundaryForksInsteadOfContinuing(t *testing.T) {
	cases := map[string]struct{ first, second string }{
		"died before the continuation": {
			first: buildScript + `return [b.status]`,
			second: buildScript + `const r = await agent.call('research next', { resume: b, label: 'research' })
return [b.status, r.status]`,
		},
		"the continuation's prompt changed": {
			first: buildScript + `const r = await agent.call('research one', { resume: b, label: 'research' })
return [b.status, r.status]`,
			second: buildScript + `const r = await agent.call('research two', { resume: b, label: 'research' })
return [b.status, r.status]`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.edits(nil)
			e.run(c.first)
			old := e.wt("n1", 1, 0)
			oldSnapshot := e.snapshot(old)
			buildThread := str(e.lastRow("build", engine.EvThreadStarted).Data, "thread_id")
			buildTurn := str(e.lastRow("build", engine.EvTurnStarted).Data, "turn_id")
			pending := str(e.lastRow("build", engine.EvCompleted).Data, "pending_tree")
			turnsBefore := len(e.requests("turn/start"))
			e.newProcess("proc-2")
			e.edits(nil)
			res := e.resume(c.second)
			if got := value[[]string](t, res); got[1] != "completed" {
				t.Fatalf("%v", got)
			}
			restored := e.wt("n1", 2, 1)
			forks := e.requests("thread/fork")
			if len(forks) != 1 || forks[0].Params["threadId"] != buildThread || forks[0].Params["lastTurnId"] != buildTurn || forks[0].Params["cwd"] != restored {
				t.Fatalf("thread/fork requests %+v, want one at %s for %s in %s", forks, buildTurn, buildThread, restored)
			}
			var onOriginal int
			for _, r := range e.requests("turn/start")[turnsBefore:] {
				if r.Params["threadId"] == buildThread {
					onOriginal++
				}
			}
			if onOriginal != 0 {
				t.Errorf("%d turns were sent to the original thread", onOriginal)
			}
			if got := e.snapshot(old); got != oldSnapshot {
				t.Errorf("the old worktree's snapshot changed: %s, was %s", got, oldSnapshot)
			}
			ref := engine.SupersededRef(e.runID, 1, "root", "n1")
			if git(t, e.repo, "rev-parse", ref+"^{tree}") != oldSnapshot {
				t.Errorf("the superseded ref %s does not keep the old worktree's snapshot", ref)
			}
			forkThread := str(e.lastRow("research", engine.EvForkStarted).Data, "thread_id")
			view, _ := e.threads.Thread(forkThread)
			if view.Cwd != restored || view.ForkedFromID != buildThread {
				t.Errorf("the fork on the host: cwd %q forked from %q", view.Cwd, view.ForkedFromID)
			}
			if _, err := os.Stat(filepath.Join(restored, "research.txt")); err != nil {
				t.Errorf("the file the continuation wrote is not in the restored worktree: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(restored, "build.txt")); err != nil || !strings.Contains(string(got), "build the thing") {
				t.Errorf("the restored worktree lost the pending tree: %q %v", got, err)
			}
			if got := git(t, e.repo, "rev-parse", pending+"^{tree}"); got != pending {
				t.Errorf("the pending tree %s is gone", pending)
			}
			if _, err := os.Stat(filepath.Join(old, "research.txt")); err == nil && name == "died before the continuation" {
				t.Errorf("the continuation wrote into the retired worktree")
			}
		})
	}
}

// A turn the previous process left running in the old suffix is interrupted and confirmed idle before anything of the suffix
// is superseded or its worktree is kept: nothing writes to the worktree while its state is saved.
func TestLiveTurnOfOldSuffixInterruptedFirst(t *testing.T) {
	e := newEnv(t)
	e.threads.OnTurn(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Hang: true, Detached: true}
	})
	started := make(chan struct{})
	var once sync.Once
	e.journal.onAfter(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvTurnStarted {
			once.Do(func() { close(started) })
		}
	})
	e.execAsync(false, `const b = await agent.call('build slowly', { agentType: 'executor', node: 'n1', label: 'build' })
return b.status`)
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the build's turn did not start")
	}
	// the process dies with its turn running on the host: it writes no more rows, and the host's turn goes on
	e.journal.freeze()
	oldThread := str(e.lastRow("build", engine.EvThreadStarted).Data, "thread_id")
	if view, _ := e.threads.Thread(oldThread); len(view.Turns) != 1 || view.Turns[0].Status != "inProgress" {
		t.Fatalf("the old turn is not running: %+v", view.Turns)
	}
	e.journal.thaw()
	e.spawn("proc-2")
	e.edits(nil)
	// the order the resume does things in: what the host had seen of interrupts when each row was written
	var order []string
	interrupts := map[string]int{}
	e.journal.onBefore(func(ev engine.Event) {
		switch {
		case ev.Event == engine.EvSuperseded && ev.Kind == engine.KindCall:
			order = append(order, "superseded")
			interrupts["superseded"] = len(e.threads.Interrupts())
		case ev.Event == engine.EvWorktreePreserved:
			order = append(order, "preserved")
			interrupts["preserved"] = len(e.threads.Interrupts())
		case ev.Event == engine.EvWorktreeRestored:
			order = append(order, "restored")
		case ev.Event == engine.EvPrepared && ev.Label == "build":
			order = append(order, "prepared")
		}
	})
	res := e.resume(`const b = await agent.call('build quickly', { agentType: 'executor', node: 'n1', label: 'build' })
return b.status`)
	if got := value[string](t, res); got != "completed" {
		t.Fatalf("%s", got)
	}
	if !slices.Equal(order, []string{"superseded", "preserved", "restored", "prepared"}) {
		t.Errorf("the resume did %v", order)
	}
	if got := e.threads.Interrupts(); !slices.Contains(got, oldThread) {
		t.Errorf("the old thread %s was not interrupted: %v", oldThread, got)
	}
	if interrupts["superseded"] < 1 || interrupts["preserved"] < 1 {
		t.Errorf("the old turn was superseded after %d interrupts and its worktree kept after %d", interrupts["superseded"], interrupts["preserved"])
	}
	if view, _ := e.threads.Thread(oldThread); view.Turns[0].Status == "inProgress" {
		t.Errorf("the old turn still runs")
	}
	sup := e.lastRow("build", engine.EvSuperseded)
	if str(sup.Data, "reason") != engine.ReasonSupersededByResume || lineageOf(sup.Data) != 1 {
		t.Errorf("superseded row %v", sup.Data)
	}
	if got := str(e.flowRows(engine.EvWorktreeRestored)[0].Data, "snapshot_digest"); got != git(t, e.repo, "rev-parse", e.base+"^{tree}") {
		t.Errorf("the restored boundary %s is not the tree of the base commit", got)
	}
}

// the checkpoint scenario: the build ended and was not committed; the continuation's prompt changed; the original thread
// cannot be read (the resume runs against a host that has lost it).
func checkpointScenario(t *testing.T) (e *env, restored string, pending string) {
	t.Helper()
	e = newEnv(t)
	e.edits(nil)
	e.run(buildScript + `const r = await agent.call('research one', { resume: b, label: 'research' })
return [b.status, r.status]`)
	pending = str(e.lastRow("build", engine.EvCompleted).Data, "pending_tree")
	e.newProcess("proc-2")
	// the host forgot the threads: a new one, with the same socket path discipline
	e.startHost()
	e.spawn("proc-2")
	e.edits(nil)
	res := e.resume(buildScript + `const r = await agent.call('research two', { resume: b, label: 'research' })
return [b.status, r.status]`)
	if got := value[[]string](t, res); got[1] != "completed" {
		t.Fatalf("%v", got)
	}
	return e, e.wt("n1", 2, 1), pending
}

// A continuation whose thread cannot be read starts a new thread of the same role instead, in the restored worktree, with a
// checkpoint packet built from the journal's record of the turns up to the through turn.
func TestForkUnavailableUsesCheckpointThread(t *testing.T) {
	e, restored, _ := checkpointScenario(t)
	if forks := e.requests("thread/fork"); len(forks) != 0 {
		t.Errorf("%d thread/fork requests for a thread the host cannot read", len(forks))
	}
	starts := e.requests("thread/start")
	if len(starts) != 1 || starts[0].Params["cwd"] != restored {
		t.Fatalf("thread/start requests %+v, want one in %s", starts, restored)
	}
	var turn string
	for _, r := range e.requests("turn/start") {
		if in, ok := r.Params["input"].([]any); ok && len(in) > 0 {
			if m, ok := in[0].(map[string]any); ok {
				turn = str(m, "text")
			}
		}
	}
	for _, want := range []string{"build the thing", "done", "research two", "crw-flow-op: "} {
		if !strings.Contains(turn, want) {
			t.Errorf("the first turn of the checkpoint thread lacks %q: %q", want, turn)
		}
	}
	rows := e.flowRows(engine.EvCheckpointStarted)
	if len(rows) != 1 || str(rows[0].Data, "checkpoint_digest") == "" || str(rows[0].Data, "thread_id") == "" {
		t.Errorf("checkpoint_started rows %+v", rows)
	}
	if got := str(e.lastRow("research", engine.EvThreadStarted).Data, "cwd"); got != restored {
		t.Errorf("the checkpoint thread runs in %s, want %s", got, restored)
	}
}

// The checkpoint thread adopts the restored worktree: the thread/start cwd is the path of the worktree_restored row, its files
// are the pending tree P, the prepared row carries the same C, P and snapshot digest, and no other worktree was allocated.
func TestCheckpointThreadAdoptsRestoredPendingTree(t *testing.T) {
	e, restored, pending := checkpointScenario(t)
	rows := e.flowRows(engine.EvWorktreeRestored)
	if len(rows) != 1 {
		t.Fatalf("worktree_restored rows %+v", rows)
	}
	row := rows[0].Data
	if row["path"] != restored || row["P"] != pending || row["C"] != e.base {
		t.Errorf("worktree_restored %v, want %s at %s with %s", row, restored, e.base, pending)
	}
	starts := e.requests("thread/start")
	if len(starts) != 1 || starts[0].Params["cwd"] != row["path"] {
		t.Fatalf("thread/start %+v", starts)
	}
	if got := e.snapshot(restored); got != pending {
		t.Errorf("the files of the restored worktree are %s, want the pending tree %s", got, pending)
	}
	if got, err := os.ReadFile(filepath.Join(restored, "build.txt")); err != nil || !strings.Contains(string(got), "build the thing") {
		t.Errorf("build.txt: %q %v", got, err)
	}
	alloc, _ := e.lastRow("research", engine.EvPrepared).Data["allocation"].(map[string]any)
	if alloc["kind"] != "restore" || alloc["path"] != row["path"] || alloc["start_commit"] != row["C"] || alloc["pending"] != row["P"] || alloc["snapshot_digest"] != row["snapshot_digest"] {
		t.Errorf("the prepared allocation %v does not carry the restore %v", alloc, row)
	}
	fresh := 0
	for _, ev := range e.journal.Events() {
		if a, ok := ev.Data["allocation"].(map[string]any); ev.Event == engine.EvPrepared && ok && a["kind"] == "fresh" {
			fresh++
		}
	}
	if fresh != 1 {
		t.Errorf("%d fresh allocations: the build's, and none for the continuation", fresh)
	}
	if n := strings.Count(git(t, e.repo, "worktree", "list", "--porcelain"), "worktree "); n != 3 {
		t.Errorf("%d worktrees in the repository, want the main one, the build's and the restored one", n)
	}
}

// A process that dies after the restored worktree was adopted - the call's prepared row written, nothing else - leaves a first
// failing position: the next resume keeps the old restored worktree on a ref of its lineage and rebuilds the same C and P in a
// worktree of the new lineage, verified against the same snapshot digest.
func TestCrashAfterRestoreRebuildsSameBoundary(t *testing.T) {
	e := newEnv(t)
	e.edits(nil)
	e.run(buildScript + `return [b.status]`)
	continuation := buildScript + `const r = await agent.call('research next', { resume: b, label: 'research' })
return [b.status, r.status]`
	e.newProcess("proc-2")
	e.edits(nil)
	e.killWhen(func(ev engine.Event) bool {
		_, adopted := ev.Data["allocation"].(map[string]any)
		return ev.Kind == engine.KindCall && ev.Label == "research" && ev.Event == engine.EvPrepared && adopted
	})
	e.resume(continuation)
	first := e.flowRows(engine.EvWorktreeRestored)
	if len(first) != 1 || first[0].Data["path"] != e.wt("n1", 2, 1) {
		t.Fatalf("after the crash: worktree_restored rows %+v", first)
	}
	lineage1 := e.snapshot(e.wt("n1", 2, 1))

	e.newProcess("proc-3")
	e.edits(nil)
	res := e.resume(continuation)
	if got := value[[]string](t, res); got[1] != "completed" {
		t.Fatalf("%v", got)
	}
	rows := e.flowRows(engine.EvWorktreeRestored)
	if len(rows) != 2 {
		t.Fatalf("worktree_restored rows %+v", rows)
	}
	a, b := rows[0].Data, rows[1].Data
	if b["path"] != e.wt("n1", 3, 2) || b["C"] != a["C"] || b["P"] != a["P"] || b["snapshot_digest"] != a["snapshot_digest"] || b["P"] == nil {
		t.Errorf("the rebuilt boundary %v is not the first one %v in a new lineage path", b, a)
	}
	ref := engine.SupersededRef(e.runID, 2, "root", "n1")
	if got := git(t, e.repo, "rev-parse", ref+"^{tree}"); got != lineage1 {
		t.Errorf("the superseded ref %s keeps %s, the old restored worktree's snapshot was %s", ref, got, lineage1)
	}
	if got := e.snapshot(e.wt("n1", 2, 1)); got != lineage1 {
		t.Errorf("the old restored worktree changed: %s", got)
	}
	// the build was replayed both times, and the continuation adopted the new worktree
	if n := len(e.requests("thread/start")); n != 1 {
		t.Errorf("%d thread/start requests: the build ran again", n)
	}
	alloc, _ := e.lastRow("research", engine.EvPrepared).Data["allocation"].(map[string]any)
	if alloc["kind"] != "restore" || alloc["path"] != b["path"] {
		t.Errorf("the continuation's allocation %v", alloc)
	}
}

// turnText is the text of the input of a turn/start request.
func turnText(r request) string {
	if in, ok := r.Params["input"].([]any); ok && len(in) > 0 {
		if m, ok := in[0].(map[string]any); ok {
			return str(m, "text")
		}
	}
	return ""
}

// firstLine and rest split a turn's text at the operation marker line.
func markerOf(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return strings.TrimPrefix(line, agent.MarkerPrefix)
}

const packet = "TASK: build the parser\nCRITERIA: it parses the examples"

// A reviewer turn that started and failed serverOverloaded is held (delivered). The resume goes on in place: no thread is
// started, one turn t2 of the same call is sent to the same thread with the fixed continuation instruction, hold_cleared is
// written, the crw.hold record that followed the held position is superseded, and the other path replays with no live call.
func TestHeldCallRetriedOnResumeSameThread(t *testing.T) {
	e := newEnv(t)
	e.edits(func(r fakehost.TurnRequest) *fakehost.TurnPlan {
		if strings.Contains(r.Text, "review the diff") && r.N == 1 {
			return &fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "busy", CodexErrorInfo: "serverOverloaded"}}
		}
		return nil
	})
	body := `return await parallel([
  async () => (await agent.call('side work', { agentType: 'explorer', label: 'side' })).status,
  async () => {
    const r = await agent.call('review the diff', { agentType: 'reviewer', label: 'review' })
    if (r.status === 'held') { await crw.hold('n1', r.reason) }
    return r.status
  },
], { keys: ['side', 'rev'] })`
	if got := value[[]string](t, e.run(body)); !slices.Equal(got, []string{"completed", "held"}) {
		t.Fatalf("%v", got)
	}
	thread := str(e.lastRow("review", engine.EvThreadStarted).Data, "thread_id")
	callID := e.lastRow("review", engine.EvHeld).CallID
	starts, turns := len(e.requests("thread/start")), len(e.requests("turn/start"))
	e.newProcess("proc-2")
	e.edits(nil)
	if got := value[[]string](t, e.resume(body)); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	if n := len(e.requests("thread/start")) - starts; n != 0 {
		t.Errorf("%d threads were started", n)
	}
	sent := e.requests("turn/start")[turns:]
	if len(sent) != 1 || sent[0].Params["threadId"] != thread || markerOf(turnText(sent[0])) != callID+"/t2" {
		t.Fatalf("turns sent by the resume: %+v, want one t2 of %s on %s", sent, callID, thread)
	}
	text := turnText(sent[0])
	for _, want := range []string{"host_infra_serverOverloaded", "Continue the same task", "output schema is unchanged"} {
		if !strings.Contains(text, want) {
			t.Errorf("the continuation %q lacks %q", text, want)
		}
	}
	if strings.Contains(text, "review the diff") {
		t.Errorf("a delivered packet was sent again: %q", text)
	}
	cleared := e.flowRows(engine.EvHoldCleared)
	if len(cleared) != 1 || cleared[0].Data["call_id"] != callID || lineageOf(cleared[0].Data) != 1 {
		t.Errorf("hold_cleared rows %+v", cleared)
	}
	var holdRows []string
	for _, ev := range e.journal.Events() {
		if ev.Kind == engine.KindCall && ev.Op == "crw.hold" {
			holdRows = append(holdRows, ev.Event)
		}
	}
	if !slices.Equal(holdRows, []string{engine.EvCompleted, engine.EvSuperseded}) {
		t.Errorf("the crw.hold record's rows %v: it is superseded by the resume", holdRows)
	}
	if got := kinds(e.rows("review")); countOf(got, engine.EvPrepared) != 1 {
		t.Errorf("review rows %v: the call keeps its attempt", got)
	}
	if rows := kinds(e.rows("side")); countOf(rows, engine.EvPrepared) != 1 {
		t.Errorf("the other path's call ran again: %v", rows)
	}
}

// A call held before its thread existed - a budget stop at the gate, or a thread/start the host refused - goes on in place:
// the cwd is listed to show that nothing was made, then one thread is made at the same cwd and the first turn carries the
// packet as it was, with no continuation instruction.
func TestHeldBeforeThreadResumesWithOriginalPacket(t *testing.T) {
	build := `const warm = await agent.call('warm up', { agentType: 'explorer', label: 'warm' })
const b = await agent.call(` + "`" + packet + "`" + `, { agentType: 'executor', node: 'n1', label: 'build' })
return [warm.status, b.status, b.reason]`
	check := func(t *testing.T, e *env, wantSend int) {
		t.Helper()
		cwd := e.wt("n1", 1, 0)
		callID := e.lastRow("build", engine.EvHeld).CallID
		var listed, started []int
		for i, r := range e.host.Requests() {
			var p map[string]any
			_ = json.Unmarshal(r.Params, &p)
			switch {
			case r.Method == "thread/list" && p["cwd"] == cwd:
				listed = append(listed, i)
			case r.Method == "thread/start" && p["cwd"] == cwd:
				started = append(started, i)
			}
		}
		if len(listed) == 0 || len(started) == 0 || listed[len(listed)-1] > started[len(started)-1] {
			t.Errorf("the cwd was listed at requests %v and the thread started at %v: the listing shows nothing was made before it", listed, started)
		}
		// the packet goes out as the call's first turn
		var turn request
		for _, r := range e.requests("turn/start") {
			if markerOf(turnText(r)) == callID+"/t1" {
				turn = r
			}
		}
		text := turnText(turn)
		if !strings.Contains(text, "TASK: build the parser") || !strings.Contains(text, "CRITERIA: it parses the examples") || strings.Contains(text, "Continue the same task") {
			t.Errorf("the first turn %q is not the original packet", text)
		}
		creations := 0
		for _, ev := range e.rows("build") {
			if ev.Event == engine.EvThreadRequested {
				creations++
				if wantSend > 0 && lineSend(ev) != wantSend && ev.Seq > e.lastRow("build", engine.EvHeld).Seq {
					t.Errorf("the creation sent again has the send ordinal %d, want %d", lineSend(ev), wantSend)
				}
			}
		}
		if got := e.lastRow("build", engine.EvCompleted); got.CallID != callID {
			t.Errorf("the call did not complete")
		}
		if n := countOf(kinds(e.rows("build")), engine.EvPrepared); n != 1 {
			t.Errorf("%d prepared rows: the call keeps its attempt and its allocation", n)
		}
		if n := countOf(kinds(e.rows("build")), engine.EvThreadStarted); n != 1 {
			t.Errorf("%d threads were started for the call", n)
		}
	}
	t.Run("a budget stop before thread_requested", func(t *testing.T) {
		e := newEnv(t)
		e.budget = 1
		got := value[[]string](t, e.run(build))
		if got[1] != "held" || got[2] != engine.ReasonBudgetExhausted {
			t.Fatalf("%v", got)
		}
		if held := e.lastRow("build", engine.EvHeld); held.Data["input_state"] != "not_delivered" || slices.Contains(kinds(e.rows("build")), engine.EvThreadRequested) {
			t.Fatalf("held row %v rows %v", held.Data, kinds(e.rows("build")))
		}
		e.newProcess("proc-2")
		e.budget = 0
		e.edits(nil)
		if got := value[[]string](t, e.resume(build)); got[1] != "completed" {
			t.Fatalf("%v", got)
		}
		check(t, e, 1)
	})
	t.Run("thread/start refused with usageLimitExceeded", func(t *testing.T) {
		e := newEnv(t)
		e.threads.OnStart(func(p map[string]any) map[string]any {
			if strings.Contains(str(p, "cwd"), string(filepath.Separator)+"wt"+string(filepath.Separator)) {
				return fakehost.ErrorWithInfo(-32600, "usage limit", "usageLimitExceeded")
			}
			return nil
		})
		got := value[[]string](t, e.run(build))
		if got[1] != "held" || got[2] != "host_infra_usageLimitExceeded" {
			t.Fatalf("%v", got)
		}
		e.newProcess("proc-2")
		e.threads.OnStart(nil)
		e.edits(nil)
		if got := value[[]string](t, e.resume(build)); got[1] != "completed" {
			t.Fatalf("%v", got)
		}
		check(t, e, 2)
	})
}

func lineSend(ev engine.Event) int {
	switch n := ev.Data["send"].(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// The first turn/start refused with activeTurnNotSteerable: the call is held (not delivered). Once the thread is idle the
// resume shows that no turn carries <call_id>/t1, and sends the original packet as t2. A turn that does carry the marker is an
// outcome nobody can prove: outcome_unknown, and no turn/start.
func TestFirstTurnRejectedResumesWithOriginalPacket(t *testing.T) {
	body := `const b = await agent.call(` + "`" + packet + "`" + `, { agentType: 'executor', node: 'n1', label: 'build' })
return [b.status, b.reason]`
	refuse := func(e *env) {
		e.edits(func(r fakehost.TurnRequest) *fakehost.TurnPlan {
			if r.N == 1 {
				return &fakehost.TurnPlan{StartError: fakehost.ErrorWithInfo(-32600, "an active turn cannot accept another turn/start", map[string]any{"activeTurnNotSteerable": map[string]any{"turnKind": "regular"}})}
			}
			return nil
		})
	}
	t.Run("the marker is absent", func(t *testing.T) {
		e := newEnv(t)
		refuse(e)
		got := value[[]string](t, e.run(body))
		if got[0] != "held" || got[1] != "host_infra_activeTurnNotSteerable" {
			t.Fatalf("%v", got)
		}
		callID := e.lastRow("build", engine.EvHeld).CallID
		thread := str(e.lastRow("build", engine.EvThreadStarted).Data, "thread_id")
		turns := len(e.requests("turn/start"))
		e.newProcess("proc-2")
		e.edits(nil)
		if got := value[[]string](t, e.resume(body)); got[0] != "completed" {
			t.Fatalf("%v", got)
		}
		sent := e.requests("turn/start")[turns:]
		if len(sent) != 1 || sent[0].Params["threadId"] != thread || markerOf(turnText(sent[0])) != callID+"/t2" {
			t.Fatalf("turns sent by the resume %+v, want one t2", sent)
		}
		text := turnText(sent[0])
		if !strings.Contains(text, "TASK: build the parser") || !strings.Contains(text, "CRITERIA: it parses the examples") || strings.Contains(text, "Continue the same task") {
			t.Errorf("the t2 input %q is not the original packet", text)
		}
		view, _ := e.threads.Thread(thread)
		for _, tu := range view.Turns {
			if tu.ClientID == callID+"/t1" {
				t.Errorf("the refused turn exists on the host")
			}
		}
	})
	t.Run("a turn carries the marker", func(t *testing.T) {
		e := newEnv(t)
		refuse(e)
		value[[]string](t, e.run(body))
		callID := e.lastRow("build", engine.EvHeld).CallID
		thread := str(e.lastRow("build", engine.EvThreadStarted).Data, "thread_id")
		// the request the host refused made a turn after all (one that edits nothing)
		e.threads.OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: "done"} })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		op := callID + "/t1"
		if _, err := e.client.Call(ctx, "turn/start", map[string]any{"threadId": thread, "clientUserMessageId": op,
			"input": []any{map[string]any{"type": "text", "text": agent.MarkerPrefix + op + "\n\n" + packet, "text_elements": []any{}}}}); err != nil {
			t.Fatal(err)
		}
		turns := len(e.requests("turn/start"))
		e.newProcess("proc-2")
		e.edits(nil)
		res := e.resume(body)
		got := value[[]string](t, res)
		if got[0] != "unknown" || got[1] != engine.ReasonDispatchUnknown {
			t.Fatalf("%v", got)
		}
		if n := len(e.requests("turn/start")) - turns; n != 0 {
			t.Errorf("%d turn/start requests after a marker was found", n)
		}
		if rows := kinds(e.rows("build")); rows[len(rows)-1] != engine.EvOutcomeUnknown {
			t.Errorf("rows %v", rows)
		}
		if len(e.flowRows(engine.EvEscalation)) == 0 {
			t.Errorf("no escalation names the node")
		}
	})
}

// An executor build held after it edited files. When the worktree is still the state the hold recorded, the call goes on in
// place - the same thread, the same worktree, turn t2, no new allocation. When the files changed during the hold, the held
// position is superseded and runs again in a worktree restored at the boundary, on a new thread; the state the hold recorded is
// kept on a superseded ref.
func TestHeldExecutorDirtyWorktreeResume(t *testing.T) {
	body := `const b = await agent.call('heldme build the thing', { agentType: 'executor', node: 'n1', label: 'build' })
return [b.status]`
	setup := func(t *testing.T) *env {
		e := newEnv(t)
		e.edits(func(r fakehost.TurnRequest) *fakehost.TurnPlan {
			if r.N == 1 && strings.Contains(r.Text, "heldme") {
				writeFile(t, filepath.Join(e.threadCwd(r.ThreadID), "heldme.txt"), "edited then held\n")
				return &fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "busy", CodexErrorInfo: "serverOverloaded"}}
			}
			return nil
		})
		if got := value[[]string](t, e.run(body)); got[0] != "held" {
			t.Fatalf("%v", got)
		}
		return e
	}
	t.Run("the worktree is the state the hold recorded", func(t *testing.T) {
		e := setup(t)
		callID := e.lastRow("build", engine.EvHeld).CallID
		thread := str(e.lastRow("build", engine.EvThreadStarted).Data, "thread_id")
		starts := len(e.requests("thread/start"))
		e.newProcess("proc-2")
		e.edits(nil)
		if got := value[[]string](t, e.resume(body)); got[0] != "completed" {
			t.Fatalf("%v", got)
		}
		if n := len(e.requests("thread/start")) - starts; n != 0 {
			t.Errorf("%d threads started", n)
		}
		turns := e.requests("turn/start")
		last := turns[len(turns)-1]
		if last.Params["threadId"] != thread || markerOf(turnText(last)) != callID+"/t2" {
			t.Errorf("the last turn %+v is not t2 of %s on %s", last, callID, thread)
		}
		if n := countOf(kinds(e.rows("build")), engine.EvPrepared); n != 1 {
			t.Errorf("%d prepared rows: a new allocation was made", n)
		}
		if len(e.flowRows(engine.EvWorktreeRestored)) != 0 {
			t.Errorf("a worktree was restored")
		}
		entries, _ := os.ReadDir(filepath.Join(e.state, "flow", e.runID, "wt", "n1"))
		if len(entries) != 1 {
			t.Errorf("worktree directories %v", entries)
		}
	})
	t.Run("the files changed during the hold", func(t *testing.T) {
		e := setup(t)
		callID := e.lastRow("build", engine.EvHeld).CallID
		held := str(e.lastRow("build", engine.EvHeld).Data, "p_held")
		old := e.wt("n1", 1, 0)
		writeFile(t, filepath.Join(old, "heldme.txt"), "changed by someone while the call was held\n")
		changed := e.snapshot(old)
		e.newProcess("proc-2")
		e.edits(nil)
		if got := value[[]string](t, e.resume(body)); got[0] != "completed" {
			t.Fatalf("%v", got)
		}
		sup := e.lastRow("build", engine.EvSuperseded)
		if str(sup.Data, "reason") != engine.ReasonSupersededByResume || lineageOf(sup.Data) != 1 {
			t.Errorf("superseded row %v", sup.Data)
		}
		restored := e.wt("n1", 2, 1)
		alloc, _ := e.lastRow("build", engine.EvPrepared).Data["allocation"].(map[string]any)
		if alloc["kind"] != "restore" || alloc["path"] != restored || alloc["start_commit"] != e.base {
			t.Errorf("the new attempt's allocation %v", alloc)
		}
		starts := e.requests("thread/start")
		if len(starts) != 2 || starts[1].Params["cwd"] != restored {
			t.Errorf("thread/start requests %+v: the second must be in %s", starts, restored)
		}
		if got := e.flowRows(engine.EvWorktreeRestored); len(got) != 1 || got[0].Data["P"] != nil || got[0].Data["C"] != e.base {
			t.Errorf("worktree_restored %+v", got)
		}
		// the files of the old worktree as they are, and the state the hold recorded, are both kept
		ref := engine.SupersededRef(e.runID, 1, "root", "n1")
		if git(t, e.repo, "rev-parse", ref+"^{tree}") != changed {
			t.Errorf("the superseded ref %s does not keep the worktree as it was left", ref)
		}
		heldRef := engine.SupersededHeldRef(e.runID, 1, "root", "n1", callID)
		if git(t, e.repo, "rev-parse", heldRef+"^{tree}") != held || held == changed {
			t.Errorf("the held state %s is not kept on %s", held, heldRef)
		}
	})
}
