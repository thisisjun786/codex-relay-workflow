package mergeturn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-768: the merge train. These tests use a temporary store, a forge stand-in and a temporary git
// repository, never the live relay service, the production store or a real Codex task. The forge
// stand-in answers what a forge would; the real TrainForgeReader and TrainCheckoutProver are exercised
// against real git in TestTrainChainProverAgainstRealGit.

const (
	trRepo   = "owner/repo"
	trBase   = "dev"
	trLeader = "task-leader"
	trHost   = "host-a"
	trLane   = "PRJ-LEADER"
)

var trISO = registry.ISO(time.Unix(1_700_000_000, 0))

// trTip is the base reader, one tip per (repository, base).
type trTip struct{ tips map[[2]string]string }

func (f *trTip) set(repository, base, sha string) { f.tips[[2]string{repository, base}] = sha }
func (f *trTip) Tip(_ context.Context, repository, base string) (Tip, error) {
	sha, ok := f.tips[[2]string{repository, base}]
	if !ok {
		return Tip{}, &TargetUnreadable{"the test set no tip for " + base + " of " + repository}
	}
	return Tip{SHA: sha, Source: "fake", Reference: "refs/heads/" + base, Repository: repository}, nil
}

// trForge is the forge stand-in: pull requests, runs and commits, all under the test's control.
type trForge struct {
	pulls    map[int64]TrainPullRequest
	runs     map[string]TrainRun
	commits  map[string]TrainCommit
	compares map[string]string
	fail     string
}

func newTrForge() *trForge {
	return &trForge{pulls: map[int64]TrainPullRequest{}, runs: map[string]TrainRun{}, commits: map[string]TrainCommit{}, compares: map[string]string{}}
}

func (f *trForge) PullRequest(_ context.Context, _ string, n int64) (TrainPullRequest, error) {
	if f.fail == "pull" {
		return TrainPullRequest{}, errors.New("the forge is unreachable")
	}
	p, ok := f.pulls[n]
	if !ok {
		return TrainPullRequest{}, errors.New("no such pull request")
	}
	return p, nil
}
func (f *trForge) Run(_ context.Context, _ string, id string) (TrainRun, error) {
	if f.fail == "run" {
		return TrainRun{}, errors.New("the forge is unreachable")
	}
	r, ok := f.runs[id]
	if !ok {
		return TrainRun{}, errors.New("no such run")
	}
	return r, nil
}
func (f *trForge) Commit(_ context.Context, _ string, sha string) (TrainCommit, error) {
	if f.fail == "commit" {
		return TrainCommit{}, errors.New("the forge is unreachable")
	}
	c, ok := f.commits[sha]
	if !ok {
		return TrainCommit{}, errors.New("no such commit")
	}
	return c, nil
}
func (f *trForge) Compare(_ context.Context, _, base, head string) (string, error) {
	if f.fail == "compare" {
		return "", errors.New("the forge is unreachable")
	}
	if s, ok := f.compares[base+".."+head]; ok {
		return s, nil
	}
	return "ahead", nil
}

// trProof is the checkout stand-in: a chain, or the refusal the test names.
type trProof struct {
	tree string
	err  error
	// workflow is the text .github/workflows/ci.yml holds at the head, and fileErr the refusal the
	// stand-in answers with instead. Empty means the repository's own workflow, which is what the
	// bundle's head normally holds (CRW-897, answer 6).
	workflow string
	fileErr  error
}

func (p *trProof) Chain(_ context.Context, _ string, _, _ string, _ []TrainMemberExpectation) (TrainChain, error) {
	if p.err != nil {
		return TrainChain{}, p.err
	}
	return TrainChain{Tree: p.tree}, nil
}

func (p *trProof) File(_ context.Context, _, _, _ string) (string, error) {
	if p.fileErr != nil {
		return "", p.fileErr
	}
	if p.workflow != "" {
		return p.workflow, nil
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// tr is the test's world: a temporary store, the registry, the service, the base reader and the forge.
type tr struct {
	t     *testing.T
	ctx   context.Context
	s     *store.Store
	r     *registry.Registry
	m     *Service
	tip   *trTip
	forge *trForge
	proof *trProof
}

func newTr(t *testing.T) *tr {
	t.Helper()
	ctx := context.Background()
	s := openFresh(t)
	now := func() string { return trISO }
	r := &registry.Registry{Store: s, Now: now, Policy: registry.ResolveRolePolicy(map[string]string{})}
	w := &tr{t: t, ctx: ctx, s: s, r: r, tip: &trTip{tips: map[[2]string]string{}}, forge: newTrForge(), proof: &trProof{tree: "tree-bundle"}}
	w.m = &Service{Store: s, Registry: r, Now: now}
	w.tip.set(trRepo, trBase, "base-0")
	w.bind(trLane, trLeader, trHost)
	return w
}

func (w *tr) bind(project, task, host string) {
	w.t.Helper()
	endpoint := registry.Endpoint{TaskID: task, HostID: host, Cwd: sql.NullString{String: "/" + task, Valid: true}}
	if _, err := w.r.BindScope(w.ctx, "parent", project, endpoint); err != nil {
		w.t.Fatal(err)
	}
}

// claim makes one turn on the target for a holder in its own project.
func (w *tr) claim(project, task, head string, pr int64) map[string]any {
	w.t.Helper()
	options := ClaimOptions{PR: sql.NullInt64{Int64: pr, Valid: pr > 0}, Relationship: sql.NullString{String: "rel-" + task, Valid: true}}
	answer, err := w.m.Request(w.ctx, trRepo, trBase, project, task, trHost, head, true, options)
	if err != nil {
		w.t.Fatal(err)
	}
	w.acceptOn("rel-"+task, head)
	return answer
}

// waiting makes a waiting turn for a member: another project's parent asks for the same target, and
// the member's relationship is accepted on that head, which is what the bundle gate requires.
func (w *tr) waiting(project, task, head string, pr int64) {
	w.t.Helper()
	w.bind(project, task, trHost)
	w.claim(project, task, head, pr)
}

// acceptOn writes one active acceptance of a relationship standing on a head, the shape dag-accept
// leaves: the acceptance row plus its forge row.
func (w *tr) acceptOn(relationship, head string) {
	w.t.Helper()
	var existing int64
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT count(*) FROM dag_acceptances WHERE relationship_id = ? AND state = 'active'", relationship).Scan(&existing); err != nil {
		w.t.Fatal(err)
	}
	if existing > 0 {
		return
	}
	id := "acc-" + relationship
	w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, "plan-x", "node-"+relationship, "manifest-"+relationship, relationship, int64(1), "ev-"+relationship, "rev-"+relationship, "crit-"+relationship, "verified",
		head, trRepo, int64(1), "bound", "turn-"+relationship, "{}", trLeader, int64(0), "2026-10-01T00:00:00Z", "active")
	w.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?, 'owner/repo', 1)", id)
}

// pr registers a pull request the forge answers with.
func (w *tr) pr(n int64, head string, labels ...string) {
	w.forge.pulls[n] = TrainPullRequest{Number: n, State: "open", BaseRef: trBase, HeadSHA: head, Labels: labels}
}

