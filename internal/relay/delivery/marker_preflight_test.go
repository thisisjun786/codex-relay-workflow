package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The ownership checks the fence makes before a marker command writes anything, and where it
// makes none. cli.py main runs check_start before intent-declare recording the selected store
// and intent-register confirming against it; cmd_intent_claim runs it on the store the intent
// names, and intent-disposition opens that store (declarations.Held) before publishing. On a
// store the other runtime owns, each refuses with no marker fact written (a Go store's mirror
// phase is no longer judged, decision 56, so its draining case is gone). A
// legacy store (no ownership key, no mirror) passes check_start, so intent-declare records it.
// A broken store answers what check_start raises: an unreadable mirror refuses in the fence's
// words, an unreadable database is the host error.
//
// testdata/marker_preflight.json holds Python's answers, captured once from the live Python
// console script before todo 44 removed it; the suite runs only the Go CLI against them.

const markerDispatch = "dispatch-1"

type markerCase struct {
	name string
	// build shapes the side's state directory (and marker root) before the command. own is
	// the side's runtime, other the other one.
	build   func(t *testing.T, s *cliSide, own, other string)
	command []string
}

// markerOutcome is what a case compares: the normalized stdout, the exit code, every marker
// file afterwards (relative to the marker root, the workspace key normalized) and whether any
// byte of the state directory changed.
type markerOutcome struct {
	Exit         int      `json:"exit"`
	Stdout       string   `json:"stdout"`
	Markers      []string `json:"markers"`
	StateChanged bool     `json:"stateChanged"`
}

func markerArgs(name string, extra ...string) []string {
	base := []string{name, "--workspace", "<work>"}
	switch name {
	case "intent-declare":
		base = append(base, "--dispatch-request-id", markerDispatch, "--issue", "REL-1", "--declared-at", "2026-01-01T00:00:00+00:00")
	case "intent-register":
		base = append(base, "--assignment", AssignmentID(markerDispatch), "--relationship", "rel-0123456789abcdef", "--dispatch-request-id", markerDispatch)
	case "intent-claim":
		base = append(base, "--assignment", AssignmentID(markerDispatch), "--session", "01child-task", "--dispatch-request-id", markerDispatch)
	case "intent-disposition":
		base = append(base, "--assignment", AssignmentID(markerDispatch), "--session", "01child-task", "--turn", "t1", "--outcome", "in_progress")
	}
	return append(base, extra...)
}

func markerDB(s *cliSide) string { return filepath.Join(s.state, "relay.sqlite3") }

// declared is a store this side owns that an intent then names: intent-declare recorded it.
func declared(t *testing.T, s *cliSide, own string) {
	t.Helper()
	testsupport.Create(t, markerDB(s), "", own)
	if out, code := s.run(s.expand(markerArgs("intent-declare"))...); code != 0 {
		t.Fatalf("declare the intent: exit %d\n%s", code, out)
	}
}

