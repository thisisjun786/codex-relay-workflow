package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// The lane lab of CRW-1277: the run process of the shipped binary (ProductionWorld, with the lane operations SchedulerLane builds) over a
// temporary relay store with a real plan put into it, a temporary git repository whose target branch the run integrates into, and the
// fake App Server host with a scripted model. Only what the run reaches outside the store and git is replaced: the verification of a head
// and of a merged tree (a sealed verification-record/1 of the commit, as crw-dev ci local writes one) and the skills digest.

// labIssue is an issue of the lab's handoff.
type labIssue struct {
	Key, Milestone string
	After          []string
	// Regions are the issue's planned regions; node-<key>.txt when none are given.
	Regions []compile.Region
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
	// afterRow, when a test sets it, runs with every row the current process's journal appends (CRW-1329): a test kills the
	// process the moment a row it waits for is written.
	afterRow func(engine.Event)
	// kill: the process is ended after the killAt-th effect-bearing call of the relay (0: never), counted over the lab's life.
	killAt  int
	writes  int
	killed  bool
	cancel  context.CancelFunc
	effects []string
	// the pull request seams of a bundle's e2e (CRW-1423): prFactory the tracking adapter factory, prRepo the tracking
	// repository override, mergeRequester the parent's merge-request delivery (nil records nothing).
	prFactory      func(string) (tracking.PullRequests, error)
	prRepo         string
	mergeRequester ops.MergeRequester
	// verifications counts the verifications of merged trees; processes the run processes the lab ran.
	verifications int
	processes     int
	// after is called after every effect-bearing call of the relay, with the relay of the process and the call's name.
	after func(r *SchedRelay, effect string)
	// killBetween kills the process once, between the batch of the integration of this node and its observation.
	killBetween      string
	pendingIntegrate string
	// reports are the milestone reports the runs sent up; reportBodies the body the shipped Reporter would store for each
	// (report.MilestoneBody of the very ops.Report the op built), in the same order.
	reports      []string
	reportBodies []string
	// tipErr, when a test sets it, is asked before every read of the integration tip the lane makes through the relay, and an error it
	// answers is the read's (the relay's answer at a step after the lanes, CRW-1382).
	tipErr func() error
	// owner is the thread the plan's author, the project's parent binding and the runs are (labOwner); target the target branch.
	owner, target string
	// releaseAnswer, when a test sets it, is given every answer of the relay's release and answers the one the lane reads (CRW-1404).
	releaseAnswer func(req ops.ReleaseRequest, out ops.ReleaseResult) ops.ReleaseResult
	// designEdit, when a test sets it, edits the DESIGN the lab's architect answers.
	designEdit func(d *ops.Design)
	// verifyFail, when a test sets it, is asked by labVerifier for every verification request and a true answer fails the
	// verification of that head (CRW-1490 close repair tests).
	verifyFail func(req ops.VerifyRequest) bool
	// mergeVerifyFail, when a test sets it, is the error verifyMerged answers for the integration batch's merged tree
	// (CRW-1595): the batch's verification fails with it.
	mergeVerifyFail func(vars map[string]string) error
	// turn, when a test sets it, is asked first for every turn of the model: a plan it answers with true is the turn's, false leaves
	// the turn to the happy path below (CRW-1452).
	turn func(c labCall, r fakehost.TurnRequest) (fakehost.TurnPlan, bool)
	// texts are the turn texts the model was given, by the label of their call (the latest turn of a label), for a test's asserts.
	texts map[string]string
	// uc is the lab's unit checker, the lane's crw.unitCheck tool (UnitChecks scripts it).
	uc *labUnitChecker
	// cc is the lab's crosschecker, the engine of crw.crosscheck (Crosschecks scripts it).
	cc *labCrosschecker
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

// TestLaneLabIgnoresAShortHost (CRW-1295): the lab does not read the host's memory. The environment names a proc root whose
// MemAvailable is 1 GiB, far under the bound's floor, before the lab is made; the lab points the bound at its own proc fixture, so the
// release of A-1 is not deferred (defer:host_memory) and writes its execution.
func TestLaneLabIgnoresAShortHost(t *testing.T) {
	short := t.TempDir()
	if err := os.WriteFile(filepath.Join(short, "meminfo"), []byte("MemTotal: 64395904 kB\nMemAvailable: 1048576 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_DAG_HOST_PROC_ROOT", short)
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.buildLane("run-shorthost01", l.handoffBytes(nil), LaneOptions{})
	got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], RunID: r.RunID, RequestID: "req-short"})
	if err != nil || !got.Released {
		t.Fatalf("the release of A-1 on a host the environment names short of memory: %+v %v", got, err)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions"); n != 1 {
		t.Errorf("%d executions written, want 1", n)
	}
}