// open opens a train and returns the answer.
func (w *tr) open(turn, actor, base string, members ...int64) (map[string]any, error) {
	return w.m.Open(w.ctx, turn, actor, base, members, w.tip, w.forge)
}

func (w *tr) count(query string, args ...any) int64 {
	w.t.Helper()
	var n int64
	if err := w.s.DB.QueryRowContext(w.ctx, query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *tr) trainEvents() int64 { return w.count("SELECT count(*) FROM merge_train_events") }

func (w *tr) turn(turn string) store.MergeTurnsRow {
	w.t.Helper()
	row, err := w.s.MergeTurn(w.ctx, turn)
	if err != nil {
		w.t.Fatal(err)
	}
	return row
}

// reasonOf is the refusal reason of an error.
func trReason(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return "no_refusal:" + err.Error()
}

// threeMembers builds a leader and two waiting members whose pull requests the forge answers for.
func (w *tr) threeMembers() (string, []int64) {
	w.t.Helper()
	leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
	w.pr(101, "head-lead")
	w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
	w.pr(102, "head-m2")
	w.waiting("PRJ-M3", "task-m3", "head-m3", 103)
	w.pr(103, "head-m3")
	return leader, []int64{101, 102, 103}
}

// runFor builds a completed successful ci.yml run with every expected job, each test leg carrying a
// successful test step.
func runFor(head string) TrainRun {
	jobs := make([]TrainJob, 0, len(TrainExpectedJobs))
	for _, name := range TrainExpectedJobs {
		job := TrainJob{Name: name, Conclusion: "success", StepsReadable: true}
		if strings.HasPrefix(name, "go-product (test-") {
			job.Steps = []TrainStep{{Name: evidenceStepName("test-1"), Conclusion: "success"}}
		}
		jobs = append(jobs, job)
	}
	return TrainRun{ID: "run-1", Path: ".github/workflows/ci.yml", HeadSHA: head, HeadRepository: trRepo, Status: "completed", Conclusion: "success", Attempt: 1, Jobs: jobs}
}

func evidenceStepName(part string) string {
	return evidence.LightTestStepPrefix + part + ")"
}

// TestTrainOpenVerifyLandCloseDone is the happy path: three members open, verify, land and close done
// with every member turn landed.
func TestTrainOpenVerifyLandCloseDone(t *testing.T) {
	w := newTr(t)
	leader, members := w.threeMembers()
	answer, err := w.open(leader, trLeader, "base-0", members...)
	if err != nil {
		t.Fatal(err)
	}
	train := answer["train"].(map[string]any)["trainId"].(string)
	if state := answer["state"]; state != "opened" {
		t.Fatalf("state after open = %v, want opened", state)
	}
	if n := w.count("SELECT count(*) FROM merge_train_members"); n != 3 {
		t.Fatalf("members = %d, want 3", n)
	}
	// verify: the bundle pull request on dev, labelled crw-lane, head H, with a green run
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	answer, err = w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof)
	if err != nil {
		t.Fatal(err)
	}
	if answer["state"] != "verified" {
		t.Fatalf("state after verify = %v, want verified", answer["state"])
	}
	// land: the dev tip is M, M's parents are D and H, every member head is an ancestor of M
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	answer, err = w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "merge-1", w.tip, w.forge)
	if err != nil {
		t.Fatal(err)
	}
	if answer["state"] != "landed" {
		t.Fatalf("state after land = %v, want landed", answer["state"])
	}
	// every member turn landed, the leader's included, with M: the two waiting members too
	memberTurns, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? ORDER BY seq", train)
	if err != nil || len(memberTurns) != 3 {
		t.Fatalf("members: %v", err)
	}
	for _, m := range memberTurns {
		turn := m.Get("turn_id").(string)
		row := w.turn(turn)
		if row.State != "landed" || row.LandedSHA.String != "merge-1" {
			t.Fatalf("member turn %s = %s/%s, want landed/merge-1", turn, row.State, row.LandedSHA.String)
		}
		if !strings.Contains(row.CloseReason.String, "landed via bundle "+train+" merge-1") {
			t.Fatalf("member turn %s close reason = %q", turn, row.CloseReason.String)
		}
	}
	_ = leader
	// close done
	answer, err = w.m.Close(w.ctx, train, trLeader, "done", "every member landed")
	if err != nil {
		t.Fatal(err)
	}
	if answer["state"] != "done" {
		t.Fatalf("state after close = %v, want done", answer["state"])
	}
	if n := w.trainEvents(); n != 4 {
		t.Fatalf("events = %d, want opened, verified, landed, done", n)
	}
}

// TestTrainOpenRefusals: every open refusal the issue names, each with no event written.
func TestTrainOpenRefusals(t *testing.T) {
	cases := []struct {
		name   string
		build  func(w *tr) (string, []int64, string)
		reason string
	}{
		{"a pull request with no waiting turn", func(w *tr) (string, []int64, string) {
			leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
			w.pr(101, "head-lead")
			w.pr(102, "head-m2")
			return leader, []int64{101, 102}, "disposition_conflict"
		}, "disposition_conflict"},
		{"the same pull request twice", func(w *tr) (string, []int64, string) {
			leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
			w.pr(101, "head-lead")
			return leader, []int64{101, 101}, "disposition_conflict"
		}, "disposition_conflict"},
		{"a member head that moved", func(w *tr) (string, []int64, string) {
			leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
			w.pr(101, "head-lead")
			w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
			w.pr(102, "head-m2-moved")
			return leader, []int64{101, 102}, "disposition_conflict"
		}, "disposition_conflict"},
		{"a base other than the forge's dev tip", func(w *tr) (string, []int64, string) {
			leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
			w.pr(101, "head-lead")
			return leader, []int64{101}, "disposition_conflict"
		}, "disposition_conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTr(t)
			leader, members, want := tc.build(w)
			base := "base-0"
			if tc.name == "a base other than the forge's dev tip" {
				base = "base-stale"
			}
			_, err := w.open(leader, trLeader, base, members...)
			if err == nil {
				t.Fatal("the open was accepted")
			}
			if got := trReason(err); got != want {
				t.Fatalf("reason = %s, want %s (%v)", got, want, err)
			}
			if n := w.count("SELECT count(*) FROM merge_train_events"); n != 0 {
				t.Fatalf("a refused open wrote %d event(s)", n)
			}
		})
	}
}

// TestTrainOneMemberIsTodayLane: a bundle of one member makes no train.
func TestTrainOneMemberIsTodayLane(t *testing.T) {
	w := newTr(t)
	leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
	w.pr(101, "head-lead")
	answer, err := w.open(leader, trLeader, "base-0", 101)
	if err != nil {
		t.Fatal(err)
	}
	if answer["lane"] != "single" || answer["train"] != nil {
		t.Fatalf("answer = %v, want lane single and no train", answer)
	}
	if n := w.count("SELECT count(*) FROM merge_trains"); n != 0 {
		t.Fatalf("a one-member bundle opened %d train(s)", n)
	}
}

