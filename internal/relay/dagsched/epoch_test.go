package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// The coordinator epoch (epoch.go): the claim, the fence on every write that decides, and the rows that carry the epoch. The scenarios are the contract's
// two-parent cases: a newer session of the same parent task (F-3) and a replacement parent task (F-2).

// allRows is every row of every table of the store, the DAG zone included: the oracle of "a refusal changed nothing".
func allRows(t testing.TB, db *sql.DB) map[string][]map[string]any {
	t.Helper()
	return testsupport.TableRows(t, db, "name LIKE 'dag\\_%' ESCAPE '\\' OR name NOT LIKE 'dag\\_%' ESCAPE '\\'")
}

func isStale(err error) bool { return refusalReason(err) == "stale_coordinator_epoch" }

// claim is a parent session raising the plan's epoch with the scheduler it uses; the scheduler then names the epoch it was given.
func (k *releaseKit) claim(s *Scheduler, plan, actor, nonce string) ClaimResult {
	k.t.Helper()
	res, err := s.ClaimEpoch(context.Background(), plan, ClaimInput{Actor: actor, SessionNonce: nonce})
	if err != nil {
		k.t.Fatalf("claim by %s (%s): %v", actor, nonce, err)
	}
	s.ExpectedEpoch = res.Epoch
	return res
}

// session is another parent session: its own connection to the store, its own scheduler wired like the kit's, and the epoch its claim returned.
func (k *releaseKit) session(plan, actor, nonce string) *Scheduler {
	k.t.Helper()
	s, err := store.Open(context.Background(), k.path, "")
	if err != nil {
		k.t.Fatal(err)
	}
	k.t.Cleanup(func() { _ = s.Close() })
	sched := &Scheduler{Store: s, Now: k.clock}
	k.wire(sched)
	sched.Tips, sched.Ancestry = k.sched.Tips, k.sched.Ancestry
	k.claim(sched, plan, actor, nonce)
	return sched
}

// replaceParent is the replacement of the project's parent: the binding of the old parent is archived under the binding of the new one.
func (k *releaseKit) replaceParent(to string) {
	k.t.Helper()
	now := k.clock()
	if err := storeseed.ArchiveScopeBinding(context.Background(), k.s, "bind-parent", "archived", "bind-"+to, now); err != nil {
		k.t.Fatal(err)
	}
	if err := storeseed.InsertScopeBinding(context.Background(), k.s, store.ScopeBindingsRow{BindingID: "bind-" + to, Role: "parent", ScopeKind: "project", ScopeKey: "P-TEST", TaskID: to, HostID: "host",
		Status: "active", Revision: 2, Supersedes: sql.NullString{String: "bind-parent", Valid: true}, CreatedAt: now, UpdatedAt: now}); err != nil {
		k.t.Fatal(err)
	}
}

// revision is a revision document of the plan, written by author as a session that holds epoch.
func (k *releaseKit) revision(plan, request string, parent int, author string, epoch int64, changes ...doc) dag.Revision {
	k.t.Helper()
	list := make([]any, len(changes))
	for i, c := range changes {
		list[i] = c
	}
	raw, err := json.Marshal(doc{"schema": dag.SchemaRevision, "plan_id": plan, "project_key": "P-TEST", "request_id": request, "expected_parent_revision": parent,
		"author_task_id": author, "coordinator_epoch": epoch, "changes": list})
	if err != nil {
		k.t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		k.t.Fatalf("decode: %v", err)
	}
	return rev
}

