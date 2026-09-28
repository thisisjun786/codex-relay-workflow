package hook

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// errInputLate reports that the host had not supplied the rest of its payload
// when the hook's input allocation ran out while it was waiting.
var errInputLate = errors.New("the Stop payload did not arrive within the input allocation")

// readInput takes the Stop payload under the startup/input allocation (decision
// 24). Decision 30: the allocation bounds how long the hook WAITS for its host to
// supply bytes, and is judged only where it would otherwise wait. A descriptor is
// polled, so bytes that are already readable when the hook looks, including a
// complete payload written while this process was not scheduled, are taken rather
// than released as late; a payload that has not arrived by the deadline is still
// released. Readers that are not descriptors cannot be asked about readiness and
// keep the plain deadline. The work deadline bounds the whole read either way.
func readInput(work context.Context, input io.Reader, deadline time.Time) ([]byte, error) {
	if file, ok := input.(*os.File); ok {
		type polled struct {
			raw  []byte
			used bool
		}
		result, err := bounded(work, func() (polled, error) {
			raw, used, err := pollInput(file, deadline)
			return polled{raw, used}, err
		})
		if err != nil || result.used {
			return result.raw, err
		}
	}
	inputCtx, cancel := context.WithDeadline(work, deadline)
	defer cancel()
	return bounded(inputCtx, func() ([]byte, error) { return io.ReadAll(input) })
}

// pollInput reports used=false, having consumed nothing, when the descriptor
// cannot be polled; the caller then falls back to the plain deadline.
func pollInput(file *os.File, deadline time.Time) (raw []byte, used bool, err error) {
	conn, err := file.SyscallConn()
	if err != nil {
		return nil, false, nil
	}
	var readErr error
	if err := conn.Control(func(fd uintptr) { raw, used, readErr = pollDescriptor(int(fd), deadline) }); err != nil {
		return nil, false, nil
	}
	return raw, used, readErr
}

func pollDescriptor(fd int, deadline time.Time) ([]byte, bool, error) {
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