// TestLaneLabIgnoresTheCallersHostThresholds (CRW-1369): the lab does not read the caller's thresholds of the host memory bound either.
// The environment exports a floor of 100 GiB (over the fixture's 64 GiB) or a threshold that is not a number, for each variable
// HostMemoryFromEnvironment reads, before the lab is made; the lab pins them, so A-1 is released and writes its execution.
func TestLaneLabIgnoresTheCallersHostThresholds(t *testing.T) {
	for _, c := range []struct{ name, value string }{
		{"CRW_DAG_HOST_MIN_AVAILABLE_GIB", "100"},
		{"CRW_DAG_HOST_MIN_AVAILABLE_GIB", "lots"},
		{"CRW_DAG_HOST_MAX_SWAP_PERCENT", "lots"},
		{"CRW_DAG_HOST_MAX_PRESSURE_SOME_AVG60", "lots"},
		{"CRW_DAG_HOST_MAX_PRESSURE_FULL_AVG10", "lots"},
	} {
		t.Run(c.name+"="+c.value, func(t *testing.T) {
			t.Setenv(c.name, c.value)
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			r := l.buildLane("run-thresholds1", l.handoffBytes(nil), LaneOptions{})
			got, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], RunID: r.RunID, RequestID: "req-thresholds"})
			if err != nil || !got.Released {
				t.Fatalf("the release of A-1 with %s=%s in the caller's environment: %+v %v", c.name, c.value, got, err)
			}
			if n := l.count("SELECT COUNT(*) FROM dag_agent_executions"); n != 1 {
				t.Errorf("%d executions written, want 1", n)
			}
		})
	}
}

// labProcFixture writes a /proc of its own into a temporary directory: ample MemAvailable, no swap, and memory pressure far under the
// bound's ceilings. newLaneLab points CRW_DAG_HOST_PROC_ROOT at it and clears the threshold variables, so no lane-lab test reads the memory or the thresholds of the host it runs on.
func labProcFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pressure"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"meminfo":         "MemTotal: 134217728 kB\nMemAvailable: 67108864 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"pressure/memory": "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// newLaneLab makes the world: the repository with its target branch "integration", the plan of the issues put into the store at
// revision 1, the project's parent binding of the owner thread, the fake host, and the lane's operations installed as the shipped
// binary installs them with the lab's verifications, and the proc root of the host memory bound set to the lab's own fixture with its threshold variables cleared.
func newLaneLab(t *testing.T, issues ...labIssue) *laneLab {
	t.Helper()
	return newLaneLabFor(t, labOwner, "integration", issues...)
}

// labRegionsOf are the planned regions of a lab issue: its own, or the file node-<key>.txt. nil is the default; an explicitly empty
// list means the issue declares no regions, so its node is undeclared.
func labRegionsOf(is labIssue) []compile.Region {
	if is.Regions != nil {
		return is.Regions
	}
	return []compile.Region{{Path: "node-" + is.Key + ".txt", Kind: "file"}}
}

