package delivery

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// A context that ends in the middle of a replay is a stop, whichever step of the replay it ends
// in: the answer is the context's error, never a verdict about a marker the replay did not get to
// read (a conflict where the fact is the same, an unbound generation where the bind stands).
func TestAStopDuringAReplayIsNeverAMarkerVerdict(t *testing.T) {
	t.Run("publish or compare", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "facts", "intent.json")
		payload := Obj{{Key: "dispatchRequestId", Value: "dispatch-1"}, {Key: "at", Value: "2026-01-01T00:00:00+00:00"}}
		if outcome, err := publishOrCompare(context.Background(), target, payload, []string{"dispatchRequestId"}, "", nil); err != nil || outcome != Published {
			t.Fatalf("first publication: %q, %v", outcome, err)
		}
		stops := 0
		for n := 0; n < 80; n++ {
			outcome, err := publishOrCompare(testsupport.StopAfter(n), target, payload, []string{"dispatchRequestId"}, "", nil)
			switch {
			case err == nil && outcome == Unchanged:
			case errors.Is(err, context.Canceled):
				stops++
			default:
				t.Fatalf("a replay whose context ended after %d checks answered %q, %v; want unchanged or the stop", n, outcome, err)
			}
		}
		if stops == 0 {
			t.Fatal("no context ended inside the replay: the boundary was not reached")
		}
	})
	t.Run("bind identity", func(t *testing.T) {
		root, work := filepath.Join(t.TempDir(), "markers"), t.TempDir()
		t.Setenv(MarkerEnv, "")
		const at = "2026-01-01T00:00:00+00:00"
		if _, err := DeclareIntent(context.Background(), root, IntentDeclaration{Workspace: work, DispatchRequestID: "dispatch-request-1", IssueKey: "REL-1", DeclaredAt: at}); err != nil {
			t.Fatal(err)
		}
		assignment := AssignmentID("dispatch-request-1")
		if first, err := BindIdentity(context.Background(), root, work, assignment, child, child, at); err != nil || pyjson.Text(first.Get("outcome")) != Bound {
			t.Fatalf("first bind: %v, %v", first, err)
		}
		stops := 0
		for n := 0; n < 120; n++ {
			out, err := BindIdentity(testsupport.StopAfter(n), root, work, assignment, child, child, at)
			switch {
			case err == nil && pyjson.Text(out.Get("outcome")) == Unchanged:
			case errors.Is(err, context.Canceled):
				stops++
			default:
				t.Fatalf("a replayed bind whose context ended after %d checks answered %v, %v; want unchanged or the stop", n, out, err)
			}
		}
		if stops == 0 {
			t.Fatal("no context ended inside the replayed bind: the boundary was not reached")
		}
	})
}
