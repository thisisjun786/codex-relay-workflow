package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// The lane lab of CRW-1277: the run process of the shipped binary (ProductionWorld, with the lane operations SchedulerLane builds) over a
// temporary relay store with a real plan put into it, a temporary git repository whose target branch the run integrates into, and the
// fake App Server host with a scripted model. Only what the run reaches outside the store and git is replaced: the verification of a head
// and of a merged tree (a sealed verification-record/1 of the commit, as crw-dev ci local writes one) and the skills digest.

// labIssue is an issue of the lab's handoff.
type labIssue struct {
	Key, Milestone string
	After          []string
}

// laneLab is one world of the lane lab.
type laneLab struct {
	t      *testing.T
	w      *world
	td     *testDeps
	repo   string
	plan   string
	host   *fakehost.Server
	input  compile.Input
	file   string
	nodeOf map[string]string // issue -> node id
	issue  map[string]string // node id -> issue

	mu  sync.Mutex
	cur *labJournal
	// kill: the process is ended after the killAt-th effect-bearing call of the relay (0: never), counted over the lab's life.
	killAt  int
	writes  int
	killed  bool
	cancel  context.CancelFunc
	effects []string
	// verifications counts the verifications of merged trees; processes the run processes the lab ran.
	verifications int
	processes     int
	// after is called after every effect-bearing call of the relay, with the relay of the process and the call's name.
	after func(r *SchedRelay, effect string)
	// killBetween kills the process once, between the batch of the integration of this node and its observation.
	killBetween      string
	pendingIntegrate string
	// reports are the milestone reports the runs sent up.
	reports []string
}

const labOwner = "thread-owner"

