package mergeturn

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func step(sha, subject string, parents ...string) Step {
	return Step{SHA: sha, Parents: parents, Subject: subject}
}

// outsideMerges decides from a reading and the lane's own landings; each row is one way a move
// is, or is not, confirmed as merges made outside the lane.
func TestCRW403_OutsideMergesJudgement(t *testing.T) {
	line := func(steps ...Step) Movement { return Movement{From: "O", To: steps[0].SHA, Steps: steps} }
	for _, tc := range []struct {
		name   string
		m      Movement
		from   string
		to     string
		landed map[string]string
		count  int    // steps confirmed
		why    string // fragment of the reason, empty when confirmed
	}{
		{name: "one merge", m: line(step("t", "Merge pull request #1", "O", "s")), from: "O", to: "t", count: 1},
		{name: "two merges", m: line(step("t", "m2", "u", "s2"), step("u", "m1", "O", "s1")), from: "O", to: "t", count: 2},
		{name: "case is ignored", m: line(step("T", "m", "o", "s")), from: "O", to: "t", count: 1},
		{name: "steps below the base are not judged", m: line(step("t", "m", "O", "s"), step("O", "a direct commit", "N")), from: "O", to: "t", count: 1},
		{name: "a direct commit", m: line(step("t", "work", "O")), from: "O", to: "t", why: "has one parent"},
		{name: "a direct commit under a merge", m: line(step("t", "m", "u", "s"), step("u", "work", "O")), from: "O", to: "t", why: "has one parent"},
		{name: "a line that never reaches the base", m: line(step("t", "m", "x", "s"), step("x", "m", "y", "s")), from: "O", to: "t", why: "does not lead from"},
		{name: "a line that stops at a root", m: line(step("t", "m", "x", "s"), step("x", "root")), from: "O", to: "t", why: "does not lead from"},
		{name: "a broken line", m: line(step("t", "m", "u", "s"), step("w", "m", "O", "s")), from: "O", to: "t", why: "is broken at"},
		{name: "a landing of the lane", m: line(step("t", "m", "O", "s")), from: "O", to: "t", landed: map[string]string{"t": "mtn-1"}, why: "is the landing of turn 'mtn-1'"},
		{name: "a landing of the lane, in other case", m: line(step("T", "m", "O", "s")), from: "O", to: "t", landed: map[string]string{"t": "mtn-1"}, why: "recorded by this lane"},
		{name: "another base was read", m: Movement{From: "Z", To: "t", Steps: []Step{step("t", "m", "Z", "s")}}, from: "O", to: "t", why: "does not run from the recorded base"},
		{name: "another tip was read", m: Movement{From: "O", To: "z", Steps: []Step{step("z", "m", "O", "s")}}, from: "O", to: "t", why: "does not run from the recorded base"},
		{name: "nothing was read", m: Movement{From: "O", To: "t"}, from: "O", to: "t", why: "nothing was read from the tip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps, why := outsideMerges(tc.m, tc.from, tc.to, tc.landed)
			if tc.why == "" {
				if why != "" || len(steps) != tc.count {
					t.Fatalf("want %d confirmed steps, got %d (%s)", tc.count, len(steps), why)
				}
				return
			}
			if len(steps) != 0 || !strings.Contains(why, tc.why) {
				t.Fatalf("want a refusal containing %q, got %d steps and %q", tc.why, len(steps), why)
			}
		})
	}
}

func TestCRW403_CleanSubject(t *testing.T) {
	long := strings.Repeat("é", 150)
	for in, want := range map[string]string{
		"Merge pull request #2 from a/b\n\nbody": "Merge pull request #2 from a/b",
		"tab\there\x1b[31m red":                  "tab here [31m red",
		long:                                     strings.Repeat("é", 100),
		"   padded   ":                           "padded",
		"":                                       "",
	} {
		if got := cleanSubject(in); got != want {
			t.Errorf("cleanSubject(%q) = %q, want %q", in, got, want)
		}
	}
}

// The recovery command is one a person can paste: a task id the registry accepts may hold spaces.
func TestCRW403_RecoveryCommandQuotesTheHolder(t *testing.T) {
	landing := store.Row{{Name: "turn_id", Value: "mtn-1"}, {Name: "observed_base_sha", Value: "O"}, {Name: "holder_task_id", Value: "task alpha"}, {Name: "project_key", Value: "PRJ-A"}}
	detail := staleLandingDetail(store.MergeTurnsRow{BaseRef: "dev", Repository: "owner/repo"}, landing, "T", "T", "no reason")
	if !strings.Contains(detail, "merge-turn-restate-base --turn mtn-1 --actor 'task alpha' --evidence '<why the base moved>'") {
		t.Fatalf("the recovery command: %s", detail)
	}
}
