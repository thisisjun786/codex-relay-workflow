package cli_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

type amaCapture struct {
	Captures []any    `json:"captures"`
	Problems []string `json:"problems"`
}

func amaTree(t *testing.T, method string) (string, amaCapture) {
	t.Helper()
	amaRoot := t.TempDir()
	home := t.TempDir()
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repositoryRoot(t), "internal/relay/delivery/testdata/capture.py"), amaRoot, "test_attempt_message_atomicity")
	cmd.Dir = filepath.Join(repositoryRoot(t), "packages/codex-session-relay")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home, "PYTHONPATH="+filepath.Join(repositoryRoot(t), "packages/codex-session-relay/src")+":"+filepath.Join(repositoryRoot(t), "packages/codex-session-relay"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(&captureFailure{err, string(output)})
	}
	tree := filepath.Join(amaRoot, "AttemptMessageAtomicity."+method)
	raw, err := os.ReadFile(filepath.Join(tree, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var captured amaCapture
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	if len(captured.Problems) > 0 {
		t.Fatal(captured.Problems)
	}
	return tree, captured
}

type captureFailure struct {
	err    error
	output string
}

func (e *captureFailure) Error() string { return e.err.Error() + "\n" + e.output }
func amaShow(t *testing.T, tree string, event string) map[string]any {
	t.Helper()
	home := pythonHome(t)
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
func amaCompare(t *testing.T, got, want []any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("assertions Go=%v Python=%v", got, want)
	}
}
func Test21_AMA4_show_message_returns_the_sent_attempt_after_a_send(t *testing.T) {
	tree, py := amaTree(t, "test_show_message_returns_the_sent_attempt_after_a_send")
	payload := amaShow(t, tree, amaEvent(t, tree))
	entries := payload["attemptMessages"].([]any)
	e := entries[0].(map[string]any)
	_, preview := payload["previewMessage"]
	amaCompare(t, []any{float64(len(entries)), e["requestId"], e["status"], amaTokenCLI(t, e["message"]), preview}, py.Captures)
}
func Test21_AMA5_show_message_offers_a_preview_only_before_anything_is_prepared(t *testing.T) {
	tree, py := amaTree(t, "test_show_message_offers_a_preview_only_before_anything_is_prepared")
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
	home := pythonHome(t)
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
	amaCompare(t, got, py.Captures)
}
func eRequest(entries []any) any { return entries[0].(map[string]any)["requestId"] }
func Test21_AMA6_show_message_after_a_retry_lists_both_attempts_distinctly(t *testing.T) {
	tree, py := amaTree(t, "test_show_message_after_a_retry_lists_both_attempts_distinctly")
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
	amaCompare(t, got, py.Captures)
}