// isolateGit points every git command of the test at its own home and no global or system configuration.
func isolateGit(t *testing.T) {
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

func (l *laneLab) git(dir string, args ...string) string {
	l.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		l.t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newLaneLab makes the world: the repository with its target branch "integration", the plan of the issues put into the store at
// revision 1, the project's parent binding of the owner thread, the fake host, and the lane's operations installed as the shipped
// binary installs them with the lab's verifications.
func newLaneLab(t *testing.T, issues ...labIssue) *laneLab {
	t.Helper()
	isolateGit(t)
	l := &laneLab{t: t, w: newWorld(t), plan: "plan-1", nodeOf: map[string]string{}, issue: map[string]string{}}
	l.repo = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(l.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.git(l.repo, "init", "-q", "-b", "main")
	l.git(l.repo, "add", "-A")
	l.git(l.repo, "commit", "-q", "-m", "base")
	l.git(l.repo, "branch", "integration")

	p := compile.Project{Key: "P", PlanID: l.plan, Target: &compile.Target{Repository: l.repo, BaseRef: "integration"}}
	seen := map[string]bool{}
	for _, is := range issues {
		p.Issues = append(p.Issues, compile.Issue{Key: is.Key, Title: "issue " + is.Key, Criteria: []string{"the work of " + is.Key + " is in node-file of its node"}, Milestone: is.Milestone,
			After: is.After, Regions: []compile.Region{{Path: "node-" + is.Key + ".txt", Kind: "file"}}})
		if !seen[is.Milestone] {
			seen[is.Milestone] = true
			p.Milestones = append(p.Milestones, compile.Milestone{Key: is.Milestone, Title: "milestone " + is.Milestone, Criteria: []string{is.Milestone + " is integrated"},
				Surfaces: []string{"logic"}, Grade: "C2", RequiredCommands: []string{}})
		}
	}
	l.input = compile.Input{Schema: compile.InputSchema, AuthorTaskID: labOwner, Projects: []compile.Project{p}}
	res, err := compile.Compile(l.input, compile.Options{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := (&dag.Repo{Store: l.w.st}).Put(context.Background(), *res.Plans[0].Change); err != nil {
		t.Fatalf("put: %v", err)
	}
	for _, is := range res.Plans[0].Preview.Issues {
		l.nodeOf[is.Key], l.issue[is.NodeID] = is.NodeID, is.Key
	}
	now := "2026-10-12T08:00:00.000000Z"
	if err := storeseed.InsertScopeBinding(context.Background(), l.w.st, store.ScopeBindingsRow{BindingID: "bind-parent", Role: "parent", ScopeKind: "project", ScopeKey: "P",
		TaskID: labOwner, HostID: "host", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(l.input, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	l.file = filepath.Join(t.TempDir(), "handoff.json")
	if err := os.WriteFile(l.file, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	l.host = fakehost.Start(t)
	l.host.Threads().ThreadIDs(func(n int) string { return fmt.Sprintf("0190a000-0000-7000-8000-%012x", n) })
	l.host.Threads().OnTurn(l.onTurn)

	l.td = l.w.deps()
	l.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		w, err := ProductionWorld(ctx, req)
		if err != nil {
			return World{}, err
		}
		j := &labJournal{Journal: w.Journal}
		l.mu.Lock()
		l.cur = j
		l.mu.Unlock()
		w.Journal = j
		return w, nil
	}
	old := OpsFor
	OpsFor = func(ctx context.Context, req LaneRequest) (LaneWorld, error) {
		if _, ok := req.Reporter.(report.Reporter); !ok {
			t.Errorf("the run's world hands the lane no reporter of F11: %T", req.Reporter)
		}
		return BuildLane(ctx, req, LaneOptions{Verifier: labVerifier{l}, IntegrationVerify: l.verifyMerged, SkillsDigest: "sha256:lab-skills", Reporter: &labReporter{l: l},
			Relay: func(r *SchedRelay) ops.Relay {
				r.testAfterBatch = func() error {
					l.mu.Lock()
					hit := l.killBetween != "" && strings.HasPrefix(l.pendingIntegrate, l.killBetween)
					if hit {
						l.killBetween = ""
					}
					l.mu.Unlock()
					if hit {
						l.kill()
						return errKilled
					}
					return nil
				}
				return &labRelay{SchedRelay: r, l: l}
			}})
	}
	t.Cleanup(func() { OpsFor = old })
	return l
}

// ---- the run ----

// start is `crw flow start` with the lab's state and host; it answers the spec of the run it started (the process is not run yet).
func (l *laneLab) start(argv ...string) Spec {
	l.t.Helper()
	before := len(l.td.spawned)
	code, out, errOut := l.td.do(append(argv, "--thread", labOwner, "--state", l.w.state, "--socket", l.host.SocketPath, "--repo", l.repo, "--force")...)
	if code != 0 || len(l.td.spawned) != before+1 {
		l.t.Fatalf("start %v: exit %d\n%s%s", argv, code, out, errOut)
	}
	return l.td.spawned[len(l.td.spawned)-1].Spec
}

// process runs the run's process as `crw flow _run` does, in this process: a new run, or the resume of one. It answers the exit status
// and whether the lab killed it.
func (l *laneLab) process(run string, resume bool) (int, bool) {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	l.mu.Lock()
	l.cancel, l.killed = cancel, false
	l.processes++
	l.mu.Unlock()
	argv := []string{ProcessCommand, "--state", l.w.state, "--socket", l.host.SocketPath}
	if resume {
		argv = append(argv, "--resume")
	}
	argv = append(argv, run)
	var errOut bytes.Buffer
	l.w.close()
	code := RunWith(ctx, l.td.Deps, argv, strings.NewReader(""), &bytes.Buffer{}, &errOut)
	l.w.open()
	l.mu.Lock()
	killed := l.killed
	l.mu.Unlock()
	if code != 0 && !killed {
		l.t.Logf("process %s (resume %v): exit %d: %s", run, resume, code, errOut.String())
	}
	return code, killed
}

// runToEnd runs a run's process, and resumes it after every kill, until a process ends without being killed; it answers the result.
func (l *laneLab) runToEnd(run string) Result {
	l.t.Helper()
	resume := false
	for i := 0; i < 20; i++ {
		_, killed := l.process(run, resume)
		if !killed {
			res, ok := ReadResult(l.w.state, run)
			if !ok {
				l.t.Fatalf("run %s left no result", run)
			}
			return res
		}
		resume = true
	}
	l.t.Fatalf("run %s was killed 20 times", run)
	return Result{}
}

// laneValue is the value of a project lane run.
type laneValue struct {
	State string `json:"state"`
	Tip   string `json:"tip"`
	Lanes []struct {
		Node, State, Reason string
	} `json:"lanes"`
}

func (l *laneLab) closed(res Result) laneValue {
	l.t.Helper()
	var v laneValue
	if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "closed" {
		for _, e := range l.events() {
			if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
				l.t.Logf("escalation: %v", e.Data)
			}
		}
		l.t.Fatalf("the run did not close its milestone: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	return v
}

// labJournal is the journal of the current process: the model reads the calls from it, and a killed process writes no more rows.
type labJournal struct {
	engine.Journal
	mu   sync.Mutex
	dead bool
}

func (j *labJournal) Append(e engine.Event) engine.Event {
	j.mu.Lock()
	dead := j.dead
	j.mu.Unlock()
	if dead {
		return e
	}
	return j.Journal.Append(e)
}

// SetOnCommit forwards the commit notice as runJournal does, so the lab's runs register the engine's upward hook as the shipped binary's do.
func (j *labJournal) SetOnCommit(notify func()) {
	if n, ok := j.Journal.(engine.CommitNotifier); ok {
		n.SetOnCommit(notify)
	}
}

func (j *labJournal) Err() error {
	if f, ok := j.Journal.(engine.FailingJournal); ok {
		return f.Err()
	}
	return nil
}

func (l *laneLab) events() []engine.Event {
	l.mu.Lock()
	j := l.cur
	l.mu.Unlock()
	if j == nil {
		return nil
	}
	return j.Events()
}

// kill ends the current process the way a crash does: it writes no more rows and its context ends.
func (l *laneLab) kill() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur != nil {
		l.cur.mu.Lock()
		l.cur.dead = true
		l.cur.mu.Unlock()
	}
	l.killed = true
	if l.cancel != nil {
		l.cancel()
	}
}

// labRelay is the run's SchedRelay with the lab's kill: the process ends right after the relay answered the killAt-th effect-bearing
// call, before the lane recorded what it was told, and a killed process asks the relay nothing more.
type labRelay struct {
	*SchedRelay
	l *laneLab
}

var errKilled = errors.New("the process was killed")

func (r *labRelay) effect(name string, do func() error) error {
	r.l.mu.Lock()
	killed := r.l.killed
	r.l.mu.Unlock()
	if killed {
		return errKilled
	}
	err := do()
	r.l.mu.Lock()
	r.l.writes++
	r.l.effects = append(r.l.effects, name)
	hit := r.l.killAt > 0 && r.l.writes == r.l.killAt
	after := r.l.after
	r.l.mu.Unlock()
	if after != nil {
		after(r.SchedRelay, name)
	}
	if hit {
		r.l.kill()
	}
	return err
}

func (r *labRelay) Release(ctx context.Context, req ops.ReleaseRequest) (out ops.ReleaseResult, err error) {
	err = r.effect("release:"+req.Node, func() (e error) { out, e = r.SchedRelay.Release(ctx, req); return })
	return
}

func (r *labRelay) Accept(ctx context.Context, req ops.AcceptRequest) (out ops.AcceptResult, err error) {
	err = r.effect("accept:"+req.Node, func() (e error) { out, e = r.SchedRelay.Accept(ctx, req); return })
	return
}

func (r *labRelay) Integrate(ctx context.Context, req ops.IntegrateRequest) (out ops.IntegrateResult, err error) {
	r.l.mu.Lock()
	r.l.pendingIntegrate = req.Node
	r.l.mu.Unlock()
	err = r.effect("integrate:"+req.Node, func() (e error) { out, e = r.SchedRelay.Integrate(ctx, req); return })
	return
}

func (r *labRelay) Close(ctx context.Context, req ops.CloseRequest) (out ops.CloseResult, err error) {
	err = r.effect("close:"+req.Node, func() (e error) { out, e = r.SchedRelay.Close(ctx, req); return })
	return
}

// labReporter is the delivery of the milestone report: it records the reports instead of telling a thread (report.Reporter sends
// through `crw manage send-parent`, a process the test does not start).
type labReporter struct{ l *laneLab }

func (r *labReporter) ReportUp(_ context.Context, rep ops.Report) (string, error) {
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	r.l.reports = append(r.l.reports, rep.Plan+"/"+rep.Milestone)
	return "rep_" + rep.Milestone, nil
}

// ---- the verifications ----

// sealed is the verification-record/1 crw-dev ci local leaves for a commit over a base: the keys of the commit in the repository, sealed.
func (l *laneLab) sealed(ctx context.Context, dir, head, base string) ([]byte, error) {
	keys, err := dagsched.CommitVerificationKeys(ctx, dir, head)
	if err != nil {
		return nil, err
	}
	tree, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", head+"^{tree}").Output()
	if err != nil {
		return nil, err
	}
	return dagsched.SealVerificationRecord(dagsched.VerificationRecord{Runner: "local", Repository: l.repo, BaseCommit: base, HeadCommit: head, TreeHash: strings.TrimSpace(string(tree)),
		CiDigest: keys.CiDigest, Tools: keys.Pins, Pins: keys.Pins, PinMismatch: []string{}, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Result: "pass", Jobs: []json.RawMessage{}})
}

// labVerifier verifies a node's head and the final gate's commands: a pass, with the sealed record of the head over the base.
type labVerifier struct{ l *laneLab }

func (v labVerifier) Verify(ctx context.Context, req ops.VerifyRequest) (ops.VerifyOutcome, error) {
	base := req.Base
	if base == "" {
		base = req.Head
	}
	raw, err := v.l.sealed(ctx, v.l.repo, req.Head, base)
	if err != nil {
		return ops.VerifyOutcome{}, err
	}
	digest, err := dagsched.VerificationRecordDigest(raw)
	if err != nil {
		return ops.VerifyOutcome{}, err
	}
	cmd := req.Command
	if cmd == "" {
		cmd = ops.CILocalCommand
	}
	return ops.VerifyOutcome{Pass: true, Command: cmd, Digest: digest, Record: raw, Summary: "pass", Failures: []string{}}, nil
}

// verifyMerged is the verification of an integration batch's merged tree: the sealed record of the merged commit at the path the batch
// gives.
func (l *laneLab) verifyMerged(ctx context.Context, dir string, env []string) error {
	vars := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	raw, err := l.sealed(ctx, dir, vars["CRW_VERIFY_HEAD"], vars["CRW_VERIFY_BASE"])
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.verifications++
	l.mu.Unlock()
	return os.WriteFile(vars["CRW_VERIFY_RECORD"], raw, 0o600)
}

// ---- the model ----

var labOpLine = regexp.MustCompile(`^crw-flow-op: (c_[0-9a-f]+)/t(\d+)`)

// labCall is the call a turn belongs to.
type labCall struct {
	ID, Label, Role, Node, Cwd string
	N                          int
}

func (l *laneLab) callOf(r fakehost.TurnRequest) labCall {
	c := labCall{N: r.N}
	m := labOpLine.FindStringSubmatch(strings.SplitN(r.Text, "\n", 2)[0])
	if m == nil {
		return c
	}
	c.ID = m[1]
	ev := l.events()
	for i := len(ev) - 1; i >= 0; i-- {
		e := ev[i]
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && e.CallID == c.ID {
			c.Label, c.Role, c.Node = e.Label, str(e.Data, "role"), str(e.Data, "node")
			if a, ok := e.Data["allocation"].(map[string]any); ok && str(a, "path") != "" {
				c.Cwd = str(a, "path")
			} else {
				c.Cwd = str(e.Data, "thread_cwd")
			}
			break
		}
	}
	return c
}

func labSay(v any) fakehost.TurnPlan {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return fakehost.TurnPlan{Message: string(raw)}
}

// milestoneNodes are the nodes of the milestone the current run read.
func (l *laneLab) milestoneNodes() []ops.PlanNode {
	ev := l.events()
	for i := len(ev) - 1; i >= 0; i-- {
		e := ev[i]
		if e.Kind == engine.KindCall && e.Op == ops.OpMilestone && e.Event == engine.EvCompleted {
			var ms ops.Milestone
			if s, ok := e.Data["result"].(string); ok && json.Unmarshal([]byte(s), &ms) == nil {
				return ms.Nodes
			}
		}
	}
	return nil
}

func (l *laneLab) design() ops.Design {
	d := ops.Design{Decisions: []ops.Decision{{ID: "D1", Evidence: []string{"README.md"}, Change: "each node writes its own file", Alternatives: []string{}, Assumptions: []string{}}},
		OpenQuestions: []ops.OpenQuestion{}, Risks: []string{}}
	for _, n := range l.milestoneNodes() {
		after := n.After
		if after == nil {
			after = []string{}
		}
		d.Issues = append(d.Issues, ops.DesignIssue{Issue: n.Issue, Node: n.ID, SizeFit: "executor", IssueSizeDecision: "ok", EditRegions: []string{"node-" + n.Issue + ".txt"},
			Approach: "write the file of " + n.ID, TestsFirst: []string{"the file of " + n.ID + " exists"}, DoneCondition: "the file of " + n.ID + " is committed", DecisionRefs: []string{"D1"}, OrderAfter: after})
	}
	return d
}

// onTurn is the model: every role answers what the happy path needs, and an executor writes its issue's file in its worktree.
func (l *laneLab) onTurn(r fakehost.TurnRequest) fakehost.TurnPlan {
	c := l.callOf(r)
	switch {
	case c.Role == "architect" && strings.HasPrefix(c.Label, "reflection:"):
		return labSay(map[string]any{"verdict": "ALIGNED", "misaligned": []any{}})
	case c.Role == "architect":
		return labSay(l.design())
	case c.Role == "reviewer-final":
		return labSay(map[string]any{"verdict": "verified", "criteria": []any{map[string]any{"id": "c1", "met": true, "evidence": "read the range"}}, "defects": []any{},
			"alignment": map[string]any{"verdict": "ALIGNED", "drift": []any{}}, "summary": "reviewed the range"})
	case c.Role == "reviewer" && strings.HasPrefix(c.Label, "design-audit"):
		return labSay(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "the DESIGN holds"})
	case c.Role == "reviewer":
		return labSay(map[string]any{"verdict": "verified", "criteria": []any{map[string]any{"id": "c1", "met": true, "evidence": "read the head"}}, "defects": []any{}, "summary": "reviewed"})
	case c.Role == "explorer":
		return labSay(map[string]any{"answer": "answer", "evidence": []string{"README.md"}, "verified": []string{"README.md"}, "inferred": []string{}, "unknown": []string{}})
	case c.Role == "executor" || c.Role == "executor-t1":
		if c.Cwd != "" && c.Node != "" {
			path := filepath.Join(c.Cwd, "node-"+l.issue[c.Node]+".txt")
			prior, _ := os.ReadFile(path)
			if err := os.WriteFile(path, append(prior, []byte(c.Label+"\n")...), 0o644); err != nil {
				l.t.Errorf("the executor cannot write %s: %v", path, err)
			}
		}
		return labSay(map[string]any{"outcome": "ready_for_review", "files": []string{}, "verification": []any{}, "summary": "done", "gap": nil, "research_question": nil, "plan_mismatch": nil})
	}
	return fakehost.TurnPlan{Message: "ok"}
}

// ---- what the relay holds ----

func (l *laneLab) count(query string, args ...any) int {
	l.t.Helper()
	var n int
	if err := l.w.st.DB.QueryRow(query, args...).Scan(&n); err != nil {
		l.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// effectsOnce checks that every node of the issues was released once, accepted once and merged by one batch, holds no slot, and that its
// file is on the target branch.
func (l *laneLab) effectsOnce(issues ...string) {
	l.t.Helper()
	tip := l.git(l.repo, "rev-parse", "integration")
	for _, key := range issues {
		node := l.nodeOf[key]
		if n := l.count("SELECT COUNT(*) FROM dag_agent_executions WHERE plan_id = ? AND node_id = ?", l.plan, node); n != 1 {
			l.t.Errorf("%s has %d agent executions", node, n)
		}
		if n := l.count("SELECT COUNT(*) FROM dag_agent_acceptances WHERE plan_id = ? AND node_id = ?", l.plan, node); n != 1 {
			l.t.Errorf("%s has %d agent acceptances", node, n)
		}
		if n := l.count("SELECT COUNT(*) FROM dag_integration_batches WHERE plan_id = ? AND merged_json LIKE ?", l.plan, `%"`+node+`"%`); n != 1 {
			l.t.Errorf("%s was merged by %d batches", node, n)
		}
		if n := l.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = ? AND state = 'held'", l.plan+"/"+node); n != 0 {
			l.t.Errorf("%s still holds %d slots", node, n)
		}
		if n := l.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = ?", l.plan+"/"+node); n != 1 {
			l.t.Errorf("%s had %d slot tenures", node, n)
		}
		if out, err := exec.Command("git", "-C", l.repo, "cat-file", "-e", tip+":node-"+key+".txt").CombinedOutput(); err != nil {
			l.t.Errorf("the work of %s is not on the target branch: %v %s", key, err, out)
		}
	}
}
