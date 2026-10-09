package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// receiptLockHeld reports whether another open file description holds the receipt's lock file. It asks on a
// handle of its own: flock locks belong to the open file description, so this contends with the run under test even
// inside one process.
func receiptLockHeld(t *testing.T, root string) bool {
	t.Helper()
	lock, err := receiptLockFile(expectedReceiptPath(root))
	receiptMust(t, err)
	defer lock.Close() // drops the lock if the probe took it
	err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true
	}
	receiptMust(t, err)
	return false
}

func receiptDirNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(expectedReceiptPath(root)))
	receiptMust(t, err)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// Run A withdraws the receipt it just published after a late cancellation, and run B of the same session publishes
// its own receipt while A sits between comparing the bytes and unlinking them (receiptLockAfterCompareHook). B has
// already passed its run-start removal: it is blocked in its command until the hook releases it. With the directory
// lock B's publication waits for A to finish, so B's receipt is the one left on disk. Without it B's rename lands
// inside the window and A unlinks B's receipt, which is the loss this test was written to catch.
func TestReceiptWithdrawalKeepsAnotherRunsReceipt(t *testing.T) {
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	stdinR, stdinW, err := os.Pipe()
	receiptMust(t, err)
	readyR, readyW, err := os.Pipe()
	receiptMust(t, err)
	t.Cleanup(func() { // lets B end if the test fails before it releases B
		for _, f := range []*os.File{stdinR, stdinW, readyR, readyW} {
			_ = f.Close()
		}
	})
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	var before, published atomic.Int32
	bAtPublish := make(chan struct{})
	receiptBeforePublishHook = func() {
		if before.Add(1) == 2 { // A's call is the first, B's the second
			close(bAtPublish)
		}
	}
	defer func() { receiptBeforePublishHook = nil }()
	receiptAfterPublishHook = func() {
		if published.Add(1) == 1 { // A's publication cancels A late; B's does not
			cancelA()
		}
	}
	defer func() { receiptAfterPublishHook = nil }()
	path := expectedReceiptPath(root)
	receiptLockAfterCompareHook = func() {
		mine, err := os.ReadFile(path)
		receiptMust(t, err)
		receiptMust(t, stdinW.Close()) // B's command ends and B goes on to publish
		select {
		case <-bAtPublish:
		case <-time.After(10 * time.Second):
			t.Error("run B never reached its publication")
			return
		}
		// B's receipt replaces A's within milliseconds when nothing makes B wait. After the fix this bound simply runs
		// out with A's receipt still there, which is the expected outcome; the assertions below are on the final state.
		for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if now, err := os.ReadFile(path); err == nil && !bytes.Equal(now, mine) {
				return
			}
		}
	}
	defer func() { receiptLockAfterCompareHook = nil }()

	argsB := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "block")}
	optsB := ReceiptRunOptions{Stdin: stdinR, Stdout: readyW, Stderr: io.Discard}
	argsA := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	type outcome struct {
		result ReceiptCLIResult
		err    error
	}
	gotB := make(chan outcome, 1)
	go func() {
		result, err := RunReceiptCLI(argsB, optsB)
		gotB <- outcome{result, err}
	}()
	ready := make(chan error, 1)
	go func() { _, err := io.ReadFull(readyR, make([]byte, len("ready\n"))); ready <- err }()
	select {
	case err := <-ready:
		receiptMust(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("run B never reached its command")
	}

	got := receiptRun(t, argsA, ReceiptRunOptions{Context: ctxA})
	if got.Code != 1 || got.Output != receiptInterrupted {
		t.Fatalf("run A = %#v, want the interrupted refusal", got)
	}
	select {
	case b := <-gotB:
		receiptMust(t, b.err)
		if b.result != (ReceiptCLIResult{Output: path}) {
			t.Fatalf("run B = %#v, want its receipt path", b.result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run B never finished")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("run B's receipt is gone after run A's withdrawal: %v", err)
	}
	if !strings.Contains(string(data), "--receipt-helper block") {
		t.Fatalf("the receipt on disk is not run B's: %s", data)
	}
}

// The lock is the receipt's lock file (test-receipt.json.lock): held from the publication through the withdrawal's
// unlink and released when the call returns. The file stays in the session directory once a run has taken it, and it
// is the only file besides the receipt that a run leaves there.
func TestReceiptDirectoryIsLockedFromPublishThroughWithdrawal(t *testing.T) {
	root := receiptRepo(t)
	t.Setenv("CRW_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var afterPublish, afterCompare bool
	receiptAfterPublishHook = func() {
		afterPublish = receiptLockHeld(t, root)
		cancel()
	}
	defer func() { receiptAfterPublishHook = nil }()
	receiptLockAfterCompareHook = func() { afterCompare = receiptLockHeld(t, root) }
	defer func() { receiptLockAfterCompareHook = nil }()
	a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
	got := receiptRun(t, a, ReceiptRunOptions{Context: ctx})
	if got.Code != 1 || got.Output != receiptInterrupted {
		t.Fatalf("withdrawn run = %#v", got)
	}
	if !afterPublish || !afterCompare {
		t.Fatalf("directory locked after the publication = %v, after the comparison = %v; want both", afterPublish, afterCompare)
	}
	if receiptLockHeld(t, root) {
		t.Fatal("the lock outlives the call")
	}
	if names := receiptDirNames(t, root); !reflect.DeepEqual(names, []string{"test-receipt.json.lock"}) {
		t.Fatalf("withdrawn run left %v, want only the lock file", names)
	}

	receiptAfterPublishHook, receiptLockAfterCompareHook = nil, nil
	got = receiptRun(t, a, ReceiptRunOptions{})
	if got != (ReceiptCLIResult{Output: expectedReceiptPath(root)}) {
		t.Fatalf("uninterrupted run = %#v", got)
	}
	if names := receiptDirNames(t, root); !reflect.DeepEqual(names, []string{"test-receipt.json", "test-receipt.json.lock"}) {
		t.Fatalf("uninterrupted run left %v, want the receipt and its lock file", names)
	}
	if receiptLockHeld(t, root) {
		t.Fatal("the lock outlives the call")
	}
}

// A run that waits for a lock another holder has ends with its context: it publishes nothing and answers the
// interrupted refusal, whether the context ended before the wait began or during it.
func TestReceiptLockWaitEndsWithTheContext(t *testing.T) {
	for _, tc := range []struct {
		name          string
		endDuringWait bool
	}{{"ended before the wait", false}, {"ends during the wait", true}} {
		t.Run(tc.name, func(t *testing.T) {
			root := receiptRepo(t)
			t.Setenv("CRW_HOME", t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ended := make(chan struct{})
			// close(ended) before cancel(): the run returns as soon as it sees the cancellation, so a mark
			// set after it can land after that return.
			end := func() { close(ended); cancel() }
			var holder *os.File
			defer func() {
				if holder != nil {
					holder.Close()
				}
			}()
			parked := make(chan struct{})
			receiptBeforePublishHook = func() { // the directory exists here; another holder takes its lock
				var err error
				holder, err = receiptLockFile(expectedReceiptPath(root))
				receiptMust(t, err)
				receiptMust(t, unix.Flock(int(holder.Fd()), unix.LOCK_EX))
				if !tc.endDuringWait {
					end()
				}
			}
			defer func() { receiptBeforePublishHook = nil }()
			if tc.endDuringWait {
				// The wait signals once the run is parked on the held lock, after its first refused attempt:
				// ending the context there is the wait's own event, not a timer that can fire before it.
				receiptLockWaitParkedHook = func() { close(parked) }
				defer func() { receiptLockWaitParkedHook = nil }()
			}
			a := ReceiptCLIArgs{Verb: "test", Cwd: root, Session: "s1", Command: receiptCommand(t, "exit", "0")}
			done := make(chan ReceiptCLIResult, 1)
			failed := make(chan error, 1)
			go func() {
				got, err := RunReceiptCLI(a, ReceiptRunOptions{Context: ctx, Stdout: io.Discard, Stderr: io.Discard})
				done <- got
				failed <- err
			}()
			if tc.endDuringWait {
				select {
				case <-parked:
				case <-time.After(10 * time.Second):
					t.Fatal("the lock wait never parked on the held lock")
				}
				end()
			}
			var got ReceiptCLIResult
			select {
			case got = <-done:
				receiptMust(t, <-failed)
			case <-time.After(10 * time.Second):
				t.Fatal("the lock wait ignores the context")
			}
			select {
			case <-ended:
			default:
				t.Fatal("the run returned while the lock was held and its context live")
			}
			if got != (ReceiptCLIResult{Output: receiptInterrupted, Code: 1}) {
				t.Fatalf("got %#v, want the interrupted refusal", got)
			}
			receiptAbsent(t, root)
			if names := receiptDirNames(t, root); !reflect.DeepEqual(names, []string{"test-receipt.json.lock"}) {
				t.Fatalf("the refused run left %v, want only the lock file", names)
			}
		})
	}
}
