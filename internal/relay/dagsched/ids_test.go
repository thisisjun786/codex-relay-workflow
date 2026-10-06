package dagsched

import (
	"strings"
	"testing"
)

// The ids and digests the scheduler stores are pinned against literals computed outside this code: the pre-image is written out here and hashed with the
// formula the contract names (sha256 of the text, lower-case hex, the first 32 or 40 characters). A guard that is green on the baseline: nothing that folds
// the copies of these formulas may move a byte of them.
func TestExistingIDsAndDigestsKeepTheirBytes(t *testing.T) {
	t.Parallel()
	a40, c40 := strings.Repeat("a", 40), strings.Repeat("c", 40)
	manifest := dig("manifest")
	rows := []struct{ name, got, want string }{
		{"release request id", ReleaseRequestID("plan-1", "node-a", manifest), "dag-fd3558c735eb7e8c6049a420d0d74c6c22fc702b"},
		{"recovery request id", RecoveryRequestID("plan-1", "node-a", manifest, "dag-closed"), "dag-154a658e38093af8fd15ec94b53fe00ddf1f2823"},
		{"base refresh id", refreshDigest("acc-1", "rel-1", 2, "evt-1", "rev-1", a40, "owner/repo", "dev", c40, `{"steps":[]}`, `["x.json"]`), "dbr-b21b64ba4154713d2272327c9a33175d"},
		{"landing set ref", landingRef([]string{"dio-b", "dio-a"}), "dio-set-72b0da20e8ba50756a9ef6770694d69e"},
		{"landing ref of one observation", landingRef([]string{"dio-a"}), "dio-a"},
		{"landing ref of none", landingRef(nil), ""},
	}
	for _, row := range rows {
		if row.got != row.want {
			t.Errorf("%s = %s, want %s", row.name, row.got, row.want)
		}
	}
}

// The ids the transactions write, one constructor each, pinned against pre-images hashed outside this code (sha256 of the fields joined by "|", or of their canonical text for the
// two that hash an object). The head, tip and base values differ from each other, so a call that passes them in another order is not the id pinned here.
func TestScheduledIDsKeepTheirBytes(t *testing.T) {
	t.Parallel()
	a40, b40, c40, d40 := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	rows := []struct{ name, got, want string }{
		{"merge check id", mergeCheckID("acc-1", 2), "dmc-d1c106c2a1aa2e0594c66f3d4fb28703"},
		{"re-validation id", revalidationID("acc-1", 3), "drv-18a4aac08500294bbe64fe621bb243e5"},
		{"integration observation id", integrationObservationID("acc-1", "owner/repo", "dev", 4), "dio-fa8298d1efa1d72d7f0264bbf98a9b13"},
		{"pair observation id", pairObservationID("plan-1", "node-a", "node-b", a40, b40, c40), "dco-4ef6c4be3d2b3c0127302e09de466322"},
		{"tip observation id", tipObservationID("plan-1", "node-a", a40, d40, c40), "dto-90380824165464e762fecac1232d603d"},
		{"decision id", decisionID("plan-1", "merge holds", dig("subject"), "approved", "user", "u-1", 3), "dec-b26dc5de3179df7ba05dd812dfef27df"},
		{"summary id", summaryID("plan-1", "doc-1", 5, dig("subject"), 2), "sum-8493e879c1a6f96912dd7abeb421ad9b"},
	}
	for _, row := range rows {
		if row.got != row.want {
			t.Errorf("%s = %s, want %s", row.name, row.got, row.want)
		}
	}
}

// The evidence body is the text a merge check stores and its digest the acceptance's evidence digest: a check of another head is left out, the checks and the required names
// are sorted, and an absent provider or stamp is absent from the row.
func TestEvidenceBodyKeepsItsBytes(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	pr := PullRequest{HeadSHA: head, RequiredDeclared: []string{"test", "lint"}, ReviewDigest: "rd", Checks: []Check{
		{Name: "test", RunID: "2", HeadSHA: head, Conclusion: "failure", Attempt: 2},
		{Name: "lint", RunID: "1", HeadSHA: head, Conclusion: "success", Attempt: 1, Provider: "gh", Stamp: "s1"},
		{Name: "old", RunID: "0", HeadSHA: strings.Repeat("b", 40), Conclusion: "success", Attempt: 1},
	}}
	const wantJSON = `{"checks":[{"attempt":1,"conclusion":"success","head_sha":"` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `","name":"lint","provider":"gh","run_id":"1","stamp":"s1"},` +
		`{"attempt":2,"conclusion":"failure","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"test","run_id":"2"}],"required":["lint","test"],"review_digest":"rd"}`
	const wantDigest = "b6be6de44442b8472ac7bed829fe3965f36c981de208c0db4825116dfa24c4f9"
	body := EvidenceBodyOf(pr)
	if got := body.JSON(); got != wantJSON {
		t.Errorf("evidence json = %s, want %s", got, wantJSON)
	}
	if got := EvidenceDigest(body); got != wantDigest {
		t.Errorf("evidence digest = %s, want %s", got, wantDigest)
	}
	if got, err := RecomputeEvidenceDigest(wantJSON); err != nil || got != wantDigest {
		t.Errorf("recomputed digest = %s (%v), want %s", got, err, wantDigest)
	}
}
