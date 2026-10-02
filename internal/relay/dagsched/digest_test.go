package dagsched

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func baseAcceptance() Acceptance {
	return Acceptance{PlanID: "p", NodeID: "n", ManifestDigest: dig("m"), RelationshipID: "rel-1", ExecutionGeneration: 1, EventID: "evt", RevisionHash: dig("r"),
		CriteriaSetDigest: dig("c"), Verdict: "verified", HeadSHA: strings.Repeat("a", 40), Repository: "o/r", PRNumber: 7,
		EvidenceDigest: dig("e"), AckTier: "host_read", VerdictTurnID: "vt", RuleVersionJSON: "{}", AcceptedByTask: "parent", AcceptedAt: "t", State: "active"}
}

// Contract 4.3: the identity is the digest of the included fields; everything else is recorded and moves nothing.
func TestAcceptanceDigestSensitivity(t *testing.T) {
	base := AcceptanceDigest(baseAcceptance())
	included := map[string]func(*Acceptance){
		"plan_id":              func(a *Acceptance) { a.PlanID = "q" },
		"node_id":              func(a *Acceptance) { a.NodeID = "m" },
		"manifest_digest":      func(a *Acceptance) { a.ManifestDigest = dig("m2") },
		"relationship_id":      func(a *Acceptance) { a.RelationshipID = "rel-2" },
		"execution_generation": func(a *Acceptance) { a.ExecutionGeneration = 2 },
		"event_id":             func(a *Acceptance) { a.EventID = "evt2" },
		"revision_hash":        func(a *Acceptance) { a.RevisionHash = dig("r2") },
		"criteria_set_digest":  func(a *Acceptance) { a.CriteriaSetDigest = dig("c2") },
		"head_sha":             func(a *Acceptance) { a.HeadSHA = strings.Repeat("b", 40) },
		"repository":           func(a *Acceptance) { a.Repository = "o/other" },
		"pr_number":            func(a *Acceptance) { a.PRNumber = 8 },
	}
	for name, change := range included {
		a := baseAcceptance()
		change(&a)
		if AcceptanceDigest(a) == base {
			t.Errorf("changing %s did not change the acceptance digest", name)
		}
	}
	excluded := map[string]func(*Acceptance){
		"evidence_digest": func(a *Acceptance) { a.EvidenceDigest = dig("e2") },
		"ack_tier":        func(a *Acceptance) { a.AckTier = "other" },
		"verdict_turn_id": func(a *Acceptance) { a.VerdictTurnID = "vt2" },
		"rule_version":    func(a *Acceptance) { a.RuleVersionJSON = "{\"model\":\"x\"}" },
		"accepted_by":     func(a *Acceptance) { a.AcceptedByTask = "someone" },
		"epoch":           func(a *Acceptance) { a.CoordinatorEpoch = 3 },
		"accepted_at":     func(a *Acceptance) { a.AcceptedAt = "later" },
		"supersedes":      func(a *Acceptance) { a.SupersedesAcceptanceID = "old" },
		"state":           func(a *Acceptance) { a.State = "superseded" },
		"output_manifest": func(a *Acceptance) { a.OutputManifestRef = "/frozen" },
	}
	for name, change := range excluded {
		a := baseAcceptance()
		change(&a)
		if AcceptanceDigest(a) != base {
			t.Errorf("changing %s changed the acceptance digest, but contract 4.3 leaves it out", name)
		}
	}
	// A non_pr acceptance has no head, repository or pull request: absent is absent, whatever spells it.
	bare := baseAcceptance()
	bare.HeadSHA, bare.Repository, bare.PRNumber = "", "", 0
	if AcceptanceDigest(bare) == base || len(AcceptanceDigest(bare)) != 64 {
		t.Error("an acceptance without a code head digests like one with it")
	}
}

