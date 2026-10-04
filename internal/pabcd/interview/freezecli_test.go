package interview

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// freezeEntry is one file of a recorded scenario: its path below the state directory and its data as bytes (base64 in the recording)
// and its kind (file, dir, link, remove). freezeStep is one run of the command with what the oracle printed, the manifest it left
// (frozenAt as <TS>) and the tree below the workspace (the state directory written as a STATE placeholder; null where not recorded).
type freezeEntry struct {
	P []byte
	K string
	D []byte
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
	p := filepath.Join(ws, ".crw") + "/" + string(e.P)
	var err error
	switch e.K {
	case "remove":
		err = os.RemoveAll(p)
	case "dir":
		err = os.MkdirAll(p, 0o777)
	default:
		if err = os.MkdirAll(filepath.Dir(p), 0o777); err == nil && e.K == "link" {
			err = os.Symlink(string(e.D), p)
		} else if err == nil {
			err = os.WriteFile(p, e.D, 0o666)
		}
	}
	if errors.Is(err, syscall.EILSEQ) || errors.Is(err, syscall.EINVAL) { // a file system may refuse a name that is not UTF-8
		t.Skipf("this file system refuses the name: %v", err)
	} else if err != nil {
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

// freezeFileReader is state.ReadState for these tests (this package cannot import state): the slug and tracker of the session file, or
// a default session when it is absent or has no valid phase.
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
				if got := ts.ReplaceAllString(decodeUTF8(manifest), "\"frozenAt\": \"<TS>\""); (err == nil) != (step.Manifest != nil) || (step.Manifest != nil && got != *step.Manifest) {
					t.Errorf("step %d manifest (%v)\n got %q\nwant %v", i, err, got, step.Manifest)
				}
				tree := freezeTree(t, ws, "")
				for j, p := range tree {
					tree[j] = strings.Replace(p, ".crw", "$"+"{STATE}", 1)
				}
				if step.Tree != nil && !slices.Equal(tree, step.Tree) {
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

// parseFreezeArgs reads process.cwd() while parsing, when --cwd gives none, so even a help request fails in a deleted directory.
func TestFreezeParseArgsFailsInADeletedWorkingDirectory(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	if err := os.Mkdir(gone, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Skipf("this system does not remove a working directory: %v", err)
	}
	for _, argv := range [][]string{{"--help"}, {"--help", "--cwd"}} {
		if _, err := ParseFreezeArgs(argv); err == nil {
			t.Errorf("ParseFreezeArgs(%q) succeeded without a working directory", argv)
		}
	}
	if got, err := ParseFreezeArgs([]string{"--cwd", "--help"}); err != nil || got.Cwd != "--help" || !got.Help {
		t.Errorf("an explicit --cwd needs no working directory: %+v, %v", got, err)
	}
}

// A plan directory that exists and cannot be read is an error, not an empty plan (the absent and file cases are recorded scenarios).
func TestFreezeAnUnreadablePlanDirectoryFails(t *testing.T) {
	closed := filepath.Join(t.TempDir(), "closed")
	if err := os.Mkdir(closed, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := ListPlanFiles(filepath.Dir(closed), closed); err == nil && os.Geteuid() != 0 {
		t.Error("a plan directory that cannot be read must fail")
	}
}

// A link planted in the working directory must not take the manifest write or the plan reads outside it: each placement is refused with
// an error, and what lies outside keeps its entries and content. A link that stays inside is followed (the recorded scenario symlinks).
func TestFreezeRefusesLinksOutOfTheWorkspace(t *testing.T) {
	for _, tc := range []struct {
		place string
		file  bool // the link names a file outside, not a directory
	}{{".crw", false}, {".crw/interview", false}, {".crw/interview/freeze.json", true}, {".crw/plan", false}, {".crw/plan/default/link.md", true}, {".crw/plan/default/sub", false}} {
		t.Run(tc.place, func(t *testing.T) {
			ws, outside := t.TempDir(), t.TempDir()
			secret, link, to := filepath.Join(outside, "secret"), filepath.Join(ws, tc.place), outside
			if tc.file {
				to = secret
			}
			if err := errors.Join(os.WriteFile(secret, []byte("keep"), 0o666), os.Mkdir(filepath.Join(outside, "default"), 0o777), os.MkdirAll(filepath.Dir(link), 0o777), os.Symlink(to, link)); err != nil {
				t.Fatal(err)
			}
			if _, err := RunFreeze(FreezeCliArgs{Cwd: ws, SessionID: "default"}, freezeFileReader); err == nil {
				t.Error("freeze went through the link")
			}
			kept, _ := os.ReadFile(secret)
			if entries, _ := os.ReadDir(outside); string(kept) != "keep" || len(entries) != 2 {
				t.Errorf("outside the workspace: %q and %d entries, want the content kept and no new entry", kept, len(entries))
			}
		})
	}
}

// The check judges the path the kernel opens. "<root>/jump/.." (jump a link) is composed as <root>, and ".." from a process whose $PWD is
// a link to its directory is that directory's real parent: neither is refused, and nothing is created beside the links.
func TestFreezeConfinementUsesTheCwdAsComposed(t *testing.T) {
	root, target, logical := t.TempDir(), filepath.Join(t.TempDir(), "target"), t.TempDir()
	child := filepath.Join(root, "child")
	if err := errors.Join(os.Mkdir(target, 0o777), os.Mkdir(child, 0o777), os.Symlink(target, filepath.Join(root, "jump")), os.Symlink(child, filepath.Join(logical, "alias"))); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(logical, "alias")) // the kernel's directory is root/child while $PWD names the link
	for _, cwd := range []string{root + "/jump/..", ".."} {
		if _, err := RunFreeze(FreezeCliArgs{Cwd: cwd, SessionID: "default"}, freezeFileReader); err != nil {
			t.Errorf("cwd %q: %v", cwd, err)
		}
	}
	for dir, want := range map[string]int{target: 0, logical: 1} {
		if entries, _ := os.ReadDir(dir); len(entries) != want {
			t.Errorf("%s holds %d entries, want %d", dir, len(entries), want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".crw", "interview", "freeze.json")); err != nil {
		t.Error(err)
	}
}

// The manifest is published, not rewritten in place: a second freeze replaces the file by rename (a new inode), keeps its mode and
// leaves no temp file, so a reader or a crash never sees a truncated manifest.
func TestFreezePublishesTheManifestAtomically(t *testing.T) {
	ws := t.TempDir()
	manifest := filepath.Join(ws, ".crw", "interview", "freeze.json")
	freeze := func() os.FileInfo {
		_, err := RunFreeze(FreezeCliArgs{Cwd: ws, SessionID: "default"}, freezeFileReader)
		info, statErr := os.Stat(manifest)
		if err != nil || statErr != nil {
			t.Fatal(err, statErr)
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
