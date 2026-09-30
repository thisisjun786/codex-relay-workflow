package delivery

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// intent-register --db-path naming a store the other runtime owns, as a completed takeover
// leaves it: the fence's registration_hold does not yield its Admission's OwnershipRefused as an
// unheld hold, and register_relationship re-raises it, so both runtimes answer reason
// store_owned_by_other in the fence's words, exit 2, and publish no relationship fact. Each side
// runs its own real CLI over its own store; the marker facts before and after are compared too.
// The commands are the marker-only forms (--no-db-path, --db-path), which cli.py's
// _reads_no_selected_store exempts from the check_start preflight, so the hold itself answers.
func TestCLI_intent_register_refuses_a_store_the_other_runtime_owns_like_python(t *testing.T) {
	work := filepath.Join(parityTree(t), "work")
	py, gosd := newSide(t, true, work), newSide(t, false, work)
	rid := regexp.MustCompile(`rel-[0-9a-f]{16}`).FindString(sqliteDump(t, py, "SELECT relationship_id FROM relationships"))
	if rid == "" {
		t.Fatal("no seeded relationship")
	}
	if pyoracle.Live() {
		testsupport.HandOver(t, filepath.Join(py.state, "relay.sqlite3"), "go")
	}
	testsupport.HandOver(t, filepath.Join(gosd.state, "relay.sqlite3"), "python")
	assignment := AssignmentID("dispatch-1")
	cases := [][]string{
		{"intent-declare", "--workspace", "<work>", "--dispatch-request-id", "dispatch-1", "--issue", "REL-1", "--declared-at", "2026-01-01T00:00:00+00:00", "--no-db-path"},
		{"intent-bind", "--workspace", "<work>", "--assignment", assignment, "--session", "01child-task", "--task-id", "01child-task"},
		{"intent-register", "--workspace", "<work>", "--assignment", assignment, "--relationship", rid, "--dispatch-request-id", "dispatch-1", "--db-path", "<state>/relay.sqlite3"},
		{"intent-show", "--workspace", "<work>", "--assignment", assignment, "--now", "2026-01-01T00:10:00+00:00"},
	}
	var registered string
	for i, args := range cases {
		expand := func(s *cliSide) []string {
			out := make([]string, 0, len(args)+2)
			for _, a := range args {
				out = append(out, strings.NewReplacer("<work>", s.work, "<state>", s.state).Replace(a))
			}
			return append(out, "--marker-root", filepath.Join(s.home, "markers"))
		}
		pout, pcode := py.run(expand(py)...)
		gout, gcode := gosd.run(expand(gosd)...)
		pn, gn := py.normal(pout), gosd.normal(gout)
		if pcode != gcode || pn != gn {
			t.Fatalf("case %d %v: exit python %d go %d\npython:\n%s\ngo:\n%s", i, args, pcode, gcode, pn, gn)
		}
		if args[0] == "intent-register" {
			registered = pn
			if pcode != 2 {
				t.Fatalf("register exit %d:\n%s", pcode, pn)
			}
		}
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(registered), &answer); err != nil {
		t.Fatal(err)
	}
	if answer["error"] != "refused" || answer["reason"] != "store_owned_by_other" {
		t.Fatalf("register answered %s", registered)
	}
}
