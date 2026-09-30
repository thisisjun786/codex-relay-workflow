package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// selectionPython is what testdata/selection_capture.py answers, run on a copy Python owns of
// the fixture's store as it stands (pythonCopy), recorded (pythonOutput). The first argument
// names the answer's kind; the arguments follow the store's path on the driver's command line.
func selectionPython(t *testing.T, f *stageFixture, what string, args ...string) []byte {
	t.Helper()
	return pythonOutput(t, pyKey(t, what), func() ([]byte, error) {
		root := repoRoot(t)
		script, err := filepath.Abs("testdata/selection_capture.py")
		if err != nil {
			return nil, err
		}
		cmd := exec.Command("uv", append([]string{"run", "--no-sync", "python", script, pythonCopy(t, f)}, args...)...)
		cmd.Dir = filepath.Join(root, "packages/codex-session-relay")
		home := t.TempDir()
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "CODEX_HOME="+home, "TMPDIR="+os.TempDir())
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("Python %s: %v %s", what, err, output)
		}
		return output, nil
	}, pyoracle.Substitute(f.root, "<fixture>"))
}

func compareSelectionPython(t *testing.T, f *stageFixture, o Obligation, recipient string, now *float64) {
	t.Helper()
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	recipientArg := "<none>"
	if recipient != "" {
		recipientArg = recipient
	}
	nowArg := "<none>"
	if now != nil {
		nowArg = strconv.FormatFloat(*now, 'f', 6, 64)
	}
	output := selectionPython(t, f, "select", string(raw), recipientArg, nowArg)
	var want map[string]any
	if err = json.Unmarshal(output, &want); err != nil {
		t.Fatal(err)
	}
	got, err := f.c.Selection(context.Background(), o, recipient, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if err = json.Unmarshal(encoded, &actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, want) {
		t.Errorf("Go=%s Python=%s", jsonText(actual), jsonText(want))
	}
}
func Test24_SR_5_ConfirmedVerdictDischarges(t *testing.T) {
	f := fixture24(t)
	o := f.obligation(t)
	compareSelectionPython(t, f, o, "", nil)
	if _, err := f.s.DB.Exec("INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')"); err != nil {
		t.Fatal(err)
	}
	compareSelectionPython(t, f, o, "", nil)
	for _, tc := range []struct{ id, state, ref, created string }{
		{"pending-one", "pending", "doc-1", "2023-11-14T22:13:20Z"},
		{"confirmed-two", "confirmed", "doc-1", "1999-01-01T00:00:00Z"},
		{"pending-three", "pending", "doc-1", "1980-01-01T00:00:00Z"},
	} {
		_, err := f.s.DB.Exec("INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES (?,?,?,?,?,'verdict',?,?,?,?,?,?)", tc.id, "rel-1", "REL-1", "coordination_document", tc.ref, "event-1", "digest", "summary", tc.state, tc.created, tc.created)
		if err != nil {
			t.Fatal(err)
		}
		compareSelectionPython(t, f, o, "", nil)
	}
}
func Test24_SR_23_LatestRulingNotWallClock(t *testing.T) {
	f := fixture24(t)
	o := f.obligation(t)
	if _, err := f.s.DB.Exec("INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, state, created string }{{"old-confirmed", "confirmed", "2023-11-14T22:13:20Z"}, {"new-pending", "pending", "1999-01-01T00:00:00Z"}} {
		_, err := f.s.DB.Exec("INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES (?,?,?,?,?,'verdict',?,?,?,?,?,?)", tc.id, "rel-1", "REL-1", "coordination_document", "doc-1", "event-1", "digest", "summary", tc.state, tc.created, tc.created)
		if err != nil {
			t.Fatal(err)
		}
		compareSelectionPython(t, f, o, "", nil)
	}
}
func Test24_SR_6_Contactability(t *testing.T) {
	f := fixture24(t)
	o := f.obligation(t)
	now := float64(1700000000)
	compareSelectionPython(t, f, o, "", &now)
	compareSelectionPython(t, f, o, "01supervisor-task", &now)
	if _, err := f.s.DB.Exec("INSERT INTO recipient_lifecycle (task_id,deliverable,withhold_reason,observed_at) VALUES (?,?,?,?)", "01supervisor-task", "yes", nil, f.at); err != nil {
		t.Fatal(err)
	}
	compareSelectionPython(t, f, o, "01supervisor-task", &now)
	later := now + 961
	compareSelectionPython(t, f, o, "01supervisor-task", &later)
	future := now - 61
	compareSelectionPython(t, f, o, "01supervisor-task", &future)
	skew := now - 5
	compareSelectionPython(t, f, o, "01supervisor-task", &skew)
	compareSelectionPython(t, f, o, "01supervisor-task", nil)
	if _, err := f.s.DB.Exec("UPDATE recipient_lifecycle SET deliverable='no',withhold_reason='recipient_paused' WHERE task_id='01supervisor-task'"); err != nil {
		t.Fatal(err)
	}
	compareSelectionPython(t, f, o, "01supervisor-task", &now)
}
