package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// errInputLate reports that the host had not supplied the rest of its payload
// when the hook's input allocation ran out while it was waiting.
var errInputLate = errors.New("the Stop payload did not arrive within the input allocation")

// readFailure is a failed read of the Stop payload with the cause it was given where it arose
// (one of StdinReadCauses). It changes nothing about the error: Unwrap keeps errors.Is working.
type readFailure struct {
	cause string
	err   error
}

func (f *readFailure) Error() string { return f.err.Error() }
func (f *readFailure) Unwrap() error { return f.err }

// named gives a failure readInput returns its cause. One a read closure raised already has it. What
// bounded itself returns is a context that gave up, or a recovered panic. For a context the cause
// says which gave up first: the input context carries errInputLate for its own deadline, anything
// else is the work context ending (on the descriptor path bounded watches only work).
func named(err error, watched context.Context) error {
	var failure *readFailure
	switch {
	case err == nil || errors.As(err, &failure):
		return err
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		if cause := context.Cause(watched); errors.Is(cause, errInputLate) {
			return &readFailure{StdinInputLate, cause} // the same words the descriptor path says
		}
		return &readFailure{StdinWorkEnded, err}
	}
	return &readFailure{StdinReadError, err}
}

// counted tallies what the reads beneath it report, before their error is looked at.
type counted struct {
	io.Reader
	taken *atomic.Int64
}

func (c counted) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	if n > 0 {
		c.taken.Add(int64(n))
	}
	return n, err
}

// readInput takes the Stop payload under the startup/input allocation (decision
// 24). Decision 32: the allocation bounds how long the hook WAITS for its host to
// supply bytes, and is judged only where it would otherwise wait. A descriptor is
// polled, so bytes that are already readable when the hook looks, including a
// complete payload written while this process was not scheduled, are taken rather
// than released as late; a payload that has not arrived by the deadline is still
// released. Readers that are not descriptors cannot be asked about readiness and
// keep the plain deadline. The work deadline bounds the whole read either way.
//
// taken counts the bytes the completed reads reported, whether or not the read then
// failed: a failure drops the bytes themselves, and the goroutine that reads can outlive
// this return, so the count is what a row can say of them. An error it returns is always
// a *readFailure.
func readInput(work context.Context, input io.Reader, deadline time.Time, taken *atomic.Int64) ([]byte, error) {
	if file, ok := input.(*os.File); ok {
		type polled struct {
			raw  []byte
			used bool
		}
		result, err := bounded(work, func() (polled, error) {
			raw, used, err := pollInput(file, deadline, taken)
			if errors.Is(err, errInputLate) {
				err = &readFailure{StdinInputLate, err}
			} else if err != nil {
				err = &readFailure{StdinReadError, err}
			}
			return polled{raw, used}, err
		})
		if err != nil || result.used {
			return result.raw, named(err, work)
		}
	}
	inputCtx, cancel := context.WithDeadlineCause(work, deadline, errInputLate)
	defer cancel()
	raw, err := bounded(inputCtx, func() ([]byte, error) {
		raw, err := io.ReadAll(counted{input, taken})
		if err != nil {
			err = &readFailure{StdinReadError, err}
		}
		return raw, err
	})
	return raw, named(err, inputCtx)
}

// stdinReadRecord is the row's account of a read that failed: the cause, the Go error text, the bytes
// the completed reads reported and the wait as offsets from process entry (the origin of elapsedMs).
// It holds no payload: an error text is the OS's or a context's, about the descriptor, and a
// recovered panic is named by its Go type because its value could be anything.
func stdinReadRecord(cause string, err error, bytes int64, waitStarted, waitEnded time.Duration) Object {
	text := err.Error()
	var panicked *recoveredPanic
	if errors.As(err, &panicked) {
		text = fmt.Sprintf("panic in the stdin reader: %T", panicked.value)
	}
	return Object{{Key: "cause", Value: cause}, {Key: "error", Value: text}, {Key: "bytesRead", Value: bytes}, {Key: "waitStartedMs", Value: waitStarted.Milliseconds()}, {Key: "waitEndedMs", Value: waitEnded.Milliseconds()}}
}

// pollInput reports used=false, having consumed nothing, when the descriptor
// cannot be polled; the caller then falls back to the plain deadline.
func pollInput(file *os.File, deadline time.Time, taken *atomic.Int64) (raw []byte, used bool, err error) {
	conn, err := file.SyscallConn()
	if err != nil {
		return nil, false, nil
	}
	var readErr error
	if err := conn.Control(func(fd uintptr) { raw, used, readErr = pollDescriptor(int(fd), deadline, taken) }); err != nil {
		return nil, false, nil
	}
	return raw, used, readErr
}

func pollDescriptor(fd int, deadline time.Time, taken *atomic.Int64) ([]byte, bool, error) {
	raw := []byte{}
	buffer := make([]byte, 64<<10)
	for {
		timeout := 0
		if wait := time.Until(deadline); wait > 0 {
			timeout = int((wait + time.Millisecond - 1) / time.Millisecond)
		}
		ready := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(ready, timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || (n > 0 && ready[0].Revents&unix.POLLNVAL != 0) {
			if len(raw) == 0 {
				return nil, false, nil
			}
			if err == nil {
				err = unix.EBADF
			}
			return nil, true, err
		}
		if n == 0 {
			if time.Now().Before(deadline) {
				continue
			}
			return nil, true, errInputLate
		}
		count, err := unix.Read(fd, buffer)
		if count > 0 {
			taken.Add(int64(count))
		}
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return nil, true, err
		}
		if count == 0 {
			return raw, true, nil
		}
		raw = append(raw, buffer[:count]...)
	}
}