func TestClaimEpoch(t *testing.T) {
	ctx := context.Background()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")

	first, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent", SessionNonce: "session-1"})
	if err != nil || first.Epoch != 1 || first.Replayed || first.PreviousEpoch != 0 || first.BindingID != "bind-parent" || first.BindingRevision != 1 || first.ProjectKey != "P-TEST" || first.TaskID != "parent" {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if again, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent", SessionNonce: "session-1"}); err != nil || !again.Replayed || again.Epoch != 1 {
		t.Fatalf("a session repeating its claim = %+v, %v", again, err)
	}
	second, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent", SessionNonce: "session-2"})
	if err != nil || second.Epoch != 2 || second.PreviousEpoch != 1 || second.PreviousTask != "parent" {
		t.Fatalf("a new session of the same task = %+v, %v", second, err)
	}
	if k.count("SELECT COUNT(*) FROM dag_coordinator_claims WHERE plan_id = 'rp'") != 2 {
		t.Fatal("a repeated claim wrote a row")
	}
	// a session that was replaced does not take the plan back by repeating itself
	if _, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent", SessionNonce: "session-1"}); !isStale(err) {
		t.Fatalf("the nonce of a replaced session = %v", err)
	}
	if _, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "intruder", SessionNonce: "session-2"}); refusalReason(err) != "scope_role_mismatch" {
		t.Fatalf("a task that is not the project's parent, with a session's nonce = %v", err)
	}
	if _, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "intruder", SessionNonce: "session-3"}); refusalReason(err) != "scope_role_mismatch" {
		t.Fatalf("a task that is not the project's parent = %v", err)
	}
	for name, in := range map[string]ClaimInput{
		"no nonce":             {Actor: "parent"},
		"a nonce with a space": {Actor: "parent", SessionNonce: "two words"},
		"a nonce too long":     {Actor: "parent", SessionNonce: strings.Repeat("a", 129)},
		"no actor":             {SessionNonce: "session-9"},
		"another project":      {Actor: "parent", SessionNonce: "session-9", Project: "P-OTHER"},
	} {
		if _, err := k.sched.ClaimEpoch(ctx, "rp", in); refusalReason(err) != "malformed_receipt" {
			t.Errorf("%s = %v, want malformed_receipt", name, err)
		}
	}
	if k.count("SELECT COUNT(*) FROM dag_coordinator_claims WHERE plan_id = 'rp'") != 2 {
		t.Fatal("a refused claim wrote a row")
	}
	// the project changes hands: the new parent cannot claim with a nonce another task's session used, and claims with its own
	k.replaceParent("parent-2")
	if _, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent-2", SessionNonce: "session-2"}); !isStale(err) {
		t.Fatalf("another task using a session's nonce = %v", err)
	}
	if res, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: "parent-2", SessionNonce: "session-3"}); err != nil || res.Epoch != 3 || res.PreviousTask != "parent" || res.BindingID != "bind-parent-2" {
		t.Fatalf("the replacement parent's claim = %+v, %v", res, err)
	}
}

// A plan that has no revision has no header and so no project: the claim names it, and the fence holds the first revision to it.
func TestClaimOfAPlanWithoutARevision(t *testing.T) {
	ctx := context.Background()
	k := newReleaseKit(t)
	if _, err := k.sched.ClaimEpoch(ctx, "fresh", ClaimInput{Actor: "parent", SessionNonce: "s1"}); refusalReason(err) != "malformed_receipt" {
		t.Fatalf("a claim that names no project for a plan without a revision = %v", err)
	}
	res, err := k.sched.ClaimEpoch(ctx, "fresh", ClaimInput{Actor: "parent", SessionNonce: "s1", Project: "P-TEST"})
	if err != nil || res.Epoch != 1 || res.ProjectKey != "P-TEST" {
		t.Fatalf("claim = %+v, %v", res, err)
	}
	if _, err := k.repo.Put(ctx, k.revision("fresh", "f-r1", 0, "parent", 1, addNode("a", dag.NodeNonPR))); err != nil {
		t.Fatalf("the first revision of the claimed plan: %v", err)
	}
	// the claim was made under the binding of P-TEST: a first revision that names another project is refused by the fence, nothing is written
	if _, err := k.sched.ClaimEpoch(ctx, "other", ClaimInput{Actor: "parent", SessionNonce: "s1", Project: "P-TEST"}); err != nil {
		t.Fatal(err)
	}
	other := k.revision("other", "o-r1", 0, "parent", 1, addNode("a", dag.NodeNonPR))
	other.ProjectKey = "P-OTHER"
	before := allRows(t, k.s.DB)
	if _, err := k.repo.Put(ctx, other); !isStale(err) {
		t.Fatalf("a first revision in another project than the claim's = %v", err)
	}
	if !reflect.DeepEqual(before, allRows(t, k.s.DB)) {
		t.Fatal("the refusal changed the store")
	}
}

// Two sessions claim at the same moment: the store's write lock and the (plan, epoch) key give them two different epochs.
func TestClaimsAreSerialised(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	var wg sync.WaitGroup
	results := make([]ClaimResult, 6)
	errs := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := store.Open(context.Background(), k.path, "")
			if err != nil {
				errs[i] = err
				return
			}
			defer s.Close()
			results[i], errs[i] = (&Scheduler{Store: s, Now: k.clock}).ClaimEpoch(context.Background(), "rp", ClaimInput{Actor: "parent", SessionNonce: fmt.Sprintf("racer-%d", i)})
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("claim %d: %v", i, errs[i])
		}
		if seen[r.Epoch] {
			t.Fatalf("epoch %d was given twice", r.Epoch)
		}
		seen[r.Epoch] = true
	}
	for epoch := int64(1); epoch <= 6; epoch++ {
		if !seen[epoch] {
			t.Fatalf("epochs given: %v, want 1 to 6", seen)
		}
	}
}

