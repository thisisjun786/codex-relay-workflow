package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// acceptedA accepts A with its artifact and returns what the successor B will consume.
func (k *releaseKit) acceptedA() accepted {
	k.t.Helper()
	return k.acceptNode("rp", "A", acceptOpts{})
}

// pinned accepts I at head1 with its forge row and the pull request the scripted forge reports for it, and declares the regions of I and J (disjoint) so J is not held back
// by the edit-region rule.
func (k *releaseKit) pinned() accepted {
	k.t.Helper()
	k.declare("rp", "I", "i.go")
	k.declare("rp", "J", "j.go")
	a := k.acceptNode("rp", "I", acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"})
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
	return a
}

// Criterion c3 and D-15: the release is coupled with slot reservation, and a refusal of the reservation leaves no intent behind. Each case is one way the reservation fails.
func TestReleaseCapacityRefusals(t *testing.T) {
	t.Run("no slot is free: the contest is recorded and nothing else is", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.declareLimit("project", "P-TEST", "runs", 1)
		k.holdSlots(1)
		if n := k.read("rp").node("A"); n.Reason != DeferNoCapacity {
			t.Fatalf("the reading says %+v, want defer:no_capacity (the release is where the ceiling is enforced)", n)
		}
		slots := k.count("SELECT COUNT(*) FROM execution_slots")
		_, err := k.release("rp", "A")
		if refusalReason(err) != "capacity_exhausted" {
			t.Fatalf("release = %v", err)
		}
		rows := k.rows()
		if rows.conflicts != 1 || rows.releases != 0 || rows.manifests != 0 || rows.requests != 0 || k.count("SELECT COUNT(*) FROM execution_slots") != slots {
			t.Fatalf("rows = %+v", rows)
		}
		if created, _ := k.host.counts(); created != 0 {
			t.Fatal("a child was created without a slot")
		}
	})
	t.Run("the standing cap of six holds without a cap basis", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.declareLimit("project", "P-TEST", "runs", 20)
		k.holdSlots(6)
		if _, err := k.release("rp", "A"); refusalReason(err) != "capacity_exhausted" {
			t.Fatalf("a seventh child under an unbased ceiling of 20: %v", err)
		}
		if k.rows().releases != 0 {
			t.Fatal("a release was recorded above the standing cap")
		}
	})
	t.Run("the actor is not the registered parent", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		snap := k.snapshot("rp")
		n, _ := nodeOf(snap, "A")
		_, err := k.sched.Release(context.Background(), "rp", "A", "intruder", k.request(n.Kind == dag.NodeImplementation))
		if refusalReason(err) != "scope_role_mismatch" {
			t.Fatalf("release by another task = %v", err)
		}
		if rows := k.rows(); rows.conflicts != 1 || rows.releases != 0 || rows.manifests != 0 || rows.slots != 0 {
			t.Fatalf("rows = %+v", rows)
		}
	})
	t.Run("a failure after the reservation rolls everything back", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.sched.testAfterReserve = func() error { return fmt.Errorf("the disk is full") }
		if _, err := k.release("rp", "A"); err == nil {
			t.Fatal("the injected failure was swallowed")
		}
		if rows := k.rows(); rows != (rowCounts{}) {
			t.Fatalf("rows after the rollback = %+v", rows)
		}
		k.sched.testAfterReserve = nil
		if res := k.mustRelease("rp", "A"); !res.Bound {
			t.Fatalf("the release after the rollback = %+v", res)
		}
	})
}

