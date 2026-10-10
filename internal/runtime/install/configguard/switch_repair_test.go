//go:build unix

package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/hookswitch"
)

// switchRepairCases are the switch.json files the installer does not accept as they are (CRW-1174):
// the entries the hook cannot read, and the documents it reads but Parse refuses.
var switchRepairCases = map[string]struct {
	make func(t *testing.T, home, file string)
	// kind is the file type the entry has, which the backup keeps.
	kind os.FileMode
}{
	"dangling link": {func(t *testing.T, home, file string) {
		if err := os.Symlink(filepath.Join(home, "gone.json"), file); err != nil {
			t.Fatal(err)
		}
	}, os.ModeSymlink},
	"fifo": {func(t *testing.T, home, file string) {
		if err := syscall.Mkfifo(file, 0o600); err != nil {
			t.Fatal(err)
		}
	}, os.ModeNamedPipe},
	"directory": {func(t *testing.T, home, file string) {
		if err := os.Mkdir(file, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(file, "inner"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}, os.ModeDir},
	"over the size bound": {func(t *testing.T, home, file string) {
		doc := `{"active":"cxc","changedAt":"` + strings.Repeat("x", hookswitch.MaxBytes) + `","by":"y"}`
		if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}, 0},
	"not json": {func(t *testing.T, home, file string) {
		if err := os.WriteFile(file, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}, 0},
	"unknown active": {func(t *testing.T, home, file string) {
		if err := os.WriteFile(file, []byte(`{"active":"both","changedAt":"a","by":"b"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}, 0},
	"unknown field with the target active": {func(t *testing.T, home, file string) {
		if err := os.WriteFile(file, []byte(`{"active":"crw","changedAt":"a","by":"b","extra":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}, 0},
}

func switchRepairHome(t *testing.T, mk func(t *testing.T, home, file string)) (home, file string) {
	t.Helper()
	home = switchHost(t, switchConfigs["standard"])
	file = hookswitch.Path(home)
	if err := os.MkdirAll(filepath.Dir(file), 0o777); err != nil {
		t.Fatal(err)
	}
	mk(t, home, file)
	return home, file
}

func switchHookReads(home string) hookswitch.Reading {
	return hookswitch.Read(func(k string) (string, bool) { return home, k == "CODEX_HOME" })
}

func TestSwitchRepairsASwitchFileItCannotUse(t *testing.T) {
	for name, c := range switchRepairCases {
		t.Run(name, func(t *testing.T) {
			home, file := switchRepairHome(t, c.make)
			before, err := os.Lstat(file)
			if err != nil {
				t.Fatal(err)
			}
			r, err := RunSwitch(switchDeps(home), "crw")
			if err != nil {
				t.Fatalf("the switch did not repair switch.json: %v", err)
			}
			st, err := hookswitch.Load(home)
			if err != nil || st == nil || *st != (hookswitch.State{Active: hookswitch.CRW, ChangedAt: switchStampA, By: SwitchBy}) {
				t.Fatalf("switch.json after the repair = %+v, %v", st, err)
			}
			if h := switchHookReads(home); !h.On || h.Problem != "" {
				t.Fatalf("a hook reads %+v", h)
			}
			backup := file + ".crw-2026-10-10T01-00-00-000Z.bak"
			kept, err := os.Lstat(backup)
			if err != nil || kept.Mode().Type() != before.Mode().Type() {
				t.Fatalf("backup %v, %v; the entry was %v", kept, err, before.Mode())
			}
			if c.kind == os.ModeDir {
				if b, err := os.ReadFile(filepath.Join(backup, "inner")); err != nil || string(b) != "x" {
					t.Fatalf("the kept directory lost its content: %q, %v", b, err)
				}
			}
			if !strings.Contains(strings.Join(r.Notes, "\n"), backup) {
				t.Fatalf("the report does not name the backup %s: %v", backup, r.Notes)
			}
			// The way back works on the repaired file.
			if _, err := RunSwitch(switchDeps(home), "cxc"); err != nil {
				t.Fatal(err)
			}
			if st, err := hookswitch.Load(home); err != nil || st.Active != hookswitch.CXC {
				t.Fatalf("switch.json after cxc = %+v, %v", st, err)
			}
		})
	}
}

// A step after switch.json that fails puts the entry back as it was, backup name included.
func TestSwitchRollbackRestoresTheBrokenSwitchFile(t *testing.T) {
	for name, c := range switchRepairCases {
		t.Run(name, func(t *testing.T) {
			home, file := switchRepairHome(t, c.make)
			before, err := os.Lstat(file)
			if err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if c.kind == 0 {
				raw, _ = os.ReadFile(file)
			}
			deps := switchDeps(home)
			deps.Fail = func(step string) error {
				if step == "config" {
					return os.ErrInvalid
				}
				return nil
			}
			if _, err := RunSwitch(deps, "crw"); err == nil {
				t.Fatal("the injected failure was not reported")
			}
			after, err := os.Lstat(file)
			if err != nil || after.Mode().Type() != before.Mode().Type() {
				t.Fatalf("after the rollback the entry is %v, %v; it was %v", after, err, before.Mode())
			}
			if c.kind == 0 {
				if got, _ := os.ReadFile(file); string(got) != string(raw) {
					t.Fatal("the rollback changed the file's bytes")
				}
			}
			if c.kind == os.ModeDir {
				if b, err := os.ReadFile(filepath.Join(file, "inner")); err != nil || string(b) != "x" {
					t.Fatalf("the restored directory lost its content: %q, %v", b, err)
				}
			}
			matches, _ := filepath.Glob(file + ".crw-*.bak")
			if len(matches) != 0 {
				t.Fatalf("a backup name remains after the rollback: %v", matches)
			}
		})
	}
}

// A switch.json that could not be set aside stops the switch before it changes the file.
func TestSwitchStopsWhenTheBrokenFileCannotBeKept(t *testing.T) {
	home, file := switchRepairHome(t, switchRepairCases["not json"].make)
	if err := os.WriteFile(file+".crw-2026-10-10T01-00-00-000Z.bak", []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := switchTree(t, home)
	if _, err := RunSwitch(switchDeps(home), "crw"); err == nil {
		t.Fatal("the switch replaced a file it could not keep")
	}
	if b, _ := os.ReadFile(file); string(b) != "{" {
		t.Fatalf("switch.json = %q", b)
	}
	delete(before, InstallManifestName)
	after := switchTree(t, home)
	delete(after, InstallManifestName)
	if d := switchDiff(before, after); d != "" {
		t.Fatalf("the host changed:\n%s", d)
	}
}
