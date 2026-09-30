package faults

import (
	"fmt"
	"testing"
)

// These scenarios replay the complete CLI output and every SQLite table against
// the live Python package. They cover the three review findings on Record.
func TestRecordReviewFindingsWholeOutput(t *testing.T) {
	goldenParent(t)
	t.Run("resolved recurrence queues reopen update", func(t *testing.T) {
		ctx, gd, pd := f1ReplayStores(t)
		invoke := func(args ...string) map[string]any { return f1ReplayCLI(t, ctx, gd, pd, args) }
		observed := invoke("fault-observe", "--observation", reviewObservation("reopen", "first", "ISSUE-1", false))
		id := observed["faultId"].(string)
		f1SeedBoth(t, ctx, gd, pd, []string{fmt.Sprintf("UPDATE fault_ledger SET external_ref='ISSUE-1',state='resolved',resolved_at='stamp' WHERE fault_id='%s'", id)})
		invoke("fault-observe", "--observation", reviewObservation("reopen", "second", "ISSUE-1", false))
	})

	t.Run("clear refunds a claimed publication", func(t *testing.T) {
		ctx, gd, pd := f1ReplayStores(t)
		invoke := func(args ...string) map[string]any { return f1ReplayCLI(t, ctx, gd, pd, args) }
		invoke("fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P")
		observed := invoke("fault-observe", "--observation", reviewObservation("withdraw", "first", "ISSUE-2", false))
		publication := observed["publication"].(map[string]any)["publicationId"].(string)
		invoke("fault-claim", "--publication", publication, "--owner", "writer")
		invoke("fault-observe", "--observation", reviewObservation("withdraw", "clear", "ISSUE-2", true))
	})

	t.Run("same target key does not update scope text", func(t *testing.T) {
		ctx, gd, pd := f1ReplayStores(t)
		invoke := func(args ...string) map[string]any { return f1ReplayCLI(t, ctx, gd, pd, args) }
		invoke("fault-observe", "--observation", reviewObservation("scope", "first", "ISSUE-OLD", false))
		invoke("fault-observe", "--observation", reviewObservation("scope", "second", "ISSUE-NEW", false))
	})
}

func reviewObservation(turn, occurrence, issue string, cleared bool) string {
	return fmt.Sprintf(`{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":%q},"occurrenceKey":%q,"scope":{"projectKey":"P","issueKey":%q},"cleared":%t}`, turn, occurrence, issue, cleared)
}