// TestTrainOpenRefusesAnOrderAgainstAPlanEdge: a member whose plan predecessor is a later member is
// refused disposition_conflict and writes no event.
func TestTrainOpenRefusesAnOrderAgainstAPlanEdge(t *testing.T) {
	w := newTr(t)
	leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
	w.pr(101, "head-lead")
	w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
	w.pr(102, "head-m2")
	// PR 102 is node B of plan P, and a live edge makes node A (PR 101) its predecessor
	w.exec("INSERT INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES ('P','PRJ-LEADER','task-leader','2026-10-01T00:00:00Z')")
	w.exec("INSERT INTO dag_plan_revisions (plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, author_task_id, recorded_at) VALUES ('P',1,0,'req-1','d-1','{}','sd-1','task-leader','2026-10-01T00:00:00Z')")
	w.exec("INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, criteria_set_digest) VALUES ('P','A',1,NULL,'sd-A','A','implementation','c-A')")
	w.exec("INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, criteria_set_digest) VALUES ('P','B',1,NULL,'sd-B','B','implementation','c-B')")
	w.exec("INSERT INTO dag_edges (plan_id, edge_id, introduced_rev, retired_rev, from_node_id, to_node_id, kind) VALUES ('P','e1',1,NULL,'A','B','artifact_verified')")
	w.accept("acc-A", "P", "A", "head-lead", 101)
	w.accept("acc-B", "P", "B", "head-m2", 102)
	// the bundle puts B (102) before its predecessor A (101): refused
	_, err := w.open(leader, trLeader, "base-0", 102, 101)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("order against a plan edge: %v", err)
	}
	if n := w.trainEvents(); n != 0 {
		t.Fatalf("a refused open wrote %d event(s)", n)
	}
	// the right order is accepted
	if _, err := w.open(leader, trLeader, "base-0", 101, 102); err != nil {
		t.Fatalf("the plan order was refused: %v", err)
	}
}

func (w *tr) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, query, args...); err != nil {
		w.t.Fatal(err)
	}
}

// accept writes one active acceptance with a forge pull request.
func (w *tr) accept(id, plan, node, head string, pr int64) {
	w.t.Helper()
	w.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, plan, node, "manifest-"+id, "rel-"+id, int64(1), "ev-"+id, "rev-"+id, "crit-"+id, "verified",
		head, "owner/repo", pr, "bound", "turn-"+id, "{}", trLeader, int64(0), "2026-10-01T00:00:00Z", "active")
	w.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?, 'owner/repo', ?)", id, pr)
}

// openedTrain builds a verified-ready three-member train.
func (w *tr) openedTrain() string {
	w.t.Helper()
	leader, members := w.threeMembers()
	answer, err := w.open(leader, trLeader, "base-0", members...)
	if err != nil {
		w.t.Fatal(err)
	}
	return answer["train"].(map[string]any)["trainId"].(string)
}

// TestTrainVerifyRefusals: every verify refusal the issue names, each with no verified event.
func TestTrainVerifyRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(w *tr)
		actor  string
		pr     string
		head   string
		run    string
		reason string
	}{
		{"a non-leader actor", func(w *tr) { w.pr(900, "head-bundle", TrainLaneLabel); w.forge.runs["run-1"] = runFor("head-bundle") }, "task-m2", "900", "head-bundle", "run-1", "disposition_conflict"},
		{"an unlabelled pull request", func(w *tr) { w.pr(900, "head-bundle"); w.forge.runs["run-1"] = runFor("head-bundle") }, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"another head", func(w *tr) { w.pr(900, "head-bundle", TrainLaneLabel); w.forge.runs["run-1"] = runFor("head-bundle") }, trLeader, "900", "head-other", "run-1", "disposition_conflict"},
		{"another workflow", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-bundle")
			r.Path = ".github/workflows/other.yml"
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"a fork run", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-bundle")
			r.HeadRepository = "someone/repo"
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"another head_sha", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-other")
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"an empty job list", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-bundle")
			r.Jobs = nil
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"a list missing an expected name", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-bundle")
			r.Jobs = r.Jobs[1:]
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"an unfinished run", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-bundle")
			r.Status = "in_progress"
			r.Conclusion = ""
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"a failed run", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-bundle")
			r.Conclusion = "failure"
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"a test leg whose test step was skipped", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-bundle")
			for i := range r.Jobs {
				if r.Jobs[i].Name == "go-product (test-2)" {
					r.Jobs[i].Steps = []TrainStep{{Name: evidenceStepName("test-2"), Conclusion: "skipped"}}
				}
			}
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"a test leg whose test step is absent", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-bundle")
			for i := range r.Jobs {
				if r.Jobs[i].Name == "go-product (test-3)" {
					r.Jobs[i].Steps = nil
				}
			}
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "disposition_conflict"},
		{"a test leg with a missing step list", func(w *tr) {
			w.pr(900, "head-bundle", TrainLaneLabel)
			r := runFor("head-bundle")
			for i := range r.Jobs {
				if r.Jobs[i].Name == "go-product (test-4)" {
					r.Jobs[i].StepsReadable = false
					r.Jobs[i].Steps = nil
				}
			}
			w.forge.runs["run-1"] = r
		}, trLeader, "900", "head-bundle", "run-1", "merge_target_unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTr(t)
			train := w.openedTrain()
			tc.mutate(w)
			_, err := w.m.Verify(w.ctx, train, tc.actor, tc.pr, tc.head, tc.run, "/checkout", w.forge, w.proof)
			if err == nil {
				t.Fatal("the verify was accepted")
			}
			if got := trReason(err); got != tc.reason {
				t.Fatalf("reason = %s, want %s (%v)", got, tc.reason, err)
			}
			if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
				t.Fatalf("a refused verify wrote %d verified event(s)", n)
			}
		})
	}
}

// TestTrainVerifyRefusesAHandResolvedMerge: the checkout prover's refusal is passed through with no event.
func TestTrainVerifyRefusesAHandResolvedMerge(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	w.proof.err = trainConflict("the tree of merge commit m2 is t-a and git merges t-b from its parents")
	_, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof)
	if err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a hand-resolved merge: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
		t.Fatalf("a refused verify wrote %d verified event(s)", n)
	}
}

// TestTrainVerifyAnswersUnreadableWhenTheForgeOrGitCannotAnswer.
func TestTrainVerifyAnswersUnreadableWhenTheForgeOrGitCannotAnswer(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	w.forge.fail = "run"
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); trReason(err) != "merge_target_unreadable" {
		t.Fatalf("an unreadable run: %v", err)
	}
	w.forge.fail = ""
	w.proof.err = trainUnreadable("git could not be started")
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); trReason(err) != "merge_target_unreadable" {
		t.Fatalf("an unreadable checkout: %v", err)
	}
}

