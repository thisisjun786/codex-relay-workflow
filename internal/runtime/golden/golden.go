// Package golden reads internal/runtime/testdata/goldens.json, the answers
// internal/runtime/testdata/python_goldens.py captured from scripts/crw_runtime, for the
// runtime packages' tests. It is imported by tests only.
package golden

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// Dir is internal/runtime/testdata.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata")
}

// Root is the repository root.
func Root() string { return filepath.Join(Dir(), "..", "..", "..") }

var load = sync.OnceValues(func() (record.Object, error) {
	raw, err := os.ReadFile(filepath.Join(Dir(), "goldens.json"))
	if err != nil {
		return nil, err
	}
	value, err := reading.Decode(raw)
	if err != nil {
		return nil, err
	}
	return value.(record.Object), nil
})

// Section is one top-level section of the goldens.
func Section(t testing.TB, name string) any {
	t.Helper()
	all, err := load()
	if err != nil {
		t.Fatalf("goldens: %v", err)
	}
	value, ok := record.Lookup(all, name)
	if !ok {
		t.Fatalf("goldens: no section %q", name)
	}
	return value
}

// Canon is a value's canonical JSON, for comparing a Go answer with a Python one whatever the
// key order.
func Canon(v any) string { return evidence.Dumps(v, true, true, false) }

// Obj is v as an object.
func Obj(v any) record.Object {
	o, _ := v.(record.Object)
	return o
}

// List is v as a list.
func List(v any) []any {
	l, _ := v.([]any)
	return l
}

// helperEnv starts the test binary as a helper process instead of a test run: "wait" waits
// for stdin to close, "flock:<path>" first holds an exclusive flock on path.
const helperEnv = "CRW_RUNTIME_TEST_HELPER"

// Helper runs the helper when this process was started as one. TestMain calls it first.
func Helper() {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	if path, ok := strings.CutPrefix(mode, "flock:"); ok {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			os.Exit(3)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
			os.Exit(4)
		}
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// Spawn starts a helper whose argv[0] is argv0 ("" keeps the test binary's), holding an flock
// on lockPath when it is set, and returns its pid and a release that ends it.
func Spawn(t testing.TB, argv0, lockPath string) (int, func()) {
	t.Helper()
	mode := "wait"
	if lockPath != "" {
		mode = "flock:" + lockPath
	}
	if argv0 == "" {
		argv0 = os.Args[0]
	}
	cmd := &exec.Cmd{Path: os.Args[0], Args: []string{argv0}, Env: append(os.Environ(), helperEnv+"="+mode)}
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
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper did not become ready: %q %v", line, err)
	}
	done := false
	release := func() {
		if done {
			return
		}
		done = true
		_ = stdin.Close()
		_ = cmd.Wait()
	}
	t.Cleanup(release)
	return cmd.Process.Pid, release
}
