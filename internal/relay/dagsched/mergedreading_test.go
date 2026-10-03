package dagsched

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// CRW-448. A merged pull request is the one forge reading whose verdict is unknown by construction: nothing is left to hand over, so merge-evidence says candidate_not_open, and the forge
// gives no merge state for it, so it says candidate_unknown. ClassifyPullRequest is the one place that decides such a reading is readable, and the readers that take a pull request from the
// forge (release's freshness check, dag-accept, dag-merge-judge, dag-base-refresh) then decide what a merged state means to them. These tests keep the two apart.

// mergedGH is the forge answering for a pull request that merged: GitHub reports it closed and merged and has no mergeability to give for it.
func mergedGH() *ghScript {
	g := newGHScript()
	p := g.pull(forgeHead)
	p["state"], p["merged"], p["mergeable"], p["mergeable_state"] = "closed", true, nil, "unknown"
	g.pulls = []map[string]any{p}
	return g
}

// asCollected is a pull request the way the collector reads it once it merged: the state merged and the verdict unknown, with the two problems and nothing else.
func asCollected(pr PullRequest) PullRequest {
	pr.State, pr.Verdict = "merged", evidence.UnknownVerdict
	pr.Problems = []Problem{{Code: evidence.CandidateNotOpen, Detail: "this pull request is already merged, so there is nothing left to hand over"},
		{Code: evidence.CandidateUnknown, Detail: "the forge reports merge state \"UNKNOWN\""}}
	return pr
}

func problemCodes(pr PullRequest) []string {
	var codes []string
	for _, p := range pr.Problems {
		codes = append(codes, p.Code)
	}
	slices.Sort(codes)
	return codes
}

