package ops

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1288: the release of a node compares the plan slice the flow compared with the plan the relay has at its release commit.

// solDelayedReleaseRelay stops Relay.Release before it commits, so the test can change the plan in that window (the F09 verifier's probe).
type solDelayedReleaseRelay struct {
	*stubRelay
	entered chan struct{}
	commit  chan struct{}
}

func (relay *solDelayedReleaseRelay) Release(ctx context.Context, request ReleaseRequest) (ReleaseResult, error) {
	close(relay.entered)
	<-relay.commit
	return relay.stubRelay.Release(ctx, request)
}

func TestSolReleaseMustCompareAtRelayCommit(t *testing.T) {
	world := newWorld(t)
	world.relay.plan = planOfNodes(map[string]string{"A": "a1"}, "A")
	world.ledgered(true, designOf("A"))
	relay := &solDelayedReleaseRelay{stubRelay: world.relay, entered: make(chan struct{}), commit: make(chan struct{})}
	world.lane.d.Relay = relay
	type answer struct {
		raw json.RawMessage
		err error
	}
	completed := make(chan answer, 1)
	go func() {
		raw, err := world.op("release", "plan-1", "A", "M", map[string]any{"pinned": true})
		completed <- answer{raw: raw, err: err}
	}()
	<-relay.entered
	world.relay.plan = planOfNodes(map[string]string{"A": "a2"}, "A")
	close(relay.commit)
	result := <-completed
	if result.err != nil {
		t.Fatal(result.err)
	}
	var released ReleaseOutcome
	if err := json.Unmarshal(result.raw, &released); err != nil {
		t.Fatal(err)
	}
	if released.Released && len(world.relay.releases) > 0 {
		for _, event := range world.j.Events() {
			if event.Op == OpRelease && dataBool(event, "released") {
				t.Fatalf("release invariant violated: released %s under %s; journal slice=%s; live relay slice at Release=%s; release request=%+v", released.Node, released.Revision, dataStr(event, "slice_digest"), world.relay.plan.Nodes[0].SliceDigest, world.relay.releases[0])
			}
		}
	}
}

// replayingRelay answers a release as the replay of the release the relay committed under the slice committed.
type replayingRelay struct {
	*stubRelay
	committed string
}

func (relay *replayingRelay) Release(ctx context.Context, request ReleaseRequest) (ReleaseResult, error) {
	res, err := relay.stubRelay.Release(ctx, request)
	res.Replayed, res.SliceDigest = true, relay.committed
	return res, err
}

// releaseRows are the completed crw.release rows of the journal.
func (w *world) releaseRows() []engine.Event {
	var out []engine.Event
	for _, event := range w.j.Events() {
		if event.Op == OpRelease && event.Event == engine.EvCompleted {
			out = append(out, event)
		}
	}
	return out
}

// TestReleaseJournalsTheCommittedSlice (CRW-1288): the release carries the slice the flow compared, and the slice_digest of a released
// crw.release row is the slice the relay committed the release under: a release the relay answers as the replay of one it committed
// under another slice records that slice, not the one the flow read (the flow records the relay's answer; the relay itself answers a
// replay only under the expected slice, TestReleaseReplayComparesTheExpectedSlice).
func TestReleaseJournalsTheCommittedSlice(t *testing.T) {
	world := newWorld(t)
	world.relay.plan = planOfNodes(map[string]string{"A": "a1"}, "A")
	world.ledgered(true, designOf("A"))
	world.lane.d.Relay = &replayingRelay{stubRelay: world.relay, committed: "a0"}
	if _, err := world.op("release", "plan-1", "A", "M", map[string]any{"pinned": true}); err != nil {
		t.Fatal(err)
	}
	if len(world.relay.releases) != 1 || world.relay.releases[0].SliceDigest != "a1" || world.relay.releases[0].PlanDigest != digestOf(*world.relay.plan) {
		t.Fatalf("the release request = %+v, want the slice a1 of the plan the flow compared", world.relay.releases)
	}
	rows := world.releaseRows()
	if len(rows) != 1 || !dataBool(rows[0], "released") || dataStr(rows[0], "slice_digest") != "a0" {
		t.Fatalf("the release row = %+v, want released with the committed slice a0", rows)
	}
}

// TestReleaseRefusedForAPlanTheFlowDoesNotReadIsNotAskedAgain (CRW-1288): a release the relay refuses as plan_changed while the plan
// the flow reads again is still the pinned one is relay_plan_changed: no signal can come of a change the flow does not read, so the
// lane does not ask again (a plan_changed answer would make it ask again for ever). A plan the flow reads as changed stays plan_changed.
func TestReleaseRefusedForAPlanTheFlowDoesNotReadIsNotAskedAgain(t *testing.T) {
	for _, seen := range []bool{false, true} {
		world := newWorld(t)
		world.relay.plan = planOfNodes(map[string]string{"A": "a1"}, "A")
		world.ledgered(true, designOf("A"))
		world.relay.relaySlice = map[string]string{"A": "a2"}
		if seen {
			world.relay.afterRelease = func() { world.relay.plan = planOfNodes(map[string]string{"A": "a2"}, "A") }
		}
		raw, err := world.op("release", "plan-1", "A", "M", map[string]any{"pinned": true})
		if err != nil {
			t.Fatal(err)
		}
		var out ReleaseOutcome
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		want := ReasonRelayPlanChanged
		if seen {
			want = ReasonPlanChanged
		}
		if out.Released || out.Reason != want {
			t.Fatalf("the plan the flow reads changed %v: %+v, want %s", seen, out, want)
		}
	}
}
