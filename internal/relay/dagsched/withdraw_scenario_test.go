package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// withdrawKit is the shape CRW-446 is about, on real git and the relay's own writers where the kits have them: a node I whose generation 1 report is ruled verified and not accepted yet, a base that
// moved after the ruling, a generation 2 the coordinator opened by hand to send the child the base merge and never sent, and the coordinator refreshing the branch itself instead.
type withdrawKit struct {
	*integrationKit
	rid, event1 string
	h1, head    string // the head the child reported and the head the pull request shows after the coordinator merged dev into it
	criteria    string
}

func newWithdrawKit(t *testing.T) *withdrawKit {
	t.Helper()
	k := newIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature")
	h1 := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	r := k.reportNode("g", "I", acceptOpts{})
	k.holdSlotsFor("g", "I")
	n, _ := nodeOf(k.snapshot("g"), "I")
	w := &withdrawKit{integrationKit: k, rid: r.Acceptance.RelationshipID, event1: r.Event, h1: h1, head: h1, criteria: n.CriteriaSetDigest}
	w.show(h1)
	return w
}

// show scripts the forge: pull request 7 is open and ready at a head, with both required checks green on it.
func (w *withdrawKit) show(head string) {
	w.t.Helper()
	pr := openPR("owner/repo", 7, head)
	pr.BaseSHA = w.repo.git("rev-parse", "dev")
	pr.Checks = []Check{{RunID: "1", Name: "A", HeadSHA: head, Conclusion: "success", Attempt: 1}, {RunID: "2", Name: "B", HeadSHA: head, Conclusion: "success", Attempt: 1}}
	pr.RequiredDeclared = []string{"A", "B"}
	w.forge.by["owner/repo#7"] = pr
}

// openUnsent is the coordinator opening generation 2 by hand for a base refresh (generation-open) and not sending it to the child, so it is never bound.
func (w *withdrawKit) openUnsent() {
	w.t.Helper()
	rvOpenByHand(w.t, w.releaseKit, w.rid, "refresh-"+w.rid, false)
}

// refreshBranch is what the coordinator does instead of sending the generation: dev moved, and it merges dev into the branch of the pull request itself.
func (w *withdrawKit) refreshBranch() {
	w.t.Helper()
	repo := w.repo
	repo.commit("other.txt", "dev moved")
	repo.git("checkout", "-q", "feature")
	repo.git("merge", "-q", "--no-ff", "-m", "merge dev into the branch", "dev")
	w.head = repo.git("rev-parse", "HEAD")
	repo.git("checkout", "-q", "dev")
	w.show(w.head)
}

func (w *withdrawKit) accept() (AcceptResult, error) {
	w.t.Helper()
	return w.releaseKit.accept("g", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}})
}

func (w *withdrawKit) withdraw(generation int64, reason string) (WithdrawResult, error) {
	w.t.Helper()
	return w.sched.WithdrawGeneration(context.Background(), "g", "I", "parent", WithdrawInput{Generation: generation, Reason: reason})
}

func (w *withdrawKit) generations() int {
	return w.count("SELECT COUNT(*) FROM generations WHERE relationship_id = ?", w.rid)
}

func (w *withdrawKit) withdrawals() int {
	return w.count("SELECT COUNT(*) FROM dag_generation_withdrawals WHERE relationship_id = ?", w.rid)
}

func (w *withdrawKit) pointer() int64 {
	w.t.Helper()
	var g int64
	if err := w.s.DB.QueryRow("SELECT execution_generation FROM relationships WHERE relationship_id = ?", w.rid).Scan(&g); err != nil {
		w.t.Fatal(err)
	}
	return g
}