// TestTrainLandRefusals: every land refusal the issue names, each writing nothing.
func TestTrainLandRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(w *tr)
		landed string
		reason string
	}{
		{"a dev tip other than M", func(w *tr) {
			w.tip.set(trRepo, trBase, "somewhere-else")
			w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
		}, "merge-1", "disposition_conflict"},
		{"an M whose second parent is not H", func(w *tr) {
			w.tip.set(trRepo, trBase, "merge-1")
			w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-other"}, Tree: "tree-bundle"}
		}, "merge-1", "disposition_conflict"},
		{"a member whose accepted head is not an ancestor of M", func(w *tr) {
			w.tip.set(trRepo, trBase, "merge-1")
			w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
			w.forge.compares["head-m2..merge-1"] = "diverged"
		}, "merge-1", "disposition_conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTr(t)
			train := w.verifiedTrain()
			tc.mutate(w)
			_, err := w.m.TrainLand(w.ctx, train, trLeader, tc.landed, "", w.tip, w.forge)
			if err == nil {
				t.Fatal("the land was accepted")
			}
			if got := trReason(err); got != tc.reason {
				t.Fatalf("reason = %s, want %s (%v)", got, tc.reason, err)
			}
			if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
				t.Fatalf("a refused land wrote %d landed event(s)", n)
			}
			if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
				t.Fatalf("a refused land landed %d turn(s)", n)
			}
		})
	}
}

// verifiedTrain opens and verifies a three-member train.
func (w *tr) verifiedTrain() string {
	w.t.Helper()
	train := w.openedTrain()
	w.pr(900, "head-bundle", TrainLaneLabel)
	w.forge.runs["run-1"] = runFor("head-bundle")
	if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err != nil {
		w.t.Fatal(err)
	}
	return train
}

// TestTrainMemberTurnsRefuseCheckAndLand: a member of a live train does not check or land on its own,
// and a turn outside a train is unchanged.
func TestTrainMemberTurnsRefuseCheckAndLand(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	// the member's waiting turn
	var memberTurn string
	rows, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 2", train)
	if err != nil || len(rows) != 1 {
		t.Fatalf("member 2: %v", err)
	}
	memberTurn = rows[0].Get("turn_id").(string)
	if _, err := w.m.Check(w.ctx, memberTurn, "task-m2", "head-m2", "base-0", []any{}, contract.OrderedObject{}, nil, w.tip); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("merge-turn-check on a member: %v", err)
	}
	if _, err := w.m.TrainLand(w.ctx, memberTurn, "task-m2", "merge-1", "", w.tip, w.forge); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("merge-turn-land on a member: %v", err)
	}
	// a turn outside a train is unchanged: an ordinary check on a non-member holding turn proceeds.
	// It is another target, so the leader's turn on dev does not make it a waiter.
	w.tip.set(trRepo, "main", "base-main")
	w.bind("PRJ-OTHER", "task-other", trHost)
	otherAnswer, err := w.m.Request(w.ctx, trRepo, "main", "PRJ-OTHER", "task-other", trHost, "head-other", true,
		ClaimOptions{PR: sql.NullInt64{Int64: 201, Valid: true}, Relationship: sql.NullString{String: "rel-other", Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	other := otherAnswer["turnId"].(string)
	w.pr(201, "head-other")
	// the member guard must not fire for it: the check proceeds to the ordinary evidence checks
	if _, err := w.m.Check(w.ctx, other, "task-other", "head-other", "base-main", []any{}, contract.OrderedObject{}, nil, w.tip); err != nil {
		if trReason(err) == "disposition_conflict" {
			t.Fatalf("a turn outside a train was refused by the member guard: %v", err)
		}
	}
}

// TestTrainAbandonedReturnsMembersToWaiting: an abandoned train leaves its member turns in the queue
// they were in before it opened. The leader still holds the turn (so the reopen, which needs a
// holding turn, has one) and every other member is still waiting (so it can ride a replacement).
func TestTrainAbandonedReturnsMembersToWaiting(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	rows, _ := w.s.All(w.ctx, "SELECT turn_id, seq FROM merge_train_members WHERE train_id = ? ORDER BY seq", train)
	var leaderTurn string
	waiting := map[string]bool{}
	for _, row := range rows {
		turn := row.Get("turn_id").(string)
		if seq, _ := row.Get("seq").(int64); seq == 1 {
			leaderTurn = turn
		} else {
			waiting[turn] = true
		}
	}
	answer, err := w.m.Close(w.ctx, train, trLeader, "abandoned", "the base moved out of lane")
	if err != nil {
		t.Fatal(err)
	}
	if answer["state"] != "abandoned" {
		t.Fatalf("state = %v, want abandoned", answer["state"])
	}
	if state := w.turn(leaderTurn).State; state != "holding" {
		t.Fatalf("the leader turn = %s, and the reopen needs it holding", state)
	}
	for turn := range waiting {
		if state := w.turn(turn).State; state != "waiting" {
			t.Fatalf("member turn %s = %s, want waiting", turn, state)
		}
	}
}

// TestTrainChainProverAgainstRealGit exercises TrainCheckoutProver against a temporary git repository:
// a clean two-parent merge passes with its tree, a hand-resolved merge and a wrong order are refused.
func TestTrainChainProverAgainstRealGit(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "dev")
	write("a.txt", "base\n")
	git("add", "a.txt")
	git("commit", "-q", "-m", "D")
	base := git("rev-parse", "HEAD")

	// member one: its own file, merged with --no-ff
	git("checkout", "-q", "-b", "m1")
	write("m1.txt", "one\n")
	git("add", "m1.txt")
	git("commit", "-q", "-m", "member one")
	m1 := git("rev-parse", "HEAD")
	git("checkout", "-q", "dev")
	git("merge", "-q", "--no-ff", "-m", "merge m1", "m1")
	afterOne := git("rev-parse", "HEAD")

	// member two
	git("checkout", "-q", "-b", "m2")
	write("m2.txt", "two\n")
	git("add", "m2.txt")
	git("commit", "-q", "-m", "member two")
	m2 := git("rev-parse", "HEAD")
	git("checkout", "-q", "dev")
	git("merge", "-q", "--no-ff", "-m", "merge m2", "m2")
	head := git("rev-parse", "HEAD")

	members := []TrainMemberExpectation{{AcceptedHead: m1}, {AcceptedHead: m2}}
	proof := TrainCheckoutProver{}
	chain, err := proof.Chain(context.Background(), repo, head, base, members)
	if err != nil {
		t.Fatalf("a clean chain was refused: %v", err)
	}
	if chain.Tree != git("rev-parse", head+"^{tree}") {
		t.Fatalf("tree = %s", chain.Tree)
	}

	// a wrong order: member two named first
	if _, err := proof.Chain(context.Background(), repo, head, base, []TrainMemberExpectation{{AcceptedHead: m2}, {AcceptedHead: m1}}); trReason(err) != "disposition_conflict" {
		t.Fatalf("a wrong order: %v", err)
	}
	// a chain that does not reach the base
	if _, err := proof.Chain(context.Background(), repo, head, afterOne, members); trReason(err) != "disposition_conflict" {
		t.Fatalf("a chain not reaching its base: %v", err)
	}

	// a hand-resolved merge: a merge whose tree is not what git merges from its parents
	git("checkout", "-q", "dev")
	git("checkout", "-q", "-b", "m3")
	write("m3.txt", "three\n")
	git("add", "m3.txt")
	git("commit", "-q", "-m", "member three")
	m3 := git("rev-parse", "HEAD")
	git("checkout", "-q", "dev")
	git("merge", "-q", "--no-ff", "-m", "merge m3", "m3")
	git("commit", "-q", "--amend", "-m", "merge m3 amended")
	write("sneak.txt", "hand\n")
	git("add", "sneak.txt")
	git("commit", "-q", "--amend", "--no-edit")
	hand := git("rev-parse", "HEAD")
	if _, err := proof.Chain(context.Background(), repo, hand, head, []TrainMemberExpectation{{AcceptedHead: m3}}); trReason(err) != "disposition_conflict" {
		t.Fatalf("a hand-resolved merge: %v", err)
	}
}

// TestTrainDecisionTenMapping: the opened, verified and landed details carry every per-member column
// decision 10 names (the relationship, the ruled event, the head fixed at the ruling, the accepted
// stand head, the member head and the order) and the train-level base D, combined head H, tree and
// merge SHA M, each with a non-empty value; and a change of base, member head or order after a verify
// is refused by land.
func TestTrainDecisionTenMapping(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	// the opened event records the mapping and the base
	assertTrainMapping(t, "opened", w.eventDetail(train, "opened"), []string{"baseSha"}, []string{"members"})
	// the verified event records the mapping, the base, the head, the tree and the run
	assertTrainMapping(t, "verified", w.eventDetail(train, "verified"), []string{"baseSha", "head", "tree", "run"}, []string{"bundlePr", "members"})
	// the landed event records the mapping, the base, the head, the tree and the merge commit
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	if _, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge); err != nil {
		t.Fatal(err)
	}
	assertTrainMapping(t, "landed", w.eventDetail(train, "landed"), []string{"baseSha", "head", "tree", "landedSha"}, []string{"members"})
}

