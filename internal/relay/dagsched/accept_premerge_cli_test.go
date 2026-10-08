package dagsched

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// CRW-952 c2 and c5 through the built binary: dag-accept on the commit path, with a temporary store and a real repository,
// and no parent script in the path. Each refusal name, the clean accept, the pure replay and the after-evaluation and
// merge-chain rules take the argv a parent would type.

// premergeCLIRun closes the kit's store (the binary is the only user from here) and runs dag-accept of node I on the
// commit path with the record given. A nil record names none.
func premergeCLIRun(t *testing.T, k *commitAcceptKit, record []byte) (map[string]any, int) {
	t.Helper()
	state := filepath.Dir(k.fixture.path)
	_ = k.fixture.s.Close()
	rule := fmt.Sprintf(`{"skills_digest":%q,"model":"m","effort":"none"}`, dig("skills"))
	args := []string{"dag-accept", "--plan", "g", "--node", "I", "--actor", "parent", "--commit", k.head, "--base", k.base,
		"--checkout", k.repo.path, "--verification", k.record(k.treeOf(k.head), "pass", nil), "--rule-version", rule}
	if record != nil {
		args = append(args, "--premerge", string(record))
	}
	out, code := crw(t, state, args...)
	return parseOut(t, out), code
}

// premergeCLIExpect asserts the binary's answer: an accept (exit 0, ok true) or a refusal with its name (exit 2).
func premergeCLIExpect(t *testing.T, m map[string]any, code int, want string) {
	t.Helper()
	if want == "" {
		if code != 0 || m["ok"] != true {
			t.Fatalf("a passing record must accept: exit %d %v", code, m)
		}
		return
	}
	if code != 2 || m["reason"] != want {
		t.Fatalf("want exit 2 with reason %s, got exit %d %v", want, code, m)
	}
}

// TestPremergeCLINamesEachRefusal drives the nine refusal names that a record can cause, plus the clean accept, through
// the binary. The tenth name, premerge_after_evaluation_missing, is driven by TestPremergeCLIAfterEvaluationAndMergeChains.
func TestPremergeCLINamesEachRefusal(t *testing.T) {
	cases := []struct {
		name   string
		none   bool
		mutate func(*premerge.Record)
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
			m, code := premergeCLIRun(t, k, raw)
			premergeCLIExpect(t, m, code, c.want)
		})
	}
}

// TestPremergeCLIReplayNeedsNoRecord: the clean accept stores the record; the same output again is a replay that names
// no record and is answered replayed true.
func TestPremergeCLIReplayNeedsNoRecord(t *testing.T) {
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	m, code := premergeCLIRun(t, k, premergeRaw(t, premergeRecordOf(t, k)))
	premergeCLIExpect(t, m, code, "")
	if m["acceptance_id"] == nil || m["acceptance_id"] == "" {
		t.Fatalf("the clean accept must name its acceptance: %v", m)
	}
	replay, code := premergeCLIRun(t, k, nil)
	premergeCLIExpect(t, replay, code, "")
	if replay["replayed"] != true {
		t.Fatalf("the same output without a record must replay: %v", replay)
	}
}

// premergeCLIShape builds the kit for one head shape and returns it with the evaluated head the record names:
// plain (one commit after the evaluation), merge1 and merge4 (a chain of dev-only merges), merge5 (five merges),
// hand (a merge hand-resolved with an extra file) and outside (a merge of a commit that is not in dev).
func premergeCLIShape(t *testing.T, shape string) (*commitAcceptKit, string) {
	t.Helper()
	k := newCommitAcceptKit(t)
	evaluated := k.head
	merges := map[string]int{"merge1": 1, "merge4": 4, "merge5": 5, "hand": 1}[shape]
	var devs []string
	for i := 0; i < merges; i++ {
		k.repo.git("checkout", "-q", "dev")
		devs = append(devs, k.repo.commit(fmt.Sprintf("dev%d.txt", i), "dev"))
	}
	k.repo.git("checkout", "-q", "dev")
	k.base = k.repo.git("rev-parse", "dev")
	k.repo.git("checkout", "-q", "feature")
	switch shape {
	case "plain":
		k.head = k.repo.commit("later.txt", "later")
	case "outside":
		k.repo.git("checkout", "-q", "-b", "side", evaluated)
		side := k.repo.commit("side.txt", "side")
		k.repo.git("checkout", "-q", "feature")
		k.repo.git("merge", "--no-ff", "-q", "-m", "CRW-952 test merge of a side commit", side)
		k.head = k.repo.git("rev-parse", "HEAD")
	case "hand":
		k.repo.git("merge", "--no-ff", "--no-commit", "-q", devs[0])
		k.head = k.repo.commit("hand.txt", "hand resolution")
	default:
		for _, d := range devs {
			k.repo.git("merge", "--no-ff", "-q", "-m", "CRW-952 test merge of dev", d)
		}
		k.head = k.repo.git("rev-parse", "HEAD")
	}
	return k, evaluated
}

// TestPremergeCLIAfterEvaluationAndMergeChains: a plain commit after the evaluated head needs the afterEvaluation statement
// (20 characters or more); a chain of up to four dev-only merges keeps the evaluation; a fifth merge, a hand-resolved merge
// and a merge of a commit outside dev do not (CRW-952 answer 2 and c5).
func TestPremergeCLIAfterEvaluationAndMergeChains(t *testing.T) {
	const statement = "the later commit adds one file"
	cases := []struct {
		name      string
		shape     string
		statement string
		want      string
	}{
		{name: "plain commit without the statement", shape: "plain", want: "premerge_after_evaluation_missing"},
		{name: "plain commit with the statement", shape: "plain", statement: statement, want: ""},
		{name: "one dev-only merge keeps the evaluation", shape: "merge1", want: ""},
		{name: "four dev-only merges keep the evaluation", shape: "merge4", want: ""},
		{name: "five dev-only merges do not", shape: "merge5", want: "premerge_head_mismatch"},
		{name: "a hand-resolved merge does not", shape: "hand", want: "premerge_head_mismatch"},
		{name: "a merge of a commit outside dev does not", shape: "outside", want: "premerge_head_mismatch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, evaluated := premergeCLIShape(t, c.shape)
			k.report("g", "I")
			rec := premergeRecordOf(t, k)
			rec.Head = evaluated
			rec.Dispositions.AfterEvaluation = c.statement
			m, code := premergeCLIRun(t, k, premergeRaw(t, rec))
			premergeCLIExpect(t, m, code, c.want)
		})
	}
}