// newLaneLabFor is newLaneLab with the owner thread and the target branch named (a session run's owner is a thread of the fake
// host).
func newLaneLabFor(t *testing.T, owner, target string, issues ...labIssue) *laneLab {
	t.Helper()
	isolateGit(t)
	// the host memory bound reads this proc root, never the host's own /proc: a lab test does not depend on the memory of the machine
	t.Setenv("CRW_DAG_HOST_PROC_ROOT", labProcFixture(t))
	// and so do its thresholds (CRW-1369): an empty value is unset, so the bound judges the fixture by its defaults whatever the caller exported
	for _, name := range []string{dagsched.EnvHostMinAvailableGiB, dagsched.EnvHostMaxSwapPercent, dagsched.EnvHostMaxPressure, dagsched.EnvHostMaxPressureFull} {
		t.Setenv(name, "")
	}
	l := &laneLab{t: t, w: newWorld(t), plan: "plan-1", nodeOf: map[string]string{}, issue: map[string]string{}, owner: owner, target: target, uc: newLabUnitChecker(),
		cc: newLabCrosschecker()}
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
	l.git(l.repo, "branch", target)

	p := compile.Project{Key: "P", PlanID: l.plan, Target: &compile.Target{Repository: l.repo, BaseRef: target}}
	seen := map[string]bool{}
	for _, is := range issues {
		p.Issues = append(p.Issues, compile.Issue{Key: is.Key, Title: "issue " + is.Key, Criteria: []string{"the work of " + is.Key + " is in node-file of its node"}, Milestone: is.Milestone,
			After: is.After, Regions: labRegionsOf(is)})
		if !seen[is.Milestone] {
			seen[is.Milestone] = true
			p.Milestones = append(p.Milestones, compile.Milestone{Key: is.Milestone, Title: "milestone " + is.Milestone, Criteria: []string{is.Milestone + " is integrated"},
				Surfaces: []string{"logic"}, Grade: "C2", RequiredCommands: []string{}})
		}
	}
	l.input = compile.Input{Schema: compile.InputSchema, AuthorTaskID: owner, Projects: []compile.Project{p}}
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
		TaskID: owner, HostID: "host", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
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
		j := &labJournal{Journal: w.Journal, after: func(e engine.Event) {
			l.mu.Lock()
			hook := l.afterRow
			l.mu.Unlock()
			if hook != nil {
				hook(e)
			}
		}}
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
			UnitCheck: l.uc.seam(), Crosscheck: l.cc.seam(), NewPullRequests: l.prFactory, PRRepository: l.prRepo,
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

// start is `crw flow start` with the lab's state and host, on the App Server backend the lab's scripted model answers; it answers the spec of the run it started (the process is not run yet).
func (l *laneLab) start(argv ...string) Spec {
	l.t.Helper()
	before := len(l.td.spawned)
	code, out, errOut := l.td.do(append(argv, "--thread", l.owner, "--state", l.w.state, "--socket", l.host.SocketPath, "--repo", l.repo, "--force", "--backend", "app-server")...)
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
	mu    sync.Mutex
	dead  bool
	after func(engine.Event)
}

func (j *labJournal) Append(e engine.Event) engine.Event {
	j.mu.Lock()
	dead := j.dead
	j.mu.Unlock()
	if dead {
		return e
	}
	written := j.Journal.Append(e)
	if j.after != nil {
		j.after(written)
	}
	return written
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
	if err == nil && r.l.releaseAnswer != nil {
		out = r.l.releaseAnswer(req, out)
	}
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

func (r *labRelay) IntegrationTip(ctx context.Context, plan string) (string, error) {
	r.l.mu.Lock()
	tipErr := r.l.tipErr
	r.l.mu.Unlock()
	if tipErr != nil {
		if err := tipErr(); err != nil {
			return "", err
		}
	}
	return r.SchedRelay.IntegrationTip(ctx, plan)
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
	r.l.reportBodies = append(r.l.reportBodies, string(report.MilestoneBody(rep)))
	return "rep_" + rep.Milestone, nil
}

// MergeRequest delegates to the mergeRequester a test set; without one the call records nothing (a direct landing never asks).
func (r *labReporter) MergeRequest(ctx context.Context, mr ops.MergeRequest) (string, error) {
	r.l.mu.Lock()
	m := r.l.mergeRequester
	r.l.mu.Unlock()
	if m == nil {
		return "", nil
	}
	return m.MergeRequest(ctx, mr)
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

// labVerifier verifies a node's head and the final gate's commands: a pass, with the sealed record of the head over the base. A
// laneLab.verifyFail script may answer true for a request, which the verification records as a failure of that head.
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
	out := ops.VerifyOutcome{Pass: true, Command: cmd, Digest: digest, Record: raw, Summary: "pass", Failures: []string{}}
	if v.l.verifyFail != nil && v.l.verifyFail(req) {
		out.Pass, out.Summary, out.Failures = false, "the lab's scripted failure", []string{"the lab's scripted failure"}
	}
	return out, nil
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
	l.mu.Lock()
	fail := l.mergeVerifyFail
	l.mu.Unlock()
	if fail != nil {
		return fail(vars)
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

// labCall is the call a turn belongs to. Head is the head its prepared row pins (empty for a call that pins none).
type labCall struct {
	ID, Label, Role, Node, Cwd, Head string
	N                                int
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
			c.Label, c.Role, c.Node, c.Head = e.Label, str(e.Data, "role"), str(e.Data, "node"), str(e.Data, "head")
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
	if l.designEdit != nil {
		l.designEdit(&d)
	}
	return d
}

// onTurn is the model: every role answers what the happy path needs (the critic approves the DESIGN), and an executor writes its issue's
// file in its worktree.
func (l *laneLab) onTurn(r fakehost.TurnRequest) fakehost.TurnPlan {
	c := l.callOf(r)
	l.mu.Lock()
	if l.texts == nil {
		l.texts = map[string]string{}
	}
	if c.Label != "" {
		l.texts[c.Label] = r.Text
	}
	turn := l.turn
	l.mu.Unlock()
	if turn != nil {
		if plan, ok := turn(c, r); ok {
			return plan
		}
	}
	switch {
	case c.Role == "architect" && strings.HasPrefix(c.Label, "reflection:"):
		return labSay(map[string]any{"verdict": "ALIGNED", "misaligned": []any{}})
	case c.Role == "architect":
		return labSay(l.design())
	case c.Role == "critic" && strings.HasPrefix(c.Label, "design-audit"):
		return labSay(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "the DESIGN holds", "root": nil, "first_check": nil})
	case c.Role == "reviewer":
		return labSay(labVerdict(true))
	case c.Role == "explorer":
		return labSay(map[string]any{"answer": "answer", "evidence": []string{"README.md"}, "verified": []string{"README.md"}, "inferred": []string{}, "unknown": []string{}})
	case c.Role == "executor" || c.Role == "executor-t1":
		return l.executorTurn(c, nil)
	}
	return fakehost.TurnPlan{Message: "ok"}
}

// executorTurn is the executor's turn of the happy path: it writes its issue's file in its worktree and answers ready_for_review; the
// fields of receipt replace the RECEIPT's (a test's turn hook scripts a call by its label with it, e.g. a plan_mismatch).
func (l *laneLab) executorTurn(c labCall, receipt map[string]any) fakehost.TurnPlan {
	if c.Cwd != "" && c.Node != "" {
		path := filepath.Join(c.Cwd, "node-"+l.issue[c.Node]+".txt")
		prior, _ := os.ReadFile(path)
		if err := os.WriteFile(path, append(prior, []byte(c.Label+"\n")...), 0o644); err != nil {
			l.t.Errorf("the executor cannot write %s: %v", path, err)
		}
	}
	out := map[string]any{"outcome": "ready_for_review", "files": []string{}, "verification": []any{}, "summary": "done", "gap": nil, "research_question": nil, "plan_mismatch": nil}
	for k, v := range receipt {
		out[k] = v
	}
	return labSay(out)
}

// labVerdict is a reviewer's VERDICT: verified with its one criterion met, or needs_changes with the defects given (labDefect).
func labVerdict(verified bool, defects ...map[string]any) map[string]any {
	v := "verified"
	if !verified {
		v = "needs_changes"
	}
	if defects == nil {
		defects = []map[string]any{}
	}
	return map[string]any{"verdict": v, "criteria": []any{map[string]any{"id": "c1", "met": verified, "evidence": "read the head"}}, "defects": defects, "summary": "reviewed"}
}

// labDefect is one defect of a reviewer's VERDICT, of the kind correctness.
func labDefect(ref, severity, finding string) map[string]any {
	return map[string]any{"ref": ref, "kind": "correctness", "severity": severity, "introduced": false, "impact": "core_function", "inPromise": false, "file": "README.md", "line": 1,
		"finding": finding, "evidence_question": nil}
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
	tip := l.git(l.repo, "rev-parse", l.target)
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

// ---- the unit checker of the lab (CRW-1463) ----

// The lab's unit checker stands in for the review tool the shipped lane runs (shippedUnitCheck): the lane gives it the request the op
// built, and it answers a record without running a tool. Its default answer is a verified check with no finding by Bugbot's pair
// (engine family grok, model grok-4.7-medium, effort medium); a test scripts the answers of a node and mode in order (UnitChecks) and
// may change the default (unitCheckDefault), the pair included, so the independence of the check from the authors can be scripted too.
// The node is read from the prompt (unitCheckPrompt names it) and the mode from the profile the lane asked for (labCheckProfile+mode).

const (
	labCheckProfile = "lab-check-"
	labCheckModel   = "grok-4.7-medium"
)

// labCheck is one answer of the lab's unit checker. An empty State is ok (verdict verified when none is given); an empty pair is the
// default's; Err makes the call fail with that Go error.
type labCheck struct {
	State                 reviewadapter.State
	Verdict               string
	Findings              []reviewadapter.Finding
	Family, Model, Effort string
	// OmitPrompt makes the check answer as a profile that was not given the prompt (PromptSent false), as a Codex review is
	// not (CRW-1490).
	OmitPrompt bool
	Err        error
}

// labCheckCall is one request the lab's unit checker was given.
type labCheckCall struct {
	Node, Mode string
	Request    reviewadapter.Request
}

// labUnitChecker is the lab's ops.UnitChecker.
type labUnitChecker struct {
	mu    sync.Mutex
	def   labCheck
	queue map[string][]labCheck // by node "/" mode
	calls []labCheckCall
}

var labCheckNode = regexp.MustCompile(`unit check of node (\S+) `)

func newLabUnitChecker() *labUnitChecker {
	return &labUnitChecker{def: labCheck{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictVerified, Family: "grok", Model: labCheckModel, Effort: "medium"},
		queue: map[string][]labCheck{}}
}

// seam is the unit check the lab's lane is given (LaneOptions.UnitCheck).
func (u *labUnitChecker) seam() *ops.UnitCheck {
	return &ops.UnitCheck{Checker: u, Limit: time.Minute, Profile: func(mode string) (reviewadapter.Profile, error) {
		return reviewadapter.JSONProfile(labCheckProfile+mode, []string{"lab-unit-check"}, labCheckModel, "medium"), nil
	}}
}

// script queues the answers of the next checks of a node in a mode ("uncommitted" or "committed").
func (u *labUnitChecker) script(node, mode string, checks ...labCheck) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.queue[node+"/"+mode] = append(u.queue[node+"/"+mode], checks...)
}

// setDefault makes c the answer of every check no script answers; its empty fields keep the default's.
func (u *labUnitChecker) setDefault(c labCheck) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.def = u.fill(c)
}

func (u *labUnitChecker) fill(c labCheck) labCheck {
	if c.State == "" {
		c.State = reviewadapter.StateOK
	}
	if c.State == reviewadapter.StateOK && c.Verdict == "" {
		c.Verdict = reviewadapter.VerdictVerified
	}
	if c.Family == "" {
		c.Family = u.def.Family
	}
	if c.Model == "" {
		c.Model = u.def.Model
	}
	if c.Effort == "" {
		c.Effort = u.def.Effort
	}
	return c
}

// requests are the requests the checker was given, in order.
func (u *labUnitChecker) requests() []labCheckCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]labCheckCall(nil), u.calls...)
}

