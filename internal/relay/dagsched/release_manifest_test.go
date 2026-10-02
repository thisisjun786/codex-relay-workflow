package dagsched

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"syscall"
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
	// decoys a looser query would pick: another plan's decision with a higher revision, and a later observation that was reverted
	k.exec("INSERT INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES ('other', 'P-TEST', 't', 't')")
	k.exec("INSERT INTO dag_decisions (decision_id, plan_id, subject, digest, disposition, authority_kind, authority_ref, revision, state, recorded_by_task_id, coordinator_epoch, recorded_at) VALUES ('dec-other', 'other', 'merge holds', ?, 'approved', 'user', 'ref', 99, 'active', 'parent', 0, 't')", dig("subject dl"))
	k.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, reverted_by, observed_at) VALUES ('decoy', ?, 'owner/repo', 'dev', ?, 'wrong-tip', 1, 'git', 99, 'someone', 't')", i.Acceptance.AcceptanceID, head1)
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

// A frozen copy is named by its own bytes: the same bytes are one file, other bytes are another file, and a file that holds other bytes than its name says is never taken for the manifest.
func TestFreezeManifest(t *testing.T) {
	root := t.TempDir()
	path, err := FreezeManifest(root, []byte("one"))
	if err != nil || filepath.Base(path) != shaOf([]byte("one"))+".json" || filepath.Dir(path) != filepath.Join(root, "dag-input-manifests") {
		t.Fatalf("freeze = %q %v", path, err)
	}
	if again, err := FreezeManifest(root, []byte("one")); err != nil || again != path {
		t.Fatalf("the same bytes again = %q %v", again, err)
	}
	other, err := FreezeManifest(root, []byte("two"))
	if err != nil || other == path {
		t.Fatalf("other bytes = %q %v: a body of the same manifest built later must not meet a file it did not write", other, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v %v", info, err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FreezeManifest(root, []byte("one")); err == nil {
		t.Fatal("a file that holds other bytes than its name was taken for the manifest")
	}
}

// The child owns its artifact root, so what it can plant there must neither take the parent's write outside the root nor hold the parent: a directory or a file that is a link, and a
// file that is a pipe (opening one blocks), are refused promptly and nothing is written outside.
func TestFreezeManifestRefusesWhatTheChildCanPlant(t *testing.T) {
	canonical := []byte("the manifest")
	named := shaOf(canonical) + ".json"
	within := func(t *testing.T, plant func(root, dir, outside string)) error {
		t.Helper()
		root, outside := t.TempDir(), t.TempDir()
		plant(root, filepath.Join(root, "dag-input-manifests"), outside)
		planted, _ := os.ReadDir(outside)
		done := make(chan error, 1)
		go func() {
			_, err := FreezeManifest(root, canonical)
			done <- err
		}()
		select {
		case err := <-done:
			if entries, _ := os.ReadDir(outside); len(entries) != len(planted) {
				t.Fatalf("something was written outside the root: %v", entries)
			}
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("the freeze is blocked")
			return nil
		}
	}
	if err := within(t, func(root, dir, outside string) {
		if err := os.Symlink(outside, dir); err != nil {
			t.Fatal(err)
		}
	}); err == nil {
		t.Error("a directory that is a link to elsewhere was written through")
	}
	if err := within(t, func(root, dir, outside string) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "planted"), []byte("the manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "planted"), filepath.Join(dir, named)); err != nil {
			t.Fatal(err)
		}
	}); err == nil {
		t.Error("a file that is a link to another file with the same bytes was accepted")
	}
	if err := within(t, func(root, dir, outside string) {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(dir, named), 0o600); err != nil {
			t.Fatal(err)
		}
	}); err == nil {
		t.Error("a pipe was taken for the manifest")
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

// H3: an artifact left out of a receipt's list is an omission the remaining files cannot reveal. The list must hash to the revision the parent accepted, in the reading, in a release and in
// the verification of a manifest, whether or not the files are read.
func TestReleaseDetectsAnArtifactLeftOutOfTheList(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	a := k.acceptNode("rp", "A", acceptOpts{Artifacts: 2})
	snap := k.snapshot("rp")
	n, _ := nodeOf(snap, "B")
	ctx := context.Background()
	in := ManifestInput{RuleVersion: k.request(false).RuleVersion, CreatedByTaskID: "parent", CreatedAt: "2026-10-02T00:00:00Z"}
	body, blocked, err := k.sched.BuildManifest(ctx, k.s.Q(ctx), "rp", snap, n, in, VerifyOptions{ArtifactRoots: []string{k.root}})
	if err != nil || len(blocked) != 0 || len(body["inputs"].([]any)[0].(map[string]any)["artifacts"].([]any)) != 2 {
		t.Fatalf("build: %v %+v", err, blocked)
	}
	// a manifest that lists one of the two, under its own recomputed digest, is not what the parent accepted (both with and without reading files)
	short := map[string]any{}
	for key, v := range body {
		short[key] = v
	}
	input := map[string]any{}
	for key, v := range body["inputs"].([]any)[0].(map[string]any) {
		input[key] = v
	}
	input["artifacts"] = input["artifacts"].([]any)[:1]
	short["inputs"] = []any{input}
	short["manifest_digest"] = dag.ManifestDigest(short)
	for _, skip := range []bool{false, true} {
		findings, err := k.sched.VerifyManifest(ctx, k.s.Q(ctx), "rp", snap, n, short, VerifyOptions{SkipFileBytes: skip, ArtifactRoots: []string{k.root}})
		if err != nil || len(findings) != 1 || findings[0].Code != "B-17" {
			t.Fatalf("skip=%v: a manifest that lists one of two artifacts: %v %+v", skip, err, findings)
		}
	}
	// and the receipt itself: one entry removed, both files still on disk
	entries, _ := receiptEntries(k.receiptOf(a))
	k.exec("UPDATE events SET receipt = ? WHERE event_id = ?", `{"manifest":[{"path":`+jsonString(entries[0].Path)+`,"sha256":"`+entries[0].SHA256+`","bytes":`+itoa64(*entries[0].Bytes)+`}]}`, a.Event)
	before := k.rows()
	if _, err := k.release("rp", "B"); refusalReason(err) != "manifest_unverified" {
		t.Fatalf("release over a receipt with an entry removed = %v", err)
	}
	if k.rows() != before {
		t.Fatalf("rows = %+v, want %+v", k.rows(), before)
	}
	if n := k.read("rp").node("B"); n.Reason != BlockedInputMissing {
		t.Fatalf("B = %+v", n)
	}
	skipped, err := k.sched.Read(ctx, "rp", ReadyOptions{SkipArtifactBytes: true})
	if err != nil || skipped.node("B").Reason != BlockedInputMissing {
		t.Fatalf("the store half = %+v %v, want the list check kept", skipped.node("B"), err)
	}
}

func (f *fixture) receiptOf(a accepted) string {
	f.t.Helper()
	var receipt string
	if err := f.s.DB.QueryRow("SELECT receipt FROM events WHERE event_id = ?", a.Event).Scan(&receipt); err != nil {
		f.t.Fatal(err)
	}
	return receipt
}

// A verifier that compared only the number of inputs would accept the same edge twice for an edge that has none.
func TestVerifyManifestRejectsADuplicatedInput(t *testing.T) {
	k := newReleaseKit(t)
	k.putPlan("dup", 0, "dup-r1", addRelNode("A", dag.NodeNonPR), addRelNode("C", dag.NodeNonPR), addRelNode("G", dag.NodeNonPR),
		addEdge("ag", "A", "G", dag.EdgeArtifactVerified, nil), addEdge("cg", "C", "G", dag.EdgeArtifactVerified, nil))
	k.acceptNode("dup", "A", acceptOpts{})
	k.acceptNode("dup", "C", acceptOpts{})
	snap := k.snapshot("dup")
	n, _ := nodeOf(snap, "G")
	ctx := context.Background()
	in := ManifestInput{RuleVersion: k.request(false).RuleVersion, CreatedByTaskID: "parent", CreatedAt: "2026-10-02T00:00:00Z"}
	body, blocked, err := k.sched.BuildManifest(ctx, k.s.Q(ctx), "dup", snap, n, in, VerifyOptions{ArtifactRoots: []string{k.root}})
	if err != nil || len(blocked) != 0 {
		t.Fatalf("build: %v %+v", err, blocked)
	}
	forged := map[string]any{}
	for key, v := range body {
		forged[key] = v
	}
	inputs := body["inputs"].([]any)
	forged["inputs"] = []any{inputs[0], inputs[0]}
	forged["manifest_digest"] = dag.ManifestDigest(forged)
	findings, err := k.sched.VerifyManifest(ctx, k.s.Q(ctx), "dup", snap, n, forged, VerifyOptions{ArtifactRoots: []string{k.root}})
	if err != nil || len(findings) != 1 || findings[0].Code != "B-01" {
		t.Fatalf("a manifest with one input twice: %v %+v", err, findings)
	}
}

// The request document is written by hand in snake_case: every field of it must be read.
func TestDecodeReleaseRequestReadsTheDocumentedFields(t *testing.T) {
	req, err := DecodeReleaseRequest([]byte(`{
	  "schema": "dag-release-request/1",
	  "base": {"repository": "owner/repo", "ref": "dev"},
	  "rule_version": {"skills_digest": "d1", "model": "gpt-5", "effort": "high", "prompt_template": "t1", "relay_build": "b1"},
	  "volatile": [{"source": "linear:doc", "snapshot_uri": "/tmp/x/doc.md", "sha256": "abc", "captured_at": "2026-10-02T00:00:00Z"}],
	  "instructions": "Do the work.",
	  "criteria": [{"id": "c1", "title": "works", "required": true}],
	  "criteria_source": "issue:CRW-1", "scope_ref": "issue:CRW-1",
	  "artifact_roots": ["/tmp/x"], "allowed_recipients": ["parent"],
	  "parent": {"host_id": "host", "settings": {"model": "gpt-5"}},
	  "child": {"host_id": "host", "title": "Child", "settings": {"model": "gpt-5"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Base.Repository != "owner/repo" || req.Base.Ref != "dev" || req.RuleVersion.PromptTemplate != "t1" || req.RuleVersion.RelayBuild != "b1" || req.RuleVersion.SkillsDigest != "d1" ||
		len(req.Volatile) != 1 || req.Volatile[0].SnapshotURI != "/tmp/x/doc.md" || req.Volatile[0].CapturedAt != "2026-10-02T00:00:00Z" || req.Child.Title != "Child" || req.ArtifactRoots[0] != "/tmp/x" {
		t.Fatalf("request = %+v", req)
	}
}