func TestClassifyPullRequestReadsAMergedPullRequest(t *testing.T) {
	t.Run("the reading the collector gives for a merged pull request is readable", func(t *testing.T) {
		pr := mergedGH().read(t)
		// the premise this rule rests on, from the real collector over a scripted forge: unknown by construction, with exactly the two problems
		if pr.State != "merged" || pr.Verdict != evidence.UnknownVerdict || !slices.Equal(problemCodes(pr), []string{evidence.CandidateNotOpen, evidence.CandidateUnknown}) {
			t.Fatalf("the collector's reading of a merged pull request = state %s, verdict %s, problems %v", pr.State, pr.Verdict, problemCodes(pr))
		}
		if err := ClassifyPullRequest(pr); err != nil {
			t.Fatalf("a merged pull request whose only problems are the merged ones was refused: %v", err)
		}
	})
	t.Run("a merged pull request whose evidence was left incomplete is still the host's failure", func(t *testing.T) {
		g := mergedGH()
		g.checkTotal = 5
		pr := g.read(t)
		if pr.State != "merged" || pr.Verdict != evidence.UnknownVerdict || !slices.Contains(problemCodes(pr), evidence.EnumerationCountDisagrees) {
			t.Fatalf("the collector's reading = state %s, verdict %s, problems %v", pr.State, pr.Verdict, problemCodes(pr))
		}
		if err := ClassifyPullRequest(pr); err == nil || !strings.HasPrefix(refusalReason(err), "not a refusal") || !strings.Contains(err.Error(), "could not be read completely") {
			t.Fatalf("classification = %v, want the host's failure", err)
		}
	})
	// A merged reading is classified as an open one is, with the two problems merging explains set aside. Rows marked synthetic carry a verdict and problems the collector cannot give together
	// (VerdictOf derives the verdict from the problems); they pin the boundary of the rule and keep a mutation of it from surviving.
	merged := []string{evidence.CandidateNotOpen, evidence.CandidateUnknown}
	t.Run("a merged pull request whose rules cannot be read is still the host's failure", func(t *testing.T) {
		g := mergedGH()
		g.rulesOK = false
		pr := g.read(t)
		if pr.State != "merged" || pr.Verdict != evidence.UnknownVerdict || !slices.Contains(problemCodes(pr), evidence.UnreadableCode) {
			t.Fatalf("the collector's reading = state %s, verdict %s, problems %v", pr.State, pr.Verdict, problemCodes(pr))
		}
		if err := ClassifyPullRequest(pr); err == nil || !strings.HasPrefix(refusalReason(err), "not a refusal") {
			t.Fatalf("classification = %v, want the host's failure", err)
		}
	})
	rows := []struct {
		name    string
		state   string
		verdict string
		codes   []string
		want    string // "" readable, "host" the host's failure, else a refusal reason
	}{
		{"merged, unknown, the two merged problems", "merged", "unknown", merged, ""},
		{"merged, unknown, the unknown merge state alone (synthetic)", "merged", "unknown", []string{evidence.CandidateUnknown}, ""},
		{"merged, unknown, the merged problem alone (synthetic)", "merged", "unknown", []string{evidence.CandidateNotOpen}, ""},
		{"merged, unknown, a judgement problem besides the merged ones", "merged", "unknown", append([]string{evidence.ReviewIncomplete}, merged...), ""},
		{"merged, unknown, a read failure besides the merged ones", "merged", "unknown", append([]string{evidence.EnumerationTruncated}, merged...), "host"},
		{"merged, unknown, a read failure and no merged problem", "merged", "unknown", []string{evidence.ReviewSetUnstable}, "host"},
		{"merged, unknown, an unreadable code besides the merged ones", "merged", "unknown", append([]string{evidence.UnreadableCode}, merged...), "host"},
		{"merged, unknown, an unstable review set besides the merged ones", "merged", "unknown", append([]string{evidence.ReviewSetUnstable}, merged...), "host"},
		{"merged, unknown, no problem at all (synthetic)", "merged", "unknown", nil, "host"},
		{"merged, stale, the candidate moved while it was read", "merged", "stale", []string{evidence.CandidateNotOpen, evidence.CandidateMoved}, "merge_candidate_moved"},
		{"merged, stale, the gates moved while they were read", "merged", "stale", []string{evidence.CandidateNotOpen, evidence.GatesMoved}, "merge_evidence_malformed"},
		{"merged, stale, the base branch is missing", "merged", "stale", []string{evidence.CandidateNotOpen, evidence.BaseRefMissing}, "merge_evidence_malformed"},
		{"merged, stale, only the merged problems (synthetic)", "merged", "stale", merged, "host"},
		{"merged, an unrecognised verdict, only the merged problems (synthetic)", "merged", "mostly fine", merged, "host"},
		{"merged, no verdict, only the merged problems (synthetic)", "merged", "", merged, "host"},
		{"merged, ready (synthetic)", "merged", "ready", nil, ""},
		{"merged, not ready", "merged", "not_ready", []string{evidence.CandidateNotOpen}, ""},
		{"open, unknown, the same two problems", "open", "unknown", merged, "host"},
		{"closed, unknown, the same two problems", "closed", "unknown", merged, "host"},
		{"no state read, unknown, the same two problems", "", "unknown", merged, "host"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pr := PullRequest{Repository: "owner/repo", Number: 7, State: row.state, Verdict: row.verdict}
			for _, code := range row.codes {
				pr.Problems = append(pr.Problems, Problem{Code: code, Detail: "a detail"})
			}
			err := ClassifyPullRequest(pr)
			got := refusalReason(err)
			switch row.want {
			case "":
				if err != nil {
					t.Fatalf("classification = %v, want it readable", err)
				}
			case "host":
				if err == nil || !strings.HasPrefix(got, "not a refusal") {
					t.Fatalf("classification = %v, want the host's failure", err)
				}
			default:
				if got != row.want {
					t.Fatalf("classification = %v, want %s", err, row.want)
				}
			}
		})
	}
}