// lane is the coordinator's order after an acceptance, with the relay's own commands: ask the merge lane for a turn, read and acknowledge the grant, let the lane check the head, merge on the base branch,
// land, mark the merge on the event the relay shows as current, observe where the head landed.
func (w *withdrawKit) lane(expectedEvent string) IntegrationResult {
	w.t.Helper()
	ctx := context.Background()
	judged, turn, err := w.sched.RequestMergeTurn(ctx, "g", "I", "parent", MergeRequestInput{Host: "host"})
	if err != nil || !judged.Eligible() {
		w.t.Fatalf("merge turn = %v %+v", err, judged)
	}
	service := &mergeturn.Service{Store: w.s, Registry: &registry.Registry{Store: w.s}, Now: w.sched.now, Delivery: mergeturn.StoreDelivery{Store: w.s}}
	id := turn["turnId"].(string)
	if grant, _ := turn["grant"].(map[string]any); grant != nil {
		if _, err := service.Acknowledge(ctx, id, "parent", grant["grantId"].(string), "re-read the store before merging"); err != nil {
			w.t.Fatalf("acknowledging the grant: %v", err)
		}
	}
	var list []any
	for _, c := range w.forge.by["owner/repo#7"].Checks {
		list = append(list, contract.OrderedObject{{Key: "runId", Value: c.RunID}, {Key: "name", Value: c.Name}, {Key: "headSha", Value: c.HeadSHA}, {Key: "conclusion", Value: c.Conclusion}, {Key: "attempt", Value: json.Number("1")}})
	}
	green := contract.OrderedObject{{Key: "hasNextPage", Value: false}, {Key: "pagesRead", Value: json.Number("1")}, {Key: "totalCount", Value: json.Number("0")}, {Key: "threadsSeen", Value: []any{}}, {Key: "unresolved", Value: json.Number("0")}}
	if _, err := service.Check(ctx, id, "parent", w.head, w.repo.git("rev-parse", "dev"), list, green, []string{"A", "B"}, mergeturn.TargetReader{}); err != nil {
		w.t.Fatalf("the lane's check: %v", err)
	}
	w.repo.git("merge", "-q", "--no-ff", "-m", "merge the pull request", "feature")
	if _, err := service.Land(ctx, id, "parent", w.repo.git("rev-parse", "dev"), "", "merged by the forge", mergeturn.TargetReader{}); err != nil {
		w.t.Fatalf("landing: %v", err)
	}
	if _, err := registry.NewAssignmentView(&registry.Registry{Store: w.s}).Mark(ctx, w.rid, "merged", "merged, with the head the acceptance names", "parent", expectedEvent); err != nil {
		w.t.Fatalf("assignment-mark: %v", err)
	}
	res, err := w.observe()
	if err != nil {
		w.t.Fatalf("dag-integration-observe: %v", err)
	}
	return res
}

// The state CRW-446 is about, on the code without a way out: generation 1 is ruled verified, generation 2 was opened by hand and neither bound nor sent. Every route the scheduler has refuses the node
// for the reason of its own, and each refusal writes nothing (the withdrawal below is the route out).
func TestAnUnsentGenerationLeavesNoRouteButTheWithdrawal(t *testing.T) {
	k := newWithdrawKit(t)
	k.openUnsent()
	k.refreshBranch()
	rows := k.rows()
	ctx := context.Background()

	if _, err := k.accept(); refusalReason(err) != "stale_generation" || !strings.Contains(err.Error(), "is not recorded as an execution of I") {
		t.Fatalf("dag-accept = %v, want stale_generation: generation 2 is not recorded as an execution", err)
	}
	if _, err := k.sched.RecordCorrection(ctx, "g", "I", "parent", ""); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "is not stale") {
		t.Fatalf("dag-correct = %v, want disposition_conflict: the result is not stale", err)
	}
	if _, err := k.sched.RecordBaseRefresh(ctx, "g", "I", "parent", RefreshInput{}); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "no accepted head") {
		t.Fatalf("dag-base-refresh = %v, want disposition_conflict: the node has no accepted head", err)
	}
	var child string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", k.rid).Scan(&child); err != nil {
		t.Fatal(err)
	}
	k.exec("UPDATE relationships SET allowed_recipients = ? WHERE relationship_id = ?", `["parent","`+child+`"]`, k.rid)
	if _, err := delivery.NewAck(delivery.NewService(k.s, delivery.SystemClock{})).RecordVerdict(ctx, k.event1, "needs_changes", "verdict-turn-probe", nil, []any{restoration("do it again")}, nil, k.criteria); err == nil {
		t.Fatal("a ruling on the event of generation 1 was taken while the relationship stands on generation 2")
	}
	if k.pointer() != 2 || k.generations() != 2 || k.rows() != rows {
		t.Fatalf("a refusal wrote: pointer %d, %d generations, rows %+v -> %+v", k.pointer(), k.generations(), rows, k.rows())
	}
	view := registry.NewAssignmentView(&registry.Registry{Store: k.s})
	if _, err := view.Mark(ctx, k.rid, "merged", "merged", "parent", k.event1); refusalReason(err) != "revision_ambiguous" {
		t.Fatalf("assignment-mark = %v, want revision_ambiguous: generation 2 has no head", err)
	}
	var open sql.NullString
	if err := k.s.DB.QueryRow("SELECT dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = 2", k.rid).Scan(&open); err != nil || open.Valid {
		t.Fatalf("generation 2 = %v %v, want it unbound", open, err)
	}
}

