package sync

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"golang.org/x/sys/unix"
)

func Test23_LedgerPythonGoInteropAndBlockingLock(t *testing.T) {
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "nested", "ledger.json")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	script := "import sys,json; sys.path[:0]=" + pythonPaths(root) + "; from codex_session_relay import receiver; p=sys.argv[1]; receiver.save_ledger(p,receiver.empty_ledger('child'));\nwith receiver.ledger_lock(p):\n print('locked',flush=True); sys.stdin.readline()\nprint('released',flush=True)\nsys.stdin.readline()\nprint(json.dumps(receiver.load_ledger(p,'child')),flush=True)"
	cmd := exec.CommandContext(ctx, "uv", "run", "--no-sync", "python", "-c", script, path)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, ".venv"), "UV_CACHE_DIR="+t.TempDir()+"/uv")
	stdin, e := cmd.StdinPipe()
	if e != nil {
		t.Fatal(e)
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	cmd.Stderr = os.Stderr
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	scan := bufio.NewScanner(stdout)
	if !scan.Scan() || scan.Text() != "locked" {
		t.Fatal("Python never acquired ledger lock", scan.Err())
	}
	lock, e := os.OpenFile(path+".lock", os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != unix.EWOULDBLOCK {
		t.Fatalf("Python lock must exclude Go: %v", e)
	}
	if e = lock.Close(); e != nil {
		t.Fatal(e)
	}
	before, e := os.Stat(path + ".lock")
	if e != nil {
		t.Fatal(e)
	}
	// Subscribe before triggering. The nonblocking probe establishes contention without a sleep;
	// the real API's blocking flock then resumes after the Python owner releases it.
	done := make(chan error, 1)
	go func() {
		done <- reception.WithLedgerLock(path, func() error {
			ledger, e := reception.LoadLedger(path, "child")
			if e != nil {
				return e
			}
			answers, _ := evidence.Object(reception.Get(ledger, "answered"))
			reception.Set(&answers, "msg", reception.O("contentDigest", "digest", "disposition", "accepted", "applied", false, "toldToAct", true))
			reception.Set(&ledger, "answered", answers)
			return reception.SaveLedger(path, ledger)
		})
	}()
	if _, e = fmt.Fprintln(stdin, "release"); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !scan.Scan() || scan.Text() != "released" {
		t.Fatal(scan.Err())
	}
	if _, e = fmt.Fprintln(stdin, "read"); e != nil {
		t.Fatal(e)
	}
	if !scan.Scan() {
		t.Fatal(scan.Err())
	}
	var read map[string]any
	if e = json.Unmarshal(scan.Bytes(), &read); e != nil {
		t.Fatal(e)
	}
	entry := read["answered"].(map[string]any)["msg"].(map[string]any)
	if entry["contentDigest"] != "digest" || entry["toldToAct"] != true || entry["applied"] != false {
		t.Fatal(read)
	}
	after, e := os.Stat(path + ".lock")
	if e != nil {
		t.Fatal(e)
	}
	if !os.SameFile(before, after) {
		t.Fatal("lock sidecar inode replaced")
	}
	if e = stdin.Close(); e != nil {
		t.Fatal(e)
	}
	if e = cmd.Wait(); e != nil {
		t.Fatal(e)
	}
}
