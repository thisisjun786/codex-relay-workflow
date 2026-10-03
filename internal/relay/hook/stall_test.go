package hook

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// Decision 32. A loaded host can leave a runnable hook unscheduled for longer than
// its 100 ms startup/input allocation. Passing a process entry time 300 ms in the
// past reproduces that stall exactly: every allocation anchored at entry has
// already elapsed before the hook does any work, while the host's settings and
// payload were ready all along. The hook must behave as it does unstalled.
const stall = 300 * time.Millisecond

// pipeInput is the host's stdin: a real descriptor already holding the complete
// payload and its EOF, as the host leaves it when it writes and closes before the
// hook runs. The payload fits the pipe buffer, so both happen before this returns.
func pipeInput(t *testing.T, payload []byte) *os.File {
	t.Helper()
	if len(payload) >= 4096 {
		t.Fatal("a pipe payload must fit the pipe buffer to be complete before the hook starts")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	_, err = writer.Write(payload)
	if err = errors.Join(err, writer.Close()); err != nil {
		t.Fatal(err)
	}
	return reader
}

// fileInput is a prefilled regular file on stdin, as review D9 supplies 5 MiB.
func fileInput(t *testing.T, payload []byte) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin.json")
	writeTest(t, path, payload)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func prescanRow(t *testing.T, home string) Object {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, "journal", "[0-9]*", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("want exactly one journal row, found %v (%v); rows %v", paths, err, rowsAt(t, home))
	}
	row, ok := ReadNativePrescanRow(paths[0])
	if !ok {
		t.Fatalf("not the decision-22 pre-scan row: %v", rowsAt(t, home))
	}
	if row.Get("errno") != "ENOENT" {
		t.Fatal(row)
	}
	for _, p := range []string{"journal/accepted", "crw-completion-hook"} {
		if _, err := os.Stat(filepath.Join(home, p)); !os.IsNotExist(err) {
			t.Fatalf("pre-scan path claimed %s: %v", p, err)
		}
	}
	return row
}

// Before decision 32 the settings read was cut at entry+100 ms (no row at all),
// the payload was released as stdin_unreadable at entry+100 ms, and the pre-scan
// row was skipped at entry+125 ms. Test33NativeJournalReaderPythonLive and the
// Domain/hook corpus observed exactly these missing rows on loaded CI runners.
func Test33StalledEntryKeepsThePrescanRow(t *testing.T) {
	payload := `{"session_id":"s","turn_id":"t","stop_hook_active":false,"last_assistant_message":"done","transcript_path":"/must-not-be-read"}`
	for _, c := range []struct {
		name  string
		input func(*testing.T, []byte) *os.File
		pad   int
	}{{"pipe", pipeInput, 0}, {"prefilled_5MiB_file", fileInput, 5 << 20}} {
		t.Run(c.name, func(t *testing.T) {
			home := hookHome(t, 5)
			t.Setenv("CODEX_HOME", home)
			raw := []byte(payload)
			if c.pad > 0 {
				raw = []byte(strings.TrimSuffix(payload, "}") + `,"padding":"` + strings.Repeat("x", c.pad) + `"}`)
			}
			var out bytes.Buffer
			if code := runAdapter(context.Background(), nil, c.input(t, raw), &out, time.Now().Add(-stall), nil); code != 0 || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%s", code, &out)
			}
			row := prescanRow(t, home)
			if row.Get("sessionId") != "s" || row.Get("turnId") != "t" {
				t.Fatal(row)
			}
		})
	}
}

// A stall between the dial's start and its connect turned a missing socket into a
// dial timeout: the post-claim ETIMEDOUT shape instead of the pre-scan ENOENT row
// (and, with a live peer, guard_unreachable instead of its answer).
func Test33StalledDialStillConnects(t *testing.T) {
	home := hookHome(t, 5)
	t.Setenv("CODEX_HOME", home)
	input := pipeInput(t, []byte(`{"session_id":"s","turn_id":"t","stop_hook_active":false,"last_assistant_message":"done","transcript_path":"/must-not-be-read"}`))
	synctest.Test(t, func(t *testing.T) {
		// Virtual time: the pause advances the clock by a full second while
		// nothing waits, which is what an unscheduled process experiences.
		ctx := context.WithValue(context.Background(), beforeDialKey{}, func() { time.Sleep(time.Second) })
		var out bytes.Buffer
		if code := runAdapter(ctx, nil, input, &out, time.Now(), nil); code != 0 || out.Len() != 0 {
			t.Fatalf("code=%d stdout=%s", code, &out)
		}
	})
	prescanRow(t, home)
}

