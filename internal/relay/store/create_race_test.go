package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Test30FirstOpenerProcess is a child process, not a test: a writable first opener of
// CRW_CRASH_DB that prints "admitted" or "refused: <error>". With CRW30_PAUSE_POINT it
// stops at that creation seam, prints "paused" and waits for a line on stdin.
func Test30FirstOpenerProcess(t *testing.T) {
	// Serial: child-process entry point (a parent test re-executes this binary for it); it assigns the createFault seam and returns at once otherwise.
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

type firstOpener struct {
	runtime string
	command *exec.Cmd
	stdin   io.WriteCloser
	lines   chan string
	done    chan struct{}
}

// startOpener starts runtime's first opener of path; paused stops it once its gate is placed.
// The runtime is Go's: the Python fence's first opener raced these until todo 44 (rollback to
// Python closed at todo 43, rollback_allowed=0, and the Python runtime leaves in todo 44).
func startOpener(t *testing.T, runtime, path string, paused bool) *firstOpener {
	t.Helper()
	if runtime != "go" {
		t.Fatalf("no %s first opener", runtime)
	}
	command := exec.Command(os.Args[0], "-test.run=^Test30FirstOpenerProcess$")
	command.Env = append(os.Environ(), "CRW_CRASH_DB="+path, "CRW30_FIRST_OPENER=1")
	if paused {
		command.Env = append(command.Env, "CRW30_PAUSE_POINT=gate-placed")
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
// the other runtime's store otherwise, and never refused as a partial store. Only Go's first
// openers race here since todo 44; the Python fence's took part until rollback to Python closed
// at todo 43 (rollback_allowed=0).
func Test30ConcurrentFirstOpenersNeverSeeAPartialStore(t *testing.T) {
	// Serial: its first case asserts that the second opener says nothing for a fixed one-second window; under parallel load the opener may not have reached its wait yet and the check would pass vacuously.
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
		physical, err := ownership.Physical(path)
		must(t, err)
		if record.Owner != stamp.Owner || record.Epoch != stamp.Epoch || record.StoreID != stamp.StoreID || record.Database != physical ||
			record.RollbackAllowed != stamp.RollbackAllowed || record.CompatibilityBuild != stamp.CompatibilityBuild || record.Phase != "active" || record.Transition != nil {
			t.Fatalf("mirror %+v disagrees with stamp %+v", record, stamp)
		}
	}
	// Go's first openers alone: the Python fence's creator and waiter took part until todo 44.
	for _, tc := range []struct{ creator, second string }{{"go", "go"}} {
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
	for _, pair := range [][2]string{{"go", "go"}} {
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
// holds a gate and no D. The start preflight of a service command or a daemon takes that for no
// partial store, and no longer lets it through at once either (PR #202 review: a service form
// changes S with no admitted open after it, so it acted on a store the other runtime was still
// creating): it waits, within CreationWait and without blocking, until the creator no longer
// holds the gate EX, then judges the store again from the start, as ownership.py refuse_partial
// does. Its runtime's own store passes, the other runtime's is refused, and a creator that
// keeps the gate past the bound is refused as a creation still in progress. A creator that died
// after placing its gate leaves a gate nobody holds, which the same preflight refuses as a
// partial store, at once or once it has waited. A reader never probes the gate.
func Test31StartPreflightWaitsForACreationAndRefusesAnAbandonedGate(t *testing.T) {
	// Serial: assigns the package-level CreationWait seam, which every other running test would read.
	partial := "partial store: write-gate.lock without a database"
	bound := CreationWait
	t.Cleanup(func() { CreationWait = bound })
	// A Go creator alone: a Python creator took part until todo 44.
	for _, creator := range []string{"go"} {
		t.Run(creator, func(t *testing.T) {
			detail := func(err error) string {
				t.Helper()
				var refused *RefusedError
				if !errors.As(err, &refused) || refused.Reason != "store_owned_by_other" {
					t.Fatalf("not an ownership refusal: %#v", err)
				}
				return refused.Detail
			}
			preflight := func(path string) <-chan error {
				verdict := make(chan error, 1)
				go func() { verdict <- StartPreflight(t.Context(), path) }()
				return verdict
			}
			waiting := func(verdict <-chan error) {
				t.Helper()
				select {
				case err := <-verdict:
					t.Fatalf("the preflight did not wait for the creator: %v", err)
				case <-time.After(500 * time.Millisecond):
				}
			}
			settled := func(verdict <-chan error) error {
				t.Helper()
				select {
				case err := <-verdict:
					return err
				case <-time.After(60 * time.Second):
					t.Fatal("the preflight is still waiting after the creator let the gate go")
					return nil
				}
			}
			path := filepath.Join(stateDir(t), "state", "relay.sqlite3")
			first := startOpener(t, creator, path, true)
			if line := first.next(t); line != "paused" {
				t.Fatalf("creator: %s", line)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("D exists before the creation: %v", err)
			}
			CreationWait = 200 * time.Millisecond
			if got := detail(StartPreflight(t.Context(), path)); got != "store creation in progress: write-gate.lock still held after 0.2s; retry" {
				t.Fatalf("a creator holding the gate past the bound: %q", got)
			}
			CreationWait = bound
			if _, err := openForRead(t.Context(), path, ""); detail(err) != partial {
				t.Fatalf("a reader during the creation: %v", err)
			}
			verdict := preflight(path)
			waiting(verdict)
			_, err := io.WriteString(first.stdin, "go\n")
			must(t, err)
			if answer := first.answer(t); answer != "admitted" {
				t.Fatalf("creator: %s", answer)
			}
			err = settled(verdict)
			if err != nil {
				t.Fatalf("the created %s store: %v", creator, err)
			}
			abandoned := filepath.Join(stateDir(t), "state", "relay.sqlite3")
			dead := startOpener(t, creator, abandoned, true)
			if line := dead.next(t); line != "paused" {
				t.Fatalf("creator: %s", line)
			}
			must(t, dead.command.Process.Kill())
			<-dead.done
			if got := detail(StartPreflight(t.Context(), abandoned)); got != partial {
				t.Fatalf("a gate its dead creator left: %q", got)
			}
			dying := filepath.Join(stateDir(t), "state", "relay.sqlite3")
			gaveUp := startOpener(t, creator, dying, true)
			if line := gaveUp.next(t); line != "paused" {
				t.Fatalf("creator: %s", line)
			}
			verdict = preflight(dying)
			waiting(verdict)
			must(t, gaveUp.command.Process.Kill())
			<-gaveUp.done
			if got := detail(settled(verdict)); got != partial {
				t.Fatalf("a creator that died while the preflight waited: %q", got)
			}
		})
	}
}

var (
	relayOnce  sync.Once
	relayAlias string
	relayErr   error
)

// relayCLI is the Go relay CLI of this checkout (testsupport.CRW) spelled codex-session-relay,
// linked once per test binary under the isolation root.
func relayCLI(t *testing.T) string {
	t.Helper()
	relayOnce.Do(func() {
		dir := filepath.Join(isolationRoot, "relay-cli")
		if relayErr = os.MkdirAll(dir, 0o700); relayErr != nil {
			return
		}
		var built string
		if built, relayErr = testsupport.CRWPath(); relayErr != nil {
			return
		}
		relayAlias = filepath.Join(dir, "codex-session-relay")
		relayErr = os.Symlink(built, relayAlias)
	})
	if relayErr != nil {
		t.Fatal(relayErr)
	}
	return relayAlias
}

type relayAnswer struct {
	code           int
	stdout, stderr string
}

// relayStart starts one runtime's relay CLI with argv and its scope registry at scopes, and
// delivers its answer once it exits.
func relayStart(t *testing.T, program, scopes string, argv ...string) <-chan relayAnswer {
	t.Helper()
	command := exec.Command(program, argv...)
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "CODEX_SESSION_RELAY_SCOPE_DIR="+scopes)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	must(t, command.Start())
	answer, done := make(chan relayAnswer, 1), make(chan struct{})
	go func() {
		defer close(done)
		code, err := 0, command.Wait()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			code = -1
		}
		answer <- relayAnswer{code, stdout.String(), stderr.String()}
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-done
	})
	return answer
}

// PR #202 review, both directions: while one runtime's first opener is creating the store (its
// gate held EX, no D yet), the other runtime's service forms and socketed daemon wait for it in
// their start preflight, then judge the store it leaves, a store of the creator's runtime, and
// refuse it in the fence's words before anything lands in S or the scope registry: no
// service.json, daemon.lock or daemon.json, no scope claim. The runtime's own creation lets the
// same command go on once it is done. Each answer is the same bytes in both runtimes. Each form
// meets its own creator in its own S and scope registry, so no form's lock meets another's.
// Since todo 44 only Go's relay acts on a store Go's first opener creates: the Python relay and
// fence took the other roles until rollback to Python closed at todo 43 (rollback_allowed=0).
func Test31ServiceFormsWaitForACreatorAndJudgeTheStoreItLeaves(t *testing.T) {
	// Serial: it sleeps a fixed second and then asserts that no service form has answered; under parallel load a form may not have reached its wait yet and the check would pass vacuously.
	// Go's relay alone: the Python relay acted and created here until todo 44.
	relays := map[string]string{"go": relayCLI(t)}
	foreign := "{\n  \"error\": \"refused\",\n  \"reason\": \"store_owned_by_other\",\n  \"detail\": \"the relay store belongs to another runtime\"\n}\n"
	type pending struct {
		form          []string
		state, scopes string
		creator       *firstOpener
		answer        <-chan relayAnswer
	}
	for _, creator := range []string{"go"} {
		for _, actor := range []string{"go"} {
			t.Run(actor+"-acts-while-"+creator+"-creates", func(t *testing.T) {
				forms := [][]string{{"service", "disable"}, {"service", "enable"}, {"service", "declare", "--forget-execution-policy"},
					{"daemon", "--allow-isolated-scope", "--max-ticks", "0"}}
				if actor == creator {
					forms = forms[:1]
				}
				var cases []pending
				for _, form := range forms {
					home := stateDir(t)
					c := pending{form: form, state: filepath.Join(home, "state"), scopes: filepath.Join(home, "scopes")}
					c.creator = startOpener(t, creator, filepath.Join(c.state, "relay.sqlite3"), true)
					if line := c.creator.next(t); line != "paused" {
						t.Fatalf("creator: %s", line)
					}
					c.answer = relayStart(t, relays[actor], c.scopes, append([]string{"--state", c.state, "--socket", filepath.Join(home, "app.sock")}, form...)...)
					cases = append(cases, c)
				}
				time.Sleep(time.Second)
				for _, c := range cases {
					select {
					case answer := <-c.answer:
						entries, _ := os.ReadDir(c.state)
						var names []string
						for _, entry := range entries {
							names = append(names, entry.Name())
						}
						t.Errorf("%s %v did not wait for the %s creation (S now holds %v): exit %d\n%s%s", actor, c.form, creator, names, answer.code, answer.stdout, answer.stderr)
					default:
					}
				}
				if t.Failed() {
					return
				}
				for _, c := range cases {
					_, err := io.WriteString(c.creator.stdin, "go\n")
					must(t, err)
				}
				for _, c := range cases {
					if answer := c.creator.answer(t); answer != "admitted" {
						t.Fatalf("creator: %s", answer)
					}
					var answer relayAnswer
					select {
					case answer = <-c.answer:
					case <-time.After(60 * time.Second):
						t.Fatalf("%s %v still waiting after the creation", actor, c.form)
					}
					if actor == creator && (answer.code != 0 || !strings.Contains(answer.stdout, `"ok": true`)) ||
						actor != creator && (answer.code != 2 || answer.stdout != foreign) {
						t.Errorf("%s %v on a store %s created: exit %d\n%s%s", actor, c.form, creator, answer.code, answer.stdout, answer.stderr)
					}
					stamp, err := ownership.SnapshotMeta(t.Context(), filepath.Join(c.state, "relay.sqlite3"))
					must(t, err)
					if stamp.Owner != creator {
						t.Fatalf("stamp %+v, created by %s", stamp, creator)
					}
					entries, err := os.ReadDir(c.state)
					must(t, err)
					var names []string
					for _, entry := range entries {
						names = append(names, entry.Name())
					}
					if actor == creator {
						if !slices.Contains(names, "service.json") {
							t.Errorf("%s %v on its own store wrote no intent: %v", actor, c.form, names)
						}
						continue
					}
					for _, name := range names {
						switch name {
						case "relay.sqlite3", "relay.sqlite3-wal", "relay.sqlite3-shm", "takeover.json", "write-gate.lock":
						default:
							t.Errorf("%s %v wrote %s into S of a store %s created: %v", actor, c.form, name, creator, names)
						}
					}
					if claims, err := os.ReadDir(c.scopes); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("%s %v touched the scope registry: %v %v", actor, c.form, claims, err)
					}
				}
			})
		}
	}
}
