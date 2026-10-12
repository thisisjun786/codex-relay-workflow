package manage

import (
	"context"
	"strings"
	"testing"
	"time"
)

// pumpSignalPlanStore is a fixture store with one plan and, when withChild is true, a child B opened
// after the plan that has no release (child_without_release).
func pumpSignalPlanStore(t *testing.T, withChild bool) string {
	t.Helper()
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	if withChild {
		f.relationship("relationship-B", "CRW-B", dagReviewAt(10))
		f.scope("relationship-B", "project-1")
	}
	f.close()
	return f.dir
}

// pumpSignalPlanReview makes the pump's DAG source read a real DagReview over the store the test
// names, so the review carries the checks a real plan produces.
func pumpSignalPlanReview(t *testing.T, store *string) {
	t.Helper()
	saved := pumpSignalDagReview
	pumpSignalDagReview = func(ctx context.Context, _ *Env, _ *Config) (Review, error) {
		return DagReview(ctx, dagReviewEnv(t), dagReviewConfig(t, *store, nil, 0))
	}
	t.Cleanup(func() { pumpSignalDagReview = saved })
}

// CRW-1272: a store that holds a real plan makes DagReview carry the check
// released_before_predecessor as unmeasured for good (the scheduler exports no edge reading), and
// that check does not stand in the way of the clear: A, clear, A is three events.
func TestPumpSignalDagClearsWithARealPlanPresent(t *testing.T) {
	withAnomaly := pumpSignalPlanStore(t, true)
	clean := pumpSignalPlanStore(t, false)
	store := withAnomaly
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	pumpSignalPlanReview(t, &store)

	st := f.round()
	pumpSignalWant(t, "anomaly", st.Pending, "signal_dag#1")
	if st.Pending[0].Kind != pumpKindDag || !strings.Contains(st.Pending[0].Text, "CRW-B") {
		t.Fatalf("event = %+v, want an urgent dag event naming CRW-B", st.Pending[0])
	}
	review, err := pumpSignalDagReview(context.Background(), f.e, f.cfg)
	if err != nil || !dagReviewUnmeasuredCheck(review, dagReviewKindReleasedBeforePredecessor) {
		t.Fatalf("the review over a real plan = %+v, %v; want released_before_predecessor unmeasured", review.Checks, err)
	}
	f.deliverAll()

	store = clean
	f.advance(10 * time.Minute)
	st = f.round()
	pumpSignalWant(t, "clear", st.Pending, "signal_dag#2")
	if st.Pending[0].Kind != pumpKindInfo {
		t.Fatalf("event = %+v, want the clear (info) event", st.Pending[0])
	}
	f.deliverAll()

	store = withAnomaly
	f.advance(10 * time.Minute)
	st = f.round()
	pumpSignalWant(t, "again", st.Pending, "signal_dag#3")
	if st.Pending[0].Kind != pumpKindDag {
		t.Fatalf("event = %+v, want an urgent dag event", st.Pending[0])
	}
}

// CRW-1272: a check that is unmeasured for one round (a read that failed, a rollout that could not
// be read) still blocks the clear, as before: only the check that is unmeasured for good does not.
func TestPumpSignalDagTransientUnmeasuredStillBlocksClear(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	dag := pumpSignalTestDag(t)
	dag.review = pumpSignalReview(nil, pumpSignalAnomaly(dagReviewKindChildWithoutRelease, "p1", "", "CRW-1", "no release"))
	pumpSignalWant(t, "anomaly", f.round().Pending, "signal_dag#1")
	f.deliverAll()

	// The anomaly is not seen this round, but a rollout read failed next to the standing gap.
	dag.review = pumpSignalReview([]Check{
		{Name: dagReviewKindReleasedBeforePredecessor, State: dagReviewUnmeasured, Detail: "the scheduler exports no edge reading with the time it became satisfied"},
		{Name: "parent_rollout:thread-1", State: dagReviewUnmeasured, Detail: "read failed"},
	})
	f.advance(10 * time.Minute)
	if st := f.round(); len(st.Pending) != 0 {
		t.Fatalf("pending = %v, want no clear while a check is unmeasured for the round", pumpSignalIDs(st.Pending))
	}

	// The standing gap alone does not block, the failed reading did.
	dag.review = pumpSignalReview([]Check{
		{Name: dagReviewKindReleasedBeforePredecessor, State: dagReviewUnmeasured, Detail: "the scheduler exports no edge reading with the time it became satisfied"},
	})
	f.advance(10 * time.Minute)
	pumpSignalWant(t, "clear", f.round().Pending, "signal_dag#2")
}

// CRW-1272: the pump's DAG review recognizes a parent's relay call by its program word: a bare
// parameter expansion ($RELAY), codex-session-relay (by basename) or crw followed by relay, also
// after exec. A wrapper script that calls the relay inside is not recognized, so a refusal the
// parent got through `/tmp/s6r dag-accept ...` is not reported: the review cannot tell what the
// script runs, and reading every output that looks like a refusal would report the refusals of
// commands that are not the relay's. The forms are pinned here and written in CRW-692.md.
func TestDagHostRelayCommandFormsAreTheDocumentedOnes(t *testing.T) {
	cases := []struct {
		command string
		want    []string
	}{
		{"$RELAY dag-accept --plan p1", []string{"dag-accept"}},
		{`"${RELAY}" dag-accept --plan p1`, []string{"dag-accept"}},
		{"codex-session-relay dag-accept --plan p1", []string{"dag-accept"}},
		{"/opt/crw/bin/codex-session-relay dag-accept --plan p1", []string{"dag-accept"}},
		{"crw relay dag-accept --plan p1", []string{"dag-accept"}},
		{"/opt/crw/bin/crw relay dag-accept --plan p1", []string{"dag-accept"}},
		{"exec crw relay dag-accept --plan p1", []string{"dag-accept"}},
		{"cd /work && crw relay dag-accept --plan p1", []string{"dag-accept"}},
		{"/tmp/s6r dag-accept --plan p1", nil},
		{"./wrapper.sh crw relay dag-accept", nil},
		{"bash -c 'crw relay dag-accept'", nil},
		{"grep dag-accept notes.md", nil},
		{"crw relay status", nil},
	}
	for _, test := range cases {
		got, err := dagHostRelaySubcommands(test.command)
		if err != nil {
			t.Fatalf("%q: %v", test.command, err)
		}
		if strings.Join(got, ",") != strings.Join(test.want, ",") {
			t.Errorf("%q: relay dag- subcommands = %v, want %v", test.command, got, test.want)
		}
	}
}