// A plan, an acceptance or a registration may move between the judgement and the intent. The intent is judged again under the lock, and nothing is written when it no longer holds.
func TestReleaseRefusesWhenInputsChangeBetweenReadAndIntent(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		move   func(k *releaseKit, a accepted, c accepted)
	}{
		{"the predecessor's acceptance is superseded", "disposition_conflict", func(k *releaseKit, a, c accepted) {
			k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
		}},
		{"the criteria are registered again", "criteria_set_changed", func(k *releaseKit, a, c accepted) {
			k.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = ?", dig("registered again"), a.Acceptance.RelationshipID)
		}},
		{"a plan revision adds an edge that is already satisfied", "disposition_conflict", func(k *releaseKit, a, c accepted) {
			k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-r2", addEdge("cb", "C", "B", dag.EdgeArtifactVerified, nil))
		}},
		{"the artifact's file is removed (the store half still holds)", "", func(k *releaseKit, a, c accepted) {
			_ = os.Remove(a.Files[0])
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			k.putPlan("rp", 1, "rp-r1b", addRelNode("C", dag.NodeNonPR))
			a := k.acceptedA()
			cc := k.acceptNode("rp", "C", acceptOpts{})
			before := k.rows()
			k.sched.testBetweenReadAndIntent = func() { c.move(k, a, cc) }
			_, err := k.release("rp", "B")
			if c.reason == "" {
				// a file change after the unlocked read is the file half's: the lock does not re-hash, and the child re-verifies on its side.
				if err != nil {
					t.Fatalf("release = %v", err)
				}
				return
			}
			if refusalReason(err) != c.reason {
				t.Fatalf("release = %v, want %s", err, c.reason)
			}
			if rows := k.rows(); rows != before {
				t.Fatalf("rows = %+v, want the %+v the setup left", rows, before)
			}
			if created, _ := k.host.counts(); created != 0 {
				t.Fatal("a child was created")
			}
		})
	}
}

// E-10: a push to a predecessor's pull request after its acceptance stops the release of everything that builds on it.
func TestReleaseRefusesStaleCodeHead(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.pinned()
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, "2222222222222222222222222222222222222222")
	_, err := k.release("rp", "J")
	if refusalReason(err) != "merge_candidate_moved" {
		t.Fatalf("release of J after a push = %v", err)
	}
	if rows := k.rows(); rows.releases != 0 || rows.slots != 0 || rows.merges != 1 {
		t.Fatalf("rows = %+v, want one stale_head observation and no release", rows)
	}
	var outcome, observed string
	if err := k.s.DB.QueryRow("SELECT outcome, observed_head_sha FROM dag_merge_checks").Scan(&outcome, &observed); err != nil || outcome != "stale_head" || observed != "2222222222222222222222222222222222222222" {
		t.Fatalf("merge check = %s %s %v", outcome, observed, err)
	}
	if n := k.read("rp").node("J"); n.Reason != BlockedStaleHead {
		t.Fatalf("J = %+v, want blocked:stale_head", n)
	}
	// the same observation again is a replay, not a second row
	if _, err := k.release("rp", "J"); refusalReason(err) != "merge_candidate_moved" {
		t.Fatal("second attempt")
	}
	if k.rows().merges != 1 {
		t.Fatal("a repeated observation wrote a second row")
	}
}

func TestReleaseAllowsMergedPinnedPredecessor(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.pinned()
	pr := openPR("owner/repo", 7, head1)
	pr.State = "merged"
	k.forge.by["owner/repo#7"] = pr
	res, err := k.release("rp", "J")
	if err != nil || !res.Bound {
		t.Fatalf("release of J over a merged predecessor: %v %+v", err, res)
	}
	if k.rows().merges != 0 {
		t.Fatal("an unchanged head was recorded as stale")
	}
}

// The forge is unreadable or the evidence incomplete: a host failure and no rows, retryable; a candidate that moved while it was read is a refusal.
func TestReleaseFreshnessFailsClosed(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.pinned()
	good := openPR("owner/repo", 7, head1)
	before := k.rows()
	for name, c := range map[string]struct {
		pr      PullRequest
		err     error
		refused string
	}{
		"the reader fails":                  {err: fmt.Errorf("gh: timeout")},
		"unknown: a truncated check list":   {pr: PullRequest{Repository: "owner/repo", Number: 7, HeadSHA: head1, Verdict: "unknown", Problems: []Problem{{Code: "enumeration_truncated", Detail: "page budget"}}, Checks: good.Checks}},
		"stale: the candidate moved":        {pr: PullRequest{Repository: "owner/repo", Number: 7, HeadSHA: head1, Verdict: "stale", Problems: []Problem{{Code: "candidate_moved", Detail: "moved"}}}, refused: "merge_candidate_moved"},
		"stale: the gates moved":            {pr: PullRequest{Repository: "owner/repo", Number: 7, HeadSHA: head1, Verdict: "stale", Problems: []Problem{{Code: "gates_moved", Detail: "moved"}}}, refused: "merge_evidence_malformed"},
		"a verdict the scheduler never saw": {pr: PullRequest{Repository: "owner/repo", Number: 7, HeadSHA: head1, Verdict: "maybe"}},
	} {
		k.forge.err = c.err
		k.forge.by["owner/repo#7"] = c.pr
		_, err := k.release("rp", "J")
		switch {
		case c.refused != "" && refusalReason(err) != c.refused:
			t.Fatalf("%s: %v, want a refusal %s", name, err, c.refused)
		case c.refused == "" && (err == nil || refusalReason(err) != "not a refusal: "+err.Error()):
			t.Fatalf("%s: %v, want a host failure", name, err)
		}
		if k.rows() != before {
			t.Fatalf("%s: rows = %+v", name, k.rows())
		}
	}
	k.forge.err = nil
	k.forge.by["owner/repo#7"] = good
	if res, err := k.release("rp", "J"); err != nil || !res.Bound {
		t.Fatalf("the release after the evidence became readable: %v %+v", err, res)
	}
}

