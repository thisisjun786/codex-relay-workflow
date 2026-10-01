package delivery

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The eight intent-* marker commands through the built `crw relay`, with its own home, marker root
// and seeded store, over a fixed workspace path (a parityTree: the workspace key hashes it). Every
// stdout is checked whole against the golden after the wall-clock stamps, the home and the pid
// (loserProcess) become tokens, and so is every exit code. The goldens began as the Python console
// script's answers over the same workspace path.

var loserProcess = regexp.MustCompile(`"loserProcess": "\d+"`)

func TestCLI_every_intent_command_answers_byte_for_byte_like_python(t *testing.T) {
	side := newSide(t, filepath.Join(parityTree(t), "work"))
	seeded := sqliteDump(t, side, "SELECT relationship_id FROM relationships")
	side.expect("sqlite SELECT relationship_id FROM relationships", seeded)
	rid := regexp.MustCompile(`rel-[0-9a-f]{16}`).FindString(seeded)
	if rid == "" {
		t.Fatal("the side is seeded with a relationship")
	}
	a1, a2 := AssignmentID("dispatch-1"), AssignmentID("dispatch-2")
	cases := [][]string{
		{"intent-show", "--workspace", "<work>"},
		{"intent-show", "--workspace", "<work>", "--assignment", a1},
		{"intent-show", "--workspace", "<work>", "--assignment", "../escape"},
		{"intent-declare", "--workspace", "<work>", "--dispatch-request-id", "dispatch-1", "--issue", "REL-1", "--declared-at", "2026-01-01T00:00:00+00:00", "--criteria-source", "doc-a", "--settings", `{"sandbox": "workspace-write", "n": [1, 2.5]}`},
		{"intent-declare", "--workspace", "<work>", "--dispatch-request-id", "dispatch-1", "--issue", "REL-1", "--declared-at", "2026-01-01T00:05:00+00:00", "--criteria-source", "doc-a", "--settings", `{"sandbox": "workspace-write", "n": [1, 2.5]}`},
		{"intent-declare", "--workspace", "<work>", "--dispatch-request-id", "dispatch-1", "--issue", "REL-9"},
		{"intent-declare", "--workspace", "<work>", "--dispatch-request-id", "  ", "--issue", "REL-1"},
		{"intent-declare", "--workspace", "<work>", "--dispatch-request-id", "dispatch-2", "--issue", "REL-2", "--no-db-path", "--declared-at", "2026-01-01T00:00:00+00:00"},
		{"intent-show", "--workspace", "<work>", "--now", "2026-01-01T00:10:00+00:00"},
		{"intent-attempt", "--workspace", "<work>", "--assignment", a1, "--outcome", "accepted", "--task-id", "01child-task"},
		{"intent-attempt", "--workspace", "<work>", "--assignment", a1, "--outcome", "maybe"},
		{"intent-attempt", "--workspace", "<work>", "--assignment", "not-hex", "--outcome", "failed"},
		{"intent-bind", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--task-id", "01child-task"},
		{"intent-bind", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--task-id", "01child-task"},
		{"intent-bind", "--workspace", "<work>", "--assignment", a1, "--session", "01other", "--task-id", "01other"},
		{"intent-bind", "--workspace", "<work>", "--assignment", a1, "--session", " ", "--task-id", "01other"},
		{"intent-register", "--workspace", "<work>", "--assignment", a1, "--relationship", rid, "--dispatch-request-id", "some-other-dispatch"},
		{"intent-register", "--workspace", "<work>", "--assignment", a1, "--relationship", rid, "--dispatch-request-id", "dispatch-1", "--db-path", "<home>/no-such-store.sqlite3"},
		{"intent-register", "--workspace", "<work>", "--assignment", a1, "--relationship", "rel-0123456789abcdef", "--dispatch-request-id", "dispatch-1"},
		{"intent-register", "--workspace", "<work>", "--assignment", a1, "--relationship", rid, "--dispatch-request-id", "dispatch-1"},
		{"intent-register", "--workspace", "<work>", "--assignment", a1, "--relationship", rid, "--dispatch-request-id", "dispatch-1", "--db-path", "<state>/relay.sqlite3"},
		{"intent-claim", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--dispatch-request-id", "dispatch-1", "--first-turn", "turn-1"},
		{"intent-claim", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--dispatch-request-id", "dispatch-1", "--first-turn", "turn-1"},
		{"intent-claim", "--workspace", "<work>", "--assignment", a1, "--session", "../escape", "--dispatch-request-id", "dispatch-1"},
		{"intent-claim", "--workspace", "<work>", "--assignment", a1, "--session", "01second", "--dispatch-request-id", "dispatch-9"},
		{"intent-claim", "--workspace", "<work>", "--assignment", a2, "--session", "01child-two", "--dispatch-request-id", "dispatch-2"},
		{"intent-disposition", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--turn", "turn-1", "--outcome", "ready_for_review"},
		{"intent-disposition", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--turn", "turn-1", "--outcome", "ready_for_review"},
		{"intent-disposition", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--turn", "turn-1", "--outcome", "failed"},
		{"intent-disposition", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--turn", "turn-2", "--outcome", "done"},
		// A store already holding another outcome for this turn: store_disagrees, first one stands.
		{"!sql", "INSERT INTO turn_declarations VALUES ('" + a1 + "', '01child-task', 'turn-3', 'failed', 'x', 'y')"},
		{"intent-disposition", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--turn", "turn-3", "--outcome", "ready_for_review"},
		{"intent-disposition", "--workspace", "<work>", "--assignment", a2, "--session", "01child-two", "--turn", "turn-1", "--outcome", "in_progress"},
		{"intent-disposition", "--workspace", "<work>", "--assignment", a1, "--session", "01child-task", "--turn", "..", "--outcome", "failed"},
		{"intent-resolve", "--workspace", "<work>", "--assignment", a1, "--chosen-task", "01child-task", "--chosen-session", "01child-task", "--reason", "r", "--adjudicate", "conflicts/0"},
		{"intent-resolve", "--workspace", "<work>", "--assignment", a1, "--chosen-task", "01child-task", "--chosen-session", "01child-task", "--reason", "r", "--adjudicate", "conflicts/0=abc", "--adjudicate", "claims/x/claim.json=def"},
		{"intent-show", "--workspace", "<work>", "--assignment", a1, "--now", "2026-01-01T00:10:00+00:00"},
		{"intent-show", "--workspace", "<work>", "--session", "01child-task", "--now", "2026-01-01T01:00:00+00:00"},
		{"intent-show", "--workspace", "<work>", "--session", "01child-two", "--now", "2026-01-01T00:10:00+00:00"},
		{"intent-show", "--workspace", "<work>", "--marker-root", "<home>/elsewhere"},
		{"intent-show"},
	}
	for _, args := range cases {
		if args[0] == "!sql" {
			goSQLiteExec(t, filepath.Join(side.state, "relay.sqlite3"), args[1])
			continue
		}
		expand := func(s *cliSide) []string {
			out := make([]string, len(args))
			for j, a := range args {
				a = strings.ReplaceAll(a, "<work>", s.work)
				a = strings.ReplaceAll(a, "<state>", s.state)
				out[j] = strings.ReplaceAll(a, "<home>", s.home)
			}
			if !strings.Contains(strings.Join(out, " "), "--marker-root") && len(out) > 1 {
				out = append(out, "--marker-root", filepath.Join(s.home, "markers"))
			}
			return out
		}
		out, code := side.run(expand(side)...)
		side.expect("run "+strings.Join(args, " "), fmt.Sprintf("%d\n%s", code, loserProcess.ReplaceAllString(side.normal(out), `"loserProcess": "<pid>"`)))
	}
	// The mirrored store records.
	for _, table := range []string{"reporting_sessions", "turn_declarations"} {
		query := "SELECT * FROM " + table + " ORDER BY 1, 2, 3"
		rows := stamp.ReplaceAllString(side.normal(sqliteDump(t, side, query)), "<stamp>")
		side.expect("sqlite "+query, rows)
		if !strings.Contains(rows, "01child-task") {
			t.Errorf("%s holds no row of the child: %s", table, rows)
		}
	}
}

// sqliteDump is json.dumps([list(r) for r in rows]) of what query selects from the side's store.
func sqliteDump(t *testing.T, s *cliSide, query string) string {
	t.Helper()
	return goSQLiteDump(t, filepath.Join(s.state, "relay.sqlite3"), query)
}

// goSQLiteDump is what the Python one-liner json.dumps([list(r) for r in rows]) prints for a store
// read directly.
func goSQLiteDump(t *testing.T, path, query string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	mustDo(t, err)
	defer func() { mustDo(t, db.Close()) }()
	rows, err := db.Query(query)
	mustDo(t, err)
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	mustDo(t, err)
	out := []any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		mustDo(t, rows.Scan(pointers...))
		out = append(out, values)
	}
	mustDo(t, rows.Err())
	return dumps(out) + "\n"
}

// goSQLiteExec runs one statement on a store directly and commits it.
func goSQLiteExec(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	mustDo(t, err)
	_, err = db.Exec(statement)
	closeErr := db.Close()
	mustDo(t, err)
	mustDo(t, closeErr)
}
