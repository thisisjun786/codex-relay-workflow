package dagsched

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// The pre-merge gate inside acceptance and integration (CRW-952). Each refusal name is reached by calling Accept with its
// own record on a temporary store and a real repository; the head shapes are read by git.

// premergeRecordOf is the valid record the fixture builds for node I of the commit kit, decoded so a test can change one
// member and encode it again.
func premergeRecordOf(t *testing.T, k *commitAcceptKit) premerge.Record {
	t.Helper()
	in := premergeWithRecord(k.sched, context.Background(), "g", "I", "parent", AcceptInput{
		Commit: &CommitRef{Head: k.head, Base: k.base, Checkout: k.repo.path}})
	rec, err := premerge.Decode(in.Premerge)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func premergeRaw(t *testing.T, rec premerge.Record) []byte {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// acceptWithPremerge runs the commit acceptance of node I with the record given (nil names none).
func (k *commitAcceptKit) acceptWithPremerge(raw []byte) (AcceptResult, error) {
	k.t.Helper()
	return k.sched.Accept(context.Background(), "g", "I", "parent", AcceptInput{
		RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"},
		Commit:      &CommitRef{Head: k.head, Base: k.base, Checkout: k.repo.path},
		Premerge:    raw,
	})
}

func TestPremergeAcceptNamesEachRefusal(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*premerge.Record)
		none   bool
		want   string
	}{
		{name: "no record", none: true, want: "premerge_missing"},
		{name: "subject", mutate: func(r *premerge.Record) { r.Node = "D" }, want: "premerge_subject_mismatch"},
		{name: "head", mutate: func(r *premerge.Record) { r.Head = strings.Repeat("b", 40) }, want: "premerge_head_mismatch"},
		{name: "criteria", mutate: func(r *premerge.Record) { r.CriteriaDigest = strings.Repeat("0", 64) }, want: "premerge_criteria_stale"},
		{name: "undisposed", mutate: func(r *premerge.Record) {
			r.Criteria["c1"] = premerge.Criterion{Verdict: "PARTIAL", Evidence: "x"}
		}, want: "premerge_undisposed"},
		{name: "blocked", mutate: func(r *premerge.Record) {
			r.Criteria["c1"] = premerge.Criterion{Verdict: "FAIL", Evidence: "x"}
			r.Dispositions.Items = []premerge.Item{{Ref: "c1", Class: "blocking", Note: "fix it"}}
		}, want: "premerge_blocked"},
		{name: "separable", mutate: func(r *premerge.Record) {
			r.Criteria["c1"] = premerge.Criterion{Verdict: "PARTIAL", Evidence: "x"}
			r.Dispositions.Items = []premerge.Item{{Ref: "c1", Class: "separable", FollowUp: "CRW-900", Note: "independent reason of enough length"}}
		}, want: "premerge_separable_forbidden"},
		{name: "carried", mutate: func(r *premerge.Record) {
			r.Defects = []premerge.Defect{{ID: "d1", Severity: "P0", What: "x"}}
			r.Dispositions.Items = []premerge.Item{{Ref: "d1", Class: "carried", FollowUp: "CRW-902", Note: "carried with a reason of enough length"}}
		}, want: "premerge_carried_forbidden"},
		{name: "invalid class", mutate: func(r *premerge.Record) {
			r.Defects = []premerge.Defect{{ID: "d1", Severity: "P1", What: "x"}}
			r.Dispositions.Items = []premerge.Item{{Ref: "d1", Class: "maybe", Note: "x"}}
		}, want: "premerge_disposition_invalid"},
		{name: "clean record accepts", mutate: func(r *premerge.Record) {}, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newCommitAcceptKit(t)
			k.report("g", "I")
			var raw []byte
			if !c.none {
				rec := premergeRecordOf(t, k)
				c.mutate(&rec)
				raw = premergeRaw(t, rec)
			}
			_, err := k.acceptWithPremerge(raw)
			if c.want == "" {
				if err != nil {
					t.Fatalf("a passing record must accept: %v", err)
				}
				return
			}
			if got := refusalReasonOf(err); got != c.want {
				t.Fatalf("want %s, got %q (%v)", c.want, got, err)
			}
		})
	}
}

// A stored record is read back with the acceptance, and a pure replay names none (answer 2).
func TestPremergeIsStoredAndAPureReplayNeedsNone(t *testing.T) {
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	first, err := k.acceptWithPremerge(premergeRaw(t, premergeRecordOf(t, k)))
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if has, err := queryOne(context.Background(), k.sched.Store.Q(context.Background()), "SELECT record_json FROM dag_acceptance_premerge WHERE acceptance_id = ?", []any{first.AcceptanceID}, &stored); err != nil || !has {
		t.Fatalf("the accepted record must be stored with its acceptance: has=%v err=%v", has, err)
	}
	replay, err := k.acceptWithPremerge(nil)
	if err != nil || !replay.Replayed {
		t.Fatalf("a pure replay names no record and replays: %v replayed=%v", err, replay.Replayed)
	}
}

