package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// Test30FirstOpenerProcess is a child process, not a test: a writable first opener of
// CRW_CRASH_DB that prints "admitted" or "refused: <error>". With CRW30_PAUSE_POINT it
// stops at that creation seam, prints "paused" and waits for a line on stdin.
func Test30FirstOpenerProcess(t *testing.T) {
	path := os.Getenv("CRW_CRASH_DB")
	if path == "" || os.Getenv("CRW30_FIRST_OPENER") == "" {
		return
	}
	if point := os.Getenv("CRW30_PAUSE_POINT"); point != "" {
		createFault = func(p string) error {
			if p == point {
				fmt.Println("paused")
				_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
			}
			return nil
		}
	}
	db, err := Open(context.Background(), path, "")
	if err != nil {
		fmt.Println("refused:", err)
		return
	}
	if err = db.Close(); err != nil {
		fmt.Println("close:", err)
		return
	}
	fmt.Println("admitted")
}

// pythonFirstOpener is the fence's own writable first opener; "after-gate-placed" stops it
// right after it linked write-gate.lock into place, still holding it EX (test_fence.py
// test_concurrent_first_openers_never_see_a_partial_store).
const pythonFirstOpener = `import os, sys
from codex_session_relay.store import Store
pause, paused = sys.argv[2], []
real_link = os.link
def link(source, target, **options):
    real_link(source, target, **options)
    if pause == 'after-gate-placed' and os.path.basename(target) == 'write-gate.lock' and not paused:
        paused.append(True)
        print('paused', flush=True)
        sys.stdin.readline()
os.link = link
try:
    Store(sys.argv[1]).close()
except Exception as error:
    print('refused:', type(error).__name__, error, flush=True)
else:
    print('admitted', flush=True)`

type firstOpener struct {
	runtime string
	command *exec.Cmd
	stdin   io.WriteCloser
	lines   chan string
	done    chan struct{}
}

// startOpener starts runtime's first opener of path; paused stops it once its gate is placed.
func startOpener(t *testing.T, runtime, path string, paused bool) *firstOpener {
	t.Helper()
	var command *exec.Cmd
	if runtime == "go" {
		command = exec.Command(os.Args[0], "-test.run=^Test30FirstOpenerProcess$")
		command.Env = append(os.Environ(), "CRW_CRASH_DB="+path, "CRW30_FIRST_OPENER=1")
		if paused {
			command.Env = append(command.Env, "CRW30_PAUSE_POINT=gate-placed")
		}
	} else {
		pause := "none"
		if paused {
			pause = "after-gate-placed"
		}
		command = pythonFence(t, "", pythonFirstOpener, path, pause)
	}
	stdin, err := command.StdinPipe()
	must(t, err)
	stdout, err := command.StdoutPipe()
	must(t, err)
	command.Stderr = os.Stderr
	must(t, command.Start())
	o := &firstOpener{runtime: runtime, command: command, stdin: stdin, lines: make(chan string, 16), done: make(chan struct{})}
	go func() {
		defer close(o.done)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			o.lines <- scanner.Text()
		}
		_ = command.Wait()
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-o.done
	})
	return o
}

func (o *firstOpener) next(t *testing.T) string {
	t.Helper()
	select {
	case line := <-o.lines:
		return line
	case <-time.After(60 * time.Second):
		t.Fatalf("%s opener said nothing", o.runtime)
		return ""
	}
}

// answer is the opener's verdict: its first line that is neither the pause nor the Go test
// binary's own PASS.
func (o *firstOpener) answer(t *testing.T) string {
	t.Helper()
	for {
		if line := o.next(t); line != "paused" && line != "PASS" {
			return line
		}
	}
}

