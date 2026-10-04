package reset

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func resetTestTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{"sessions/s2.json", "sessions/s1.json", "sessions/notes.txt", "ledger.jsonl", "interview/freeze.json", "interviews/s1.jsonl", "goalplans/plan.json", "affordance-recovery/pending", "bg/job.json"} {
		p := filepath.Join(root, ".crw", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResetScopesAndRender(t *testing.T) {
	for _, tc := range []struct {
		scope         ResetScope
		removed, kept []string
	}{
		{State, []string{"sessions/s1.json", "sessions/s2.json", "ledger.jsonl", "interviews", "affordance-recovery"}, []string{"sessions/notes.txt", "interview/freeze.json", "goalplans/plan.json", "bg/job.json"}},
		{Generated, []string{"interview"}, []string{"sessions/s1.json", "ledger.jsonl", "goalplans/plan.json"}},
		{Goalplans, []string{"goalplans"}, []string{"sessions/s1.json", "interview/freeze.json"}},
		{All, []string{"."}, nil},
	} {
		t.Run(string(tc.scope), func(t *testing.T) {
			root := resetTestTree(t)
			outside := filepath.Join(root, "sibling.txt")
			if err := os.WriteFile(outside, []byte("sibling"), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := RunReset(root, tc.scope)
			want := []string{}
			for _, rel := range tc.removed {
				p := filepath.Join(root, ".crw", rel)
				want = append(want, p)
				if _, e := os.Lstat(p); !os.IsNotExist(e) {
					t.Errorf("not removed %s: %v", rel, e)
				}
			}
			if err != nil || got.Scope != tc.scope || !reflect.DeepEqual(got.Removed, want) {
				t.Fatalf("%+v %v; want %v", got, err, want)
			}
			for _, rel := range tc.kept {
				if _, e := os.Stat(filepath.Join(root, ".crw", rel)); e != nil {
					t.Errorf("removed preserved %s: %v", rel, e)
				}
			}
			if b, e := os.ReadFile(outside); e != nil || string(b) != "sibling" {
				t.Fatal("sibling removed")
			}
			if !strings.HasPrefix(RenderReset(got), "reset --"+string(tc.scope)+": removed ") {
				t.Fatal("render")
			}
			again, e := RunReset(root, tc.scope)
			if e != nil || len(again.Removed) != 0 || RenderReset(again) != "reset --"+string(tc.scope)+": removed 0 path(s) (nothing to remove)" {
				t.Fatalf("repeat: %+v %v", again, e)
			}
		})
	}
}

func TestParseResetScopeStrict(t *testing.T) {
	for _, s := range []ResetScope{State, Generated, Goalplans, All} {
		got, err := ParseResetScope([]string{"--" + string(s)})
		if err != nil || got != s {
			t.Fatalf("%s: %s %v", s, got, err)
		}
	}
	if got, err := ParseResetScope(nil); err != nil || got != State {
		t.Fatalf("default: %s %v", got, err)
	}
	for _, a := range [][]string{{"--bogus"}, {"--all", "--state"}, {"--state", "--state"}, {"state"}, {"--help"}} {
		if _, err := ParseResetScope(a); err == nil {
			t.Errorf("accepted %v", a)
		}
	}
	if _, err := RunReset(t.TempDir(), ResetScope("invalid")); err == nil {
		t.Fatal("invalid public scope")
	}
}

func TestResetRejectsDirectoryLinksAndPreservesOutside(t *testing.T) {
	for _, tc := range []struct{ name, link, target string }{
		{"state_root", ".crw", "outside"}, {"sessions", ".crw/sessions", "outside"}, {"internal_alias", ".crw/sessions", "goalplans"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			target := outside
			if tc.name == "state_root" {
				target = outside
				if err := os.MkdirAll(filepath.Join(outside, "sessions"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "internal_alias" {
				target = filepath.Join(root, ".crw/goalplans")
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			p := filepath.Join(target, "keep.json")
			if tc.name == "state_root" {
				p = filepath.Join(target, "sessions/keep.json")
			}
			if err := os.WriteFile(p, []byte("sentinel"), 0o644); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, tc.link)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if _, err := RunReset(root, State); err == nil {
				t.Fatal("followed directory link")
			}
			if b, err := os.ReadFile(p); err != nil || string(b) != "sentinel" {
				t.Fatalf("sentinel: %q %v", b, err)
			}
		})
	}
}

func TestResetAbsentDanglingAndAllLink(t *testing.T) {
	root := t.TempDir()
	got, err := RunReset(root, State)
	if err != nil || len(got.Removed) != 0 || len(got.Absent) != 4 {
		t.Fatalf("absent: %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".crw")); !os.IsNotExist(err) {
		t.Fatal("reset created state")
	}
	if err := os.Mkdir(filepath.Join(root, ".crw"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".crw/interview")
	if err := os.Symlink("missing", link); err != nil {
		t.Fatal(err)
	}
	got, err = RunReset(root, Generated)
	if err != nil || len(got.Removed) != 0 || len(got.Absent) != 1 {
		t.Fatalf("dangling: %+v %v", got, err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("dangling link removed")
	}
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".crw")); err != nil {
		t.Fatal(err)
	}
	if _, err := RunReset(root, All); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".crw")); !os.IsNotExist(err) {
		t.Fatal("all link not removed")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("all followed link")
	}
}

func TestResetPinRefusesReplacedDirectory(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	observed, err := root.Lstat("sessions")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(base, "sessions"), filepath.Join(base, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if pinned, err := resetPin(root, "sessions", observed); err == nil {
		pinned.Close()
		t.Fatal("pinned replacement instead of observed directory")
	}
}
