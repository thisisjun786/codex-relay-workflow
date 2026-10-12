package engine

import (
	"strings"
	"testing"
)

// The QA capture call of a final gate (SP-QA-1, CRW-1206) is an executor call the script marks with the option qa: true. The mark is
// what the backend reads to give the call the QA overlay, a scratch worktree at the head it names and no native helper, and what a
// continuation of its thread inherits.

func qaRow(h *harness, label string) Event {
	for _, e := range h.callRows(label) {
		if e.Event == EvPrepared {
			return e
		}
	}
	h.t.Fatalf("no prepared row for %s", label)
	return Event{}
}

// The prepared row of a qa call names the mark, the thread and the turns of the backend carry it, and the synthesis turn that
// continues the thread is a qa call as well.
func TestQACallMarkReachesTheBackendAndTheContinuation(t *testing.T) {
	h := newHarness(t)
	var specs []ThreadSpec
	h.fake.StartHook = func(spec ThreadSpec) (ThreadInfo, error) {
		specs = append(specs, spec)
		return ThreadInfo{ID: "thr_" + spec.CallID, Pair: spec.Pair}, nil
	}
	res := h.run(`
const cap = await agent.call('capture', { agentType: 'executor', qa: true, head: 'abcdef0', label: 'qa' })
const syn = await agent.call('synthesise', { resume: cap, label: 'qa-synthesis' })
const plain = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' })
return [cap.status, syn.status, plain.status]
`)
	if got := value[[]string](t, res); strings.Join(got, ",") != "completed,completed,completed" {
		t.Fatalf("%v", got)
	}
	if len(specs) != 2 || !specs[0].QA || specs[1].QA {
		t.Fatalf("thread specs: %+v", specs)
	}
	if got := qaRow(h, "qa").Data["qa"]; got != true {
		t.Errorf("the prepared row of the qa call: qa = %v", got)
	}
	if got := qaRow(h, "qa-synthesis").Data["qa"]; got != true {
		t.Errorf("the continuation of a qa thread does not inherit the mark: qa = %v", got)
	}
	if got, present := qaRow(h, "build").Data["qa"]; present && got != false {
		t.Errorf("a build call is marked qa: %v", got)
	}
	turns := map[string]bool{}
	for _, r := range h.fake.Requests() {
		if r.Kind == "turn" {
			turns[r.CallID] = r.QA
		}
	}
	for _, e := range h.journal.Events() {
		if e.Kind == KindCall && e.Label == "qa" && e.Event == EvPrepared {
			if !turns[e.CallID] {
				t.Errorf("the turn of the qa call does not carry the mark: %v", turns)
			}
		}
	}
}

// Only an executor call is a qa call: the capture drives a surface and edits a scratch worktree. Any other role is refused, and a
// continuation cannot turn a thread into a qa thread.
func TestQAOnlyForAnExecutorCall(t *testing.T) {
	h := newHarness(t)
	res := h.run(`
const bad = await agent.call('look', { agentType: 'explorer', qa: true, label: 'qa-explorer' })
const plain = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' })
const bad2 = await agent.call('again', { resume: plain, qa: true, label: 'qa-continuation' })
return [bad.status, bad.reason, bad2.status, bad2.reason]
`)
	got := value[[]string](t, res)
	if got[0] != "refused" || got[1] != ReasonPolicyRefused || got[2] != "refused" || got[3] != ReasonForeignHandle {
		t.Fatalf("%v", got)
	}
	if n := len(h.fake.Requests()); n != 2 {
		t.Errorf("%d requests reached the backend, want the thread and the turn of the build only", n)
	}
}

// The mark is part of what a call was given (CRW-1206 correction, d7): it changes the overlay, the placement, the snapshot and the
// native-spawn allowance, so a call whose only difference from an earlier one is the mark is another call. A resume does not replay
// the unmarked attempt as the QA call, and it does not replay the QA call as an unmarked one.
func TestResumeDoesNotReplayACallUnderAnotherQAMark(t *testing.T) {
	for _, tc := range []struct {
		name       string
		first, now string
		wantQATurn bool
	}{
		{"plain then qa", "", "qa: true,", true},
		{"qa then plain", "qa: true,", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			body := func(opt string) string {
				return `const c = await agent.call('capture', { agentType: 'executor', ` + opt + ` head: 'abcdef0', label: 'qa' })
return c.status`
			}
			if got := value[string](t, h.run(body(tc.first))); got != "completed" {
				t.Fatalf("first run: %v", got)
			}
			mark := len(h.fake.Requests())
			if got := value[string](t, h.resumeOn(body(tc.now))); got != "completed" {
				t.Fatalf("resumed run: %v", got)
			}
			var turns, qaTurns int
			for _, r := range h.fake.Requests()[mark:] {
				if r.Kind == "turn" {
					turns++
					if r.QA {
						qaTurns++
					}
				}
			}
			if turns != 1 {
				t.Fatalf("the resumed call sent %d turns, want 1: the earlier attempt is another call and is not replayed", turns)
			}
			if (qaTurns == 1) != tc.wantQATurn {
				t.Errorf("the resumed turn carries qa = %v, want %v", qaTurns == 1, tc.wantQATurn)
			}
		})
	}
}