// The forge is addressed by the owner/name the relay recorded at acceptance, never by the local path the edge targets.
func TestReleaseFreshnessUsesForgeIdentity(t *testing.T) {
	k := newReleaseKit(t)
	checkout := t.TempDir()
	pin := doc{"pins_code_head": true, "target_repository": checkout, "target_base_ref": "dev"}
	k.putPlan("lp", 0, "lp-r1", addRelNode("I", dag.NodeImplementation), addRelNode("J", dag.NodeImplementation), addEdge("ij", "I", "J", dag.EdgeArtifactVerified, pin))
	k.declare("lp", "I", "i.go")
	k.declare("lp", "J", "j.go")
	k.acceptNode("lp", "I", acceptOpts{HeadSHA: head1, PR: 9, Forge: "o/r", Repository: checkout})
	k.forge.by["o/r#9"] = openPR("o/r", 9, head1)
	req := k.request(true)
	req.Base = &BaseSpec{Repository: checkout, Ref: "dev"}
	res, err := k.sched.Release(context.Background(), "lp", "J", "parent", req)
	if err != nil || !res.Bound {
		t.Fatalf("release = %v %+v", err, res)
	}
	if strings.Join(k.forge.calls, ",") != "o/r#9" {
		t.Fatalf("the forge was read as %v, want only o/r#9", k.forge.calls)
	}
}

// Omission injection: removing the forge mapping of a pinned predecessor can never take it out of freshness checking: the node is blocked, not released unchecked.
func TestReleaseRefusesPinnedPredecessorWithoutForgeRow(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	a := k.pinned()
	k.exec("DELETE FROM dag_acceptance_forge WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
	before := k.rows()
	if n := k.read("rp").node("J"); n.Reason != BlockedAcceptanceIncomplete {
		t.Fatalf("J = %+v", n)
	}
	if _, err := k.release("rp", "J"); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("release = %v", err)
	}
	if created, _ := k.host.counts(); created != 0 || k.rows() != before {
		t.Fatalf("created %d rows %+v, want %+v", created, k.rows(), before)
	}
	if len(k.forge.calls) != 0 {
		t.Fatalf("the forge was read: %v", k.forge.calls)
	}
}