func (u *labUnitChecker) Check(ctx context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
	node := ""
	if m := labCheckNode.FindStringSubmatch(req.Prompt); m != nil {
		node = strings.TrimSuffix(m[1], ".")
	}
	mode := strings.TrimPrefix(req.Profile.Name, labCheckProfile)
	u.mu.Lock()
	u.calls = append(u.calls, labCheckCall{Node: node, Mode: mode, Request: req})
	c := u.def
	if q := u.queue[node+"/"+mode]; len(q) > 0 {
		c, u.queue[node+"/"+mode] = u.fill(q[0]), q[1:]
	}
	u.mu.Unlock()
	if c.Err != nil {
		return reviewadapter.Result{}, c.Err
	}
	fp, err := ops.Snapshot(ctx, ops.ExecGit("git"), req.Worktree)
	if err != nil {
		return reviewadapter.Result{}, fmt.Errorf("the lab's unit checker: fingerprint: %w", err)
	}
	findings := []reviewadapter.Finding{}
	verdict := ""
	if c.State == reviewadapter.StateOK {
		verdict = c.Verdict
		for _, f := range c.Findings {
			if f.ID == "" {
				f.ID = reviewadapter.FindingID(f.File, f.Text)
			}
			findings = append(findings, f)
		}
	}
	sum := sha256.Sum256([]byte(req.Prompt))
	return reviewadapter.Result{State: c.State, Verdict: verdict, Findings: findings, EngineFamily: c.Family, Model: c.Model, Effort: c.Effort, GuideRev: req.GuideRev,
		PromptDigest: hex.EncodeToString(sum[:]), PromptSent: !c.OmitPrompt, FingerprintBefore: fp, FingerprintAfter: fp, ElapsedMS: 1}, nil
}

