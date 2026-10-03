package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The hint that follows an unadmitted-turn refusal is a command line the caller pastes, so it is
// quoted as a shell reads it, and it is only returned: the refusal row the intake recorded before
// the hint was added keeps the stored wording and does not carry the hint.
func TestContinuationHintIsReturnedAndNotRecorded(t *testing.T) {
	work := filepath.Join(parityTree(t), "work")
	side := newSide(t, work)
	mustDo(t, os.WriteFile(filepath.Join(work, "out.txt"), []byte("finished later"), 0o644))
	rid := strings.Trim(sqliteDump(t, side, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	out, code := side.run(emitArgs(rid, "turn-loop-5", "completed", filepath.Join(work, "out.txt"))...)
	if code != 2 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var answer map[string]any
	mustDo(t, json.Unmarshal([]byte(out), &answer))
	if answer["reason"] != "unassigned_turn" {
		t.Fatalf("reason %v", answer["reason"])
	}
	detail, _ := answer["detail"].(string)
	if want := "re-run this emit with --continues-anchor " + dispatchTurn + " --continuation-actor <your own task id>"; !strings.Contains(detail, want) {
		t.Fatalf("the hint names the anchor as a shell word: %q", detail)
	}
	db, err := sql.Open("sqlite", filepath.Join(side.state, "relay.sqlite3"))
	mustDo(t, err)
	defer func() { mustDo(t, db.Close()) }()
	var stored []string
	rows, err := db.Query("SELECT detail FROM refusals")
	mustDo(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var text string
		mustDo(t, rows.Scan(&text))
		stored = append(stored, text)
	}
	mustDo(t, rows.Err())
	if len(stored) != 1 {
		t.Fatalf("the refused emit left %d refusal rows: %q", len(stored), stored)
	}
	if strings.Contains(stored[0], "continues-anchor") {
		t.Fatalf("the hint is returned, not recorded: %q", stored[0])
	}
	if !strings.Contains(stored[0], "turn 'turn-loop-5' is not admitted to generation 1") {
		t.Fatalf("the recorded wording is the stored spelling: %q", stored[0])
	}
}

// The turn check names a request with no attempt row as a Go reader reads it. Only the daemon's
// turn-check note prints this text; ReconcileAttempt's KeyError, which the reconcile command
// matches by prefix and the reconcile pass stores, is not this one.
func TestCheckDispatchedTurnNamesAMissingAttemptWithGoQuotes(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	mustDo(t, err)
	defer func() { mustDo(t, s.Close()) }()
	_, err = NewReconciler(NewService(s, NewFakeClock())).CheckDispatchedTurn(ctx, "del-x", nil)
	if err == nil {
		t.Fatal("a request with no attempt row is an error")
	}
	if want := `no attempt records request "del-x"`; err.Error() != want {
		t.Fatalf("error %q, want %q", err.Error(), want)
	}
}

// Currency's inactive branch is reached for an active row whose superseded_by is set (a paused or
// superseded relationship is refused earlier by RequireActive, with the stored wording). Its detail
// is only returned out of a ruling that rolls back.
func TestCurrencyNamesAnInactiveRelationshipWithGoQuotes(t *testing.T) {
	relationship := Row{"relationship_id": "rel-1", "status": "active", "superseded_by": "rel-2", "execution_generation": int64(1)}
	event := Row{"relationship_id": "rel-1", "execution_generation": int64(1)}
	state, err := Currency(context.Background(), nil, relationship, event)
	mustDo(t, err)
	if state.Get("reason") != RelationshipNotActive {
		t.Fatalf("reason %v", state.Get("reason"))
	}
	if want := `relationship "rel-1" is "active"`; state.Get("detail") != want {
		t.Fatalf("detail %v, want %q", state.Get("detail"), want)
	}
}

// A finding's restoration flag that is not a boolean is refused before any ruling is written, and
// the refusal names what was given as a message names a kind.
func TestRestorationFlagRefusalNamesTheKindOfTheValue(t *testing.T) {
	_, err := NormaliseFindings([]any{Obj{{Key: "id", Value: "c1"}, {Key: "restoration", Value: "yes"}}})
	if Reason(err) != DispositionConflict {
		t.Fatalf("reason %q: %v", Reason(err), err)
	}
	if want := "a finding declares its restoration block with true or false, not a string"; Detail(err) != want {
		t.Fatalf("detail %q, want %q", Detail(err), want)
	}
}
