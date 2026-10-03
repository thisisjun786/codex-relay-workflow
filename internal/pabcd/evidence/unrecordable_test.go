package evidence

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testdata/oracle-unrecordable.json holds what the CXC v0.2.40 oracle answered for the cases of testdata/cases-unrecordable.json,
// recorded once by testdata/record-oracle-unrecordable.mjs under Node 24 (no Node runs here). Each case runs in a fresh directory
// with the clock frozen at 2026-01-01T00:00:00.000Z; "{S}" in a case is the state directory name (.codexclaw in the oracle, .crw
// here), "<CWD>" and "<OUT>" the workspace and an outside directory. A tree lists the state directory without following links,
// with the permission bits of what is not a link, recorded and replayed under the umask 022.

type unrecSetup struct {
	File, Dir, Symlink, Chmod, Text, To, Mode, Pre, Post string
	Deep                                                 int
}

type treeEntry struct{ Path, Type, Mode, Text, To string }

type unrecCases struct {
	Marker []struct {
		ID    string
		Setup []unrecSetup
		Calls []struct{ Session, Agent string }
	}
	Status []struct {
		ID      string
		Session *string
		Setup   []unrecSetup
	}
	Budget []struct {
		ID      string
		Session *string
		Setup   []unrecSetup
	}
	Resolve []struct {
		ID             string
		StateRaw       *string
		StateDirIsFile bool
		Lock           bool
		Payloads       []map[string]any
	}
}

type unrecGolden struct {
	Constant string
	Marker   map[string]struct {
		Threw     []any
		Tree, Out []treeEntry
	}
	Status map[string]struct {
		Present, Unreadable bool
		Tree, Out           []treeEntry
	}
	Budget map[string]struct {
		Spent bool
		Tree  []treeEntry
	}
	Resolve map[string]struct {
		Returns  []bool
		Seed     *string
		State    map[string]any
		Sessions []string
	}
	Directives struct {
		Escalation string
		Verifier   []*struct{ Decision, Reason string }
	}
}

func loadUnrec(t *testing.T) (unrecCases, unrecGolden) {
	t.Helper()
	var c unrecCases
	var g unrecGolden
	for name, into := range map[string]any{"cases-unrecordable.json": &c, "oracle-unrecordable.json": &g} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err == nil {
			err = json.Unmarshal(raw, into)
		}
		if err != nil {
			t.Fatal(name, err)
		}
	}
	return c, g
}

func sessionOf(s *string) string {
	if s == nil {
		return "s1"
	}
	return *s
}

// fixUmask makes the permission bits of what the units create the ones the oracle recording shows.
func fixUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

// caseDirs makes the workspace and the outside directory of a case.
func caseDirs(t *testing.T) (cwd, out string) {
	t.Helper()
	root := t.TempDir()
	cwd, out = filepath.Join(root, "ws"), filepath.Join(root, "out")
	must(t, os.MkdirAll(cwd, 0o777))
	must(t, os.MkdirAll(out, 0o777))
	return cwd, out
}

// stage applies the setup of a case and returns what restores the modes it dropped, so the directory can be removed afterwards.
// A case that drops a permission says nothing as root, which ignores it: skip is true there.
func stage(t *testing.T, ops []unrecSetup, cwd, out string) (restore func(), skip bool) {
	t.Helper()
	at := func(p string) string {
		if p = sub(p, cwd, out); filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(cwd, p)
	}
	var dropped []string
	if os.Geteuid() == 0 && slices.ContainsFunc(ops, func(o unrecSetup) bool { return o.Chmod != "" }) {
		return func() {}, true
	}
	for _, o := range ops {
		switch {
		case o.Dir != "":
			must(t, os.MkdirAll(at(o.Dir), 0o777))
		case o.File != "":
			text := o.Text
			if o.Deep > 0 {
				text = o.Pre + strings.Repeat("[", o.Deep) + strings.Repeat("]", o.Deep) + o.Post
			}
			put(t, at(o.File), []byte(text))
		case o.Symlink != "":
			must(t, os.MkdirAll(filepath.Dir(at(o.Symlink)), 0o777))
			must(t, os.Symlink(sub(o.To, cwd, out), at(o.Symlink)))
		case o.Chmod != "":
			mode, err := strconv.ParseUint(o.Mode, 8, 32)
			must(t, err)
			must(t, os.Chmod(at(o.Chmod), os.FileMode(mode)))
			dropped = append(dropped, at(o.Chmod))
		}
	}
	return func() {
		for _, p := range dropped {
			must(t, os.Chmod(p, 0o700))
		}
	}, false
}

