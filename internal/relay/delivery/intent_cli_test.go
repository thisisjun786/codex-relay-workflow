package delivery

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The eight intent-* marker commands through the real processes: Python's codex-session-relay
// and the built `crw relay`, each with its own home, marker root and seeded store, over one
// shared workspace path (the workspace key hashes it). Every stdout is compared whole after the
// wall-clock stamps, each side's home and each side's pid (loserProcess) become tokens; exit
// codes are compared exactly.

var loserProcess = regexp.MustCompile(`"loserProcess": "\d+"`)

func TestCLI_every_intent_command_answers_byte_for_byte_like_python(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	py, gosd := newSide(t, true, work), newSide(t, false, work)
	rid := regexp.MustCompile(`rel-[0-9a-f]{16}`).FindString(sqliteDump(t, py, "SELECT relationship_id FROM relationships"))
	if rid == "" || !strings.Contains(sqliteDump(t, gosd, "SELECT relationship_id FROM relationships"), rid) {
		t.Fatal("both sides are seeded with the same relationship")
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
	for i, args := range cases {
		if args[0] == "!sql" {
			for _, side := range []*cliSide{py, gosd} {
				cmd := execUV(side, "python", "-c", "import sqlite3,sys;c=sqlite3.connect(sys.argv[1]);c.execute(sys.argv[2]);c.commit()", filepath.Join(side.state, "relay.sqlite3"), args[1])
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}
			}
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
		pout, pcode := py.run(expand(py)...)
		gout, gcode := gosd.run(expand(gosd)...)
		pn := loserProcess.ReplaceAllString(py.normal(pout), `"loserProcess": "<pid>"`)
		gn := loserProcess.ReplaceAllString(gosd.normal(gout), `"loserProcess": "<pid>"`)
		if os.Getenv("CRW_SHOW") != "" {
			t.Logf("case %d exit %d\n%s", i, pcode, pn)
		}
		if pcode != gcode || pn != gn {
			t.Errorf("case %d %v: exit python %d go %d\npython:\n%s\ngo:\n%s", i, args, pcode, gcode, pn, gn)
		}
	}
	// The mirrored store records: the same rows on both sides.
	for _, table := range []string{"reporting_sessions", "turn_declarations"} {
		query := "SELECT * FROM " + table + " ORDER BY 1, 2, 3"
		pr, gr := py.normal(sqliteDump(t, py, query)), gosd.normal(sqliteDump(t, gosd, query))
		pr, gr = stamp.ReplaceAllString(pr, "<stamp>"), stamp.ReplaceAllString(gr, "<stamp>")
		if pr != gr || !strings.Contains(pr, "01child-task") {
			t.Errorf("%s differs\npython: %s\ngo:     %s", table, pr, gr)
		}
	}
}

func execUV(s *cliSide, args ...string) *exec.Cmd {
	cmd := exec.Command("uv", append([]string{"run", "--no-sync"}, args...)...)
	cmd.Dir = filepath.Join(repoRoot(s.t), "packages", "codex-session-relay")
	cmd.Env = s.env
	return cmd
}

func sqliteDump(t *testing.T, s *cliSide, query string) string {
	t.Helper()
	cmd := execUV(s, "python", "-c", "import sqlite3,sys,json;print(json.dumps([list(r) for r in sqlite3.connect(sys.argv[1]).execute(sys.argv[2])]))", filepath.Join(s.state, "relay.sqlite3"), query)
	out, err := cmd.Output()
	mustDo(t, err)
	return string(out)
}
