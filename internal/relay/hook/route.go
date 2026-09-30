package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// SelectedStore is guard.selected_store: the receipt store Evaluate would read for this Stop,
// chosen by Evaluate's own gates without reading any store or recording anything, or "" when the
// evaluation would read none (no assignment, no declared readiness at this Stop's identity, no
// registered relationship, a marker whose published facts are not facts, an identity that is not
// named). The precedence is Evaluate's: options.DBPath, the dbPath the intent recorded, then
// options.DefaultDBPath, whose failure is returned for the caller to classify (the fence raises
// its StoreNotSelected and reads anything else as no store). Any other failure is "": the
// evaluation that follows answers it.
func SelectedStore(ctx context.Context, stop Object, options GuardOptions) (selected string, err error) {
	defer func() {
		if recover() != nil {
			selected, err = "", nil
		}
	}()
	workspace, ok := get(stop, "cwd").(string)
	if !ok || workspace == "" {
		return "", nil
	}
	session, turn := get(stop, "session_id"), get(stop, "turn_id")
	directory, marker, _, err := delivery.SelectAssignmentContext(ctx, options.Root, workspace, session)
	if err != nil || directory == "" || !delivery.ValidSegment(session) || !delivery.ValidSegment(turn) {
		return "", nil
	}
	disposition, _ := delivery.ReadDispositionContext(ctx, directory, session, turn)
	if malformedDisposition(disposition) != "" {
		return "", nil
	}
	declared, ok := evidence.Object(disposition)
	registered, rok := evidence.Object(get(marker, "relationship"))
	if !ok || get(declared, "outcome") != "ready_for_review" || !delivery.SameIdentity(get(declared, "sessionId"), session) || !delivery.SameIdentity(get(declared, "turnId"), turn) || !rok || delivery.Malformed(marker) != "" {
		return "", nil
	}
	if !delivery.Named(get(registered, "relationshipId")) || !delivery.Named(session) || !delivery.Named(turn) {
		return "", nil
	}
	if selected = options.DBPath; selected == "" {
		selected = text(get(object(get(marker, "intent")), "dbPath"))
	}
	if selected == "" && options.DefaultDBPath != nil {
		return options.DefaultDBPath()
	}
	return selected, nil
}

// routeBudget is the one bound on a routed guard-evaluate (stopadapter DEFAULT_TIMEOUT_SECONDS;
// the CLI configures no shorter one).
const routeBudget = 5 * time.Second

// Routed is the owner's answer to a guard-evaluate the CLI handed to its control.sock
// (stopadapter.socket_guard as cmd_guard_evaluate calls it). Answer is the answer decoded and Code
// the exit status it carries: 0, or 2, 3 or 4 for an error record's refused, host or usage (2
// for any other error). Readable is false when the owner closed without a readable answer (none,
// bytes that are not UTF-8 or JSON, more than 64 MiB of them, or a JSON null, as the fence reads
// them), and TimedOut when it did not answer within the budget, which Detail words. JSON nested
// deeper than the fence's json.loads reads is RouteGuard's error, as it is the fence's.
type Routed struct {
	Answer   any
	Code     int
	Readable bool
	TimedOut bool
	Detail   string
}

