package hook

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// The control protocol is one JSON object per line with a 64 MiB transport bound.
// A response is the complete legacy verdict, not the host's three fields.
const maxControlBytes = 64 << 20

type responseError struct{ outcome, reading string }

func (e *responseError) Error() string { return e.outcome }

func readFrame(r io.Reader) (Object, error) {
	reader := bufio.NewReader(io.LimitReader(r, maxControlBytes+1))
	raw, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &responseError{"guard_said_nothing", "said_nothing"}
	}
	if len(raw) > maxControlBytes {
		return nil, &responseError{"guard_output_unreadable", "said_something_unreadable"}
	}
	value, decodeErr := decodeObject(raw)
	if decodeErr != nil {
		return nil, &responseError{"guard_output_unreadable", "said_something_unreadable"}
	}
	return value, nil
}
func RequestGuard(ctx context.Context, conn net.Conn, stop Object, options GuardOptions) (Object, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, fmt.Errorf("guard request requires an absolute deadline")
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := authenticatePeer(ctx, conn); err != nil {
		return nil, err
	}
	params := Object{{Key: "markerRoot", Value: options.Root}, {Key: "stopInput", Value: stop}, {Key: "mode", Value: options.Mode}, {Key: "dbPath", Value: nullable(options.DBPath)}, {Key: "now", Value: nullable(options.Now)}, {Key: "noRecord", Value: options.NoRecord}, {Key: "deadline", Value: deadline.UTC().Format(time.RFC3339Nano)}}
	params = append(params, Field{Key: "socketPath", Value: nullable(options.SocketPath)}, Field{Key: "program", Value: nullable(options.Program)})
	request := Object{{Key: "protocol", Value: int64(1)}, {Key: "method", Value: "guard-evaluate"}, {Key: "params", Value: params}}
	if _, err := io.WriteString(conn, evidence.Dumps(request, true, false, true)+"\n"); err != nil {
		return nil, err
	}
	response, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	// Dispatch rejection is not a domain refusal: the peer understood the frame
	// but did not run a guard command. EOF alone cannot establish that distinction.
	if len(response) == 2 && get(response, "protocol") == int64(1) && get(response, "requestRejected") == true {
		return nil, &responseError{"guard_rejected_the_call", "said_nothing"}
	}
	return response, nil
}

// sunPathLimit is the shortest sockaddr_un path capacity of the supported targets
// (darwin 104, Linux 108); Python's control.py uses the same threshold.
const sunPathLimit = 104

// ControlAddress is the name a bind or connect uses for the control socket at path. A
// path a sockaddr_un cannot hold is reached through /proc/self/fd/<dirfd>, as Python's
// GuardServer and Stop adapter do; release closes that directory descriptor once the
// bind or connect has resolved it.
func ControlAddress(path string) (string, func(), error) {
	if len(path) < sunPathLimit {
		return path, func() {}, nil
	}
	fd, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("/proc/self/fd/%d/%s", fd, filepath.Base(path)), func() { _ = unix.Close(fd) }, nil
}

// rejectControl is the protocol dispatcher's answer before any guard command runs.
// It intentionally carries no relay error record or verdict.
func rejectControl(conn net.Conn) error {
	_, err := io.WriteString(conn, "{\"protocol\":1,\"requestRejected\":true}\n")
	return err
}