func markerCases() []markerCase {
	foreign := func(t *testing.T, s *cliSide, _, other string) { testsupport.Create(t, markerDB(s), "", other) }
	handedOver := func(t *testing.T, s *cliSide, own, other string) {
		declared(t, s, own)
		testsupport.HandOver(t, markerDB(s), other)
	}
	legacy := func(t *testing.T, s *cliSide, _, _ string) {
		raw, err := os.ReadFile(filepath.Join(repoRoot(t), "contract", "fixtures", "sqlite-ddl", "python-store.sqlite3"))
		mustDo(t, err)
		mustDo(t, os.MkdirAll(s.state, 0o700))
		mustDo(t, os.WriteFile(markerDB(s), raw, 0o600))
	}
	stateFile := func(t *testing.T, s *cliSide, _, _ string) {
		mustDo(t, os.WriteFile(s.state, []byte("not a directory"), 0o600))
	}
	stateUnreadable := func(t *testing.T, s *cliSide, own, _ string) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 directory")
		}
		testsupport.Create(t, markerDB(s), "", own)
		mustDo(t, os.Chmod(s.state, 0))
		t.Cleanup(func() { _ = os.Chmod(s.state, 0o700) })
	}
	mirrorCorrupt := func(t *testing.T, s *cliSide, own, _ string) {
		testsupport.Create(t, markerDB(s), "", own)
		mustDo(t, os.WriteFile(filepath.Join(s.state, "takeover.json"), []byte("{not json"), 0o600))
	}
	mirrorDirectory := func(t *testing.T, s *cliSide, own, _ string) {
		testsupport.Create(t, markerDB(s), "", own)
		mustDo(t, os.Remove(filepath.Join(s.state, "takeover.json")))
		mustDo(t, os.Mkdir(filepath.Join(s.state, "takeover.json"), 0o700))
	}
	garbage := func(t *testing.T, s *cliSide, _, _ string) {
		mustDo(t, os.MkdirAll(s.state, 0o700))
		mustDo(t, os.WriteFile(markerDB(s), []byte("this is not a database, only its name is\n"), 0o600))
	}
	// The intent names a store that turns into one of the broken ones after it was declared.
	declaredThen := func(broken func(*testing.T, *cliSide, string, string)) func(*testing.T, *cliSide, string, string) {
		return func(t *testing.T, s *cliSide, own, other string) {
			declared(t, s, own)
			mustDo(t, os.RemoveAll(s.state))
			broken(t, s, own, other)
		}
	}
	// The intent names a store at another path: one no user's home can expand, or one absent.
	renamed := func(dbPath string) func(*testing.T, *cliSide, string, string) {
		return func(t *testing.T, s *cliSide, own, _ string) {
			declared(t, s, own)
			intent := filepath.Join(s.home, "markers", s.workspaceKey(), AssignmentID(markerDispatch), "intent.json")
			raw, err := os.ReadFile(intent)
			mustDo(t, err)
			var fact map[string]any
			mustDo(t, json.Unmarshal(raw, &fact))
			fact["dbPath"] = strings.ReplaceAll(dbPath, "<home>", s.home)
			raw, err = json.Marshal(fact)
			mustDo(t, err)
			mustDo(t, os.WriteFile(intent, raw, 0o600))
		}
	}
	var cases []markerCase
	for _, command := range []string{"intent-declare", "intent-register"} {
		short := strings.TrimPrefix(command, "intent-")
		cases = append(cases,
			markerCase{short + "/owned-by-the-other-runtime", foreign, markerArgs(command)},
			markerCase{short + "/state-is-a-file", stateFile, markerArgs(command)},
			markerCase{short + "/state-unreadable", stateUnreadable, markerArgs(command)},
			markerCase{short + "/mirror-corrupt", mirrorCorrupt, markerArgs(command)},
			markerCase{short + "/mirror-a-directory", mirrorDirectory, markerArgs(command)},
			markerCase{short + "/database-garbage", garbage, markerArgs(command)},
		)
	}
	cases = append(cases, markerCase{"declare/legacy-store", legacy, markerArgs("intent-declare")})
	for _, command := range []string{"intent-claim", "intent-disposition"} {
		short := strings.TrimPrefix(command, "intent-")
		cases = append(cases,
			markerCase{short + "/intent-store-owned-by-the-other-runtime", handedOver, markerArgs(command)},
			markerCase{short + "/intent-store-mirror-corrupt", declaredThen(mirrorCorrupt), markerArgs(command)},
			markerCase{short + "/intent-store-database-garbage", declaredThen(garbage), markerArgs(command)},
			markerCase{short + "/intent-store-state-unreadable", declaredThen(stateUnreadable), markerArgs(command)},
			markerCase{short + "/intent-store-unknown-user", renamed("~crw-t32-no-such-user/relay.sqlite3"), markerArgs(command)},
			markerCase{short + "/intent-store-absent", renamed("<home>/elsewhere/relay.sqlite3"), markerArgs(command)},
			// str.strip() leaves nothing of an information separator: the intent names no store.
			markerCase{short + "/intent-store-separator", renamed("\x1f"), markerArgs(command)},
		)
	}
	return cases
}

// markerSide is the Go CLI over a home of its own: no seed, the state directory and the marker
// root left for each case to shape.
func markerSide(t *testing.T) *cliSide {
	t.Helper()
	home := t.TempDir()
	s := &cliSide{t: t, home: home, state: filepath.Join(home, "state"), work: filepath.Join(home, "work")}
	s.env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xs"), "XDG_DATA_HOME="+filepath.Join(home, "xd"), "XDG_CONFIG_HOME="+filepath.Join(home, "xc"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home, "CODEX_SESSION_RELAY_STATE=")
	s.argv0 = []string{crwBinary(t), "relay"}
	s.dir = repoRoot(t)
	mustDo(t, os.MkdirAll(s.work, 0o700))
	return s
}

