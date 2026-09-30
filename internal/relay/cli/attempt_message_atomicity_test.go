package cli_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// amaTree is the tree test_attempt_message_atomicity.py's method left (its store and artifacts),
// as the Python relay built it under a fixed root (testdata/fixtures), so every run spells the
// same paths and the ids derived from them.
func amaTree(t *testing.T, method string) string {
	t.Helper()
	amaRoot := t.TempDir()
	name := "AttemptMessageAtomicity." + method
	tree := filepath.Join(amaRoot, name)
	fixtureTree(t, "attempt-message-atomicity-"+strings.ReplaceAll(strings.TrimPrefix(method, "test_"), "_", "-")+".json", amaRoot)
	return tree
}

func amaShow(t *testing.T, tree string, event string) map[string]any {
	t.Helper()
	home := tempHome(t)
	state := filepath.Join(tree, "state")
	result := golang(t, home, "--state", state, "show", "--event", event, "--message")
	if result.code != 0 {
		t.Fatalf("show: %+v", result)
	}
	return decode(t, result.stdout)
}
func amaEvent(t *testing.T, tree string) string {
	t.Helper()
	// The Python case has exactly one event. Read its ID from its captured store.
	state := filepath.Join(tree, "state", "relay.sqlite3")
	db, err := sql.Open("sqlite", state)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var event string
	if err := db.QueryRow("SELECT event_id FROM events").Scan(&event); err != nil {
		t.Fatal(err)
	}
	return event
}
func amaTokenCLI(t *testing.T, value any) string {
	t.Helper()
	for _, line := range strings.Split(value.(string), "\n") {
		if strings.HasPrefix(line, "requestId: ") {
			return strings.TrimPrefix(line, "requestId: ")
		}
	}
	t.Fatal("message carries no requestId")
	return ""
}

// amaCheck checks the values the Python test asserted, as Go reads them from the tree the
// method left, against the golden.
func amaCheck(t *testing.T, method string, got []any) {
	t.Helper()
	expectJSON(t, "AttemptMessageAtomicity."+method+" captures", got)
}
func Test21_AMA4_show_message_returns_the_sent_attempt_after_a_send(t *testing.T) {
	const method = "test_show_message_returns_the_sent_attempt_after_a_send"
	tree := amaTree(t, method)
	payload := amaShow(t, tree, amaEvent(t, tree))
	entries := payload["attemptMessages"].([]any)
	e := entries[0].(map[string]any)
	_, preview := payload["previewMessage"]
	amaCheck(t, method, []any{float64(len(entries)), e["requestId"], e["status"], amaTokenCLI(t, e["message"]), preview})
}
func Test21_AMA5_show_message_offers_a_preview_only_before_anything_is_prepared(t *testing.T) {
	const method = "test_show_message_offers_a_preview_only_before_anything_is_prepared"
	tree := amaTree(t, method)
	event := amaEvent(t, tree)
	// Rewind a copy of the Python fixture to the queued, unprepared state. The
	// original remains intact for the after-send command.
	before := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(tree, "state", "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(before, "relay.sqlite3"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(before, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"DELETE FROM attempt_messages", "DELETE FROM attempts", "UPDATE deliveries SET attempt_count = 0, state = 'queued', next_eligible_at = NULL"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// The copy lives in its own directory: its mirror names the copy, not the original.
	testsupport.Rehome(t, filepath.Join(before, "relay.sqlite3"))
	home := tempHome(t)
	prior := golang(t, home, "--state", before, "show", "--event", event, "--message")
	if prior.code != 0 {
		t.Fatalf("before show: %+v", prior)
	}
	pre := decode(t, prior.stdout)
	preview, present := pre["previewMessage"]
	got := []any{pre["attemptMessages"], present, strings.HasSuffix(amaTokenCLI(t, preview), "-a1")}
	payload := amaShow(t, tree, event)
	entries := payload["attemptMessages"].([]any)
	_, afterPreview := payload["previewMessage"]
	got = append(got, afterPreview, eRequest(entries))
	amaCheck(t, method, got)
}
func eRequest(entries []any) any { return entries[0].(map[string]any)["requestId"] }
func Test21_AMA6_show_message_after_a_retry_lists_both_attempts_distinctly(t *testing.T) {
	const method = "test_show_message_after_a_retry_lists_both_attempts_distinctly"
	tree := amaTree(t, method)
	payload := amaShow(t, tree, amaEvent(t, tree))
	entries := payload["attemptMessages"].([]any)
	ids, statuses := []any{}, []any{}
	for _, item := range entries {
		e := item.(map[string]any)
		ids = append(ids, e["requestId"])
		statuses = append(statuses, e["status"])
	}
	got := []any{ids, statuses}
	for _, item := range entries {
		e := item.(map[string]any)
		got = append(got, amaTokenCLI(t, e["message"]))
	}
	amaCheck(t, method, got)
}
