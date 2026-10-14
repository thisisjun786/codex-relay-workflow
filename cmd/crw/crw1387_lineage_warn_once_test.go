package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1387 d3: a sub-agent of a session the relay does not know as a child (an integrator's, a parent-side worker's) whose lineage cannot be
// read leaves the warn record once per session id, not on every push. Each push still passes with the warning (fail-open); a second session id
// gets its own record; the only refusal of the guard stays the shared-branch-history rule.
func TestCRW1387AnUnreadableLineageLeavesOneWarnRecordPerSession(t *testing.T) {
	f := newSubagentPushFixture(t)
	s1, s2, s3 := lineageThread(1), lineageThread(2), lineageThread(3)
	rollouts := f.lineageRollouts(crw1303Parent, s1, s2, s3)
	if err := os.Remove(rollouts[s2]); err != nil { // the parent thread's rollout is gone: the lineage of s3 cannot be followed
		t.Fatal(err)
	}
	pushes := []string{"git push origin HEAD:dev", "git push -f origin HEAD:dev", "git push origin HEAD:feature"}
	for _, command := range pushes {
		out := f.answerFrom(s2, s3, rollouts[s3], f.parent, command)
		w, warned := guardWarning(out)
		if !warned || !strings.Contains(w, "cannot be found") || strings.Contains(out, "permissionDecision") {
			t.Errorf("%s: %q; want a pass with the unreadable-lineage warning", command, out)
		}
	}
	rs := f.records()
	if len(rs) != 1 {
		t.Fatalf("%d decision records after %d pushes of one session with an unreadable lineage, want 1: %v", len(rs), len(pushes), rs)
	}
	if r := rs[0]; r["leg"] != childPushLeg || r["decision"] != "warn" || r["sessionId"] != s2 || r["agentId"] != s3 || r["rule"] == "shared-branch-history" {
		t.Errorf("record %v", r)
	}
	// Another session id with an unreadable lineage leaves its own record, once.
	stray := filepath.Join(f.home, "sessions", "no", "such", "rollout.jsonl")
	for range 2 {
		if _, warned := guardWarning(f.answerFrom(s1, s3, stray, f.parent, "git push origin HEAD:dev")); !warned {
			t.Error("a second session's push with an unreadable lineage does not warn")
		}
	}
	bySession := map[any]int{}
	for _, r := range f.records() {
		bySession[r["sessionId"]]++
	}
	if len(bySession) != 2 || bySession[s1] != 1 || bySession[s2] != 1 {
		t.Errorf("records by session %v, want one for each of the two sessions", bySession)
	}
}