func (s *cliSide) expand(args []string) []string {
	out := make([]string, 0, len(args)+2)
	for _, a := range args {
		out = append(out, strings.ReplaceAll(a, "<work>", s.work))
	}
	return append(out, "--marker-root", filepath.Join(s.home, "markers"))
}

// workspaceKey is marker.workspace_key of the side's workspace.
func (s *cliSide) workspaceKey() string {
	resolved, err := filepath.EvalSymlinks(s.work)
	mustDo(s.t, err)
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])
}

// runMarkerCase builds the case on the side, runs its command and reports the outcome.
func runMarkerCase(t *testing.T, s *cliSide, c markerCase) markerOutcome {
	t.Helper()
	c.build(t, s, "go", "python")
	before := stateListing(s.state)
	out, code := s.run(s.expand(c.command)...)
	key := s.workspaceKey()
	outcome := markerOutcome{Exit: code, Stdout: strings.ReplaceAll(s.normal(out), key, "<workspace-key>"), Markers: []string{}}
	outcome.StateChanged = stateListing(s.state) != before
	markers := filepath.Join(s.home, "markers")
	_ = filepath.WalkDir(markers, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(markers, path)
			outcome.Markers = append(outcome.Markers, strings.ReplaceAll(rel, key, "<workspace-key>"))
		}
		return nil
	})
	slices.Sort(outcome.Markers)
	return outcome
}

// stateListing is every entry under root with its content digest, or the error that stopped the
// walk, for comparing a state directory before and after a command.
func stateListing(root string) string {
	var entries []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if !d.Type().IsRegular() {
			entries = append(entries, rel+"/"+d.Type().String())
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("%s:%x", rel, sha256.Sum256(raw)))
		return nil
	})
	if err != nil {
		entries = append(entries, "error:"+err.Error())
	}
	return strings.Join(entries, " ")
}

func readMarkerGolden(t *testing.T) map[string]markerOutcome {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "marker_preflight.json"))
	mustDo(t, err)
	golden := map[string]markerOutcome{}
	mustDo(t, json.Unmarshal(raw, &golden))
	return golden
}

func TestCLI_marker_preflight_answers_what_python_answers(t *testing.T) {
	golden := readMarkerGolden(t)
	cases := markerCases()
	if len(golden) != len(cases) {
		t.Errorf("golden has %d cases, the table %d: the golden is Python's frozen answer (its live capture left with Python in todo 44), so a new case needs its expected outcome added to testdata/marker_preflight.json by hand", len(golden), len(cases))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			want, ok := golden[c.name]
			if !ok {
				t.Fatalf("no Python answer captured for %s", c.name)
			}
			got := runMarkerCase(t, markerSide(t), c)
			if !equalOutcome(got, want) {
				t.Errorf("go:\n%s\npython:\n%s", outcomeText(got), outcomeText(want))
			}
		})
	}
}

func equalOutcome(a, b markerOutcome) bool {
	return a.Exit == b.Exit && a.Stdout == b.Stdout && slices.Equal(a.Markers, b.Markers) && a.StateChanged == b.StateChanged
}

func outcomeText(o markerOutcome) string {
	return fmt.Sprintf("exit %d, state changed %v, markers %v\n%s", o.Exit, o.StateChanged, o.Markers, o.Stdout)
}

// intent-register goes on to open the store it confirms against, and a Go opener never
// initializes a legacy store (decision 30): where the fence would stamp and adopt it, Go's
// registration hold refuses it, before the marker root or any fact exists.
func TestCLI_intent_register_refuses_a_legacy_store_before_any_marker(t *testing.T) {
	var legacy markerCase
	for _, c := range markerCases() {
		if c.name == "declare/legacy-store" {
			legacy = c
		}
	}
	legacy.command = markerArgs("intent-register")
	side := markerSide(t)
	got := runMarkerCase(t, side, legacy)
	if got.Exit != 2 || got.StateChanged || !strings.Contains(got.Stdout, `"reason": "store_owned_by_other"`) {
		t.Fatalf("register on a legacy store:\n%s", outcomeText(got))
	}
	if _, err := os.Lstat(filepath.Join(side.home, "markers")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the marker root was created (%v)", err)
	}
}
