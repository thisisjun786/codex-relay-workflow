package dagsched

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The manifest records, for every incoming edge, the value that satisfied it, shaped by the edge's kind (contract 4.2): the artifacts with uri, hash, size and scope; the head and the landed
// commit of an integration; the recorded decision.
func TestReleaseManifestInputsPerEdgeKind(t *testing.T) {
	k := newReleaseKit(t)
	k.putPlan("mk", 0, "mk-r1", addRelNode("A", dag.NodeNonPR), addRelNode("B", dag.NodeNonPR), addRelNode("I", dag.NodeImplementation), addRelNode("K", dag.NodeNonPR),
		addRelNode("D", dag.NodeNonPR), addRelNode("L", dag.NodeNonPR),
		addEdge("ab", "A", "B", dag.EdgeArtifactVerified, nil), addEdge("ik", "I", "K", dag.EdgeIntegrated, nil), addEdge("dl", "D", "L", dag.EdgeDecision, nil))
	a := k.acceptNode("mk", "A", acceptOpts{})
	i := k.acceptNode("mk", "I", pinnedOpts)
	k.integrate(i, "owner/repo", "dev", true, true)
	k.exec("INSERT INTO dag_decisions (decision_id, plan_id, subject, digest, disposition, authority_kind, authority_ref, revision, state, recorded_by_task_id, coordinator_epoch, recorded_at) VALUES ('dec-9', 'mk', 'merge holds', ?, 'approved', 'user', 'ref', 3, 'active', 'parent', 0, 't')", dig("subject dl"))
	inputOf := func(node string) map[string]any {
		res := k.mustRelease("mk", node)
		body, found, err := k.repo.ReadManifest(context.Background(), res.ManifestDigest)
		if err != nil || !found {
			t.Fatalf("manifest of %s: %v %v", node, found, err)
		}
		inputs := body["inputs"].([]any)
		if len(inputs) != 1 {
			t.Fatalf("%s: %d inputs", node, len(inputs))
		}
		return inputs[0].(map[string]any)
	}
	artifact := inputOf("B")
	files := artifact["artifacts"].([]any)
	if artifact["kind"] != dag.EdgeArtifactVerified || artifact["acceptance_id"] != a.Acceptance.AcceptanceID || artifact["revision_hash"] != a.Acceptance.RevisionHash || artifact["event_id"] != a.Event || len(files) != 1 {
		t.Fatalf("artifact input = %v", artifact)
	}
	file := files[0].(map[string]any)
	if file["uri"] != a.Files[0] || file["scope"] != a.Root || file["sha256"] == "" || file["bytes"] != int64(len("artifact of A\n")) {
		t.Fatalf("artifact = %v", file)
	}
	integrated := inputOf("K")
	if integrated["kind"] != dag.EdgeIntegrated || integrated["acceptance_id"] != i.Acceptance.AcceptanceID || integrated["head_sha"] != head1 || integrated["landed_sha"] != "tip" {
		t.Fatalf("integrated input = %v", integrated)
	}
	decision := inputOf("L")
	if decision["kind"] != dag.EdgeDecision || decision["decision_id"] != "dec-9" || decision["decision_digest"] != dig("subject dl") || decision["decision_revision"] != int64(3) {
		t.Fatalf("decision input = %v", decision)
	}
}

// A binding that does not belong to the node is refused: the managed engine answered "admitted" for a relationship of another issue, and the release must not record it as the node's child.
func TestReleaseBindRefusesAForeignRelationship(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.foreignRelationship("CRW-other")
	real := k.sched.Start
	k.sched.Start = func(ctx context.Context, raw []byte) (StartAnswer, error) {
		answer, err := real(ctx, raw)
		if err != nil {
			return answer, err
		}
		for i, f := range answer.Answer {
			switch f.Key {
			case "relationshipId":
				answer.Answer[i].Value = "rel-foreign-CRW-other"
			case "childTaskId":
				answer.Answer[i].Value = "other-child"
			}
		}
		return answer, nil
	}
	_, err := k.release("rp", "A")
	if refusalReason(err) != "relationship_conflict" {
		t.Fatalf("release = %v", err)
	}
	if k.rows().executions != 0 {
		t.Fatal("a foreign relationship was bound to the node")
	}
}

