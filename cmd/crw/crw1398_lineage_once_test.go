package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// CRW-1398 item 3: the unreadable-lineage once marker of a session is claimed only when the lineage walk cannot be completed (a rollout that
// cannot be read or found, a lineage deeper than the limit), not when the relay's record of a parent thread cannot be read at one hop, which
// a later walk can pass. A transient failure of that kind used to claim the marker, so the session's later unreadable-lineage warning, a
// real one, left no record.
func TestCRW1398AMidWalkRelayReadFailureDoesNotClaimTheOnceMarker(t *testing.T) {
	f := newSubagentPushFixture(t)
	s1, s2, s3 := lineageThread(1), lineageThread(2), lineageThread(3)
	rollouts := f.lineageRollouts(crw1303Kid, s1, s2, s3) // s3 <- s2 <- s1 <- the relay child
	var mu sync.Mutex
	failS2 := true
	childBindingOf = func(ctx context.Context, session string) (bool, string, error) {
		mu.Lock()
		fail := failS2 && session == s2
		mu.Unlock()
		if fail {
			return false, "", os.ErrDeadlineExceeded // the relay's store was busy at this hop of the walk
		}
		return hook.ChildBinding(ctx, session)
	}
	t.Cleanup(func() { childBindingOf = hook.ChildBinding })
	push := "git push origin HEAD:dev"

	// A: the walk reaches the hop whose relay record cannot be read: the push passes with the warning and a record, and no marker is claimed.
	out := f.answerFrom(s1, s3, rollouts[s3], f.parent, push)
	if w, warned := guardWarning(out); !warned || !strings.Contains(w, "relay's record of a parent thread cannot be read") {
		t.Fatalf("A: %q; want a pass with the warning that the relay's record cannot be read", out)
	}
	if n := len(f.records()); n != 1 {
		t.Fatalf("A: %d records, want 1", n)
	}
	// B: the same session, the walk passes and judges the push as the relay child's own (a push to dev warns, recorded as any such warning).
	mu.Lock()
	failS2 = false
	mu.Unlock()
	out = f.answerFrom(s1, s3, rollouts[s3], f.parent, push)
	if w, warned := guardWarning(out); !warned || !strings.Contains(w, "통합은 자식·통합기 몫") {
		t.Fatalf("B: %q; want the child's warning for a push to dev", out)
	}
	if n := len(f.records()); n != 2 {
		t.Fatalf("B: %d records, want 2", n)
	}
	// C: now the lineage really cannot be followed (the parent thread's rollout is gone): the first such warning of the session is recorded
	// in spite of A, and D, the second, leaves no record.
	if err := os.Remove(rollouts[s2]); err != nil {
		t.Fatal(err)
	}
	for i, label := range []string{"C", "D"} {
		out = f.answerFrom(s1, s3, rollouts[s3], f.parent, push)
		if w, warned := guardWarning(out); !warned || !strings.Contains(w, "cannot be found") {
			t.Fatalf("%s: %q; want a pass with the unreadable-lineage warning", label, out)
		}
		if n, want := len(f.records()), 3; n != want {
			t.Fatalf("%s: %d records, want %d (C records the first unreadable lineage of the session, D leaves none; step %d)", label, n, want, i)
		}
	}
}
