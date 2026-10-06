package spawn

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// spawnParityNodeError is the oracle's Error.message for a managed dispatch failure
// (spawn-attach-hook.ts:892-907). A direct *os.PathError is spelled the way Node's fs layer spells
// it; anything else keeps Go's wording.
func TestSpawnParityNodeError(t *testing.T) {
	direct := &os.PathError{Op: "lstat", Path: "/w/.crw/dispatches/s/one.json", Err: syscall.ENOENT}
	if got, want := spawnParityNodeError(direct), "ENOENT: no such file or directory, lstat '/w/.crw/dispatches/s/one.json'"; got != want {
		t.Errorf("direct PathError = %q, want %q", got, want)
	}
	// write and close leave the path out, as nodeErrorMessage does.
	if got, want := spawnParityNodeError(&os.PathError{Op: "write", Path: "/w/x", Err: syscall.ENOSPC}), "ENOSPC: no space left on device, write"; got != want {
		t.Errorf("write PathError = %q, want %q", got, want)
	}
	// an errno the table does not name keeps Go's wording.
	unnamed := &os.PathError{Op: "lstat", Path: "/w/x", Err: syscall.E2BIG}
	if got := spawnParityNodeError(unnamed); got != unnamed.Error() {
		t.Errorf("unnamed errno = %q, want %q", got, unnamed.Error())
	}
	// a plain error is untouched.
	plain := errors.New("managed spawn attempt is not claimed or no longer current")
	if got := spawnParityNodeError(plain); got != plain.Error() {
		t.Errorf("plain error = %q, want %q", got, plain.Error())
	}
	// A WRAPPED PathError is NOT converted: the port's own wrapper text has no oracle counterpart,
	// and rewriting the whole message from the inner PathError would drop it (a dispatch-lock-clear
	// of this session is running / lock ... left behind).
	wrapped := fmt.Errorf("%w (a dispatch-lock-clear of this session is running)", &os.PathError{Op: "mkdir", Path: "/w/x.lock", Err: syscall.EEXIST})
	if got := spawnParityNodeError(wrapped); got != wrapped.Error() {
		t.Errorf("wrapped PathError = %q, want %q (the wrapper text must survive)", got, wrapped.Error())
	}
}
