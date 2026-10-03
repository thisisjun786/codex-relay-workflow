package interview

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// freezeEntry is one file of a recorded scenario, named relative to the state directory; freezeStep is one run of the command with
// what the oracle printed, the manifest it left (frozenAt as <TS>) and the tree below the workspace (the state directory written as
// a STATE placeholder).
type freezeEntry struct {
	Path   string
	Text   *string
	B64    *string
	Link   *string
	Dir    bool
	Remove bool
}

type freezeStep struct {
	Session  string
	DryRun   bool
	Edits    []freezeEntry
	Output   *string
	Error    bool
	Manifest *string
	Tree     []string
}

func (e freezeEntry) apply(t *testing.T, ws string) {
	t.Helper()
	p := filepath.Join(ws, ".crw", e.Path)
	var err error
	switch {
	case e.Remove:
		err = os.RemoveAll(p)
	case e.Dir:
		err = os.MkdirAll(p, 0o777)
	default:
		if err = os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			break
		}
		switch {
		case e.Link != nil:
			err = os.Symlink(*e.Link, p)
		case e.B64 != nil:
			var data []byte
			if data, err = base64.StdEncoding.DecodeString(*e.B64); err == nil {
				err = os.WriteFile(p, data, 0o666)
			}
		default:
			err = os.WriteFile(p, []byte(*e.Text), 0o666)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}

// freezeTree lists what is below ws as the recorder does: sorted, a directory with a trailing slash followed by its entries.
func freezeTree(t *testing.T, ws, rel string) (list []string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(ws, rel))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		r := filepath.ToSlash(filepath.Join(rel, e.Name()))
		if e.IsDir() {
			list = append(append(list, r+"/"), freezeTree(t, ws, r)...)
		} else {
			list = append(list, r)
		}
	}
	return list
}

// freezeFileReader is the session read of these tests: the slug and tracker of .crw/sessions/<id>.json, a default session when the
// file is absent or has no valid phase (state.ReadState, which this package cannot import: state imports it).
func freezeFileReader(cwd, id string) (string, *Tracker) {
	var m map[string]any
	raw, err := os.ReadFile(filepath.Join(cwd, ".crw", "sessions", id+".json"))
	if err != nil || json.Unmarshal(raw, &m) != nil {
		return "", nil
	}
	if phase, _ := m["phase"].(string); !slices.Contains([]string{"IDLE", "I", "P", "A", "B", "C", "D"}, phase) {
		return "", nil
	}
	slug, _ := m["slug"].(string)
	return slug, ReconstructInterview(m["interview"])
}

func freezeNoSession(string, string) (string, *Tracker) { return "", nil }

