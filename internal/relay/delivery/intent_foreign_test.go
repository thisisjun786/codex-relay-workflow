package delivery

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// intent-register --db-path naming a store the other runtime owns, as a completed takeover
// leaves it: the fence's registration_hold does not yield its Admission's OwnershipRefused as an
// unheld hold, and register_relationship re-raises it, so both runtimes answer reason
// store_owned_by_other in the fence's words, exit 2, and publish no relationship fact. Each side
// runs its own real CLI over its own store; the marker facts before and after are compared too.
// The commands are the marker-only forms (--no-db-path, --db-path), which cli.py's
// _reads_no_selected_store exempts from the check_start preflight, so the hold itself answers.
func TestCLI_intent_register_refuses_a_store_the_other_runtime_owns_like_python(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	py, gosd := newSide(t, true, work), newSide(t, false, work)
	rid := regexp.MustCompile(`rel-[0-9a-f]{16}`).FindString(sqliteDump(t, py, "SELECT relationship_id FROM relationships"))
	if rid == "" {
		t.Fatal("no seeded relationship")
	}
	testsupport.HandOver(t, filepath.Join(py.state, "relay.sqlite3"), "go")
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

// The two marker forms that name the selected store - intent-declare recording it (no
// --no-db-path) and intent-register confirming against it (no --db-path) - are not exempt from
// cli.py main's check_start: on a store the other runtime owns, or one draining, both runtimes
// refuse before any marker write. Each side runs its own real CLI over its own store; stdout,
// exit, the marker root (never created) and the state directory (unchanged) are compared.
func TestCLI_intent_writers_naming_the_store_refuse_before_any_marker_like_python(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	py, gosd := newSide(t, true, work), newSide(t, false, work)
	rid := regexp.MustCompile(`rel-[0-9a-f]{16}`).FindString(sqliteDump(t, py, "SELECT relationship_id FROM relationships"))
	if rid == "" {
		t.Fatal("no seeded relationship")
	}
	assignment := AssignmentID("dispatch-1")
	cases := [][]string{
		{"intent-declare", "--workspace", "<work>", "--dispatch-request-id", "dispatch-1", "--issue", "REL-1", "--declared-at", "2026-01-01T00:00:00+00:00"},
		{"intent-register", "--workspace", "<work>", "--assignment", assignment, "--relationship", rid, "--dispatch-request-id", "dispatch-1"},
	}
	compare := func(stage string) {
		t.Helper()
		for _, args := range cases {
			answers := map[string]string{}
			codes := map[string]int{}
			for name, s := range map[string]*cliSide{"python": py, "go": gosd} {
				markers := filepath.Join(s.home, "markers")
				expanded := []string{}
				for _, a := range args {
					expanded = append(expanded, strings.ReplaceAll(a, "<work>", s.work))
				}
				before := tree(t, s.state)
				out, code := s.run(append(expanded, "--marker-root", markers)...)
				answers[name], codes[name] = s.normal(out), code
				if after := tree(t, s.state); after != before {
					t.Errorf("%s %s %s changed the state directory\nbefore %s\nafter  %s", stage, name, args[0], before, after)
				}
				if _, err := os.Lstat(markers); !os.IsNotExist(err) {
					t.Errorf("%s %s %s wrote the marker root (%v)", stage, name, args[0], err)
				}
			}
			if codes["python"] != 2 || codes["go"] != 2 || answers["python"] != answers["go"] || !strings.Contains(answers["go"], `"reason": "store_owned_by_other"`) {
				t.Errorf("%s %s: exit python %d go %d\npython:\n%s\ngo:\n%s", stage, args[0], codes["python"], codes["go"], answers["python"], answers["go"])
			}
		}
	}
	testsupport.HandOver(t, filepath.Join(py.state, "relay.sqlite3"), "go")
	testsupport.HandOver(t, filepath.Join(gosd.state, "relay.sqlite3"), "python")
	compare("owned by the other runtime")
	// Back to each side's own runtime, then draining: its own writers still refuse.
	testsupport.HandOver(t, filepath.Join(py.state, "relay.sqlite3"), "python")
	testsupport.HandOver(t, filepath.Join(gosd.state, "relay.sqlite3"), "go")
	for _, s := range []*cliSide{py, gosd} {
		mirror := filepath.Join(s.state, "takeover.json")
		raw, err := os.ReadFile(mirror)
		mustDo(t, err)
		var record map[string]any
		mustDo(t, json.Unmarshal(raw, &record))
		// A drain names the transition it is draining for: out of this owner, one epoch on.
		to := map[string]string{"python": "go", "go": "python"}[record["owner"].(string)]
		epoch, _ := record["epoch"].(float64)
		record["phase"] = "draining"
		record["transition"] = map[string]any{"id": "t32-drain", "from": record["owner"], "to": to, "targetEpoch": epoch + 1}
		raw, err = json.Marshal(record)
		mustDo(t, err)
		mustDo(t, os.WriteFile(mirror, raw, 0o600))
	}
	compare("draining")
}

// tree lists every entry under root with its size and content digest, for comparing a
// directory before and after a command.
func tree(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	mustDo(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			entries = append(entries, rel+"/")
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("%s:%x", rel, sha256.Sum256(raw)))
		return nil
	}))
	return strings.Join(entries, " ")
}