// UnitChecks scripts the next unit checks of a node (its id or its issue key) in a mode.
func (l *laneLab) UnitChecks(node, mode string, checks ...labCheck) {
	if id, ok := l.nodeOf[node]; ok {
		node = id
	}
	l.uc.script(node, mode, checks...)
}

// ---- the crosschecker of the lab (CRW-1473) ----

// The lab's crosschecker stands in for the engines the shipped lane's crosscheck runs (shippedCrosscheck): the lane gives it the request
// crw.crosscheck built, in the real checkout of the pass (Lane.crosscheckWorktree under the run's directory), and it answers a record
// without running a tool. Its default answer is a verified pass with no finding, by the pair of the profile the op asked for (astra's
// gpt-6-astra or grok's grok-4.7); a test scripts the answers of a milestone, round, evidence round, pass and attempt in order
// (Crosschecks) with any reviewadapter.Result. The pass is read from the checkout's name (<ms>-r<round>-e<evidence round>-<pass><attempt>-<head12>).

var labCheckoutName = regexp.MustCompile(`^(.+)-r(\d+)-e(\d+)-([AB])(\d+)-[0-9a-f]+$`)

// labCrosscheckCall is one request the lab's crosschecker was given, with whether its checkout existed during the call.
type labCrosscheckCall struct {
	Key     string // <ms>/r<round>/e<evidence round>/<pass><attempt>
	Request reviewadapter.Request
	Existed bool
}

