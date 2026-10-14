package golden

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	Helper()
	os.Exit(m.Run())
}

// The flock a helper holds lasts as long as the helper lives, whatever its garbage collector
// does: the descriptor it took the lock on is closed by the file's cleanup once nothing refers
// to the file, and closing it would drop the lock under a test that is still counting on it (the
// TestDoctorReportsAHeldPromotionLockAndPromotionRefuses flake of CRW-1368). The helper is asked
// to collect, which is what a loaded host's timing leaves to chance.
func TestAHelpersLockSurvivesItsGarbageCollection(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "held.lock")
	cmd := &exec.Cmd{Path: os.Args[0], Args: []string{os.Args[0]}, Env: append(os.Environ(), helperEnv+"=flock:"+lock)}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	lines := bufio.NewReader(stdout)
	if line, err := lines.ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper did not become ready: %q %v", line, err)
	}
	probe := func() error {
		file, err := os.OpenFile(lock, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	}
	if err := probe(); err == nil {
		t.Fatal("the helper was ready but its lock is not held")
	}
	if _, err := stdin.Write([]byte("collect\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := lines.ReadString('\n'); err != nil || strings.TrimSpace(line) != "collected" {
		t.Fatalf("helper did not collect: %q %v", line, err)
	}
	if err := probe(); err == nil {
		t.Fatal("the helper's lock was dropped when its garbage was collected")
	}
}
