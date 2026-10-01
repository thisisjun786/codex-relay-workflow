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
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// The control protocol is one JSON object per line with a 64 MiB transport bound.
// A response is the complete legacy verdict, not the host's three fields.
const maxControlBytes = 64 << 20

// ControlAnswerGrace is how long before its deadline HandleControl stops waiting for the request
// line, to answer a peer that never finished it: the owner's listener gives each connection 5 s
// for the whole line plus this grace.
const ControlAnswerGrace = 250 * time.Millisecond

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
	if _, err := io.WriteString(conn, pyjson.Dumps(request, pyjson.Options{Compact: true})+"\n"); err != nil {
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

// controlDepth is how deep the containers of a request may nest: the depth to which the owner
// has always read one, so every Stop a hook forwards is read as before; a deeper request is
// refused before it is decoded.
const controlDepth = 9996

// nesting is how deep the containers of the JSON text raw nest, its strings skipped. It bounds
// what a decoder is handed; whether raw is JSON at all is the decoder's to say.
func nesting(raw []byte) int {
	depth, deepest, quoted, escaped := 0, 0, false, false
	for _, c := range raw {
		switch {
		case escaped:
			escaped = false
		case quoted && c == '\\':
			escaped = true
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '[' || c == '{':
			depth++
			deepest = max(deepest, depth)
		case c == ']' || c == '}':
			depth--
		}
	}
	return deepest
}

// requestValues is how the owner reads a request: as the hook read the Stop payload it forwards
// (hookValues), so NaN, the infinities and a lone surrogate escape the hook accepted from Codex
// reach the guard as the values the hook read; objects keep their order and an integer is an
// int64. Nothing else of json.loads' reading is kept: the line is strict UTF-8 JSON.
var requestValues = pyjson.LoadOptions{Constants: true, Surrogates: true, Numbers: pyjson.Int64Numbers}

// readRequest reads the one request line: at most 64 MiB, UTF-8, JSON nested no deeper than
// controlDepth, and an object. A line the owner cannot read is refused with the host detail
// saying why; err is a transport failure only: the peer went away or said nothing in time. A
// line cut short by end-of-file is read as it stands.
func readRequest(r io.Reader) (request Object, refused string, err error) {
	raw, err := bufio.NewReader(io.LimitReader(r, maxControlBytes+1)).ReadBytes('\n')
	if len(raw) > maxControlBytes {
		// Judged before the read error: the limit ends an oversized frame with io.EOF too.
		return nil, "guard request exceeds 64 MiB", nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, "", err
	}
	if !utf8.Valid(raw) {
		return nil, "guard request is not UTF-8", nil
	}
	if nesting(raw) > controlDepth {
		return nil, fmt.Sprintf("guard request nests deeper than %d containers", controlDepth), nil
	}
	value, err := pyjson.Loads(string(raw), requestValues)
	if err != nil {
		return nil, "guard request is not JSON: " + err.Error(), nil
	}
	request, ok := evidence.Object(value)
	if !ok {
		return nil, "guard request must be an object", nil
	}
	return request, "", nil
}

// guardParams reads a guard-evaluate request's params: an object stopInput, the request's
// deadline as an RFC 3339 time still ahead of now (the hook writes it in UTC), and socketPath,
// program, mode and now strings or null. refused is the host detail for the first that is not.
func guardParams(request Object, now time.Time) (params, stop Object, deadline time.Time, refused string) {
	params, ok := evidence.Object(get(request, "params"))
	if !ok {
		return nil, nil, time.Time{}, "guard params must be an object"
	}
	if stop, ok = evidence.Object(get(params, "stopInput")); !ok {
		return nil, nil, time.Time{}, "stop input must be an object"
	}
	spelled, ok := get(params, "deadline").(string)
	if !ok {
		return nil, nil, time.Time{}, "guard deadline must be a string"
	}
	deadline, err := time.Parse(time.RFC3339Nano, spelled)
	if err != nil {
		return nil, nil, time.Time{}, "guard deadline is not an RFC 3339 time: " + err.Error()
	}
	if !deadline.After(now) {
		return nil, nil, time.Time{}, "guard request deadline expired"
	}
	// mode and now as well: only a string names either (null or "" asks for the default).
	for _, key := range []string{"socketPath", "program", "mode", "now"} {
		if value := get(params, key); value != nil {
			if _, ok := value.(string); !ok {
				return nil, nil, time.Time{}, "guard " + key + " must be a string"
			}
		}
	}
	return params, stop, deadline, ""
}

// answerHost answers a request with the relay's host record: the requester journals
// guard_host_error, never a refusal or silence.
func answerHost(conn net.Conn, detail string) error {
	_, err := io.WriteString(conn, pyjson.Dumps(Object{{Key: "error", Value: "host"}, {Key: "detail", Value: detail}}, pyjson.Options{})+"\n")
	return err
}

// rejectControl is the protocol dispatcher's answer before any guard command runs.
// It intentionally carries no relay error record or verdict.
func rejectControl(conn net.Conn) error {
	_, err := io.WriteString(conn, "{\"protocol\":1,\"requestRejected\":true}\n")
	return err
}