// labCrosschecker is the lab's ops.UnitChecker of crw.crosscheck.
type labCrosschecker struct {
	mu    sync.Mutex
	queue map[string][]reviewadapter.Result
	calls []labCrosscheckCall
	// onCheck, when a test sets it, runs with the pass's key while the check runs (a test moves the integration target mid-round).
	onCheck func(key string)
}

func newLabCrosschecker() *labCrosschecker {
	return &labCrosschecker{queue: map[string][]reviewadapter.Result{}}
}

// seam is the crosscheck the lab's lane is given (LaneOptions.Crosscheck): the shipped profiles over the run's directory, no authors
// (BuildLane sets them from the run's role table) and the shipped checkout.
func (c *labCrosschecker) seam() *ops.Crosscheck {
	return &ops.Crosscheck{Checker: c, Limit: time.Minute, Profiles: func(runDir string) ([]reviewadapter.Profile, error) {
		return []reviewadapter.Profile{reviewadapter.AstraProfile(filepath.Join(runDir, "crosscheck", "answer.strict.json")), reviewadapter.GrokProfile(reviewadapter.AnswerSchema)}, nil
	}}
}

// labCrosscheckKey is the key of a pass: milestone, round, evidence round, pass and attempt.
func labCrosscheckKey(ms string, round, evidenceRound int, pass string, attempt int) string {
	return fmt.Sprintf("%s/r%d/e%d/%s%d", ms, round, evidenceRound, pass, attempt)
}