// The replay of an intent never rebuilds the request: the frozen bytes go to the engine whatever moved since (the clock, an unrelated plan revision, the base tip, the caller's
// words); a caller that spells the selectors differently is refused before the engine is asked.
func TestReleaseReplayDoesNotRebuild(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.pinned()
	k.host.loseFirstCreation = true
	res, err := k.release("rp", "J")
	if err != nil || res.Bound {
		t.Fatalf("first call = %v %+v", err, res)
	}
	var marker, socket, selector string
	if err := k.s.DB.QueryRow("SELECT marker_root, socket, state_selector FROM dag_release_requests").Scan(&marker, &socket, &selector); err != nil || marker != k.marker || selector != k.state {
		t.Fatalf("selectors persisted as %q %q %q (%v)", marker, socket, selector, err)
	}
	k.putPlan("rp", 1, "rp-r2", addRelNode("Z", dag.NodeNonPR)) // an unrelated revision
	k.tips.sha = "3333333333333333333333333333333333333333"     // the base moved
	k.tips.err = fmt.Errorf("the target is unreachable now")    // and cannot be read at all: a replay reads no tip
	changed := k.request(true)
	changed.Instructions = "Different instructions entirely."
	changed.RuleVersion.Model = "another-model"
	again, err := k.sched.Release(context.Background(), "rp", "J", "parent", changed)
	if err != nil || !again.Bound || again.ChildTaskID != "child-1" || !again.Replayed {
		t.Fatalf("replay = %v %+v", err, again)
	}
	_, sent := k.host.counts()
	message := k.host.sent[sent-1]
	if !strings.Contains(message, "Implement the issue") || strings.Contains(message, "Different instructions") || !strings.Contains(message, head1) || strings.Contains(message, "3333333333") {
		t.Fatalf("the business message was rebuilt rather than replayed:\n%s", message)
	}
	if created, _ := k.host.counts(); created != 1 {
		t.Fatalf("created %d threads", created)
	}

	// another spelling of the selectors: refused before the engine is asked
	k2 := newReleaseKit(t)
	releasePlan(k2.fixture, "rp")
	k2.pinned()
	k2.host.loseFirstCreation = true
	if res, err := k2.release("rp", "J"); err != nil || res.Bound {
		t.Fatalf("first call = %v %+v", err, res)
	}
	for name, mutate := range map[string]func(s *Selectors){
		"marker root": func(s *Selectors) { s.MarkerRoot += "/" }, "socket": func(s *Selectors) { s.Socket = "/other.sock" }, "state": func(s *Selectors) { s.StateSelector = k2.state + "/." },
	} {
		other := &Scheduler{Store: k2.s, Now: k2.clock}
		k2.wire(other)
		mutate(&other.Selectors)
		_, sentBefore := k2.host.counts()
		_, err := other.Release(context.Background(), "rp", "J", "parent", k2.request(true))
		if refusalReason(err) != "disposition_conflict" {
			t.Fatalf("%s: %v", name, err)
		}
		if _, sentAfter := k2.host.counts(); sentAfter != sentBefore {
			t.Fatalf("%s: the engine was asked", name)
		}
	}
}

// A stored manifest that no longer digests to its name stops a replay before the engine is asked.
func TestReleaseReplayRevalidatesStoredManifest(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.host.loseFirstCreation = true
	if res, err := k.release("rp", "A"); err != nil || res.Bound {
		t.Fatalf("first call = %v %+v", err, res)
	}
	k.exec("UPDATE dag_input_manifests SET body_json = replace(body_json, 'CRW-A', 'CRW-X')")
	_, err := k.release("rp", "A")
	if refusalReason(err) != "revision_mismatch" {
		t.Fatalf("replay over a tampered manifest = %v", err)
	}
	if created, sent := k.host.counts(); created != 1 || sent != 0 {
		t.Fatalf("created %d sent %d", created, sent)
	}
	// the frozen request is held to its digest as well
	k2 := newReleaseKit(t)
	releasePlan(k2.fixture, "rp")
	k2.host.loseFirstCreation = true
	if res, err := k2.release("rp", "A"); err != nil || res.Bound {
		t.Fatalf("first call = %v %+v", err, res)
	}
	k2.exec("UPDATE dag_release_requests SET request_json = replace(request_json, 'Implement', 'Destroy')")
	if _, err := k2.release("rp", "A"); refusalReason(err) != "revision_mismatch" {
		t.Fatalf("replay over a tampered request = %v", err)
	}
}

// A store failure while the manifest is written is never a success: nothing is released, nothing reserved, and the engine is not asked.
func TestManifestStoreFailureIsNotSuccess(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.exec("CREATE TRIGGER refuse_manifest BEFORE INSERT ON dag_input_manifests BEGIN SELECT RAISE(ABORT, 'the disk refuses'); END")
	_, err := k.release("rp", "A")
	if err == nil || refusalReason(err) == "" || !strings.Contains(err.Error(), "the disk refuses") {
		t.Fatalf("release = %v", err)
	}
	if k.rows() != (rowCounts{}) {
		t.Fatalf("rows = %+v", k.rows())
	}
	if created, _ := k.host.counts(); created != 0 {
		t.Fatal("a child was created")
	}
}