// Criterion c1 of CRW-446: generation 1 ruled verified, generation 2 opened by hand before any acceptance and neither bound nor sent, the base refreshed by the coordinator, then the withdrawal: the node
// is accepted on the head the forge shows, merged inside the merge lane, the merge is marked on the event of generation 1 (the one the relationship stands on again), and the observation reads it
// integrated and returns the execution slot. The generation is kept, and the next one takes the number 3.
func TestWithdrawingTheUnsentGenerationLetsTheNodeBeAcceptedMergedAndIntegrated(t *testing.T) {
	k := newWithdrawKit(t)
	ctx := context.Background()
	k.openUnsent()
	k.refreshBranch()

	res, err := k.withdraw(2, "the coordinator refreshed the base of the branch itself")
	if err != nil || res.Replayed || res.RestoredGeneration != 1 || res.Generation != 2 || res.RelationshipID != k.rid || res.DispatchRequestID != "refresh-"+k.rid {
		t.Fatalf("withdraw = %v %+v", err, res)
	}
	if k.pointer() != 1 || k.generations() != 2 || k.withdrawals() != 1 {
		t.Fatalf("after the withdrawal: pointer %d, %d generations, %d withdrawals; want 1, 2, 1", k.pointer(), k.generations(), k.withdrawals())
	}
	if again, err := k.withdraw(2, "asked twice"); err != nil || !again.Replayed || again.RestoredGeneration != 1 || again.Reason != res.Reason || k.withdrawals() != 1 {
		t.Fatalf("the same withdrawal again = %v %+v", err, again)
	}

	acc, err := k.accept()
	if err != nil || acc.AcceptanceID == "" || acc.Replayed || acc.Generation != 1 || acc.HeadSHA != k.head || acc.Sweep == nil || acc.Sweep.State == SweepFailed {
		t.Fatalf("dag-accept = %v %+v sweep=%+v", err, acc, acc.Sweep)
	}
	if n := k.read("g").node("I"); n.Reason != DoneAccepted {
		t.Fatalf("I = %+v, want %s", n, DoneAccepted)
	}

	obs := k.lane(k.event1)
	if !obs.Integrated || !obs.MarkPresent || !obs.SlotReleased {
		t.Fatalf("observation = %+v, want integrated with the mark present and the slot returned", obs)
	}
	if n := k.read("g").node("I"); n.State != StateIntegrated {
		t.Fatalf("I = %+v", n)
	}
	if n := k.read("g").node("K"); n.Disposition != DispReady {
		t.Fatalf("K = %+v, want ready: the node it waited for landed", n)
	}
	if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", SlotSubjectKey("g", "I")) != 0 {
		t.Fatal("the execution slot is still held")
	}

	// the number is not reused: the next generation is 3, and the withdrawn one stays closed
	reg := &registry.Registry{Store: k.s}
	if g, err := reg.OpenGeneration(ctx, k.rid, "next-"+k.rid, "needs_changes_revision", sql.NullString{}); err != nil || g.Number != 3 {
		t.Fatalf("the next generation = %v %+v, want number 3", err, g)
	}
	if k.generations() != 3 || k.pointer() != 3 {
		t.Fatalf("pointer %d with %d generations", k.pointer(), k.generations())
	}
}
