package delivery

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// intent-register --db-path naming a store the other runtime owns, as a completed takeover
// leaves it: the fence's registration_hold does not yield its Admission's OwnershipRefused as an
// unheld hold, and register_relationship re-raises it, so both runtimes answer reason
// store_owned_by_other in the fence's words, exit 2, and publish no relationship fact. The built
// CLI runs over its own store; every answer, the marker facts before and after included, is
// checked against the golden, which began as the Python console script's answers.
// The commands are the marker-only forms (--no-db-path, --db-path), which cli.py's
// _reads_no_selected_store exempts from the check_start preflight, so the hold itself answers.
func TestCLI_intent_register_refuses_a_store_the_other_runtime_owns_like_python(t *testing.T) {
	side := newSide(t, filepath.Join(parityTree(t), "work"))
	rid := regexp.MustCompile(`rel-[0-9a-f]{16}`).FindString(sqliteDump(t, side, "SELECT relationship_id FROM relationships"))
	side.expect("relationship id", rid)
	if rid == "" {
		t.Fatal("no seeded relationship")
	}
	testsupport.HandOver(t, filepath.Join(side.state, "relay.sqlite3"), "python")
	assignment := AssignmentID("dispatch-1")
	cases := [][]string{
		{"intent-declare", "--workspace", "<work>", "--dispatch-request-id", "dispatch-1", "--issue", "REL-1", "--declared-at", "2026-01-01T00:00:00+00:00", "--no-db-path"},
		{"intent-bind", "--workspace", "<work>", "--assignment", assignment, "--session", "01child-task", "--task-id", "01child-task"},
		{"intent-register", "--workspace", "<work>", "--assignment", assignment, "--relationship", rid, "--dispatch-request-id", "dispatch-1", "--db-path", "<state>/relay.sqlite3"},
		{"intent-show", "--workspace", "<work>", "--assignment", assignment, "--now", "2026-01-01T00:10:00+00:00"},
	}
	var registered string
	for _, args := range cases {
		expand := func(s *cliSide) []string {
			out := make([]string, 0, len(args)+2)
			for _, a := range args {
				out = append(out, strings.NewReplacer("<work>", s.work, "<state>", s.state).Replace(a))
			}
			return append(out, "--marker-root", filepath.Join(s.home, "markers"))
		}
		out, code := side.run(expand(side)...)
		normal := side.normal(out)
		side.expect("run "+strings.Join(args, " "), fmt.Sprintf("%d\n%s", code, normal))
		if args[0] == "intent-register" {
			registered = normal
			if code != 2 {
				t.Fatalf("register exit %d:\n%s", code, normal)
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