// fencedPath is one write that decides, set up so that it succeeds for the session that holds the epoch: the stale session's refusal is then the fence's and nothing else's.
type fencedPath struct {
	name  string
	plan  string
	setup func(t *testing.T) (*releaseKit, func(s *Scheduler) error)
	// prepare runs after the first session claimed and before the second does, so the state it leaves is the first session's work.
	prepare func(t *testing.T, k *releaseKit)
}

func fencedPaths() []fencedPath {
	releaseKitFor := func(t *testing.T) *releaseKit {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		return k
	}
	put := 0
	return []fencedPath{
		{name: "a plan revision", plan: "rp", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := releaseKitFor(t)
			put++
			request := fmt.Sprintf("put-%d", put)
			return k, func(s *Scheduler) error {
				_, err := k.repo.Put(context.Background(), k.revision("rp", request, 1, "parent", s.ExpectedEpoch, addNode("X", dag.NodeNonPR)))
				return err
			}
		}},
		{name: "a release", plan: "rp", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := releaseKitFor(t)
			return k, func(s *Scheduler) error {
				_, err := s.Release(context.Background(), "rp", "A", "parent", k.request(false))
				return err
			}
		}},
		{name: "the replay of a release that bound its child", plan: "rp", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := releaseKitFor(t)
			return k, func(s *Scheduler) error {
				_, err := s.Release(context.Background(), "rp", "A", "parent", k.request(false))
				return err
			}
		}, prepare: func(t *testing.T, k *releaseKit) { k.mustRelease("rp", "A") }},
		{name: "a region declaration", plan: "rp", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := releaseKitFor(t)
			return k, func(s *Scheduler) error {
				_, err := s.DeclareRegions(context.Background(), "rp", "I", "parent", []Region{{Repository: "owner/repo", Path: "internal/x.go", Kind: "file", Change: "edit"}})
				return err
			}
		}},
		{name: "a decision record", plan: "rp", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := releaseKitFor(t)
			return k, func(s *Scheduler) error {
				_, err := s.RecordDecision(context.Background(), "rp", "parent", DecisionInput{Subject: "merge holds", Digest: dig("subject"), Disposition: "approved", AuthorityKind: "user", AuthorityRef: "ref"})
				return err
			}
		}},
		{name: "an acceptance", plan: "rp", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := releaseKitFor(t)
			return k, func(s *Scheduler) error {
				_, err := s.Accept(context.Background(), "rp", "A", "parent", AcceptInput{RuleVersion: verifier})
				return err
			}
		}, prepare: func(t *testing.T, k *releaseKit) {
			res := k.mustRelease("rp", "A")
			k.seedReport(res.RelationshipID, "A", "rp")
		}},
		{name: "the preparation of a correction", plan: "rp", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := releaseKitFor(t)
			return k, func(s *Scheduler) error {
				snapshot := writeFile(t, k.root, "notes.md", "the notes of the correction")
				_, err := s.PrepareCorrection(context.Background(), "rp", "A", "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion,
					Volatile: []Volatile{{Source: "linear:comment", SnapshotURI: snapshot, SHA256: shaOf([]byte("the notes of the correction")), CapturedAt: "2026-10-02T00:00:00Z"}}}, VerifyOptions{ArtifactRoots: []string{k.root}})
				return err
			}
		}, prepare: func(t *testing.T, k *releaseKit) { k.mustRelease("rp", "A") }},
		{name: "the record of a correction", plan: "rp", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := releaseKitFor(t)
			return k, func(s *Scheduler) error {
				_, err := s.RecordCorrection(context.Background(), "rp", "A", "parent", "")
				return err
			}
		}, prepare: func(t *testing.T, k *releaseKit) {
			res := k.mustRelease("rp", "A")
			k.seedReport(res.RelationshipID, "A", "rp")
			prepared := k.prepare()
			k.openCorrection(res.RelationshipID, []map[string]any{{"id": "c2", "verdict": "needs_changes", "note": prepared.Instruction, "restoration": true}})
		}},
		{name: "an observation of integration", plan: "g", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := newJudgeKit(t)
			return k.releaseKit, func(s *Scheduler) error {
				_, err := s.ObserveIntegration(context.Background(), "g", "I", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev"}})
				return err
			}
		}},
		{name: "a merge judgement", plan: "g", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := newJudgeKit(t)
			return k.releaseKit, func(s *Scheduler) error {
				_, err := s.Judge(context.Background(), "g", "I", "parent", JudgeInput{})
				return err
			}
		}},
		{name: "a merge request", plan: "g", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := newJudgeKit(t)
			return k.releaseKit, func(s *Scheduler) error {
				_, _, err := s.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"})
				return err
			}
		}},
		{name: "a cap basis for a plan of the project", plan: "rp", setup: func(t *testing.T) (*releaseKit, func(*Scheduler) error) {
			k := releaseKitFor(t)
			k.declareLimit("project", "P-TEST", "runs", 10)
			return k, func(s *Scheduler) error {
				return s.RecordCapBasis(context.Background(), CapBasis{Plan: "rp", LimitID: "lim-project-runs", Revision: 1, WMinutes: 30, WSource: "measured: fork/join run", SMinutes: 5, SSource: "measured: parent turns", DecidedBy: "parent"})
			}
		}},
	}
}