// RouteGuard asks the owner of the store in state to evaluate stop, as the fence's
// guard-evaluate does before it evaluates anything itself: one bounded request to
// <state>/control.sock, sent only once the peer is established as this user's direct socket in a
// directory only this user may write. When the connection or that check fails before anything
// is sent, the Stop is this runtime's to evaluate (nil, nil) only when state holds no
// takeover.json, or one naming this runtime as owner; otherwise the answer is the fence's
// refusal, exit 2 {"error": "refused", "reason": "store_owned_by_other", "detail": "the owner
// could not answer guard-evaluate: <error>"}. A failure after the request was sent is returned:
// the owner may already hold part of it, so it is never refused or evaluated here.
func RouteGuard(ctx context.Context, state string, stop Object, options GuardOptions) (*Routed, error) {
	started := time.Now()
	deadline := started.Add(routeBudget)
	path := filepath.Join(state, "control.sock")
	conn, err := dialTrusted(ctx, path)
	if err != nil {
		if localStop(state) {
			return nil, nil
		}
		return &Routed{Readable: true, Code: 2, Answer: Object{{Key: "error", Value: "refused"}, {Key: "reason", Value: "store_owned_by_other"}, {Key: "detail", Value: "the owner could not answer guard-evaluate: " + err.Error()}}}, nil
	}
	defer conn.Close()
	timedOut := &Routed{TimedOut: true, Detail: fmt.Sprintf("the owner did not answer guard-evaluate within %gs", routeBudget.Seconds())}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	params := Object{{Key: "markerRoot", Value: options.Root}, {Key: "stopInput", Value: stop}, {Key: "mode", Value: options.Mode}, {Key: "dbPath", Value: nullable(options.DBPath)}, {Key: "now", Value: nullable(options.Now)}, {Key: "noRecord", Value: options.NoRecord}, {Key: "deadline", Value: deadline.UTC().Format(time.RFC3339Nano)}, {Key: "socketPath", Value: nullable(options.SocketPath)}, {Key: "program", Value: nullable(options.Program)}}
	request := Object{{Key: "protocol", Value: int64(1)}, {Key: "method", Value: "guard-evaluate"}, {Key: "params", Value: params}}
	if _, err := io.WriteString(conn, evidence.Dumps(request, true, false, true)+"\n"); err != nil {
		if timeout(err) {
			return timedOut, nil
		}
		return nil, err
	}
	var raw []byte
	chunk := make([]byte, 65536)
	// Only the bytes just read can hold the first newline: rescanning the whole frame on every
	// read made a 64 MiB answer outlast the budget.
	for newline := false; !newline && len(raw) <= maxControlBytes; {
		n, err := conn.Read(chunk[:min(len(chunk), maxControlBytes+1-len(raw))])
		raw = append(raw, chunk[:n]...)
		newline = bytes.IndexByte(chunk[:n], '\n') >= 0
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if timeout(err) {
				return timedOut, nil
			}
			return nil, err
		}
	}
	if len(raw) == 0 || len(raw) > maxControlBytes || !store.ValidUTF8(raw) {
		return &Routed{}, nil
	}
	// The fence decodes the bytes, then json.loads them, whose C scanner takes 9998 nested
	// containers here and raises RecursionError from 9999: not a ValueError, so it leaves
	// socket_guard and cli.main reports it as a host error.
	if message, recursion := store.PythonJSONErrorWithLimit(string(raw), 9998); recursion {
		return nil, fmt.Errorf("RecursionError: %s", message)
	}
	value, err := Decode(raw)
	if err != nil || value == nil {
		// A JSON null is no answer either: the fence reads it as an empty stdout.
		return &Routed{}, nil
	}
	routed := &Routed{Answer: value, Readable: true}
	if answer, ok := evidence.Object(value); ok {
		switch kind := get(answer, "error"); kind.(type) {
		case nil:
		case Object, []any:
			// The fence looks the kind up in a dict, which raises on an unhashable one.
			return nil, fmt.Errorf("TypeError: unhashable type: '%s'", evidence.TypeName(kind))
		default:
			routed.Code = map[string]int{"refused": 2, "host": 3, "usage": 4}[text(kind)]
			if routed.Code == 0 {
				routed.Code = 2
			}
		}
	}
	return routed, nil
}

func timeout(err error) bool {
	var network net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &network) && network.Timeout()
}

// localStop is whether a Stop the owner could not be asked about is this runtime's to evaluate:
// state holds no takeover.json, or one that names Go as the owner. An unreadable record, or one
// naming another owner, is not.
func localStop(state string) bool {
	raw, err := os.ReadFile(filepath.Join(state, "takeover.json"))
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
		return true
	}
	if err != nil {
		return false
	}
	value, err := Decode(raw)
	if err != nil {
		return false
	}
	record, ok := evidence.Object(value)
	return ok && get(record, "owner") == "go"
}

// routeError is why a routed guard-evaluate could not reach or trust the owner, worded as the
// fence's Stop client words it: str(OSError), or _trusted_guard_peer's refusal.
type routeError struct{ detail string }

func (e *routeError) Error() string { return e.detail }

// dialTrusted connects to the control socket at path (through /proc/self/fd for a path a
// sockaddr_un cannot hold) and establishes the peer as the fence's Stop client does before it
// sends anything: the peer's uid, and path as a direct socket of this user's in a directory of
// this user's that no group or other user may write. Errors read as Python's str(OSError).
func dialTrusted(ctx context.Context, path string) (*net.UnixConn, error) {
	address, release, err := ControlAddress(path)
	if err != nil {
		return nil, pythonText(&os.PathError{Op: "open", Path: filepath.Dir(path), Err: err})
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	release()
	if err != nil {
		return nil, pythonText(err)
	}
	unixConn := conn.(*net.UnixConn)
	if err := trustedPeer(unixConn, path); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return unixConn, nil
}

func trustedPeer(conn *net.UnixConn, path string) error {
	peer, err := peerUID(conn)
	if err != nil {
		return pythonText(err)
	}
	ours := uint32(os.Getuid())
	endpoint, err := os.Lstat(path)
	if err != nil {
		return pythonText(err)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return pythonText(err)
	}
	refuse := func(format string, args ...any) error { return &routeError{fmt.Sprintf(format, args...)} }
	switch {
	case endpoint.Mode()&os.ModeSocket == 0:
		return refuse("guard control path is not a direct socket")
	case peer != ours:
		return refuse("guard peer uid %d differs from our uid %d", peer, ours)
	case endpoint.Sys().(*syscall.Stat_t).Uid != ours:
		return refuse("guard socket uid %d differs from our uid %d", endpoint.Sys().(*syscall.Stat_t).Uid, ours)
	case parent.Sys().(*syscall.Stat_t).Uid != ours:
		return refuse("guard socket directory uid %d differs from our uid %d", parent.Sys().(*syscall.Stat_t).Uid, ours)
	case parent.Mode().Perm()&0o022 != 0:
		return refuse("guard socket directory is group- or world-writable")
	}
	return nil
}

// pythonText is err worded as Python's str(OSError).
func pythonText(err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err
	}
	return &routeError{store.PythonOSErrorText(err)}
}
