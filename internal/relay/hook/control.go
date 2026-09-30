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
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
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

// controlDepth is the container depth to which control.py's json.loads decodes a request
// before the C scanner raises RecursionError (at 9999, measured against CPython 3.13 in the
// GuardServer's serving thread). The Go decoder has no bound of its own short of the goroutine
// stack, and overflowing that is fatal to the whole owner, not to one request.
const controlDepth = 9998

// readRequest is control.py _answer's reading of the one request line. A frame it cannot read
// as a JSON object is refused with the host detail control.py answers for it, "<exception
// class>: <message>"; err is a transport failure only: the peer went away or said nothing in
// time. A line cut short by end-of-file is read as it stands, as readline returns it.
func readRequest(r io.Reader) (request Object, refused string, err error) {
	raw, err := bufio.NewReader(io.LimitReader(r, maxControlBytes+1)).ReadBytes('\n')
	if len(raw) > maxControlBytes {
		// Judged before the read error: the limit ends an oversized frame with io.EOF too.
		return nil, "ValueError: guard request exceeds 64 MiB", nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, "", err
	}
	text, err := store.DecodeUTF8(raw)
	if err != nil {
		return nil, "UnicodeDecodeError: " + err.Error(), nil
	}
	message, recursion := store.PythonJSONErrorWithLimit(text, controlDepth)
	switch {
	case recursion:
		return nil, "RecursionError: " + message, nil
	case strings.HasPrefix(message, "Exceeds the limit"):
		// int() refuses the digits, not the JSON scanner: a ValueError, as Python raises it.
		return nil, "ValueError: " + message, nil
	case message != "":
		return nil, "JSONDecodeError: " + message, nil
	}
	value, err := decodeScanned(raw)
	if err != nil {
		return nil, "ValueError: " + err.Error(), nil
	}
	request, ok := evidence.Object(value)
	if !ok {
		return nil, "TypeError: guard request must be an object", nil
	}
	return request, "", nil
}

// guardParams is control.py _answer's reading of a guard-evaluate request's params: the
// request's own deadline, and the first check that fails as the host detail control.py
// answers, in its order and words.
func guardParams(request Object, now time.Time) (params, stop Object, deadline time.Time, refused string) {
	value, present := evidence.Lookup(request, "params")
	if !present {
		return nil, nil, time.Time{}, "KeyError: 'params'"
	}
	params, ok := evidence.Object(value)
	if !ok {
		return nil, nil, time.Time{}, "TypeError: guard params must be an object"
	}
	if value, present = evidence.Lookup(params, "stopInput"); !present {
		return nil, nil, time.Time{}, "KeyError: 'stopInput'"
	}
	if stop, ok = evidence.Object(value); !ok {
		return nil, nil, time.Time{}, "TypeError: stop input must be an object"
	}
	spelled, ok := get(params, "deadline").(string)
	if !ok {
		return nil, nil, time.Time{}, "TypeError: guard deadline must be a string"
	}
	spelled = strings.ReplaceAll(spelled, "Z", "+00:00")
	deadline, err := time.Parse(time.RFC3339Nano, spelled)
	if err != nil {
		return nil, nil, time.Time{}, "ValueError: Invalid isoformat string: " + store.PyRepr(spelled)
	}
	if !deadline.After(now) {
		return nil, nil, time.Time{}, "TimeoutError: guard request deadline expired"
	}
	for _, key := range []string{"socketPath", "program"} {
		if value := get(params, key); value != nil {
			if _, ok := value.(string); !ok {
				return nil, nil, time.Time{}, "TypeError: guard " + key + " must be a string"
			}
		}
	}
	return params, stop, deadline, ""
}

// answerHost answers a request with the relay's host record, as control.py answers a request
// it could not serve: the requester journals guard_host_error, never a refusal or silence.
func answerHost(conn net.Conn, detail string) error {
	_, err := io.WriteString(conn, evidence.Dumps(Object{{Key: "error", Value: "host"}, {Key: "detail", Value: detail}}, false, false, true)+"\n")
	return err
}

// rejectControl is the protocol dispatcher's answer before any guard command runs.
// It intentionally carries no relay error record or verdict.
func rejectControl(conn net.Conn) error {
	_, err := io.WriteString(conn, "{\"protocol\":1,\"requestRejected\":true}\n")
	return err
}