// A second caller that finds the request busy waits for the first to bind its child and then reports it; with no one to bind it, the wait ends with the caller's context.
func TestReleaseAwaitBound(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	rid := k.startNode("rp", "A")
	request := "dag-await"
	k.exec("UPDATE dag_node_executions SET managed_request_id = ? WHERE relationship_id = ?", request, rid)
	got, ok, err := k.sched.awaitBound(context.Background(), "rp", "A", ReleaseResult{PlanID: "rp", NodeID: "A", RequestID: request})
	if err != nil || !ok || !got.Bound || got.RelationshipID != rid || got.ChildTaskID != "child-A" || !got.Replayed {
		t.Fatalf("awaitBound = %+v %v %v", got, ok, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if _, ok, err := k.sched.awaitBound(ctx, "rp", "A", ReleaseResult{PlanID: "rp", NodeID: "A", RequestID: "dag-nobody"}); ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with nobody binding: ok %v err %v", ok, err)
	}
}

// Two releases of one manifest write one frozen file; a file that holds other bytes is never taken for the manifest.
func TestFreezeManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frozen", "m.json")
	if err := FreezeManifest(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := FreezeManifest(path, []byte("one")); err != nil {
		t.Fatalf("the same bytes again: %v", err)
	}
	if err := FreezeManifest(path, []byte("two")); err == nil {
		t.Fatal("another manifest was accepted over an existing file")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "one" {
		t.Fatalf("the frozen file now holds %q", raw)
	}
}

// The store half of a verification touches no file, but it still decides where a volatile snapshot may lie: outside the child's roots is B-05 whether or not the bytes are read.
func TestVerifyManifestVolatileScopeWithoutReadingFiles(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	snap := k.snapshot("rp")
	n, _ := nodeOf(snap, "A")
	outside := writeFile(t, t.TempDir(), "doc.md", "elsewhere")
	inside := writeFile(t, k.root, "doc.md", "inside")
	for name, c := range map[string]struct {
		uri  string
		code string
	}{"outside the roots": {outside, "B-05"}, "inside the roots": {inside, ""}, "a relative path": {"doc.md", "B-05"}} {
		in := ManifestInput{RuleVersion: k.request(false).RuleVersion, CreatedByTaskID: "parent", CreatedAt: "2026-10-02T00:00:00Z",
			Volatile: []Volatile{{Source: "linear:doc", SnapshotURI: c.uri, SHA256: shaOf([]byte("never read")), CapturedAt: "2026-10-02T00:00:00Z"}}}
		_, blocked, err := k.sched.BuildManifest(context.Background(), k.s.Q(context.Background()), "rp", snap, n, in, VerifyOptions{SkipFileBytes: true, ArtifactRoots: []string{k.root}})
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if len(blocked) > 0 {
			got = blocked[0].Code
		}
		if got != c.code {
			t.Errorf("%s: blocked %+v, want %q", name, blocked, c.code)
		}
	}
}

// B-02: a manifest whose digest is not the digest of its content is tampered, whichever side was altered.
func TestVerifyManifestDetectsTampering(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	a := k.acceptedA()
	snap := k.snapshot("rp")
	n, _ := nodeOf(snap, "B")
	ctx := context.Background()
	in := ManifestInput{RuleVersion: k.request(false).RuleVersion, CreatedByTaskID: "parent", CreatedAt: "2026-10-02T00:00:00Z"}
	body, blocked, err := k.sched.BuildManifest(ctx, k.s.Q(ctx), "rp", snap, n, in, VerifyOptions{ArtifactRoots: []string{k.root}})
	if err != nil || len(blocked) != 0 {
		t.Fatalf("build: %v %+v", err, blocked)
	}
	verify := func(body map[string]any) []BlockedFinding {
		findings, err := k.sched.VerifyManifest(ctx, k.s.Q(ctx), "rp", snap, n, body, VerifyOptions{ArtifactRoots: []string{k.root}})
		if err != nil {
			t.Fatal(err)
		}
		return findings
	}
	if findings := verify(body); len(findings) != 0 {
		t.Fatalf("a manifest just built does not verify: %+v", findings)
	}
	forgedDigest := map[string]any{}
	for key, v := range body {
		forgedDigest[key] = v
	}
	forgedDigest["manifest_digest"] = dig("another manifest")
	if findings := verify(forgedDigest); len(findings) != 1 || findings[0].Code != "B-02" {
		t.Fatalf("a forged digest: %+v", findings)
	}
	forgedInput := map[string]any{}
	for key, v := range body {
		forgedInput[key] = v
	}
	forgedInput["issue_key"] = "CRW-forged"
	if findings := verify(forgedInput); len(findings) != 1 || findings[0].Code != "B-02" {
		t.Fatalf("an altered field under the old digest: %+v", findings)
	}
	// an input that names another acceptance than the edge is satisfied by now
	wrong := map[string]any{}
	for key, v := range body {
		wrong[key] = v
	}
	input := map[string]any{}
	for key, v := range body["inputs"].([]any)[0].(map[string]any) {
		input[key] = v
	}
	input["acceptance_id"] = dig("not the acceptance")
	wrong["inputs"] = []any{input}
	wrong["manifest_digest"] = dag.ManifestDigest(wrong)
	if findings := verify(wrong); len(findings) != 1 || findings[0].Code != "B-06" {
		t.Fatalf("an input that rests on another acceptance: %+v (the edge is satisfied by %s)", findings, a.Acceptance.AcceptanceID)
	}
}

// The merge-check history gains a row only when the observation differs from the latest one.
func TestAppendMergeCheckIsAHistory(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	a := k.pinned()
	ctx := context.Background()
	check := func(head, outcome string) (int64, bool) {
		var seq int64
		var appended bool
		err := k.s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
			var err error
			seq, appended, err = k.sched.appendMergeCheck(txCtx, k.s.Q(txCtx), mergeCheck{Acceptance: a.Acceptance, Observed: openPR("owner/repo", 7, head), BaseTip: "tip", Round: 1, Outcome: outcome, Reason: "r"})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return seq, appended
	}
	if seq, appended := check(head1, OutcomeEligible); seq != 1 || !appended {
		t.Fatalf("first observation = %d %v", seq, appended)
	}
	if seq, appended := check(head1, OutcomeEligible); seq != 1 || appended {
		t.Fatalf("the same observation again = %d %v, want a replay", seq, appended)
	}
	if seq, appended := check("2222222222222222222222222222222222222222", OutcomeStaleHead); seq != 2 || !appended {
		t.Fatalf("a different head = %d %v", seq, appended)
	}
	if seq, appended := check(head1, OutcomeEligible); seq != 3 || !appended {
		t.Fatalf("back to the first observation after another one = %d %v, want a third row (a history, not a set)", seq, appended)
	}
}