// treeOf lists what lies under root, links not followed, with the text of the marker and counter files. A root that is not a
// directory lists nothing.
func treeOf(root, cwd, out string) []treeEntry {
	entries := []treeEntry{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		rel, relErr := filepath.Rel(root, p)
		if err != nil || relErr != nil || rel == "." {
			return nil
		}
		e := treeEntry{Path: filepath.ToSlash(rel), Type: "file"}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			e.Type, e.To = "link", strings.NewReplacer(out, "<OUT>", cwd, "<CWD>").Replace(target)
		default:
			if info, err := d.Info(); err == nil {
				e.Mode = strconv.FormatUint(uint64(info.Mode().Perm()), 8)
			}
			if d.IsDir() {
				e.Type = "dir"
			} else if strings.Contains(e.Path, UnrecordableSubdir+"/") || strings.Contains(e.Path, AttemptsSubdir+"/") {
				raw, _ := os.ReadFile(p)
				e.Text = string(raw)
			}
		}
		entries = append(entries, e)
		return nil
	})
	return entries
}

func TestUnrecordableSubdir(t *testing.T) {
	if _, g := loadUnrec(t); UnrecordableSubdir != g.Constant {
		t.Errorf("%q, want %q", UnrecordableSubdir, g.Constant)
	}
}

func TestWriteUnrecordableMarker(t *testing.T) {
	// Intentionally changed (the security fix): the oracle follows a link at the state directory or at the marker directory and
	// creates the marker, the directory and the probe in the directory it leads to, outside the workspace. The port refuses: the
	// write fails, the status denies, and nothing appears outside.
	c, g := loadUnrec(t)
	fixUmask(t)
	now := time.UnixMilli(1767225600000)
	for _, k := range c.Marker {
		cwd, out := caseDirs(t)
		restore, skip := stage(t, k.Setup, cwd, out)
		if skip {
			continue
		}
		threw := []bool{}
		for _, call := range k.Calls {
			threw = append(threw, writeUnrecordableMarker(cwd, call.Session, call.Agent, now) != nil)
		}
		restore()
		want, wantThrew := g.Marker[k.ID], []bool{}
		if linkFollowed(k.ID) {
			want.Threw, want.Out = []any{true}, []treeEntry{}
			if k.ID == "state_dir_is_symlink" {
				want.Tree = []treeEntry{}
			}
		}
		for _, v := range want.Threw {
			wantThrew = append(wantThrew, v != false)
		}
		same(t, k.ID+" threw", threw, wantThrew)
		same(t, k.ID+" tree", treeOf(filepath.Join(cwd, ".crw"), cwd, out), want.Tree)
		same(t, k.ID+" outside", treeOf(out, cwd, out), want.Out)
	}
}

func TestUnrecordableVerdictStatus(t *testing.T) {
	c, g := loadUnrec(t)
	fixUmask(t)
	for _, k := range c.Status {
		cwd, out := caseDirs(t)
		restore, skip := stage(t, k.Setup, cwd, out)
		if skip {
			continue
		}
		got := UnrecordableVerdictStatus(cwd, sessionOf(k.Session))
		restore()
		want := g.Status[k.ID]
		if linkFollowed(k.ID) {
			want.Present, want.Unreadable, want.Out = false, true, []treeEntry{}
			if k.ID == "state_dir_is_symlink" {
				want.Tree = []treeEntry{}
			}
		}
		if got != (VerdictStatus{Present: want.Present, Unreadable: want.Unreadable}) {
			t.Errorf("%s: %+v, want present %v unreadable %v", k.ID, got, want.Present, want.Unreadable)
		}
		// The probe leaves nothing behind, and the query creates the state and marker directories as the oracle's does.
		same(t, k.ID+" tree", treeOf(filepath.Join(cwd, ".crw"), cwd, out), want.Tree)
		same(t, k.ID+" outside", treeOf(out, cwd, out), want.Out) // a probe never stays in a directory a link leads to
	}
}

