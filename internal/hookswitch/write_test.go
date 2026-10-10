//go:build unix

package hookswitch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestWriteReadRoundTripAndExactBytes(t *testing.T) {
	home := t.TempDir()
	if s, err := Load(home); s != nil || err != nil {
		t.Fatalf("Load of an absent file = %v, %v; want nil, nil", s, err)
	}
	want := State{Active: CRW, ChangedAt: "2026-10-10T01:02:03.000Z", By: "crw install switch"}
	if err := Write(home, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(home)
	if err != nil || got == nil || *got != want {
		t.Fatalf("Load = %v, %v; want %v", got, err, want)
	}
	raw, _ := os.ReadFile(Path(home))
	const text = "{\n  \"active\": \"crw\",\n  \"changedAt\": \"2026-10-10T01:02:03.000Z\",\n  \"by\": \"crw install switch\"\n}\n"
	if string(raw) != text {
		t.Fatalf("bytes = %q, want %q", raw, text)
	}
	if entries, _ := os.ReadDir(filepath.Dir(Path(home))); len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %v", entries)
	}
	// What Write publishes is what a hook reads.
	if r := readWithin(t, home); !r.On || r.Problem != "" {
		t.Fatalf("a hook reads %+v, want on", r)
	}
}

func TestParseRefusesWhatItCannotRead(t *testing.T) {
	for name, in := range map[string]string{
		"unknown active": `{"active":"both","changedAt":"x","by":"y"}`,
		"empty active":   `{"changedAt":"x","by":"y"}`,
		"not json":       `active: crw`,
		"trailing":       `{"active":"crw","changedAt":"x","by":"y"} {}`,
		"unknown field":  `{"active":"crw","changedAt":"x","by":"y","extra":1}`,
		"trailing text":  `{"active":"crw","changedAt":"x","by":"y"} x`,
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: Parse accepted %q", name, in)
		}
	}
	if _, err := Marshal(State{Active: "both"}); err == nil {
		t.Error("Marshal accepted an unknown active value")
	}
}

