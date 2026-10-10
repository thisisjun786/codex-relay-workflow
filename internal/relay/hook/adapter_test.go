package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// loadProofDeadlines lengthens the hook's real absolute, guard and transcript scan deadlines for the
// test that follows. Its subject is not a deadline, and its guard does real store and journal work
// that a loaded host can stretch past the production 5 s and 3.5 s, as it can stretch the transcript
// scan past 750 ms (CRW-1161). The settings' timeoutSeconds cannot do this: it only shortens the
// deadline. The tests of the deadlines keep the production ones. The fake host's hang guard grows
// with them: a peer that reads until the adapter hangs up waits out the guard's work.
func loadProofDeadlines(t *testing.T) {
	t.Helper()
	was, wasGuard, wasPeer, wasScan := absoluteDeadline, guardDeadline, fakeControlDeadline, scanDeadline
	absoluteDeadline, guardDeadline, fakeControlDeadline, scanDeadline = 2*time.Minute, time.Minute, 2*time.Minute, time.Minute
	t.Cleanup(func() {
		absoluteDeadline, guardDeadline, fakeControlDeadline, scanDeadline = was, wasGuard, wasPeer, wasScan
	})
}

func hookHome(t *testing.T, budget float64) string {
	t.Helper()
	home, err := os.MkdirTemp("", "h33-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	})
	return hookHomeAt(t, home, budget)
}

// hookHomeAt writes hookHome's settings into home.
func hookHomeAt(t *testing.T, home string, budget float64) string {
	t.Helper()
	config := Object{{Key: "configVersion", Value: int64(1)}, {Key: "mode", Value: "observe"}, {Key: "relayExecutable", Value: filepath.Join(home, "never-run")}, {Key: "markerRoot", Value: filepath.Join(home, "markers")}, {Key: "dbPath", Value: filepath.Join(home, "state/relay.sqlite3")}, {Key: "timeoutSeconds", Value: budget}, {Key: "journalRoot", Value: filepath.Join(home, "journal")}}
	writeTest(t, filepath.Join(home, ConfigName), []byte(pyjson.Dumps(config, pyjson.Options{})))
	writeTest(t, filepath.Join(home, "never-run"), []byte("must not execute"))
	return home
}
func hookEnv(home string) []string {
	env := os.Environ()
	for _, kv := range []string{"HOME=" + home, "CODEX_HOME=" + home, "XDG_STATE_HOME=" + home + "/xdg", "CODEX_SESSION_RELAY_STATE=" + home + "/state"} {
		key, _, _ := strings.Cut(kv, "=")
		env = slices.DeleteFunc(env, func(s string) bool { return strings.HasPrefix(s, key+"=") })
		env = append(env, kv)
	}
	return env
}
func hookCommand(t *testing.T, home, payload string) *exec.Cmd {
	t.Helper()
	// The first caller builds the binary; the timeout is for the hook, not the build.
	built := binary(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, built, "hook")
	cmd.Env = hookEnv(home)
	cmd.Stdin = strings.NewReader(payload)
	return cmd
}
func rowsAt(t *testing.T, home string) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, "journal", "[0-9]*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err = json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

// fakeControlDeadline is the fake host's socket hang guard. loadProofDeadlines lengthens it with the
// hook's own deadlines, since a peer that reads until the adapter hangs up waits out the guard's work.
var fakeControlDeadline = 8 * time.Second

func fakeControl(t *testing.T, home string, serve func(net.Conn) error) (<-chan error, func()) {
	t.Helper()
	deadline := fakeControlDeadline
	path := filepath.Join(home, "state", "control.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var result error
		defer func() {
			if p := recover(); p != nil {
				result = fmt.Errorf("fake host panic: %v", p)
			}
			done <- result
		}()
		conn, err := listener.Accept()
		if err != nil {
			result = err
			return
		}
		defer conn.Close()
		if err = conn.SetDeadline(time.Now().Add(deadline)); err != nil {
			result = err
			return
		}
		result = serve(conn)
	}()
	closeHost := func() { _ = listener.Close() }
	t.Cleanup(closeHost)
	return done, closeHost
}
func awaitHost(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		t.Fatal("fake host did not finish")
	}
}
func Test33HookHappyAndInvalid(t *testing.T) {
	for _, c := range []struct{ name, response, outcome, stdout string }{
		{"hold", `{"decision":"block","state":"receipt_missing","hook_output":{"decision":"block","reason":"verify the child","continue":true}}`, "guard_answered", `{"decision": "block", "reason": "verify the child", "continue": true}`},
		{"release", `{"decision":"release","state":"unmanaged","hook_output":{}}`, "guard_answered", ""},
		{"invalid", `{"decision":"release","hook_output":{"decision":"block","reason":"wrong","continue":true}}`, "guard_verdict_incomplete", ""},
		{"malformed", "not json", "guard_output_unreadable", ""},
		{"refused", `{"error":"refused","reason":"ownership"}`, "guard_refused", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := hookHome(t, 5)
			done, _ := fakeControl(t, home, func(conn net.Conn) error {
				request, err := readFrame(conn)
				if err != nil {
					return err
				}
				if request.Get("method") != "guard-evaluate" {
					return fmt.Errorf("wrong request %v", request)
				}
				_, err = io.WriteString(conn, c.response+"\n")
				return err
			})
			cmd := hookCommand(t, home, `{"session_id":"s","turn_id":"t"}`)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil || stderr.Len() != 0 || string(out) != c.stdout {
				t.Fatalf("%v out=%s stderr=%s", err, out, stderr.String())
			}
			awaitHost(t, done)
			rows := rowsAt(t, home)
			if len(rows) != 1 || rows[0]["adapterOutcome"] != c.outcome {
				t.Fatal(rows)
			}
		})
	}
}
func Test33HookFailures(t *testing.T) {
	for _, c := range []struct{ name, input string }{{"missing", "{}"}, {"unreadable", "{}"}, {"malformed", "not json"}} {
		t.Run(c.name, func(t *testing.T) {
			home := hookHome(t, 5)
			if c.name == "missing" {
				if err := os.Remove(filepath.Join(home, ConfigName)); err != nil {
					t.Fatal(err)
				}
			}
			if c.name == "unreadable" {
				writeTest(t, filepath.Join(home, ConfigName), []byte("{"))
			}
			cmd := hookCommand(t, home, c.input)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil || len(out) != 0 || stderr.Len() != 0 {
				t.Fatalf("%v %s %s", err, out, stderr.String())
			}
			rows := rowsAt(t, home)
			if c.name == "malformed" {
				if len(rows) != 1 || rows[0]["adapterOutcome"] != "stdin_not_json" {
					t.Fatal(rows)
				}
			} else if len(rows) != 0 {
				t.Fatal(rows)
			}
		})
	}
}
func Test33HookNoSocketJournalOnly(t *testing.T) {
	home := hookHome(t, 5)
	payload := `{"session_id":"s","turn_id":"t","stop_hook_active":false,"last_assistant_message":"done","transcript_path":"/must-not-be-read"}`
	cmd := hookCommand(t, home, payload)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("%v %s", err, out)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("unreachable hook did not return")
	}
	rows := rowsAt(t, home)
	if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_unreachable" || rows[0]["errno"] != "ENOENT" || rows[0]["held"] != false || rows[0]["eventIdentity"] != nil || rows[0]["acceptance"] != nil {
		t.Fatalf("unreachable row: %v", rows)
	}
	for _, p := range []string{"journal/accepted", "crw-completion-hook", "state/relay.sqlite3"} {
		if _, err := os.Stat(filepath.Join(home, p)); !os.IsNotExist(err) {
			t.Fatalf("unexpected effect %s: %v", p, err)
		}
	}
}
func Test33HookSlowGuardDeadline(t *testing.T) {
	home := hookHome(t, .4)
	t.Setenv("CODEX_HOME", home)
	synctest.Test(t, func(t *testing.T) {
		// This test owns deadline arithmetic, not OS scheduling. The real socket
		// and process deadline are exercised by Test33LatencyAcceptance. Accept
		// then close the unused connection: a network read would prevent the
		// virtual clock from advancing while the injected evaluator is blocked.
		accepted := make(chan struct{})
		done, _ := fakeControl(t, home, func(net.Conn) error { close(accepted); return nil })
		entered := make(chan time.Time, 1)
		guardDone := make(chan struct{})
		returned := make(chan int, 1)
		var out bytes.Buffer
		start := time.Now()
		go func() {
			returned <- runAdapter(context.Background(), nil, strings.NewReader(`{"session_id":"s","turn_id":"t"}`), &out, start, func(ctx context.Context, _ Object, _ GuardOptions) (Object, error) {
				deadline, _ := ctx.Deadline()
				entered <- deadline
				<-ctx.Done()
				close(guardDone)
				return nil, ctx.Err()
			})
		}()
		select {
		case <-accepted:
		case <-time.After(time.Second):
			t.Fatal("listener never accepted connection")
		}
		var deadline time.Time
		select {
		case deadline = <-entered:
		case <-time.After(time.Second):
			t.Fatal("guard evaluator was not entered")
		}
		// 400 ms outer budget reserves one fifth (80 ms) for bookkeeping.
		if want := start.Add(320 * time.Millisecond); !deadline.Equal(want) {
			t.Fatalf("guard deadline = %v, want %v", deadline, want)
		}
		select {
		case code := <-returned:
			if code != 0 || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%s", code, &out)
			}
		case <-time.After(time.Second):
			t.Fatal("guard deadline did not release the hook")
		}
		select {
		case <-guardDone:
		case <-time.After(time.Second):
			t.Fatal("guard context was not cancelled")
		}
		awaitHost(t, done)
		if elapsed := time.Since(start); elapsed != 320*time.Millisecond {
			t.Fatalf("virtual elapsed = %v, want 320ms", elapsed)
		}
		rows := rowsAt(t, home)
		if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_timed_out" {
			t.Fatal(rows)
		}
	})
}
func Test33RecoveredGuardPanic(t *testing.T) {
	loadProofDeadlines(t)
	home := hookHome(t, 5)
	t.Setenv("CODEX_HOME", home)
	done, _ := fakeControl(t, home, func(conn net.Conn) error { _, err := io.Copy(io.Discard, conn); return err })
	var stdout bytes.Buffer
	code := runAdapter(context.Background(), nil, strings.NewReader(`{"session_id":"s"}`), &stdout, time.Now(), func(context.Context, Object, GuardOptions) (Object, error) { panic("injected guard panic") })
	awaitHost(t, done)
	rows := rowsAt(t, home)
	if code != 0 || stdout.Len() != 0 || len(rows) != 1 || rows[0]["adapterOutcome"] != "adapter_faulted" || rows[0]["held"] != false {
		t.Fatalf("code=%d stdout=%s rows=%v", code, stdout.String(), rows)
	}
}
func Test33ClaimWithoutOutcomeNeverReplayed(t *testing.T) {
	home := hookHome(t, 5)
	config, failed, _ := ReadSettings(context.Background(), filepath.Join(home, ConfigName))
	if failed != "" {
		t.Fatal(failed)
	}
	key := EventKey("s", "t", false, "i")
	host := filepath.Join(home, "crw-completion-hook/stop-events")
	slot, err := NewSlot()
	if err != nil {
		t.Fatal(err)
	}
	stop := Object{{Key: "session_id", Value: "s"}, {Key: "turn_id", Value: "t"}, {Key: "stop_hook_active", Value: false}}
	identity := Object{{Key: "answerItem", Value: "i"}}
	a, _ := ClaimEvent(context.Background(), config, key, identity, stop, slot, host)
	if a != "accepted" {
		t.Fatal(a)
	}
	path := filepath.Join(host, key+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ = ClaimEvent(context.Background(), config, key, identity, stop, slot, host)
	after, err := os.ReadFile(path)
	if err != nil || a != "duplicate" || !bytes.Equal(before, after) {
		t.Fatalf("%s %v", a, err)
	}
}

// A peer that reads until the adapter hangs up waits out the guard's work, so under the load-proof
// deadlines a guard slower than the fake host's hang guard still ends cleanly (CRW-1161).
func Test33LoadProofDeadlinesOutlastTheFakeHostsHangGuard(t *testing.T) {
	was := fakeControlDeadline
	fakeControlDeadline = 200 * time.Millisecond // stands in for the 8 s hang guard; the guard outlasts it
	t.Cleanup(func() { fakeControlDeadline = was })
	loadProofDeadlines(t)
	home := hookHome(t, 5)
	t.Setenv("CODEX_HOME", home)
	done, _ := fakeControl(t, home, func(conn net.Conn) error { _, err := io.Copy(io.Discard, conn); return err })
	var stdout bytes.Buffer
	code := runAdapter(context.Background(), nil, strings.NewReader(`{"session_id":"s"}`), &stdout, time.Now(), func(context.Context, Object, GuardOptions) (Object, error) {
		time.Sleep(time.Second)
		panic("injected guard panic")
	})
	awaitHost(t, done)
	rows := rowsAt(t, home)
	if code != 0 || stdout.Len() != 0 || len(rows) != 1 || rows[0]["adapterOutcome"] != "adapter_faulted" {
		t.Fatalf("code=%d stdout=%s rows=%v", code, stdout.String(), rows)
	}
}

// The load-proof deadlines are the hook's real ones: a guard slower than the production 3.5 s cap
// (a loaded host's store and journal work) still answers under them, and does not without them.
func Test33LoadProofDeadlinesOutlastASlowGuard(t *testing.T) {
	for _, lengthened := range []bool{false, true} {
		t.Run(fmt.Sprint("lengthened=", lengthened), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				if lengthened {
					loadProofDeadlines(t)
				}
				home := hookHome(t, 5)
				t.Setenv("CODEX_HOME", home)
				writeTest(t, filepath.Join(home, "transcript.jsonl"), []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}`+"\n"+`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t","thread_id":"s","item":{"type":"AgentMessage","id":"i","content":[{"type":"Text","text":"done"}]}}}`+"\n"))
				payload := `{"session_id":"s","turn_id":"t","stop_hook_active":false,"last_assistant_message":"done","transcript_path":` + strconv.Quote(filepath.Join(home, "transcript.jsonl")) + `}`
				done, _ := fakeControl(t, home, func(net.Conn) error { return nil })
				verdict := Object{{Key: "decision", Value: "block"}, {Key: "state", Value: "receipt_missing"}, {Key: "hook_output", Value: Object{{Key: "decision", Value: "block"}, {Key: "reason", Value: "verify"}, {Key: "continue", Value: true}}}}
				var out bytes.Buffer
				code := runAdapter(context.Background(), nil, strings.NewReader(payload), &out, time.Now(), func(ctx context.Context, _ Object, _ GuardOptions) (Object, error) {
					select {
					case <-time.After(10 * time.Second):
						return verdict, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				})
				awaitHost(t, done)
				answered := out.String() == `{"decision": "block", "reason": "verify", "continue": true}`
				if code != 0 || answered != lengthened {
					t.Fatalf("lengthened=%v code=%d answer=%q", lengthened, code, out.String())
				}
			})
		})
	}
}