// eventDetail is one event's detail object of a train.
func (w *tr) eventDetail(train, kind string) map[string]any {
	w.t.Helper()
	rows, err := w.s.All(w.ctx, "SELECT detail_json FROM merge_train_events WHERE train_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1", train, kind)
	if err != nil || len(rows) != 1 {
		w.t.Fatalf("%s event: %v", kind, err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(rows[0].Get("detail_json").(string)), &detail); err != nil {
		w.t.Fatal(err)
	}
	return detail
}

// assertTrainMapping asserts the train-level keys and every per-member column, each non-empty, in one
// event detail (the mapping test the criteria name).
func assertTrainMapping(t *testing.T, kind string, detail map[string]any, top, present []string) {
	t.Helper()
	for _, key := range append(append([]string{}, top...), present...) {
		v, ok := detail[key]
		if !ok {
			t.Fatalf("the %s detail has no %s", kind, key)
		}
		if s, isString := v.(string); isString && s == "" {
			t.Fatalf("the %s detail's %s is empty", kind, key)
		}
	}
	members, _ := detail["members"].([]any)
	if len(members) != 3 {
		t.Fatalf("the %s mapping holds %d members, want 3", kind, len(members))
	}
	for _, raw := range members {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("the %s mapping holds a non-object member", kind)
		}
		for _, key := range []string{"seq", "turnId", "prNumber", "relationshipId", "ruledEventId", "rulingHead", "acceptedHead", "memberHead"} {
			v, present := m[key]
			if !present {
				t.Fatalf("the %s member mapping has no %s", kind, key)
			}
			if s, isString := v.(string); isString && s == "" {
				t.Fatalf("the %s member mapping's %s is empty", kind, key)
			}
		}
	}
}

// TestTrainShowDerivesStateFromTheNewestEvent: show answers the newest event's kind.
func TestTrainShowDerivesStateFromTheNewestEvent(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	answer, err := w.m.Show(w.ctx, train, w.tip, w.forge)
	if err != nil {
		t.Fatal(err)
	}
	if answer["state"] != "opened" {
		t.Fatalf("show = %v, want opened", answer["state"])
	}
	if _, err := w.m.Close(w.ctx, train, trLeader, "abandoned", "out of lane"); err != nil {
		t.Fatal(err)
	}
	answer, err = w.m.Show(w.ctx, train, w.tip, w.forge)
	if err != nil {
		t.Fatal(err)
	}
	if answer["state"] != "abandoned" {
		t.Fatalf("show after close = %v, want abandoned", answer["state"])
	}
}

// TestTrainShowNeverReadsANullRowAsATrain: a NULL train_id row is not a train show can find.
func TestTrainShowNeverReadsANullRowAsATrain(t *testing.T) {
	w := newTr(t)
	// a NULL row cannot be inserted through the current zone at all (decision 9's trigger)
	if _, err := w.s.DB.ExecContext(w.ctx, "INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES (NULL,'o/r|dev','o/r','dev','h','t','2026-10-01T00:00:00Z')"); err == nil {
		t.Fatal("a NULL train_id was inserted")
	}
	if _, err := w.m.Show(w.ctx, "", w.tip, w.forge); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("show of an empty id: %v", err)
	}
}

// TestTrainDecisionTenClosureAndHalving: the failure rule's two helpers keep dependencies together.
// The members carry plan node ids (the key the edges are written in), not commit heads (finding 12).
func TestTrainDecisionTenClosureAndHalving(t *testing.T) {
	nodes := []TrainMemberNode{{PRNumber: 101, NodeID: "node-a"}, {PRNumber: 102, NodeID: "node-b"}, {PRNumber: 103, NodeID: "node-c"}, {PRNumber: 104, NodeID: "node-d"}}
	edges := []TrainPlanEdge{{FromNodeID: "node-a", ToNodeID: "node-b"}, {FromNodeID: "node-b", ToNodeID: "node-c"}}
	// a failure of A removes A, B and C (its transitive successors), and never D
	if got := TrainDependencyClosure(nodes, edges, 101); !reflect.DeepEqual(got, []int64{101, 102, 103}) {
		t.Fatalf("closure of 101 = %v, want [101 102 103]", got)
	}
	// a failure of D removes only D
	if got := TrainDependencyClosure(nodes, edges, 104); !reflect.DeepEqual(got, []int64{104}) {
		t.Fatalf("closure of 104 = %v, want [104]", got)
	}
	// a member with no node is its own closure
	if got := TrainDependencyClosure([]TrainMemberNode{{PRNumber: 201}}, edges, 201); !reflect.DeepEqual(got, []int64{201}) {
		t.Fatalf("closure of a node-less member = %v, want [201]", got)
	}
	// halving keeps A, B, C on one side even when the split would separate them: a two-member half of
	// [101 102] leaves 103 with them because 102 is its predecessor
	first, second := TrainHalve([]int64{101, 102, 103, 104}, nodes, edges)
	if len(first) == 0 || len(second) == 0 {
		t.Fatalf("halving produced %v / %v", first, second)
	}
	sideOf := map[int64]int{}
	for _, pr := range first {
		sideOf[pr] = 0
	}
	for _, pr := range second {
		sideOf[pr] = 1
	}
	if sideOf[101] != sideOf[102] || sideOf[102] != sideOf[103] {
		t.Fatalf("halving split a dependency: %v / %v", first, second)
	}
}