// Criterion c1: every write that decides checks the epoch in its own transaction, and in the two-parent scenarios 100% of the writes of the stale epoch are refused with
// stale_coordinator_epoch, leave every table of the store exactly as it was and create no child. Each path is set up so that the write would succeed for the holder of the epoch.
func TestEveryFencedWriteRefusesAStaleEpoch(t *testing.T) {
	for _, scenario := range []string{"a newer session of the same parent task", "a replacement parent task"} {
		for _, path := range fencedPaths() {
			t.Run(scenario+": "+path.name, func(t *testing.T) {
				k, op := path.setup(t)
				stale := k.sched
				k.claim(stale, path.plan, "parent", "session-1")
				if path.prepare != nil {
					path.prepare(t, k)
				}
				var current *Scheduler
				if scenario == "a replacement parent task" {
					k.replaceParent("parent-2")
					current = k.session(path.plan, "parent-2", "session-2")
				} else {
					current = k.session(path.plan, "parent", "session-2")
				}
				before, createdBefore := allRows(t, k.s.DB), 0
				createdBefore, _ = k.host.counts()
				if err := op(stale); !isStale(err) {
					t.Fatalf("the stale session's write = %v, want stale_coordinator_epoch", err)
				}
				if after := allRows(t, k.s.DB); !reflect.DeepEqual(before, after) {
					t.Fatalf("a refused write changed the store")
				}
				if created, _ := k.host.counts(); created != createdBefore {
					t.Fatalf("a refused write created a child (%d before, %d after)", createdBefore, created)
				}
				if scenario != "a newer session of the same parent task" {
					return
				}
				if err := op(current); err != nil {
					t.Fatalf("the write of the holder of the epoch was refused: %v", err)
				}
			})
		}
	}
}

// A write that names an epoch nobody claimed is refused, and a plan nobody claimed is unfenced (the first claim fences it from then on).
func TestAnUnclaimedPlanIsUnfencedAndAnUnclaimedEpochIsRefused(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.sched.ExpectedEpoch = 3
	before := allRows(t, k.s.DB)
	if _, err := k.release("rp", "A"); !isStale(err) {
		t.Fatalf("a release that names an epoch nobody holds = %v", err)
	}
	if !reflect.DeepEqual(before, allRows(t, k.s.DB)) {
		t.Fatal("the refusal changed the store")
	}
	k.sched.ExpectedEpoch = 0
	if res, err := k.release("rp", "A"); err != nil || !res.Bound {
		t.Fatalf("the unclaimed plan took no write that names no epoch: %+v, %v", res, err)
	}
	if k.count("SELECT coordinator_epoch FROM dag_releases") != 0 {
		t.Fatal("an unclaimed plan's release carries an epoch")
	}
	// the first claim fences it: the session that has not claimed names no epoch and is refused
	k.sched.ExpectedEpoch = 0
	other := k.session("rp", "parent", "session-1")
	if _, err := k.sched.RecordDecision(context.Background(), "rp", "parent", DecisionInput{Subject: "s", Digest: dig("s"), Disposition: "approved", AuthorityKind: "user", AuthorityRef: "r"}); !isStale(err) {
		t.Fatalf("a session that did not claim, on a claimed plan = %v", err)
	}
	if _, err := other.RecordDecision(context.Background(), "rp", "parent", DecisionInput{Subject: "s", Digest: dig("s"), Disposition: "approved", AuthorityKind: "user", AuthorityRef: "r"}); err != nil {
		t.Fatalf("the session that claimed: %v", err)
	}
}