// An acceptance taken before the gate has no record: the attach stores one under the same acceptance id once (answer 5).
func TestPremergeAttachStoresOnceUnderTheSameAcceptance(t *testing.T) {
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	first, err := k.acceptWithPremerge(premergeRaw(t, premergeRecordOf(t, k)))
	if err != nil {
		t.Fatal(err)
	}
	// a record-less acceptance is simulated in this temporary store: its trigger is dropped and its stored row removed
	k.exec("DROP TRIGGER dag_acceptance_premerge_no_delete")
	k.exec("DELETE FROM dag_acceptance_premerge WHERE acceptance_id = ?", first.AcceptanceID)
	attach, err := k.acceptWithPremerge(premergeRaw(t, premergeRecordOf(t, k)))
	if err != nil || attach.AcceptanceID != first.AcceptanceID || !attach.Replayed {
		t.Fatalf("the attach must store under the same acceptance: %v id=%s want %s", err, attach.AcceptanceID, first.AcceptanceID)
	}
	if _, err := k.acceptWithPremerge(premergeRaw(t, premergeRecordOf(t, k))); refusalReasonOf(err) != "disposition_conflict" {
		t.Fatalf("a second attach is refused: %v", err)
	}
}

// The head relation is read by git: plain commits keep the evaluation with a statement, dev-only merges keep it up to four
// merges, and a hand-resolved merge, a merge with a second parent outside the base, and a fifth merge do not.
func TestPremergeHeadRelationReadsTheShapesOfTheHead(t *testing.T) {
	repo := newGitRepo(t)
	repo.git("checkout", "-q", "-b", "feature")
	evaluated := repo.commit("shared.txt", "feature-evaluated")
	repo.git("checkout", "-q", "dev")
	root := repo.git("rev-parse", "dev")
	repo.git("checkout", "-q", "feature")
	plain := repo.commit("plain.txt", "plain")
	if after, err := premergeHeadRelation(context.Background(), repo.path, root, evaluated, plain); err != nil || !after {
		t.Fatalf("a plain commit after the evaluation needs its statement: after=%v err=%v", after, err)
	}
	repo.git("reset", "-q", "--hard", evaluated)
	var chain string
	for i := 1; i <= 4; i++ {
		repo.git("checkout", "-q", "dev")
		repo.commit("dev"+string(rune('0'+i))+".txt", "dev")
		repo.git("checkout", "-q", "feature")
		repo.git("merge", "--no-ff", "-q", "--no-edit", "dev")
		chain = repo.git("rev-parse", "HEAD")
	}
	devTip := repo.git("rev-parse", "dev")
	if after, err := premergeHeadRelation(context.Background(), repo.path, devTip, evaluated, chain); err != nil || after {
		t.Fatalf("four dev-only merges keep the evaluation without a statement: after=%v err=%v", after, err)
	}
	if _, err := premergeHeadRelation(context.Background(), repo.path, root, evaluated, chain); refusalReasonOf(err) != "premerge_head_mismatch" {
		t.Fatalf("a merge whose second parent is outside the base is a head mismatch: %v", err)
	}
	repo.git("checkout", "-q", "dev")
	repo.commit("dev5.txt", "dev")
	repo.git("checkout", "-q", "feature")
	repo.git("merge", "--no-ff", "-q", "--no-edit", "dev")
	fifth := repo.git("rev-parse", "HEAD")
	if _, err := premergeHeadRelation(context.Background(), repo.path, repo.git("rev-parse", "dev"), evaluated, fifth); refusalReasonOf(err) != "premerge_head_mismatch" {
		t.Fatalf("a fifth merge is a head mismatch: %v", err)
	}

	conflict := newGitRepo(t)
	conflict.git("checkout", "-q", "-b", "feature")
	base := conflict.commit("shared.txt", "feature-evaluated")
	conflict.git("checkout", "-q", "dev")
	conflict.commit("shared.txt", "dev-side")
	conflict.git("checkout", "-q", "feature")
	conflict.git("merge", "--no-ff", "-q", "--no-edit", "-X", "ours", "dev")
	hand := conflict.commit("shared.txt", "hand-resolved")
	if _, err := premergeHeadRelation(context.Background(), conflict.path, conflict.git("rev-parse", "dev"), base, hand); refusalReasonOf(err) != "premerge_head_mismatch" {
		t.Fatalf("a hand-resolved merge is a head mismatch: %v", err)
	}
}

// The integration check reads the stored record: a candidate whose record is gone is held with premerge_missing and is not
// a candidate (answer 3).
func TestPremergeHoldsACandidateWithoutItsRecord(t *testing.T) {
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	if _, err := k.acceptWithPremerge(premergeRaw(t, premergeRecordOf(t, k))); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if all, err := k.sched.AcceptedCandidates(ctx, "g"); err != nil || len(all) != 1 {
		t.Fatalf("the judged candidate is ready: %d %v", len(all), err)
	}
	k.exec("DROP TRIGGER dag_acceptance_premerge_no_delete")
	k.exec("DELETE FROM dag_acceptance_premerge")
	all, err := k.sched.AcceptedCandidates(ctx, "g")
	if err != nil || len(all) != 0 {
		t.Fatalf("a candidate without its record is not a candidate: %d %v", len(all), err)
	}
	held, err := k.sched.premergeLeftOut(ctx, "g")
	if err != nil || len(held) != 1 || held[0].Reason != "premerge_missing" || held[0].NodeID != "I" {
		t.Fatalf("the held candidate names premerge_missing: %+v %v", held, err)
	}
}
