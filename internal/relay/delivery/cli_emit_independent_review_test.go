package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

// The independentReview item the child states about its independent code review travels on the
// receipt of a reviewable emit, read from a file the child wrote. The item's meaning is the
// parent's to read (merge-evidence); emit only carries it and never reads the artifact it names.
func reviewItemFile(t *testing.T, work, body string) string {
	t.Helper()
	path := filepath.Join(work, "independent-review.json")
	mustDo(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

const reviewItem = `{"artifact": {"path": "/review/artifact.json", "sha256": "` + "0000000000000000000000000000000000000000000000000000000000000000" + `"}, "status": "complete", "invalidReviewerCalls": 0, "dispositions": []}`

func TestEmitCarriesTheIndependentReviewItemOnAReviewableReceipt(t *testing.T) {
	t.Parallel()
	work := filepath.Join(t.TempDir(), "work")
	side := newSide(t, work)
	handoff := filepath.Join(work, "handoff.json")
	mustDo(t, os.WriteFile(handoff, []byte("{}"), 0o644))
	rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	accepted, code := runJSON(t, side, "emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", child, "--turn-id", dispatchTurn, "--artifact", handoff, "--independent-review", reviewItemFile(t, work, reviewItem))
	receipt, _ := accepted["receipt"].(map[string]any)
	stated, _ := receipt["independentReview"].(map[string]any)
	if code != 0 || stated["status"] != "complete" {
		t.Fatalf("the emit that states an item: %d %v", code, accepted)
	}
	if stored := sqliteDump(t, side, "SELECT receipt FROM events"); !strings.Contains(stored, "independentReview") {
		t.Fatalf("the stored receipt does not hold the item: %s", stored)
	}
}

func TestEmitWithoutTheIndependentReviewItemStoresNoSuchMember(t *testing.T) {
	t.Parallel()
	work := filepath.Join(t.TempDir(), "work")
	side := newSide(t, work)
	handoff := filepath.Join(work, "handoff.json")
	mustDo(t, os.WriteFile(handoff, []byte("{}"), 0o644))
	rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	accepted, code := runJSON(t, side, "emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", child, "--turn-id", dispatchTurn, "--artifact", handoff)
	receipt, _ := accepted["receipt"].(map[string]any)
	if _, stated := receipt["independentReview"]; code != 0 || stated {
		t.Fatalf("an emit that states no item: %d %v", code, accepted)
	}
}

// An execution-only receipt carries no deliverable, so it carries no statement about reviewing one;
// the refusal names the correction, as the one for a file does.
func TestEmitRefusesTheIndependentReviewItemOnAnExecutionOnlyReceipt(t *testing.T) {
	t.Parallel()
	work := filepath.Join(t.TempDir(), "work")
	side := newSide(t, work)
	rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	refused, code := runJSON(t, side, "emit", "--relationship", rid, "--generation", "1", "--outcome", "blocked_needs_input", "--turn-thread", child, "--turn-id", dispatchTurn, "--independent-review", reviewItemFile(t, work, reviewItem))
	detail, _ := refused["detail"].(string)
	if code != 2 || refused["reason"] != "malformed_receipt" || !strings.Contains(detail, "without --independent-review") {
		t.Fatalf("an execution-only emit that states an item: %d %v", code, refused)
	}
	help := argparse.Help("crw relay", "emit")
	for _, flag := range flagToken.FindAllString(detail, -1) {
		if !strings.Contains(help, flag) {
			t.Fatalf("the refusal names %s, which emit does not take:\n%s", flag, help)
		}
	}
	if got := sqliteDump(t, side, "SELECT count(*) FROM events"); got != "[[0]]\n" {
		t.Fatalf("a refused emit stored %s receipt events", got)
	}
}

// A file that is not one JSON object is the child's mistake to correct before anything is stored.
func TestEmitRefusesAnIndependentReviewFileThatIsNotAnObject(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{"not JSON": "reviewed, nothing found", "a list": "[]", "null": "null"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			work := filepath.Join(t.TempDir(), "work")
			side := newSide(t, work)
			handoff := filepath.Join(work, "handoff.json")
			mustDo(t, os.WriteFile(handoff, []byte("{}"), 0o644))
			rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
			refused, code := runJSON(t, side, "emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", child, "--turn-id", dispatchTurn, "--artifact", handoff, "--independent-review", reviewItemFile(t, work, body))
			if detail, _ := refused["detail"].(string); code != 4 || !strings.Contains(detail, "--independent-review") {
				t.Fatalf("an item file that is not an object: %d %v", code, refused)
			}
			if got := sqliteDump(t, side, "SELECT count(*) FROM events"); got != "[[0]]\n" {
				t.Fatalf("a refused emit stored %s receipt events", got)
			}
		})
	}
}
