// Package golden holds what the runtime packages' tests share: the repository's paths, the
// canonical JSON a test compares values by, and a helper process. It is imported by tests only.
// (It once also read internal/runtime/testdata/goldens.json, the answers scripts/crw_runtime
// gave; those expected values are now each test's goldens, internal/testsupport/golden.)
package golden

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// Dir is internal/runtime/testdata.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "testdata")
}

// Root is the repository root.
func Root() string { return filepath.Join(Dir(), "..", "..", "..") }

// Canon is a value's canonical JSON, for comparing two values whatever their key order (and for
// a golden holding one).
func Canon(v any) string {
	return pyjson.Dumps(v, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
}

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
	var held *os.File
	if path, ok := strings.CutPrefix(mode, "flock:"); ok {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			os.Exit(3)
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
			os.Exit(4)
		}
		held = file
	}
	fmt.Println("ready")
	// A line "collect" runs the garbage collector and answers "collected" once the finalizers of what
	// it collected have run (collect): a test of the helper itself asks it to, rather than leave to
	// chance whether a collection happens while it waits.
	lines := bufio.NewScanner(os.Stdin)
	for lines.Scan() {
		if lines.Text() == "collect" {
			collect()
			fmt.Println("collected")
		}
	}
	// The lock lasts as long as this descriptor is open: the file is kept reachable to the end, or a
	// collection would close it (its cleanup) and drop the lock under the test that relies on it.
	runtime.KeepAlive(held)
	os.Exit(0)
}

// collect runs the garbage collector and returns once every finalizer it queued has run, so the
// answer that follows is the outcome of the collection and not a race with it. A file nobody refers
// to is closed by its finalizer (os.newFile sets one), and the runtime runs every finalizer on one
// goroutine, one queued batch after another. A marker whose finalizer is set only after a first
// marker's has run is queued after the batch that held the first, and the first was queued no
// earlier than anything unreachable at the collection that found it; so once the second marker's
// finalizer has run, so has the file's. A collection that has not found a marker yet (it was still
// reachable) is repeated.
func collect() {
	for range 2 {
		ran := make(chan struct{})
		queueMarker(ran)
		for done := false; !done; {
			runtime.GC()
			select {
			case <-ran:
				done = true
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}

// marker is an object only a finalizer refers to; its pointer field keeps it out of the tiny
// allocator, whose blocks may never be finalized.
type marker struct{ _ *int }

// queueMarker allocates a marker that closes ran when it is finalized; the marker is unreachable
// once this returns.
//
//go:noinline
func queueMarker(ran chan struct{}) {
	runtime.SetFinalizer(&marker{}, func(*marker) { close(ran) })
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