// The acceptance's evidence_digest and a merge check's checks_digest are one serialization, and the body stored beside the digest recomputes it.
func TestEvidenceDigestOneSerialization(t *testing.T) {
	pr := PullRequest{HeadSHA: "h1", RequiredDeclared: []string{"test", "lint"}, ReviewDigest: dig("review"), Checks: []Check{
		{RunID: "2", Name: "test", HeadSHA: "h1", Conclusion: "success", Attempt: 1},
		{RunID: "1", Name: "lint", HeadSHA: "h1", Conclusion: "success", Attempt: 1},
		{RunID: "9", Name: "old", HeadSHA: "h0", Conclusion: "failure", Attempt: 1}, // an earlier push: not evidence about this head
	}}
	body := EvidenceBodyOf(pr)
	if len(body.Checks) != 2 {
		t.Fatalf("the body kept %d checks, want the two of the current head: %+v", len(body.Checks), body.Checks)
	}
	digest := EvidenceDigest(body)
	reordered := pr
	reordered.RequiredDeclared = []string{"lint", "test"}
	reordered.Checks = []Check{pr.Checks[1], pr.Checks[2], pr.Checks[0]}
	if EvidenceDigest(EvidenceBodyOf(reordered)) != digest {
		t.Error("the digest depends on the order the checks and the required names were listed in")
	}
	recomputed, err := RecomputeEvidenceDigest(body.JSON())
	if err != nil || recomputed != digest {
		t.Errorf("the stored body recomputes to %q (%v), want %q", recomputed, err, digest)
	}
	altered := strings.Replace(body.JSON(), "success", "failure", 1)
	if got, _ := RecomputeEvidenceDigest(altered); got == digest {
		t.Error("an altered body recomputed to the recorded digest")
	}
	if _, err := RecomputeEvidenceDigest("not json"); err == nil {
		t.Error("a body that is not JSON recomputed")
	}
}

func TestReleaseRequestIDShape(t *testing.T) {
	node := strings.Repeat("n", 128)
	id := ReleaseRequestID("plan", node, dig("m"))
	if len(id) != 44 || !strings.HasPrefix(id, "dag-") {
		t.Fatalf("request id %q has length %d, want 44 with the dag- prefix", id, len(id))
	}
	if id != ReleaseRequestID("plan", node, dig("m")) {
		t.Error("the request id is not deterministic")
	}
	if id == ReleaseRequestID("plan", node, dig("m2")) || id == ReleaseRequestID("plan", "other", dig("m")) {
		t.Error("a different manifest or node produced the same request id")
	}
	// request ids are global to the store and node ids are plan-local: the same node over the same inputs in another plan is another release
	if id == ReleaseRequestID("another plan", node, dig("m")) {
		t.Error("two plans produced the same request id for a node of the same name")
	}
	// the parts are not joined into one text: a plan and a node that would join to the same text are different
	if ReleaseRequestID("a|b", "c", dig("m")) == ReleaseRequestID("a", "b|c", dig("m")) {
		t.Error("the plan and the node were joined")
	}
}

func TestReasonsClosed(t *testing.T) {
	for _, r := range EmittedReasons() {
		if !ReasonsClosed(r) {
			t.Errorf("%s is emitted and not in the closed set", r)
		}
	}
	for _, r := range append(ReservedReasons, "", "wait:edge:", "wait:edge", "defer:other", "blocked:", "ready", "skip:already_owned ") {
		if ReasonsClosed(r) {
			t.Errorf("%q is accepted as a closed reason", r)
		}
	}
	if !ReasonsClosed(WaitEdge("e1")) {
		t.Error("wait:edge:e1 is not closed")
	}
	if len(BlockedPaths) != 17 {
		t.Fatalf("contract 4.4 has 17 paths, the table has %d", len(BlockedPaths))
	}
	for i, p := range BlockedPaths {
		if p.Code != "B-"+pad(i+1) || !ReasonsClosed(p.Reason) || p.Refusal == "" {
			t.Errorf("row %d is %+v", i, p)
		}
	}
}

func pad(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// The identity is pinned against a literal canonical text written here, not against the code that builds it: sorted keys, no spaces, the
// included fields only (contract 4.3 plus the plan id, which this build adds because node ids are plan-local: docs/relay/dag-scheduler.md).
func TestAcceptanceDigestIsPinned(t *testing.T) {
	canonical := `{"criteria_set_digest":"` + dig("c") + `","event_id":"evt","execution_generation":1,"head_sha":"` + strings.Repeat("a", 40) +
		`","manifest_digest":"` + dig("m") + `","node_id":"n","plan_id":"p","pr_number":7,"relationship_id":"rel-1","repository":"o/r","revision_hash":"` + dig("r") +
		`","schema":"dag-acceptance/1","verdict":"verified"}`
	sum := sha256.Sum256([]byte(canonical))
	if got, want := AcceptanceDigest(baseAcceptance()), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("acceptance digest %s, want %s over %s", got, want, canonical)
	}
	bare := baseAcceptance()
	bare.HeadSHA, bare.Repository, bare.PRNumber = "", "", 0
	canonical = `{"criteria_set_digest":"` + dig("c") + `","event_id":"evt","execution_generation":1,"manifest_digest":"` + dig("m") +
		`","node_id":"n","plan_id":"p","relationship_id":"rel-1","revision_hash":"` + dig("r") + `","schema":"dag-acceptance/1","verdict":"verified"}`
	sum = sha256.Sum256([]byte(canonical))
	if got, want := AcceptanceDigest(bare), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("a non_pr acceptance digests to %s, want %s over %s", got, want, canonical)
	}
}