// The rows a decision leaves carry the epoch of the session that decided: the manifest, the release, the acceptance, the decision and the plan revision.
func TestTheRowsCarryTheEpochOfTheDecidingSession(t *testing.T) {
	ctx := context.Background()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.claim(k.sched, "rp", "parent", "session-1")
	k.session("rp", "parent", "session-2")
	s3 := k.session("rp", "parent", "session-3")
	if s3.ExpectedEpoch != 3 {
		t.Fatalf("epoch %d, want 3", s3.ExpectedEpoch)
	}
	res, err := s3.Release(ctx, "rp", "A", "parent", k.request(false))
	if err != nil || !res.Bound {
		t.Fatalf("release = %+v, %v", res, err)
	}
	k.seedReport(res.RelationshipID, "A", "rp")
	if _, err := s3.Accept(ctx, "rp", "A", "parent", AcceptInput{RuleVersion: verifier}); err != nil {
		t.Fatal(err)
	}
	if _, err := s3.RecordDecision(ctx, "rp", "parent", DecisionInput{Subject: "merge holds", Digest: dig("subject"), Disposition: "approved", AuthorityKind: "user", AuthorityRef: "ref"}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.repo.Put(ctx, k.revision("rp", "epoch-rev", 1, "parent", 3, addNode("X", dag.NodeNonPR))); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"dag_releases", "dag_acceptances", "dag_decisions", "dag_plan_revisions WHERE revision_no = 2", "dag_input_manifests"} {
		query := "SELECT coordinator_epoch FROM " + table
		if !strings.Contains(table, "WHERE") {
			query += " WHERE 1 = 1"
		}
		var epoch int64
		if err := k.s.DB.QueryRow(query).Scan(&epoch); err != nil || epoch != 3 {
			t.Errorf("%s: epoch %d (%v), want 3", table, epoch, err)
		}
	}
}

// A cap basis belongs to a limit and not to a plan. For a project limit the recorder names the plan it coordinates, which is held to the project; once any plan of the project is under an epoch a basis
// without a plan is refused; a limit of a wider scope is declared by a supervisor, who coordinates no plan, and takes no plan.
func TestCapBasisAndTheEpoch(t *testing.T) {
	ctx := context.Background()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.declareLimit("project", "P-TEST", "runs", 10)
	basis := CapBasis{LimitID: "lim-project-runs", Revision: 1, WMinutes: 30, WSource: "measured: fork/join run", SMinutes: 5, SSource: "measured: parent turns", DecidedBy: "parent"}
	// before any claim the project is unfenced: a basis with no plan is recorded as it always was
	if err := k.sched.RecordCapBasis(ctx, basis); err != nil {
		t.Fatal(err)
	}
	s1 := k.session("rp", "parent", "session-1")
	k.session("rp", "parent", "session-2")
	other := basis
	other.WMinutes = 31
	if err := k.sched.RecordCapBasis(ctx, other); !isStale(err) {
		t.Fatalf("a basis without a plan, after a plan of the project was claimed = %v", err)
	}
	other.Plan = "rp"
	if err := s1.RecordCapBasis(ctx, other); !isStale(err) {
		t.Fatalf("a basis for the plan under a stale epoch = %v", err)
	}
	unrelated := basis
	unrelated.Plan = "nowhere"
	if err := s1.RecordCapBasis(ctx, unrelated); refusalReason(err) != "malformed_receipt" {
		t.Fatalf("a basis for a plan that is not the project's = %v", err)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_cap_basis"); n != 1 {
		t.Fatalf("%d basis rows after the refusals", n)
	}
	// a wider-scope limit: declared by a supervisor and not fenced; naming a plan for it is a malformed call
	k.declareLimit("store", "store", "runs", 8)
	wider := CapBasis{LimitID: "lim-store-runs", Revision: 1, WMinutes: 30, WSource: "measured", SMinutes: 5, SSource: "measured", DecidedBy: "parent", Plan: "rp"}
	if err := s1.RecordCapBasis(ctx, wider); refusalReason(err) != "malformed_receipt" {
		t.Fatalf("a plan named for a limit of the store scope = %v", err)
	}
}
