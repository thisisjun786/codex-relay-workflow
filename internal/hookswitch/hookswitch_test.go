//go:build unix

package hookswitch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func readWithin(t *testing.T, codexHome string) Reading {
	t.Helper()
	env := func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return codexHome, true
		}
		return "", false
	}
	got := make(chan Reading, 1)
	go func() { got <- Read(env) }()
	select {
	case r := <-got:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Read waits on the switch file")
		return Reading{}
	}
}

func switchDir(t *testing.T) (codexHome, file string) {
	t.Helper()
	codexHome = t.TempDir()
	if err := os.MkdirAll(filepath.Join(codexHome, "crw"), 0o700); err != nil {
		t.Fatal(err)
	}
	return codexHome, Path(codexHome)
}

func mustBeOnWithProblem(t *testing.T, r Reading) {
	t.Helper()
	if !r.On || r.Problem == "" {
		t.Fatalf("a switch that is there but cannot be read must be on with a problem: %+v", r)
	}
}

// A switch.json link whose target is gone is a switch that is there and cannot be read, not an
// absent switch: the guard runs and warns.
func TestReadDanglingLinkIsOn(t *testing.T) {
	home, file := switchDir(t)
	if err := os.Symlink(filepath.Join(home, "gone.json"), file); err != nil {
		t.Fatal(err)
	}
	mustBeOnWithProblem(t, readWithin(t, home))
}

// A FIFO as the switch must not hold the hook: with no writer, and with a writer that never closes.
func TestReadFIFOIsOnWithoutWaiting(t *testing.T) {
	home, file := switchDir(t)
	if err := syscall.Mkfifo(file, 0o600); err != nil {
		t.Fatal(err)
	}
	mustBeOnWithProblem(t, readWithin(t, home))

	w, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	mustBeOnWithProblem(t, readWithin(t, home))
}

// A link to a FIFO is the same.
func TestReadLinkedFIFOIsOnWithoutWaiting(t *testing.T) {
	home, file := switchDir(t)
	fifo := filepath.Join(home, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fifo, file); err != nil {
		t.Fatal(err)
	}
	mustBeOnWithProblem(t, readWithin(t, home))
}

// A directory and an absent file keep their meaning: a directory is unreadable, nothing is off.
func TestReadAbsentIsOffDirectoryIsOn(t *testing.T) {
	home, file := switchDir(t)
	if r := readWithin(t, home); r.On || r.Problem != "" {
		t.Fatalf("absent switch: %+v", r)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	mustBeOnWithProblem(t, readWithin(t, home))
}

// A regular file and a link to one still read.
func TestReadRegularAndLinkedFile(t *testing.T) {
	home, file := switchDir(t)
	target := filepath.Join(home, "real.json")
	if err := os.WriteFile(target, []byte(`{"active":"cxc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if r := readWithin(t, home); r.On || r.Problem != "" {
		t.Fatalf("linked cxc: %+v", r)
	}
	if err := os.WriteFile(target, []byte(`{"active":"crw"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := readWithin(t, home); !r.On || r.Problem != "" {
		t.Fatalf("linked crw: %+v", r)
	}
}