// TestTrainReconcileAfterALostLandAnswer: show reads the dev tip and each member's ancestry when the
// land answer was lost, records nothing, and answers unreadable when the forge cannot answer.
func TestTrainReconcileAfterALostLandAnswer(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	// dev does not contain the member heads: not landed
	for _, head := range []string{"head-lead", "head-m2", "head-m3"} {
		w.forge.compares[head+"..base-0"] = "diverged"
	}
	answer, err := w.m.Show(w.ctx, train, w.tip, w.forge)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := answer["reconcile"].(map[string]any)
	if rec == nil || rec["landed"] != false {
		t.Fatalf("reconcile = %v, want landed false", answer["reconcile"])
	}
	// the forge cannot answer: the reading is reported, nothing is recorded
	w.forge.fail = "compare"
	answer, err = w.m.Show(w.ctx, train, w.tip, w.forge)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ = answer["reconcile"].(map[string]any)
	if rec == nil || rec["unreadable"] == nil {
		t.Fatalf("an unreadable reconcile = %v", answer["reconcile"])
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
		t.Fatalf("reconcile wrote %d landed event(s)", n)
	}
}

// TestTrainFindingsFromTheFirstReview: one test per finding the one-shot Codex review raised on the
// first head, so each fix is pinned rather than asserted in prose.
func TestTrainFindingsFromTheFirstReview(t *testing.T) {
	t.Run("finding 1: the leader PR must be the first member", func(t *testing.T) {
		w := newTr(t)
		leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
		w.pr(101, "head-lead")
		w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
		w.pr(102, "head-m2")
		if _, err := w.open(leader, trLeader, "base-0", 102, 101); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a bundle whose first member is not the leader: %v", err)
		}
		if _, err := w.open(leader, trLeader, "base-0", 102); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a bundle that omits the leader: %v", err)
		}
	})

	t.Run("finding 8: a member with no relationship is refused, not a SQLite error", func(t *testing.T) {
		w := newTr(t)
		leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
		w.pr(101, "head-lead")
		w.bind("PRJ-M2", "task-m2", trHost)
		// a waiting turn that records no relationship
		if _, err := w.m.Request(w.ctx, trRepo, trBase, "PRJ-M2", "task-m2", trHost, "head-m2", true, ClaimOptions{PR: sql.NullInt64{Int64: 102, Valid: true}}); err != nil {
			t.Fatal(err)
		}
		w.pr(102, "head-m2")
		if _, err := w.open(leader, trLeader, "base-0", 101, 102); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member with no relationship: %v", err)
		}
		if n := w.trainEvents(); n != 0 {
			t.Fatalf("a refused open wrote %d event(s)", n)
		}
	})

	t.Run("finding 9: a closed or retargeted member PR is refused", func(t *testing.T) {
		w := newTr(t)
		leader, members := w.threeMembers()
		w.forge.pulls[102] = TrainPullRequest{Number: 102, State: "closed", BaseRef: trBase, HeadSHA: "head-m2"}
		if _, err := w.open(leader, trLeader, "base-0", members...); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a closed member: %v", err)
		}
		w.forge.pulls[102] = TrainPullRequest{Number: 102, State: "open", BaseRef: "main", HeadSHA: "head-m2"}
		if _, err := w.open(leader, trLeader, "base-0", members...); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a retargeted member: %v", err)
		}
	})

	t.Run("finding 10: a member that did not declare ready is refused", func(t *testing.T) {
		w := newTr(t)
		leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
		w.pr(101, "head-lead")
		w.bind("PRJ-M2", "task-m2", trHost)
		if _, err := w.m.Request(w.ctx, trRepo, trBase, "PRJ-M2", "task-m2", trHost, "head-m2", false,
			ClaimOptions{PR: sql.NullInt64{Int64: 102, Valid: true}, Relationship: sql.NullString{String: "rel-m2", Valid: true}}); err != nil {
			t.Fatal(err)
		}
		w.pr(102, "head-m2")
		if _, err := w.open(leader, trLeader, "base-0", 101, 102); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member that did not declare ready: %v", err)
		}
	})

	t.Run("finding 6: a landed tree other than the tested tree is refused", func(t *testing.T) {
		w := newTr(t)
		train := w.verifiedTrain()
		w.tip.set(trRepo, trBase, "merge-1")
		w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "some-other-tree"}
		if _, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a landed tree other than the tested one: %v", err)
		}
		if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
			t.Fatalf("a refused land wrote %d landed event(s)", n)
		}
	})

	t.Run("finding 2: a member that moved after the train opened is not landed", func(t *testing.T) {
		w := newTr(t)
		train := w.verifiedTrain()
		var memberTurn string
		rows, _ := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 2", train)
		memberTurn = rows[0].Get("turn_id").(string)
		// the member restates its candidate head while the train is live: merge-turn-ready is not
		// guarded, and the landing must revalidate rather than record the stale row
		w.exec("UPDATE merge_turns SET candidate_head = 'head-moved' WHERE turn_id = ?", memberTurn)
		w.tip.set(trRepo, trBase, "merge-1")
		w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
		if _, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member that moved after the train opened: %v", err)
		}
		if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
			t.Fatalf("a refused land landed %d turn(s)", n)
		}
	})

	t.Run("finding 7: done only after a landing", func(t *testing.T) {
		w := newTr(t)
		train := w.openedTrain()
		if _, err := w.m.Close(w.ctx, train, trLeader, "done", "premature"); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("done from opened: %v", err)
		}
		if _, err := w.m.Close(w.ctx, train, trLeader, "abandoned", "out of lane"); err != nil {
			t.Fatalf("abandoned from opened: %v", err)
		}
	})

	t.Run("finding 7: abandoned is refused after a landing", func(t *testing.T) {
		w := newTr(t)
		verified := w.verifiedTrain()
		w.tip.set(trRepo, trBase, "merge-1")
		w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
		if _, err := w.m.TrainLand(w.ctx, verified, trLeader, "merge-1", "", w.tip, w.forge); err != nil {
			t.Fatal(err)
		}
		if _, err := w.m.Close(w.ctx, verified, trLeader, "abandoned", "after landing"); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("abandoned after a landing: %v", err)
		}
	})

	t.Run("finding 3: abandoning leaves a usable leader", func(t *testing.T) {
		w := newTr(t)
		train := w.openedTrain()
		var leaderTurn string
		rows, _ := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 1", train)
		leaderTurn = rows[0].Get("turn_id").(string)
		if _, err := w.m.Close(w.ctx, train, trLeader, "abandoned", "the base moved out of lane"); err != nil {
			t.Fatal(err)
		}
		// opening a train never moved the member turns, so abandoning leaves the leader holding and
		// the members waiting: the documented abandon-and-reopen flow has its leader
		if state := w.turn(leaderTurn).State; state != "holding" {
			t.Fatalf("the leader is %s after an abandon, and the reopen needs it holding", state)
		}
		// and the leader can open the replacement train
		if _, err := w.open(leaderTurn, trLeader, "base-0", 101, 102); err != nil {
			t.Fatalf("the leader could not reopen after an abandon: %v", err)
		}
	})

	t.Run("finding 5: landing promotes the next waiter", func(t *testing.T) {
		w := newTr(t)
		train := w.verifiedTrain()
		w.bind("PRJ-LATE", "task-late", trHost)
		if _, err := w.m.Request(w.ctx, trRepo, trBase, "PRJ-LATE", "task-late", trHost, "head-late", true,
			ClaimOptions{PR: sql.NullInt64{Int64: 301, Valid: true}, Relationship: sql.NullString{String: "rel-late", Valid: true}}); err != nil {
			t.Fatal(err)
		}
		w.tip.set(trRepo, trBase, "merge-1")
		w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
		if _, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge); err != nil {
			t.Fatal(err)
		}
		var lateState string
		if err := w.s.DB.QueryRowContext(w.ctx, "SELECT state FROM merge_turns WHERE holder_task_id = 'task-late'").Scan(&lateState); err != nil {
			t.Fatal(err)
		}
		if lateState != "holding" {
			t.Fatalf("the late candidate is %s, and a landing must promote it", lateState)
		}
	})

	t.Run("finding 4: a turn in a replacement train is guarded", func(t *testing.T) {
		w := newTr(t)
		first := w.openedTrain()
		var memberTurn string
		rows, _ := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 2", first)
		memberTurn = rows[0].Get("turn_id").(string)
		// the first train is abandoned, and the member waits in a replacement train whose id sorts
		// before the abandoned one
		if _, err := w.m.Close(w.ctx, first, trLeader, "abandoned", "out of lane"); err != nil {
			t.Fatal(err)
		}
		w.exec("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES ('aaa-replacement','tgt','owner/repo','dev','base-0','task-leader','2026-10-01T00:00:00Z')")
		w.exec("INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id, member_head) VALUES ('aaa-replacement',1,?,102,'rel-m2','head-m2')", memberTurn)
		w.exec("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES ('aaa-replacement',1,'opened','task-leader','{}','2026-10-01T00:00:00Z')")
		// the member is in a live train again, so merge-turn-check must refuse it
		if _, err := w.m.Check(w.ctx, memberTurn, "task-m2", "head-m2", "base-0", []any{}, contract.OrderedObject{}, nil, w.tip); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member of a replacement train: %v", err)
		}
	})

	t.Run("finding 11: reconcile reads the combined head", func(t *testing.T) {
		w := newTr(t)
		train := w.verifiedTrain()
		// every member head is an ancestor of the tip, but the verified head H is not
		w.tip.set(trRepo, trBase, "elsewhere")
		w.forge.compares["head-lead..elsewhere"] = "ahead"
		w.forge.compares["head-m2..elsewhere"] = "ahead"
		w.forge.compares["head-m3..elsewhere"] = "ahead"
		w.forge.compares["head-bundle..elsewhere"] = "diverged"
		answer, err := w.m.Show(w.ctx, train, w.tip, w.forge)
		if err != nil {
			t.Fatal(err)
		}
		rec, _ := answer["reconcile"].(map[string]any)
		if rec == nil || rec["landed"] != false {
			t.Fatalf("reconcile without the combined head = %v, want landed false", answer["reconcile"])
		}
		if rec["verifiedHead"] != "head-bundle" {
			t.Fatalf("reconcile did not read the verified head: %v", rec)
		}
	})
}