// Decision 30, cutover.md Step 0.4: the creator places write-gate.lock already held EX, so
// a concurrent first opener of either runtime that finds the gate waits for the creation
// and then meets the created store: admitted when its own runtime created it, refused as
// the other runtime's store otherwise, and never refused as a partial store.
func Test30ConcurrentFirstOpenersNeverSeeAPartialStore(t *testing.T) {
	judge := func(t *testing.T, path, creator string, answers map[string]string) {
		t.Helper()
		for runtime, answer := range answers {
			if strings.Contains(answer, "partial store") || strings.Contains(answer, "existing database required") || strings.Contains(answer, "write gate") {
				t.Fatalf("%s opener saw the store half-created: %s", runtime, answer)
			}
			switch {
			case runtime == creator && answer != "admitted":
				t.Fatalf("%s created the store and was refused: %s", runtime, answer)
			case runtime != creator && !strings.Contains(answer, "the relay store belongs to another runtime"):
				t.Fatalf("%s opener of a %s store: %s", runtime, creator, answer)
			}
		}
		stamp, err := ownership.SnapshotMeta(t.Context(), path)
		must(t, err)
		if stamp.Owner != creator || stamp.Epoch != 1 {
			t.Fatalf("stamp %+v, created by %s", stamp, creator)
		}
		if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".write-gate-*")); len(leftovers) != 0 {
			t.Fatalf("temporary gates left behind: %v", leftovers)
		}
		// Whichever runtime created it, both halves of the record agree.
		record, err := ownership.ReadRecord(path)
		must(t, err)
		must(t, ownership.Validate(path, record, stamp))
	}
	for _, tc := range []struct{ creator, second string }{{"go", "go"}, {"go", "python"}, {"python", "go"}} {
		t.Run(tc.creator+"-creates-"+tc.second+"-waits", func(t *testing.T) {
			path := filepath.Join(stateDir(t), "state", "relay.sqlite3")
			first := startOpener(t, tc.creator, path, true)
			if line := first.next(t); line != "paused" {
				t.Fatalf("creator: %s", line)
			}
			second := startOpener(t, tc.second, path, false)
			select {
			case line := <-second.lines:
				t.Fatalf("the second opener did not wait for the creation: %s", line)
			case <-time.After(time.Second):
			}
			_, err := io.WriteString(first.stdin, "go\n")
			must(t, err)
			answers := map[string]string{}
			created := first.answer(t)
			answers[tc.second] = second.answer(t)
			if tc.second == tc.creator {
				if created != "admitted" {
					t.Fatalf("creator: %s", created)
				}
			} else {
				answers[tc.creator] = created
			}
			judge(t, path, tc.creator, answers)
		})
	}
	// Unpaused: two first openers started together on an absent directory, several times.
	for _, pair := range [][2]string{{"go", "go"}, {"go", "python"}} {
		for round := 0; round < 4; round++ {
			t.Run(fmt.Sprintf("race-%s-%s-%d", pair[0], pair[1], round), func(t *testing.T) {
				path := filepath.Join(stateDir(t), "state", "relay.sqlite3")
				a, b := startOpener(t, pair[0], path, false), startOpener(t, pair[1], path, false)
				first, second := a.answer(t), b.answer(t)
				stamp, err := ownership.SnapshotMeta(t.Context(), path)
				must(t, err)
				if pair[0] == pair[1] {
					if first != "admitted" || second != "admitted" {
						t.Fatalf("racing first openers: %q %q", first, second)
					}
					judge(t, path, stamp.Owner, map[string]string{pair[0]: first})
					return
				}
				judge(t, path, stamp.Owner, map[string]string{pair[0]: first, pair[1]: second})
			})
		}
	}
}

// A first opener places the gate already held EX and creates D after it, so in that window S
// holds a gate and no D. The start preflight of a service command or a daemon lets it through
// (creating), as check_start and ownership.py refuse_partial do, rather than refusing it as a
// partial store; the admitted open then waits for the creation (awaitCreation). Once the
// creation is done the preflight judges the created store: its runtime's own passes, the other
// runtime's is refused. A creator that died after placing its gate leaves a gate nobody holds,
// which the same preflight refuses as a partial store. A reader never probes the gate.
func Test31StartPreflightLetsACreationThroughAndRefusesAnAbandonedGate(t *testing.T) {
	partial := "partial store: write-gate.lock without a database"
	detail := func(err error) string {
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Reason != "store_owned_by_other" {
			t.Fatalf("not an ownership refusal: %#v", err)
		}
		return refused.Detail
	}
	for _, creator := range []string{"go", "python"} {
		t.Run(creator, func(t *testing.T) {
			path := filepath.Join(stateDir(t), "state", "relay.sqlite3")
			first := startOpener(t, creator, path, true)
			if line := first.next(t); line != "paused" {
				t.Fatalf("creator: %s", line)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("D exists before the creation: %v", err)
			}
			if err := StartPreflight(t.Context(), path, ""); err != nil {
				t.Fatalf("a store being created was refused: %v", err)
			}
			if _, err := openForRead(t.Context(), path, ""); detail(err) != partial {
				t.Fatalf("a reader during the creation: %v", err)
			}
			_, err := io.WriteString(first.stdin, "go\n")
			must(t, err)
			if answer := first.answer(t); answer != "admitted" {
				t.Fatalf("creator: %s", answer)
			}
			err = StartPreflight(t.Context(), path, "")
			if creator == "go" && err != nil || creator == "python" && detail(err) != "the relay store belongs to another runtime" {
				t.Fatalf("the created %s store: %v", creator, err)
			}
			abandoned := filepath.Join(stateDir(t), "state", "relay.sqlite3")
			dead := startOpener(t, creator, abandoned, true)
			if line := dead.next(t); line != "paused" {
				t.Fatalf("creator: %s", line)
			}
			must(t, dead.command.Process.Kill())
			<-dead.done
			if got := detail(StartPreflight(t.Context(), abandoned, "")); got != partial {
				t.Fatalf("a gate its dead creator left: %q", got)
			}
		})
	}
}