func TestHasSpentBudget(t *testing.T) {
	c, g := loadUnrec(t)
	// A counter whose JSON nests deeper than Go's limit of 10,000 levels reads as spent, where the oracle, which parses it, finds
	// the attempts beside the nesting unspent. The port ends on the fail-closed side.
	port := map[string]bool{"raw_deep_sibling": true}
	fixUmask(t)
	for _, k := range c.Budget {
		cwd, out := caseDirs(t)
		restore, skip := stage(t, k.Setup, cwd, out)
		if skip {
			continue
		}
		got := HasSpentBudget(cwd, sessionOf(k.Session))
		restore()
		want := g.Budget[k.ID]
		if p, ok := port[k.ID]; ok {
			want.Spent = p
		}
		if got != want.Spent {
			t.Errorf("%s: %v, want %v", k.ID, got, want.Spent)
		}
		same(t, k.ID+" tree", treeOf(filepath.Join(cwd, ".crw"), cwd, out), want.Tree) // it writes nothing
	}
}

// stateOf is a session file as a map without its updatedAt; text that is not JSON is {"raw": text}.
func stateOf(raw []byte) map[string]any {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		m = map[string]any{"raw": string(raw)}
	}
	delete(m, "updatedAt")
	return m
}

// TestResolveTombstone replays the oracle's resolveTombstone. Four cases are intentionally changed. Three are the data-loss fix:
// the oracle writes back the verdicts its read kept, so the 65th and 66th of a list past the cap, and an entry it cannot parse,
// are lost from the file; the port writes nothing when the file holds more verdicts than the read kept, and the call reports
// false. The fourth, no_agent_id_clears_idless_tombstones, is the security fix: the oracle removes every tombstone with an empty
// agent id and the turn of a payload that has no agent id, erasing the verdicts of other agents whose ids were missing; the port
// refuses, writes nothing and reports false.
func TestResolveTombstone(t *testing.T) {
	c, g := loadUnrec(t)
	for _, k := range c.Resolve {
		cwd := t.TempDir()
		sessions := filepath.Join(cwd, ".crw", "sessions")
		want := g.Resolve[k.ID]
		if want.Seed != nil {
			put(t, filepath.Join(sessions, "s1.json"), []byte(*want.Seed))
		}
		if k.StateRaw != nil {
			put(t, filepath.Join(sessions, "s1.json"), []byte(*k.StateRaw))
		}
		if k.StateDirIsFile {
			put(t, filepath.Join(cwd, ".crw"), []byte("x"))
		}
		if k.Lock {
			put(t, filepath.Join(sessions, "s1.json.lock"), []byte("12345"))
		}
		path := filepath.Join(sessions, "s1.json")
		before, _ := os.ReadFile(path)
		returns := []bool{}
		for _, o := range k.Payloads {
			str := func(key string) string { s, _ := o[key].(string); return s }
			returns = append(returns, ResolveTombstone(cwd, "s1", Payload{AgentType: str("agent_type"), AgentID: str("agent_id"), TurnID: str("turn_id")}))
		}
		var gotState map[string]any
		after, err := os.ReadFile(path)
		if err == nil {
			gotState = stateOf(after)
		}
		var gotSessions []string
		if entries, err := os.ReadDir(sessions); err == nil {
			gotSessions = []string{}
			for _, e := range entries {
				gotSessions = append(gotSessions, e.Name())
			}
			slices.Sort(gotSessions)
		}
		if slices.Contains([]string{"sixty_six_entries_resolve_first", "malformed_entry_beside_resolved", "uppercase_key_hides_the_overflow", "no_agent_id_clears_idless_tombstones"}, k.ID) {
			want.Returns, want.State = []bool{false}, stateOf(before)
		}
		if !slices.Contains(want.Returns, true) && before != nil && !bytes.Equal(after, before) {
			t.Errorf("%s: nothing was resolved and the file changed: %q, was %q", k.ID, after, before)
		}
		same(t, k.ID+" returns", returns, want.Returns)
		same(t, k.ID+" state", gotState, want.State)
		same(t, k.ID+" sessions", gotSessions, want.Sessions)
	}
}

// linkFollowed names the recorded cases in which the oracle follows a symbolic link out of the workspace.
func linkFollowed(id string) bool {
	return slices.Contains([]string{"marker_dir_is_symlink_out", "marker_dir_symlink_to_dir", "state_dir_is_symlink"}, id)
}