// The classification passes a merged reading and the reader decides what a merged state means to it. Each case below is the reading the collector gives, not a reading with the verdict ready:
// the readers used to be tested only with a merged state the collector can never produce.
func TestEveryReaderDecidesAboutAMergedPullRequestFromItsState(t *testing.T) {
	t.Run("release compares the head of a pinned predecessor that merged", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.pinned()
		k.forge.by["owner/repo#7"] = asCollected(openPR("owner/repo", 7, head1))
		if res, err := k.release("rp", "J"); err != nil || !res.Bound {
			t.Fatalf("release of J over a predecessor merged at its accepted head = %v %+v", err, res)
		}
		if k.rows().merges != 0 {
			t.Fatal("an unchanged head was recorded as stale")
		}
	})
	t.Run("release refuses a predecessor that merged at another head", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.pinned()
		k.forge.by["owner/repo#7"] = asCollected(openPR("owner/repo", 7, "2222222222222222222222222222222222222222"))
		if _, err := k.release("rp", "J"); refusalReason(err) != "merge_candidate_moved" {
			t.Fatalf("release of J over a predecessor merged at another head = %v", err)
		}
		if rows := k.rows(); rows.releases != 0 || rows.merges != 1 {
			t.Fatalf("rows = %+v, want one stale_head observation and no release", rows)
		}
	})
	acceptKit := func(t *testing.T) *releaseKit {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.declare("rp", "I", "i.go")
		k.reportNode("rp", "I", acceptOpts{})
		return k
	}
	named := AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}}
	t.Run("accept replays an output whose pull request merged at the accepted head", func(t *testing.T) {
		k := acceptKit(t)
		k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
		if _, err := k.accept("rp", "I", named); err != nil {
			t.Fatal(err)
		}
		k.forge.by["owner/repo#7"] = asCollected(openPR("owner/repo", 7, head1))
		if res, err := k.accept("rp", "I", named); err != nil || !res.Replayed || res.HeadSHA != head1 {
			t.Fatalf("replay after the merge = %v %+v", err, res)
		}
	})
	t.Run("accept takes no new output from a pull request that merged", func(t *testing.T) {
		k := acceptKit(t)
		k.forge.by["owner/repo#7"] = asCollected(openPR("owner/repo", 7, head1))
		if _, err := k.accept("rp", "I", named); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("accept of a merged pull request = %v, want disposition_conflict", err)
		}
		if k.acceptCount() != 0 {
			t.Fatal("an acceptance was recorded from a merged pull request")
		}
	})
	t.Run("the merge lane judges nothing on a pull request that merged", func(t *testing.T) {
		k := newJudgeKit(t)
		k.forge.by["owner/repo#5"] = asCollected(k.pr)
		_, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{})
		if refusalReason(err) != "disposition_conflict" {
			t.Fatalf("judge of a merged pull request = %v, want disposition_conflict", err)
		}
		if k.count("SELECT COUNT(*) FROM dag_merge_checks") != 0 {
			t.Fatal("a merged pull request wrote a judgement")
		}
	})
}

// dag-base-refresh read a merged pull request by its own rule before the classification was one place (readable when the two merged problems were its only problems, whatever the verdict). It
// now classifies as every other reader does. These are the readings the collector can give on which the two rules differ, and the reading they agree on; the readings it cannot give (a stale,
// absent or unrecognised verdict that carries only the two merged problems) are rows of TestClassifyPullRequestReadsAMergedPullRequest, and are the host's failure there.
func TestBaseRefreshClassifiesAMergedPullRequestAsEveryReaderDoes(t *testing.T) {
	merged := []Problem{{Code: evidence.CandidateNotOpen}, {Code: evidence.CandidateUnknown}}
	cases := []struct {
		name     string
		verdict  string
		problems []Problem
		want     string // "" the refresh is recorded, "host" the host's failure, else a refusal reason
	}{
		{"the two merged problems", "unknown", merged, ""},
		{"a judgement problem besides them: not a read failure, as for an open pull request", "unknown", append([]Problem{{Code: evidence.ReviewIncomplete, Detail: "an open thread"}}, merged...), ""},
		{"a read failure besides them", "unknown", append([]Problem{{Code: evidence.EnumerationTruncated, Detail: "the review list was cut"}}, merged...), "host"},
		{"the candidate moved while it was read: a refusal, as for an open pull request", "stale", append([]Problem{{Code: evidence.CandidateMoved}}, merged...), "merge_candidate_moved"},
		{"the gates moved while they were read: a refusal, as for an open pull request", "stale", append([]Problem{{Code: evidence.GatesMoved}}, merged...), "merge_evidence_malformed"},
		{"the base branch is missing: a refusal, as for an open pull request", "stale", append([]Problem{{Code: evidence.BaseRefMissing}}, merged...), "merge_evidence_malformed"},
		{"an unknown verdict that names no problem (the collector cannot produce it)", "unknown", nil, "host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newRefreshScenario(t)
			s.openGeneration()
			s.refreshBase()
			pr := openPR("owner/repo", 7, s.head)
			pr.State, pr.Verdict, pr.Problems = "merged", c.verdict, c.problems
			s.forge.by["owner/repo#7"] = pr
			_, err := s.record("shared.json")
			switch c.want {
			case "":
				if err != nil || s.refreshRows() != 1 {
					t.Fatalf("record = %v (rows %d), want the refresh recorded", err, s.refreshRows())
				}
			case "host":
				if err == nil || !strings.HasPrefix(refusalReason(err), "not a refusal") || s.refreshRows() != 0 {
					t.Fatalf("record = %v (rows %d), want the host's failure", err, s.refreshRows())
				}
			default:
				if refusalReason(err) != c.want || s.refreshRows() != 0 {
					t.Fatalf("record = %v (rows %d), want %s", err, s.refreshRows(), c.want)
				}
			}
		})
	}
}
