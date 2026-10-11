package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

const header = "export const meta = { name: 't' }\n"

// verdictSchema is a strict outputSchema the scripts of these tests pass.
const verdictSchema = `{ type: 'object', properties: { verdict: { type: 'string' } }, required: ['verdict'], additionalProperties: false }`

const passMessage = `{"verdict":"pass"}`

// isolate points every git command of the test at its own home and no global or system configuration, so nothing of the
// real machine's git state is read or written.
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

// git runs a git command for the test setup and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitFails reports whether a git command exits non-zero.
func gitFails(dir string, args ...string) bool {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Run() != nil
}

// newRepo makes a temporary repository with one commit and returns its directory and that commit.
func newRepo(t *testing.T, root string) (dir, base string) {
	t.Helper()
	dir = filepath.Join(root, "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	return dir, git(t, dir, "rev-parse", "HEAD")
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

// gitCall is one git command the allocator ran.
type gitCall struct {
	Dir  string
	Args []string
}

// hookJournal is the in-memory journal with a hook that sees every row after it was appended.
type hookJournal struct {
	*engine.MemoryJournal
	mu    sync.Mutex
	after func(engine.Event)
	// before sees a row before it is appended.
	before func(engine.Event)
}

func (j *hookJournal) Append(e engine.Event) engine.Event {
	j.mu.Lock()
	before, after := j.before, j.after
	j.mu.Unlock()
	if before != nil {
		before(e)
	}
	out := j.MemoryJournal.Append(e)
	if after != nil {
		after(out)
	}
	return out
}

func (j *hookJournal) onAfter(fn func(engine.Event)) {
	j.mu.Lock()
	j.after = fn
	j.mu.Unlock()
}

func (j *hookJournal) onBefore(fn func(engine.Event)) {
	j.mu.Lock()
	j.before = fn
	j.mu.Unlock()
}

// env is one backend on a fake host with a temporary repository.
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
	pool    int

	gitMu sync.Mutex
	calls []gitCall
}

type envOption func(*envConfig)

type envConfig struct {
	bounds appserver.PhaseBounds
	poll   time.Duration
}

// withBounds sets the phase bounds of the client (a short ack bound makes a delayed answer a PhaseTimeout).
func withBounds(b appserver.PhaseBounds) envOption { return func(c *envConfig) { c.bounds = b } }

// withPoll sets how long a turn waits for a notification before it reads the turn.
func withPoll(d time.Duration) envOption { return func(c *envConfig) { c.poll = d } }

// newEnv starts a fake host, a temporary repository and a backend on both.
func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	isolate(t)
	cfg := envConfig{bounds: appserver.DefaultBounds, poll: 25 * time.Millisecond}
	for _, o := range opts {
		o(&cfg)
	}
	e := &env{t: t, runID: "run-test", policy: engine.FakePolicy(), journal: &hookJournal{MemoryJournal: engine.NewMemoryJournal()}}
	e.host = fakehost.Start(t)
	e.threads = e.host.Threads()
	e.threads.OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: passMessage} })
	e.root = t.TempDir()
	e.repo, e.base = newRepo(t, e.root)
	e.state = filepath.Join(e.root, "state")
	registry := agent.NewThreads()
	e.owned = registry
	e.client = appserver.New(e.host.SocketPath, cfg.bounds, agent.ClientOption(registry))
	t.Cleanup(func() { _ = e.client.Close() })
	realGit := agent.ExecGit("git")
	var err error
	e.alloc, err = agent.NewAllocator(agent.AllocatorConfig{
		Repo: e.repo, StateDir: e.state, RunID: e.runID,
		Base: func(string) (string, error) { return e.base, nil },
		Git: func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
			e.gitMu.Lock()
			e.calls = append(e.calls, gitCall{Dir: dir, Args: append([]string(nil), args...)})
			e.gitMu.Unlock()
			return realGit(ctx, dir, env, args...)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.backend, err = agent.New(agent.Config{Client: e.client, Allocator: e.alloc, Threads: registry, PollEvery: cfg.poll, InterruptWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// gitCalls returns the git commands the allocator ran so far.
func (e *env) gitCalls() []gitCall {
	e.gitMu.Lock()
	defer e.gitMu.Unlock()
	return append([]gitCall(nil), e.calls...)
}

// worktreeAdds counts the `git worktree add` commands the allocator ran.
func (e *env) worktreeAdds() int {
	n := 0
	for _, c := range e.gitCalls() {
		if len(c.Args) >= 2 && c.Args[0] == "worktree" && c.Args[1] == "add" {
			n++
		}
	}
	return n
}

// plan sets how every turn goes.
func (e *env) plan(fn func(fakehost.TurnRequest) fakehost.TurnPlan) { e.threads.OnTurn(fn) }

func (e *env) config() engine.Config {
	return engine.Config{Backend: e.backend, Policy: e.policy, Journal: e.journal, RunID: e.runID, Pool: e.pool}
}

// run executes the script body and returns its result.
func (e *env) run(body string) *engine.Result {
	e.t.Helper()
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte(header + body)})
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := engine.Run(ctx, prog, e.config())
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// value decodes the script's return value.
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

// calls returns the calls of a result keyed by label.
func calls(res *engine.Result) map[string]engine.CallResult {
	out := map[string]engine.CallResult{}
	for _, c := range res.Calls {
		out[c.Label] = c
	}
	return out
}

// request is a request the fake host received, decoded.
type request struct {
	Method string
	Params map[string]any
}

// requests returns the requests of the methods named (all of them when none is named), in order.
func (e *env) requests(methods ...string) []request {
	var out []request
	for _, r := range e.host.Requests() {
		if len(methods) > 0 {
			found := false
			for _, m := range methods {
				found = found || m == r.Method
			}
			if !found {
				continue
			}
		}
		var params map[string]any
		_ = json.Unmarshal(r.Params, &params)
		out = append(out, request{Method: r.Method, Params: params})
	}
	return out
}

// rows returns the rows of one call (by label) in order.
func (e *env) rows(label string) []engine.Event {
	var out []engine.Event
	for _, ev := range e.journal.Events() {
		if ev.Kind == engine.KindCall && ev.Label == label {
			out = append(out, ev)
		}
	}
	return out
}

// kinds are the event names of rows.
func kinds(rows []engine.Event) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Event
	}
	return out
}

// row returns the first row of a call with the event name, or fails.
func (e *env) row(label, event string) engine.Event {
	e.t.Helper()
	for _, r := range e.rows(label) {
		if r.Event == event {
			return r
		}
	}
	e.t.Fatalf("call %q has no %s row: %v", label, event, kinds(e.rows(label)))
	return engine.Event{}
}

// flowRows are the flow rows with the event name.
func (e *env) flowRows(event string) []engine.Event {
	var out []engine.Event
	for _, ev := range e.journal.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == event {
			out = append(out, ev)
		}
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// jsonEqualAny compares two values as JSON documents.
func jsonEqualAny(t *testing.T, a, b any) bool {
	t.Helper()
	norm := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// asRPC reports whether err is an App Server error answer and sets *target to it.
func asRPC(err error, target **appserver.RPCError) bool { return errors.As(err, target) }

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