// Contract 4.4: every violated path blocks the release before anything is written and the engine is asked nothing, and the baseline of the same shape releases (so detection is not
// a constant refusal). Tamper and omission injections are detected in every case.
func TestBlockedPathsTable(t *testing.T) {
	rewriteReceipt := func(k *releaseKit, a accepted, from, to string) {
		k.exec("UPDATE events SET receipt = replace(receipt, ?, ?) WHERE event_id = ?", from, to, a.Event)
	}
	cases := []struct {
		name   string
		reason string // "" = releases
		mutate func(k *releaseKit, a accepted, req *ReleaseRequest)
	}{
		{"baseline", "", func(k *releaseKit, a accepted, req *ReleaseRequest) {}},
		{"B-04 the artifact changed (same size)", "manifest_unverified", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			os.WriteFile(a.Files[0], []byte("artifact of B\n"), 0o600) // as long as "artifact of A\n"
		}},
		{"B-04 the artifact grew", "manifest_unverified", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			os.WriteFile(a.Files[0], []byte("much longer than before\n"), 0o600)
		}},
		{"B-03 the artifact is gone", "manifest_unverified", func(k *releaseKit, a accepted, req *ReleaseRequest) { os.Remove(a.Files[0]) }},
		{"B-05 the receipt names a file outside the roots", "scope_escape", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			rewriteReceipt(k, a, a.Files[0], writeFile(t, t.TempDir(), "out.md", "x"))
		}},
		{"B-17 the receipt declares no artifact", "manifest_unverified", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			k.exec("UPDATE events SET receipt = '{\"manifest\":[]}' WHERE event_id = ?", a.Event)
		}},
		{"B-16 an empty field of the rule version", "malformed_receipt", func(k *releaseKit, a accepted, req *ReleaseRequest) { req.RuleVersion.Model = "" }},
		{"B-10 the criteria the child is registered with are not the plan's", "criteria_set_changed", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			req.Criteria = []Criterion{{ID: "c1", Title: "another criterion", Required: true}}
		}},
		{"B-07 the acceptance row was altered", "revision_mismatch", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			k.exec("UPDATE dag_acceptances SET revision_hash = ? WHERE acceptance_id = ?", dig("forged"), a.Acceptance.AcceptanceID)
		}},
		{"B-08 a required column of the acceptance is blank", "disposition_conflict", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			k.exec("UPDATE dag_acceptances SET ack_tier = '' WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
		}},
		{"B-06 the acceptance is not tied to an execution", "disposition_conflict", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			k.exec("DELETE FROM dag_node_executions")
		}},
		{"a volatile snapshot that is there and right releases", "", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			p := writeFile(t, k.root, "snapshot.md", "what was captured")
			req.Volatile = []Volatile{{Source: "linear:doc", SnapshotURI: p, SHA256: shaOf([]byte("what was captured")), CapturedAt: "2026-10-02T00:00:00Z"}}
		}},
		{"volatile snapshot is missing", "manifest_unverified", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			req.Volatile = []Volatile{{Source: "linear:doc", SnapshotURI: filepath.Join(k.root, "missing.md"), SHA256: dig("x"), CapturedAt: "2026-10-02T00:00:00Z"}}
		}},
		{"volatile snapshot was altered", "manifest_unverified", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			p := writeFile(t, k.root, "doc.md", "what was captured")
			req.Volatile = []Volatile{{Source: "linear:doc", SnapshotURI: p, SHA256: dig("not these bytes"), CapturedAt: "2026-10-02T00:00:00Z"}}
		}},
		{"volatile snapshot lies outside the child's roots", "scope_escape", func(k *releaseKit, a accepted, req *ReleaseRequest) {
			p := writeFile(t, t.TempDir(), "doc.md", "elsewhere")
			req.Volatile = []Volatile{{Source: "linear:doc", SnapshotURI: p, SHA256: shaOf([]byte("elsewhere")), CapturedAt: "2026-10-02T00:00:00Z"}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			a := k.acceptedA()
			req := k.request(false)
			c.mutate(k, a, &req)
			before := k.rows()
			res, err := k.sched.Release(context.Background(), "rp", "B", "parent", req)
			if c.reason == "" {
				if err != nil || !res.Bound {
					t.Fatalf("the baseline did not release: %v %+v", err, res)
				}
				return
			}
			if refusalReason(err) != c.reason {
				t.Fatalf("release = %v, want %s", err, c.reason)
			}
			if created, _ := k.host.counts(); created != 0 {
				t.Fatal("the engine was asked")
			}
			if rows := k.rows(); rows != before {
				t.Fatalf("rows = %+v, want the %+v the setup left", rows, before)
			}
		})
	}
}

