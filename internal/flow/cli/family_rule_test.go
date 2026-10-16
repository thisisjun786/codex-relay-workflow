package cli

import "testing"

func TestLandHeadDoesNotRequireAuthorFamilySeparation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		authors func(*laneLab) []string
	}{
		{"same model", func(l *laneLab) []string { return []string{l.authorThread(labCheckModel, "high")} }},
		{"mixed families", func(l *laneLab) []string {
			return []string{l.authorThread("claude-fake-sonnet", "high"), l.authorThread(labCheckModel, "high")}
		}},
		{"unobserved author", func(*laneLab) []string { return []string{"0190a000-0000-7000-8000-ffffffffffff"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			head := l.outsideHead("A-1", "outside-family")
			spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-family", tc.authors(l)...), "--handoff", l.file)
			got := landValueOf(t, l.runToEnd(spec.RunID))
			if got.State != "landed" || got.Head != head || got.Acceptance == "" {
				t.Fatalf("a separately checked head must land without a model-family gate: %+v", got)
			}
			l.effectsOnce("A-1")
			if n := l.count("SELECT COUNT(*) FROM flow_calls WHERE run_id = ? AND role = 'reviewer'", spec.RunID); n != 0 {
				t.Fatalf("a tool review must not invent a reviewer agent: %d", n)
			}
		})
	}
}