// Every recorded scenario is run step by step over the tree the oracle had: the printed text (through the corpus replayer's name
// table), the manifest's bytes, the tree it leaves and whether it failed must be the oracle's.
func TestFreezeRunsLikeTheRecordedOracle(t *testing.T) {
	sub, err := cxccorpus.LoadSubstitution(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ts := regexp.MustCompile("\"frozenAt\": \"[^\"]*\"")
	for id, sc := range freezeOracle(t).Scenarios {
		t.Run(id, func(t *testing.T) {
			ws := t.TempDir()
			for _, f := range sc.Files {
				f.apply(t, ws)
			}
			for i, step := range sc.Steps {
				for _, f := range step.Edits {
					f.apply(t, ws)
				}
				out, err := RunFreeze(FreezeCliArgs{Cwd: ws, SessionID: step.Session, DryRun: step.DryRun}, freezeFileReader)
				if (err != nil) != step.Error {
					t.Fatalf("step %d: error %v, oracle failed: %v", i, err, step.Error)
				}
				if step.Output != nil {
					if want := strings.ReplaceAll(sub.Expected(*step.Output), "$"+"{WS}", ws); out != want {
						t.Errorf("step %d output\n got %q\nwant %q", i, out, want)
					}
				}
				manifest, err := os.ReadFile(filepath.Join(ws, ".crw", "interview", "freeze.json"))
				if got := ts.ReplaceAllString(string(manifest), "\"frozenAt\": \"<TS>\""); (err == nil) != (step.Manifest != nil) || (step.Manifest != nil && got != *step.Manifest) {
					t.Errorf("step %d manifest (%v)\n got %q\nwant %v", i, err, got, step.Manifest)
				}
				tree := freezeTree(t, ws, "")
				for j, p := range tree {
					if strings.HasPrefix(p, ".crw") {
						tree[j] = "$" + "{STATE}" + strings.TrimPrefix(p, ".crw")
					}
				}
				if !slices.Equal(tree, step.Tree) {
					t.Errorf("step %d tree\n got %q\nwant %q", i, tree, step.Tree)
				}
			}
		})
	}
}

// parseFreezeArgs: the first index of a flag, the next element as its value even when it is a flag, help for an empty argv or any
// element help, --help or -h, and the kernel's working directory when --cwd gives none.
func TestFreezeParseArgsMatchesTheRecordedOracle(t *testing.T) {
	wd, err := syscall.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range freezeOracle(t).Argv {
		want := FreezeCliArgs{Cwd: wd, SessionID: c.SessionID, DryRun: c.DryRun, Help: c.Help}
		if c.Cwd != nil {
			want.Cwd = *c.Cwd
		}
		if got, err := ParseFreezeArgs(c.Argv); err != nil || got != want {
			t.Errorf("ParseFreezeArgs(%q) = %+v, %v; oracle %+v", c.Argv, got, err, want)
		}
	}
}

func freezeReadyTracker() *Tracker {
	tr := readyTracker()
	tr.Assumptions = append(tr.Assumptions, Assumption{Text: "Assume X", Recorded: true})
	return tr
}

// freeze.test.ts "freeze --dry-run produces a summary without writing", and the two L14.2 tests: the goal-activation directive
// follows the summary only when the interview is ready.
func TestFreezeSummaryAndDirective(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".crw", "plan", "demo"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".crw", "plan", "demo", "plan.md"), []byte("# Plan\n## OPEN ASSUMPTIONS\n- A1"), 0o666); err != nil {
		t.Fatal(err)
	}
	args := FreezeCliArgs{Cwd: ws, SessionID: "s1", DryRun: true}
	out, err := RunFreeze(args, func(string, string) (string, *Tracker) { return "demo", nil })
	for _, want := range []string{"planHash: ", "planFiles: 1", "[crw freeze --dry-run]", "interviewReady: false"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("summary lacks %q (%v):\n%s", want, err, out)
		}
	}
	if strings.Contains(out, GoalActivationDirective) {
		t.Error("a not-ready freeze must not surface the handoff")
	}
	if _, err := os.Stat(filepath.Join(ws, ".crw", "interview")); err == nil {
		t.Error("a dry run wrote the interview directory")
	}
	out, err = RunFreeze(args, func(string, string) (string, *Tracker) { return "demo", freezeReadyTracker() })
	if err != nil || !strings.Contains(out, "interviewReady: true") || !strings.Contains(out, "openAssumptions: 1") || !strings.HasSuffix(out, "\n\n"+GoalActivationDirective) {
		t.Errorf("a ready freeze must end with the handoff directive (%v):\n%s", err, out)
	}
}

// ListPlanFiles: a missing plan directory is no files, a plan directory that is a file an error.
func TestFreezeListPlanFilesEdges(t *testing.T) {
	ws := t.TempDir()
	if files, err := ListPlanFiles(filepath.Join(ws, "absent")); err != nil || len(files) != 0 {
		t.Errorf("an absent directory: %v, %v", files, err)
	}
	if err := os.WriteFile(filepath.Join(ws, "file"), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ListPlanFiles(filepath.Join(ws, "file")); err == nil {
		t.Error("a plan directory that is a file must fail")
	}
}

// The manifest is published, not rewritten in place: a second freeze replaces the file by rename (a new inode), keeps its mode and
// leaves no temp file, so a reader or a crash never sees a truncated manifest.
func TestFreezePublishesTheManifestAtomically(t *testing.T) {
	ws := t.TempDir()
	manifest := filepath.Join(ws, ".crw", "interview", "freeze.json")
	freeze := func() os.FileInfo {
		if _, err := RunFreeze(FreezeCliArgs{Cwd: ws, SessionID: "default"}, freezeNoSession); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(manifest)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	first := freeze()
	if second := freeze(); os.SameFile(first, second) || second.Mode().Perm() != first.Mode().Perm() {
		t.Error("the manifest was rewritten in place or changed mode")
	}
	if entries, err := os.ReadDir(filepath.Dir(manifest)); err != nil || len(entries) != 1 {
		t.Errorf("the interview directory holds %v (%v), want only freeze.json", entries, err)
	}
}