// A manifest too large for a prompt is frozen to a file named by its digest under the child's first root, and the prompt names the path and the digest of what was written.
func TestReleaseFreezesALargeManifest(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.acceptNode("rp", "A", acceptOpts{Artifacts: 320})
	res := k.mustRelease("rp", "B")
	frozen := filepath.Join(k.root, "dag-input-manifests", res.ManifestDigest+".json")
	raw, err := os.ReadFile(frozen)
	if err != nil {
		t.Fatalf("no frozen copy: %v", err)
	}
	info, _ := os.Stat(frozen)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the frozen copy is %v", info.Mode().Perm())
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body["manifest_digest"] != res.ManifestDigest {
		t.Fatalf("the frozen copy is not the manifest: %v", err)
	}
	message := k.host.sent[0]
	if !strings.Contains(message, frozen) || !strings.Contains(message, shaOf(raw)) || strings.Contains(message, "generated-artifact-100.md") {
		t.Fatal("the prompt does not point at the frozen copy by path and digest, or still carries the whole manifest")
	}
	if len(message) > 90000 {
		t.Fatalf("message of %d characters", len(message))
	}
}

// H4 (audit): the acceptance whose pull request the relay read is the acceptance the manifest consumes. A predecessor accepted again while the forge is being read is a head that was
// never checked.
func TestReleaseFreshnessBindsTheAcceptanceItChecked(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.pinned()
	second := "2222222222222222222222222222222222222222"
	k.forge.by["owner/repo#8"] = openPR("owner/repo", 8, second)
	k.forge.onRead = func() {
		k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE node_id = 'I'")
		k.acceptNode("rp", "I", acceptOpts{Suffix: "-2", HeadSHA: second, PR: 8, Forge: "owner/repo", Repository: "owner/repo"})
	}
	_, err := k.release("rp", "J")
	if refusalReason(err) != "disposition_conflict" {
		t.Fatalf("release = %v", err)
	}
	if rows := k.rows(); rows.releases != 0 || rows.requests != 0 || rows.slots != 0 || rows.conflicts != 0 {
		t.Fatalf("rows = %+v, want no intent", rows)
	}
	if created, _ := k.host.counts(); created != 0 {
		t.Fatal("a child was created on an unchecked head")
	}
	// repeated, the current acceptance is read and checked
	res, err := k.release("rp", "J")
	if err != nil || !res.Bound {
		t.Fatalf("the repeat = %v %+v", err, res)
	}
	if strings.Join(k.forge.calls, ",") != "owner/repo#7,owner/repo#8" {
		t.Fatalf("the forge was read as %v", k.forge.calls)
	}
}

// R2-H2 (audit): a pinned predecessor whose acceptance is out of sight while the pull requests are collected cannot drop out of the freshness check by reappearing before the manifest is
// built: every pinned edge of the manifest must rest on a predecessor whose pull request the relay read.
func TestReleaseFreshnessCannotOmitAPinnedPredecessor(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	a := k.pinned()
	k.forge.err = fmt.Errorf("the forge is down: nothing may be read")
	k.sched.testAfterReading = func() {
		k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
	}
	k.sched.testAfterFreshness = func() {
		k.exec("UPDATE dag_acceptances SET state = 'active' WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
	}
	_, err := k.release("rp", "J")
	if refusalReason(err) != "disposition_conflict" {
		t.Fatalf("release = %v", err)
	}
	if rows := k.rows(); rows.releases != 0 || rows.slots != 0 {
		t.Fatalf("rows = %+v", rows)
	}
	if created, _ := k.host.counts(); created != 0 {
		t.Fatal("a child was created on a head the relay never checked")
	}
}