func TestWriteReplacesWholeAndRawRestoresBytes(t *testing.T) {
	home := t.TempDir()
	if err := Write(home, State{Active: CXC, ChangedAt: "a", By: "b"}); err != nil {
		t.Fatal(err)
	}
	prev, err := ReadRaw(home)
	if err != nil || prev == nil {
		t.Fatalf("ReadRaw = %q, %v", prev, err)
	}
	if err := Write(home, State{Active: CRW, ChangedAt: "c", By: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteRaw(home, prev); err != nil {
		t.Fatal(err)
	}
	if again, _ := ReadRaw(home); string(again) != string(prev) {
		t.Fatalf("restored %q, want %q", again, prev)
	}
}

func TestReadRawRefusesAnEntryThatIsNotAFile(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(Path(home), 0o700); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadRaw(home); err == nil {
		t.Fatalf("ReadRaw of a directory = %q, nil", b)
	}
}

func TestRemoveToleratesAnAbsentFile(t *testing.T) {
	home := t.TempDir()
	if err := Remove(home); err != nil {
		t.Fatal(err)
	}
	if err := Write(home, State{Active: CXC, ChangedAt: "a", By: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(home)); !os.IsNotExist(err) {
		t.Fatalf("file remains: %v", err)
	}
}

// The installer's strictness does not reach the hook: a document with a field the hook does not
// know is still read (and the installer replaces it at the next switch, see Parse).
func TestHookReadsAnUnknownFieldWhereParseRefusesIt(t *testing.T) {
	home, file := switchDir(t)
	doc := `{"active":"cxc","changedAt":"x","by":"y","extra":1}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := readWithin(t, home); r.On || r.Problem != "" {
		t.Fatalf("a hook reads %+v, want off without a problem", r)
	}
	if _, err := Load(home); err == nil {
		t.Fatal("Load accepted an unknown field")
	}
}

// The validity of a state is one rule: the hook's problem is the rule's own text, which Parse and
// Marshal carry.
func TestValidityIsOneRuleForReadParseAndMarshal(t *testing.T) {
	home, file := switchDir(t)
	doc := `{"active":"both","changedAt":"x","by":"y"}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	r := readWithin(t, home)
	mustBeOnWithProblem(t, r)
	_, perr := Parse([]byte(doc))
	_, merr := Marshal(State{Active: "both", ChangedAt: "x", By: "y"})
	if perr == nil || merr == nil || !strings.Contains(perr.Error(), r.Problem) || !strings.Contains(merr.Error(), r.Problem) {
		t.Fatalf("hook problem %q, Parse %v, Marshal %v", r.Problem, perr, merr)
	}
}

func TestWriteRefusesAStateWithoutItsProvenance(t *testing.T) {
	for name, s := range map[string]State{
		"no changedAt": {Active: CRW, By: "b"},
		"no by":        {Active: CXC, ChangedAt: "a"},
		"neither":      {Active: CRW},
	} {
		home := t.TempDir()
		if err := Write(home, s); err == nil {
			t.Errorf("%s: Write accepted %+v", name, s)
		}
		if _, err := Marshal(s); err == nil {
			t.Errorf("%s: Marshal accepted %+v", name, s)
		}
		if _, err := os.Lstat(Path(home)); !os.IsNotExist(err) {
			t.Errorf("%s: a file was published: %v", name, err)
		}
	}
}

// brokenEntries makes each kind of switch.json the hook cannot read at the switch path.
var brokenEntries = map[string]func(t *testing.T, home, file string){
	"dangling link": func(t *testing.T, home, file string) {
		if err := os.Symlink(filepath.Join(home, "gone.json"), file); err != nil {
			t.Fatal(err)
		}
	},
	"fifo": func(t *testing.T, home, file string) {
		if err := syscall.Mkfifo(file, 0o600); err != nil {
			t.Fatal(err)
		}
	},
	"directory": func(t *testing.T, home, file string) {
		if err := os.Mkdir(file, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(file, "inner"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	},
	"over the size bound": func(t *testing.T, home, file string) {
		if err := os.WriteFile(file, []byte(`{"active":"crw","changedAt":"`+strings.Repeat("x", MaxBytes)+`","by":"y"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	},
}

func TestKeepAsideKeepsWhateverIsAtThePathAndRestoreGivesItBack(t *testing.T) {
	for name, mk := range brokenEntries {
		t.Run(name, func(t *testing.T) {
			home, file := switchDir(t)
			mk(t, home, file)
			before, err := os.Lstat(file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadRaw(home); err == nil {
				t.Fatal("ReadRaw read it")
			}
			a, err := KeepAside(home, "s1")
			if err != nil || a == nil {
				t.Fatalf("KeepAside = %v, %v", a, err)
			}
			if !strings.HasSuffix(a.Path, "switch.json.crw-s1.bak") || filepath.Dir(a.Path) != filepath.Dir(file) {
				t.Fatalf("kept at %s", a.Path)
			}
			kept, err := os.Lstat(a.Path)
			if err != nil || kept.Mode().Type() != before.Mode().Type() {
				t.Fatalf("kept entry %v, %v; was %v", kept, err, before.Mode())
			}
			// The replacement publishes whole and leaves the kept entry as it was.
			if err := Write(home, State{Active: CRW, ChangedAt: "a", By: "b"}); err != nil {
				t.Fatal(err)
			}
			if s, err := Load(home); err != nil || s == nil || s.Active != CRW {
				t.Fatalf("Load after the replacement = %v, %v", s, err)
			}
			if r := readWithin(t, home); !r.On || r.Problem != "" {
				t.Fatalf("a hook reads %+v after the repair", r)
			}
			if kept, err := os.Lstat(a.Path); err != nil || kept.Mode().Type() != before.Mode().Type() {
				t.Fatalf("kept entry after the replacement %v, %v", kept, err)
			}
			// Restore puts the entry back and leaves no backup name.
			if err := a.Restore(); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(file)
			if err != nil || after.Mode().Type() != before.Mode().Type() {
				t.Fatalf("restored entry %v, %v; was %v", after, err, before.Mode())
			}
			if _, err := os.Lstat(a.Path); !os.IsNotExist(err) {
				t.Fatalf("the backup name remains: %v", err)
			}
		})
	}
}

// A replacement that failed before it published leaves both names on the one entry; Restore ends
// with the entry in place and no backup.
func TestKeepAsideRestoreBeforeAnyPublication(t *testing.T) {
	for name, mk := range brokenEntries {
		t.Run(name, func(t *testing.T) {
			home, file := switchDir(t)
			mk(t, home, file)
			a, err := KeepAside(home, "s2")
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Restore(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(file); err != nil {
				t.Fatalf("the entry is gone: %v", err)
			}
			if _, err := os.Lstat(a.Path); !os.IsNotExist(err) {
				t.Fatalf("the backup name remains: %v", err)
			}
		})
	}
}

func TestKeepAsideOfNothingAndOfAnExistingBackup(t *testing.T) {
	home, file := switchDir(t)
	if a, err := KeepAside(home, "s3"); a != nil || err != nil {
		t.Fatalf("KeepAside of an absent file = %v, %v", a, err)
	}
	if err := os.WriteFile(file, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file+".crw-s3.bak", []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a, err := KeepAside(home, "s3"); a != nil || err == nil {
		t.Fatalf("KeepAside over an existing backup = %v, %v", a, err)
	}
	if b, _ := os.ReadFile(file + ".crw-s3.bak"); string(b) != "older" {
		t.Fatalf("the existing backup was changed: %q", b)
	}
}

// CRW-1163: the directory sync is part of the publication, and its failure is reported as a file
// that is in place with its durability unconfirmed.
func TestWriteRawReportsADirectorySyncFailureAsPublishedNotDurable(t *testing.T) {
	for name, injected := range map[string]error{
		"open fails": &os.PathError{Op: "open", Path: "dir", Err: syscall.EACCES},
		"sync fails": &os.PathError{Op: "sync", Path: "dir", Err: syscall.EIO},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			was := syncDir
			t.Cleanup(func() { syncDir = was })
			syncDir = func(string) error { return injected }
			err := WriteRaw(home, []byte("{\"active\":\"crw\"}\n"))
			if !errors.Is(err, ErrNotDurable) || !errors.Is(err, injected) {
				t.Fatalf("WriteRaw = %v, want ErrNotDurable wrapping %v", err, injected)
			}
			if !strings.Contains(err.Error(), "in place") {
				t.Fatalf("the message does not say the file is published: %v", err)
			}
			// The file is published whole, with no temporary file left behind.
			if b, rerr := os.ReadFile(Path(home)); rerr != nil || string(b) != "{\"active\":\"crw\"}\n" {
				t.Fatalf("published file = %q, %v", b, rerr)
			}
			if entries, _ := os.ReadDir(filepath.Dir(Path(home))); len(entries) != 1 {
				t.Fatalf("a temporary file was left behind: %v", entries)
			}
			// Write carries it too.
			if werr := Write(home, State{Active: CXC, ChangedAt: "a", By: "b"}); !errors.Is(werr, ErrNotDurable) {
				t.Fatalf("Write = %v", werr)
			}
		})
	}
}

// A directory that cannot be synced at all is not a failure to report: nothing can be confirmed.
func TestWriteRawAcceptsAFilesystemThatCannotSyncADirectory(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EINVAL, syscall.ENOTSUP, syscall.ENOSYS} {
		home := t.TempDir()
		was := syncDir
		t.Cleanup(func() { syncDir = was })
		syncDir = func(dir string) error {
			// The real function's own rule, applied to an error the filesystem gives.
			return classifySyncError(&os.PathError{Op: "sync", Path: dir, Err: errno})
		}
		if err := WriteRaw(home, []byte("{}")); err != nil {
			t.Errorf("%v: WriteRaw = %v", errno, err)
		}
	}
	// And the real sync of a real directory succeeds.
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir of a directory = %v", err)
	}
}

func TestSyncDirReportsAnOpenFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	if err := syncDir(dir); err == nil {
		t.Fatal("syncDir opened a directory that cannot be read")
	}
	if err := syncDir(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("syncDir of a missing directory = nil")
	}
}