// TestTrainAcceptanceGate: Blocking 1. Open requires, for every member (the leader included), an active
// acceptance of the member turn's relationship whose stand head is the head its pull request shows and
// its turn holds.
func TestTrainAcceptanceGate(t *testing.T) {
	t.Run("open refuses a member with no active acceptance", func(t *testing.T) {
		w := newTr(t)
		leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
		w.pr(101, "head-lead")
		// a waiting turn whose relationship was never accepted
		w.bind("PRJ-M2", "task-m2", trHost)
		if _, err := w.m.Request(w.ctx, trRepo, trBase, "PRJ-M2", "task-m2", trHost, "head-m2", true,
			ClaimOptions{PR: sql.NullInt64{Int64: 102, Valid: true}, Relationship: sql.NullString{String: "rel-m2", Valid: true}}); err != nil {
			t.Fatal(err)
		}
		w.pr(102, "head-m2")
		_, err := w.open(leader, trLeader, "base-0", 101, 102)
		if err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member with no acceptance: %v", err)
		}
		if n := w.trainEvents(); n != 0 {
			t.Fatalf("a refused open wrote %d event(s)", n)
		}
	})

	t.Run("open refuses a member whose acceptance stands on another head", func(t *testing.T) {
		w := newTr(t)
		leader, members := w.threeMembers()
		// the member's acceptance stands on a head its turn does not hold
		w.exec("UPDATE dag_acceptances SET head_sha = 'head-elsewhere' WHERE relationship_id = 'rel-task-m2' AND state = 'active'")
		if _, err := w.open(leader, trLeader, "base-0", members...); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a member standing on another head: %v", err)
		}
		if n := w.trainEvents(); n != 0 {
			t.Fatalf("a refused open wrote %d event(s)", n)
		}
	})

	t.Run("open accepts a member whose acceptance stands on a recorded base-refresh head", func(t *testing.T) {
		w := newTr(t)
		leader, members := w.threeMembers()
		// the member's acceptance was refreshed: the stand head is the newest valid refresh's head, and
		// the member's turn and pull request hold that same head
		w.refreshStand("acc-rel-task-m2", "rel-task-m2", "head-m2-refreshed")
		_ = w
		w.exec("UPDATE merge_turns SET candidate_head = 'head-m2-refreshed' WHERE relationship_id = 'rel-task-m2'")
		w.forge.pulls[102] = TrainPullRequest{Number: 102, State: "open", BaseRef: trBase, HeadSHA: "head-m2-refreshed"}
		answer, err := w.open(leader, trLeader, "base-0", members...)
		if err != nil {
			t.Fatalf("a member standing on a recorded refresh head: %v", err)
		}
		train := answer["train"].(map[string]any)["trainId"].(string)
		rows, _ := w.s.All(w.ctx, "SELECT detail_json FROM merge_train_events WHERE train_id = ? AND kind = 'opened'", train)
		var detail map[string]any
		if err := json.Unmarshal([]byte(rows[0].Get("detail_json").(string)), &detail); err != nil {
			t.Fatal(err)
		}
		memberList, _ := detail["members"].([]any)
		second, _ := memberList[1].(map[string]any)
		if second["acceptedHead"] != "head-m2-refreshed" {
			t.Fatalf("the opened mapping records acceptedHead %v, want the refresh head", second["acceptedHead"])
		}
	})

	t.Run("land refuses after a member's acceptance was withdrawn", func(t *testing.T) {
		w := newTr(t)
		train := w.verifiedTrain()
		// the member's acceptance is withdrawn between verify and land
		w.exec("UPDATE dag_acceptances SET state = 'revoked' WHERE relationship_id = 'rel-task-m2' AND state = 'active'")
		w.tip.set(trRepo, trBase, "merge-1")
		w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
		_, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
		if err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a withdrawn acceptance at land: %v", err)
		}
		if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
			t.Fatalf("a refused land wrote %d landed event(s)", n)
		}
		if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
			t.Fatalf("a refused land landed %d turn(s)", n)
		}
	})

	t.Run("verify refuses a member whose acceptance moved after open", func(t *testing.T) {
		w := newTr(t)
		train := w.openedTrain()
		w.pr(900, "head-bundle", TrainLaneLabel)
		w.forge.runs["run-1"] = runFor("head-bundle")
		w.exec("UPDATE dag_acceptances SET head_sha = 'head-elsewhere' WHERE relationship_id = 'rel-task-m3' AND state = 'active'")
		if _, err := w.m.Verify(w.ctx, train, trLeader, "900", "head-bundle", "run-1", "/checkout", w.forge, w.proof); err == nil || trReason(err) != "disposition_conflict" {
			t.Fatalf("a moved acceptance at verify: %v", err)
		}
		if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'verified'"); n != 0 {
			t.Fatalf("a refused verify wrote %d verified event(s)", n)
		}
	})
}