// The load-proof deadlines include the transcript scan's: the fault and replay tests identify the
// event through a real transcript before the guard or the claim write they test, and a scan that a
// loaded host stretches past its production 750 ms would skip both. A scan deadline already spent
// (the unlengthened form) refuses the identity; the lengthened form establishes it.
func Test33LoadProofDeadlinesLengthenTheTranscriptScan(t *testing.T) {
	for _, lengthened := range []bool{false, true} {
		t.Run(fmt.Sprint("lengthened=", lengthened), func(t *testing.T) {
			previous := scanDeadline
			t.Cleanup(func() { scanDeadline = previous }) // runs after loadProofDeadlines' own restore
			scanDeadline = time.Nanosecond
			if lengthened {
				loadProofDeadlines(t)
			}
			home := t.TempDir()
			path := filepath.Join(home, "transcript.jsonl")
			writeTest(t, path, []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t"}}`+"\n"+`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t","thread_id":"s","item":{"type":"AgentMessage","id":"i","content":[{"type":"Text","text":"done"}]}}}`+"\n"))
			stop := Object{{Key: "session_id", Value: "s"}, {Key: "turn_id", Value: "t"}, {Key: "stop_hook_active", Value: false}, {Key: "last_assistant_message", Value: "done"}, {Key: "transcript_path", Value: path}}
			_, identity := EventIdentity(context.Background(), stop)
			if established := identity.Get("established") == true; established != lengthened {
				t.Fatalf("lengthened=%v identity=%v", lengthened, identity)
			}
			if !lengthened && identity.Get("reason") != "scan_timed_out" {
				t.Fatal(identity)
			}
		})
	}
}
