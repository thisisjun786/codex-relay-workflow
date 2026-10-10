package cli

import (
	"strings"
	"testing"
)

// CRW-1111 through the loop CLI. Red on dev: the annotate note was dropped when the batch was applied, so
// show had nothing to list, and the same key with another batch answered "already applied".

// The note an annotate batch carried is listed by show with the key of its batch.
func TestLoopSteerShowListsTheAnnotateNote(t *testing.T) {
	cwd, _ := loopMutWorkspace(t, nil)
	batch := `{"idempotencyKey":"k1","rationale":"user asked","evidence":"chat","ops":[{"kind":"annotate","note":"prefer streaming"}]}`
	loopMutRun(t, cwd, 0, "steer", "--session", loopMutSession, "--batch-json", batch)
	out := loopMutRun(t, cwd, 0, "show", "--session", loopMutSession)
	if !strings.Contains(out, "\n  - note k1: prefer streaming") {
		t.Fatalf("show does not list the note:\n%s", out)
	}
}

// The same key with another batch is refused with code 1; the same batch again is the duplicate.
func TestLoopSteerSameKeyWithAnotherBatchIsRefused(t *testing.T) {
	cwd, _ := loopMutWorkspace(t, nil)
	batch := `{"idempotencyKey":"k1","rationale":"user asked","evidence":"chat","ops":[{"kind":"annotate","note":"prefer streaming"}]}`
	loopMutRun(t, cwd, 0, "steer", "--session", loopMutSession, "--batch-json", batch)
	if out := loopMutRun(t, cwd, 0, "steer", "--session", loopMutSession, "--batch-json", batch); !strings.Contains(out, "k1 was already applied at ") {
		t.Errorf("the same batch again: %q", out)
	}
	other := strings.Replace(batch, "prefer streaming", "prefer batching", 1)
	if out := loopMutRun(t, cwd, 1, "steer", "--session", loopMutSession, "--batch-json", other); !strings.Contains(out, `loop steer: idempotencyKey "k1" was already used at `) {
		t.Errorf("another batch under the same key: %q", out)
	}
}