// refreshStand records one valid base refresh of an acceptance, the shape dag-base-refresh leaves: a
// dag_base_refreshes row whose id is the digest of its content, which is the table dagsched's standOf
// reads and the merge train now reads through the same package.
func (w *tr) refreshStand(acceptanceID, relationship, head string) {
	w.t.Helper()
	var seq int64
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT COALESCE(MAX(refresh_seq),0)+1 FROM dag_base_refreshes WHERE acceptance_id = ?", acceptanceID).Scan(&seq); err != nil {
		w.t.Fatal(err)
	}
	event, revision, generation := "ev-refresh-"+head, "rev-"+head, int64(2)
	id := acceptance.RefreshDigest(acceptanceID, relationship, generation, event, revision, head, trRepo, trBase, "base-0", "{}", "[]")
	w.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, acceptanceID, seq, relationship, generation, event, revision, head, trRepo, trBase, "base-0", "{}", "[]", trLeader, int64(0), "2026-10-01T00:00:00Z")
}

// TestTrainMemberGuardLand: gap (a). The merge-turn-land command's member guard is exercised through
// Service.Land, not TrainLand (which takes a train id and refuses a turn id for its own reason).
func TestTrainMemberGuardLand(t *testing.T) {
	w := newTr(t)
	train := w.openedTrain()
	rows, err := w.s.All(w.ctx, "SELECT turn_id FROM merge_train_members WHERE train_id = ? AND seq = 2", train)
	if err != nil || len(rows) != 1 {
		t.Fatalf("member 2: %v", err)
	}
	memberTurn := rows[0].Get("turn_id").(string)
	// Service.Land is merge-turn-land: on a member turn it must refuse with the member guard
	if _, err := w.m.Land(w.ctx, memberTurn, "task-m2", "merge-1", "", "merged", w.tip); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("merge-turn-land on a member: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 0 {
		t.Fatalf("the guard let %d turn(s) land", n)
	}
}

// TestTrainLandWritesNoFeatureState: gap (d). A land records member integration only; it writes no
// feature, relationship or criteria state, so partial integration never shows a feature complete.
func TestTrainLandWritesNoFeatureState(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	w.tip.set(trRepo, trBase, "merge-1")
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	before := w.stateCounts()
	if _, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge); err != nil {
		t.Fatal(err)
	}
	after := w.stateCounts()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a land changed feature/relationship/criteria state: %v -> %v", before, after)
	}
	// the member turns are the only turns that moved
	if n := w.count("SELECT count(*) FROM merge_turns WHERE state = 'landed'"); n != 3 {
		t.Fatalf("landed turns = %d, want 3", n)
	}
}

// stateCounts is every row count a land must leave alone.
func (w *tr) stateCounts() map[string]int64 {
	w.t.Helper()
	out := map[string]int64{}
	for _, table := range []string{"dag_acceptances", "dag_integration_observations", "assignment_marks", "relationships"} {
		var n int64
		if err := w.s.DB.QueryRowContext(w.ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil {
			w.t.Fatal(err)
		}
		if n == 0 {
			continue
		}
		if err := w.s.DB.QueryRowContext(w.ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			w.t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

// TestTrainOpenRefusesAMemberOfAnotherLiveTrain: gap (e).
func TestTrainOpenRefusesAMemberOfAnotherLiveTrain(t *testing.T) {
	w := newTr(t)
	// a member already riding another live train is refused before any run is spent. Build the other
	// live train first, because the zone's train rows are append-only.
	leader := w.claim(trLane, trLeader, "head-lead", 101)["turnId"].(string)
	w.pr(101, "head-lead")
	w.waiting("PRJ-M2", "task-m2", "head-m2", 102)
	w.pr(102, "head-m2")
	rows, _ := w.s.All(w.ctx, "SELECT turn_id FROM merge_turns WHERE relationship_id = 'rel-task-m2'")
	memberTurn := rows[0].Get("turn_id").(string)
	w.exec("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES ('aaa-other','tgt','owner/repo','dev','base-0','task-leader','2026-10-01T00:00:00Z')")
	w.exec("INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id, member_head) VALUES ('aaa-other',1,?,102,'rel-task-m2','head-m2')", memberTurn)
	w.exec("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES ('aaa-other',1,'opened','task-leader','{}','2026-10-01T00:00:00Z')")
	if _, err := w.open(leader, trLeader, "base-0", 101, 102); err == nil || trReason(err) != "disposition_conflict" {
		t.Fatalf("a member already in another live train: %v", err)
	}
}

// TestTrainLandRefusesAnUnreadableTree: gap (c). A forge commit answer that carries no tree is
// merge_target_unreadable, never a skipped comparison.
func TestTrainLandRefusesAnUnreadableTree(t *testing.T) {
	w := newTr(t)
	train := w.verifiedTrain()
	w.tip.set(trRepo, trBase, "merge-1")
	// the forge answers the commit with its parents and no tree
	w.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}}
	_, err := w.m.TrainLand(w.ctx, train, trLeader, "merge-1", "", w.tip, w.forge)
	if err == nil || trReason(err) != "merge_target_unreadable" {
		t.Fatalf("a commit with no tree: %v", err)
	}
	if n := w.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
		t.Fatalf("a refused land wrote %d landed event(s)", n)
	}
	// and a verified event that names no tree is unreadable too. The events are append-only, so this
	// case gets its own train and a verified event written by hand without a tree.
	w2 := newTr(t)
	train2 := w2.openedTrain()
	w2.exec("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES (?,2,'verified','task-leader','{\"head\":\"head-bundle\"}','2026-10-01T00:00:02Z')", train2)
	w2.tip.set(trRepo, trBase, "merge-1")
	w2.forge.commits["merge-1"] = TrainCommit{SHA: "merge-1", Parents: []string{"base-0", "head-bundle"}, Tree: "tree-bundle"}
	if _, err := w2.m.TrainLand(w2.ctx, train2, trLeader, "merge-1", "", w2.tip, w2.forge); err == nil || trReason(err) != "merge_target_unreadable" {
		t.Fatalf("a verified event with no tree: %v", err)
	}
	if n := w2.count("SELECT count(*) FROM merge_train_events WHERE kind = 'landed'"); n != 0 {
		t.Fatalf("a refused land wrote %d landed event(s)", n)
	}
}