// The stall must not release a turn the guard holds: Python, which has no startup
// allocation, delivers the block, and so must the native hook.
func Test33StalledEntryStillHolds(t *testing.T) {
	home := hookHome(t, 5)
	t.Setenv("CODEX_HOME", home)
	done, _ := fakeControl(t, home, func(conn net.Conn) error {
		if _, err := readFrame(conn); err != nil {
			return err
		}
		_, err := io.WriteString(conn, `{"decision":"block","state":"receipt_missing","hook_output":{"decision":"block","reason":"verify the child","continue":true}}`+"\n")
		return err
	})
	var out bytes.Buffer
	code := runAdapter(context.Background(), nil, pipeInput(t, []byte(`{"session_id":"s","turn_id":"t"}`)), &out, time.Now().Add(-stall), nil)
	rows := rowsAt(t, home)
	if code != 0 || out.String() != `{"decision": "block", "reason": "verify the child", "continue": true}` {
		t.Fatalf("code=%d stdout=%q rows=%v", code, out.String(), rows)
	}
	if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_answered" || rows[0]["held"] != true {
		t.Fatal(rows)
	}
	awaitHost(t, done)
}

// Decision 24 still holds on a descriptor: a payload the host has not written when
// the input allocation runs out is released, and bytes written afterwards are not
// taken. The payload is written only after the hook has returned, so the outcome
// cannot depend on scheduling; the bound on the wait only reports a hook that no
// longer releases (it would otherwise wait for the work deadline).
func Test33LateInputOnADescriptorIsReleased(t *testing.T) {
	home := hookHome(t, 5)
	t.Setenv("CODEX_HOME", home)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	returned := make(chan int, 1)
	var out bytes.Buffer
	go func() { returned <- runAdapter(context.Background(), nil, reader, &out, time.Now(), nil) }()
	select {
	case code := <-returned:
		if code != 0 || out.Len() != 0 {
			t.Fatalf("code=%d stdout=%s", code, &out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the input allocation did not release a payload that never arrived")
	}
	if _, err := writer.Write([]byte(`{"session_id":"s","turn_id":"t"}`)); err != nil {
		t.Fatal(err)
	}
	rows := rowsAt(t, home)
	if len(rows) != 1 || rows[0]["adapterOutcome"] != "stdin_unreadable" || rows[0]["sessionId"] != nil {
		t.Fatal(rows)
	}
}

// A host that leaves the hook unscheduled across its whole configured budget, after the guard
// was asked, must still leave the invocation's row: the guard's allocation expired while the
// process slept, so the verdict is guard_timed_out, and the small create-once journal write that
// records it is bounded by the 5 s absolute deadline, not by the shortened timeoutSeconds whose
// reserve the stall already spent. Stopping the process makes the stall exact: it starts after
// the owner has the request and ends 1.5 s later, past the whole 1 s budget. (The contract
// corpus's guard-timeout fixtures lost this row on loaded CI runners.)
func Test33StalledPastTheBudgetKeepsTheTimedOutRow(t *testing.T) {
	home := hookHome(t, 1)
	requested := make(chan struct{})
	done, _ := fakeControl(t, home, func(conn net.Conn) error {
		if _, err := readFrame(conn); err != nil {
			return err
		}
		close(requested)
		_, err := io.Copy(io.Discard, conn) // never answers; the hook's close ends the read
		return err
	})
	cmd := hookCommand(t, home, `{"session_id":"s","turn_id":"t"}`)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook never asked the owner")
	}
	if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil || output.Len() != 0 {
		t.Fatalf("%v %s", err, output.String())
	}
	awaitHost(t, done)
	rows := rowsAt(t, home)
	if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_timed_out" || rows[0]["processEnding"] != "timed_out" || rows[0]["held"] != false {
		t.Fatalf("rows after a stall past the budget: %v", rows)
	}
}