// script queues the answers of the next checks of a pass.
func (c *labCrosschecker) script(key string, results ...reviewadapter.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue[key] = append(c.queue[key], results...)
}

// requests are the requests the crosschecker was given, in order.
func (c *labCrosschecker) requests() []labCrosscheckCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]labCrosscheckCall(nil), c.calls...)
}

func (c *labCrosschecker) Check(ctx context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
	key := filepath.Base(req.Worktree)
	if m := labCheckoutName.FindStringSubmatch(key); m != nil {
		key = m[1] + "/r" + m[2] + "/e" + m[3] + "/" + m[4] + m[5]
	}
	_, statErr := os.Stat(req.Worktree)
	c.mu.Lock()
	c.calls = append(c.calls, labCrosscheckCall{Key: key, Request: req, Existed: statErr == nil})
	res := reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictVerified}
	if q := c.queue[key]; len(q) > 0 {
		res, c.queue[key] = q[0], q[1:]
	}
	hook := c.onCheck
	c.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	fp, err := ops.Snapshot(ctx, ops.ExecGit("git"), req.Worktree)
	if err != nil {
		return reviewadapter.Result{}, fmt.Errorf("the lab's crosschecker: fingerprint: %w", err)
	}
	if res.State == "" {
		res.State = reviewadapter.StateOK
	}
	if res.Findings == nil {
		res.Findings = []reviewadapter.Finding{}
	}
	for i, f := range res.Findings {
		if f.ID == "" {
			res.Findings[i].ID = reviewadapter.FindingID(f.File, f.Text)
		}
	}
	if res.EngineFamily == "" {
		res.EngineFamily = execution.ModelFamily(req.Profile.Model)
	}
	if res.Model == "" {
		res.Model, res.Effort = req.Profile.Model, req.Profile.Effort
	}
	sum := sha256.Sum256([]byte(req.Prompt))
	res.GuideRev, res.PromptDigest, res.FingerprintBefore, res.FingerprintAfter = req.GuideRev, hex.EncodeToString(sum[:]), fp, fp
	if res.ElapsedMS == 0 {
		res.ElapsedMS = 1
	}
	return res, nil
}

// Crosschecks scripts the next answers of one pass of a milestone's crosscheck.
func (l *laneLab) Crosschecks(ms string, round, evidenceRound int, pass string, attempt int, results ...reviewadapter.Result) {
	l.cc.script(labCrosscheckKey(ms, round, evidenceRound, pass, attempt), results...)
}
