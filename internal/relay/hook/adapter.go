package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// VerdictOutput reconstructs only the three host fields, never forwarding guard data.
func VerdictOutput(verdict Object) (string, bool) {
	answer, ok := evidence.Object(get(verdict, "hook_output"))
	if !ok {
		return "", false
	}
	decision := get(verdict, "decision")
	if len(answer) == 0 {
		return "", decision == "release"
	}
	reason, ok := get(answer, "reason").(string)
	if decision != "block" || get(answer, "decision") != "block" || get(answer, "continue") != true || !ok || strings.TrimSpace(reason) == "" {
		return "", false
	}
	return pyjson.Dumps(Object{{Key: "decision", Value: "block"}, {Key: "reason", Value: reason}, {Key: "continue", Value: true}}, pyjson.Options{}), true
}

type taskResult[T any] struct {
	value T
	err   error
}

// bounded is the only goroutine boundary used by the adapter. A panic is data,
// delivered to the invocation journal, never a process failure or a host decision.
func bounded[T any](ctx context.Context, run func() (T, error)) (T, error) {
	result := make(chan taskResult[T], 1)
	go func() {
		var r taskResult[T]
		defer func() {
			if p := recover(); p != nil {
				r.err = &recoveredPanic{value: p}
			}
			result <- r
		}()
		r.value, r.err = run()
	}()
	select {
	case r := <-result:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

type recoveredPanic struct{ value any }

func (e *recoveredPanic) Error() string { return faultText(e.value) }
func faultText(value any) string {
	if err, ok := value.(error); ok {
		var python *evidence.PythonError
		var recovered *recoveredPanic
		if errors.As(err, &python) {
			return python.Error()
		}
		if errors.As(err, &recovered) {
			return recovered.Error()
		}
	}
	return "RuntimeError: " + fmt.Sprint(value)
}

// beforeEmitKey is a test seam at the accepted-verdict/output boundary.
type beforeEmitKey struct{}

// beforeDialKey is a test seam between the dial's start time and its connect, where
// a scheduling stall is simulated.
type beforeDialKey struct{}

type settingsResult struct {
	config          Object
	failure, detail string
}

// Run is crw hook: with --plugin-launch the plugin's declared Stop command, and with no argument
// the settings under the Codex home read whoever owns them. Any other argument - the settings
// path the retired entry forms named (decision 66) - releases the Stop in silence. started is
// captured at main entry. Every controlled path returns zero and writes nothing to stderr.
func Run(parent context.Context, args []string, input io.Reader, output io.Writer, started time.Time) int {
	return runAdapter(parent, args, input, output, started, nil)
}

// PluginLaunch is the first argument the plugin's declared Stop command passes (decision 26).
const PluginLaunch = "--plugin-launch"

// pluginSettingsPath is crw_stop_hook.py settings_path: <CODEX_HOME>/crw-completion-hook.json,
// CODEX_HOME taken as written (no expansion) or ~/.codex when unset or empty. A plugin
// declaration carries no settings argument, so neither CRW_COMPLETION_HOOK_CONFIG nor an argv
// path is read: an override inherited from a shell profile would send the Stop to settings the
// installer never wrote (decision 26).
func pluginSettingsPath() (string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		base, ok := os.LookupEnv("HOME")
		if !ok {
			account, err := user.Current()
			if err != nil {
				return "", err
			}
			base = account.HomeDir
		}
		home = strings.TrimRight(base, "/") + "/.codex"
	}
	return filepath.Abs(filepath.Join(home, ConfigName))
}

// The evaluator seam substitutes only the guard call; routing, identity, claims,
// deadline enforcement, validation and journalling still run in fault tests.
func runAdapter(parent context.Context, args []string, input io.Reader, output io.Writer, started time.Time, evaluator func(context.Context, Object, GuardOptions) (Object, error)) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	absolute := started.Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(parent, absolute)
	defer cancel()
	// The invocation's final row is bounded by the 5 s absolute deadline even when
	// timeoutSeconds shortens every wait below. The waits spend the shortened budget;
	// the small create-once write that records how they ended waits on nothing the host
	// or the owner supplies, so a process left unscheduled past the shortened deadline
	// still leaves its row (as Python journals after a guard timeout), inside the host's
	// 10 s. With the default budget the two deadlines are the same.
	bookkeeping := ctx
	pluginLaunch := len(args) > 0 && args[0] == PluginLaunch
	if len(args) > 0 && !pluginLaunch {
		return 0
	}
	var path string
	var err error
	if pluginLaunch {
		path, err = pluginSettingsPath()
	} else {
		path, err = configurationPath("", nil)
	}
	if err != nil {
		return 0
	}
	// Decision 32: the settings are a local, 1 MiB-bounded regular-file read that
	// waits on nothing the host supplies, so only the absolute deadline bounds it.
	// Cutting it at the startup allocation lost the whole invocation record whenever
	// this process was simply not scheduled for 100 ms after entry.
	settings, err := bounded(ctx, func() (settingsResult, error) {
		c, f, d := ReadSettings(ctx, path)
		return settingsResult{c, f, d}, nil
	})
	if err != nil {
		return 0
	}
	if budget, ok := seconds(get(settings.config, "timeoutSeconds")); ok && budget < 5 {
		absolute = started.Add(time.Duration(budget * float64(time.Second)))
		ctx, cancel = context.WithDeadline(parent, absolute)
		defer cancel()
	}
	if settings.failure != "" {
		return 0
	} // Python has no usable journal configuration on these paths.
	// crw_stop_hook.py adapter_call's stand-down: settings the plugin does not own belong to
	// another registration of this Stop, so nothing is journalled, claimed or asked. Its
	// configVersion rule (absent or 1) is already the validator's, which accepts only 1 and has
	// released anything else in silence above.
	if pluginLaunch && get(settings.config, "owner") != "plugin" {
		return 0
	}
	// Keep final bookkeeping inside the caller's absolute deadline. No nested operation
	// may buy a new end-to-end budget by starting late.
	workDeadline := absolute.Add(-min(100*time.Millisecond, max(0, time.Until(absolute)/5)))
	work, workCancel := context.WithDeadline(ctx, workDeadline)
	defer workCancel()
	slot, err := NewSlot()
	if err != nil {
		return 0
	}
	record := Object{{Key: "recordVersion", Value: int64(RecordVersion)}, {Key: "event", Value: "Stop"}, {Key: "at", Value: now()}, {Key: "adapterOutcome", Value: nil}, {Key: "processEnding", Value: nil}, {Key: "stdoutReading", Value: nil}, {Key: "guardState", Value: nil}, {Key: "guardDecision", Value: nil}, {Key: "guardMode", Value: nil}, {Key: "assignmentId", Value: nil}, {Key: "guardRecordedAs", Value: nil}, {Key: "held", Value: false}, {Key: "eventKey", Value: nil}, {Key: "eventIdentity", Value: nil}, {Key: "identityScanMs", Value: nil}, {Key: "acceptance", Value: nil}, {Key: "acceptedAs", Value: nil}, {Key: "guardInvoked", Value: false}, {Key: "configuration", Value: pyvalue.FSDecode(path)}}
	claimed := ""
	finish := func(outcome string, detail any, answer string) {
		record = set(record, "adapterOutcome", outcome)
		record = set(record, "detail", detail)
		record = set(record, "elapsedMs", time.Since(started).Milliseconds())
		record = set(record, "held", answer != "")
		// Once the guard's verdict has been accepted, bookkeeping must not spend
		// the output budget and discard its block. Python returns that answer even
		// if journalling crosses the guard budget. Emit synchronously: cancellation
		// cannot undo a pipe write, and returning while it is still pending loses
		// the answer when the process exits. Guard-call timeouts still say nothing.
		if answer != "" {
			if _, e := decodeObject([]byte(answer)); e == nil {
				if before, ok := ctx.Value(beforeEmitKey{}).(func(context.Context)); ok {
					before(ctx)
				}
				if _, e := io.WriteString(output, answer); e != nil {
					panic(e)
				}
			}
		}
		_, _ = bounded(bookkeeping, func() (bool, error) {
			row, e := Journal(bookkeeping, settings.config, record, slot)
			var named any
			if row != "" {
				named = slot.Name()
			}
			if claimed != "" {
				e = errors.Join(e, RecordOutcome(bookkeeping, settings.config, claimed, record, named))
			}
			return true, e
		})
	}
	// A failed invocation has no guard result. Preserve the exact prefix reached,
	// as Python's BaseException path does; never invent exited/stdout fields.
	recordFault := func(value any) {
		record = set(record, "adapterOutcome", "adapter_faulted")
		record = set(record, "fault", faultText(value))
		record = set(record, "held", false)
		record = set(record, "elapsedMs", time.Since(started).Milliseconds())
		_, _ = bounded(bookkeeping, func() (bool, error) {
			row, err := Journal(bookkeeping, settings.config, record, slot)
			var named any
			if row != "" {
				named = slot.Name()
			}
			if claimed != "" {
				err = errors.Join(err, RecordOutcome(bookkeeping, settings.config, claimed, record, named))
			}
			return true, err
		})
	}
	defer func() {
		if p := recover(); p != nil {
			recordFault(p)
			code = 0
		}
	}()
	raw, err := readInput(work, input, started.Add(100*time.Millisecond))
	if err != nil {
		finish("stdin_unreadable", "the Stop payload could not be read from stdin", "")
		return 0
	}
	if _, err := pyjson.DecodeUTF8(raw); err != nil {
		finish("stdin_unreadable", "the Stop payload is not UTF-8: "+err.Error(), "")
		return 0
	}
	value, err := Decode(raw)
	if err != nil {
		finish("stdin_not_json", "the Stop payload is not JSON: "+err.Error(), "")
		return 0
	}
	stop, ok := evidence.Object(value)
	if !ok {
		finish("stdin_not_object", "the Stop payload is a "+pyvalue.TypeName(value)+", not an object", "")
		return 0
	}
	record = set(record, "sessionId", get(stop, "session_id"))
	record = set(record, "turnId", get(stop, "turn_id"))
	record = set(record, "stopHookActive", get(stop, "stop_hook_active"))
	record = set(record, "guardMode", get(settings.config, "mode"))
	state, err := bounded(work, func() (string, error) { return RoutingState(settings.config) })
	if err != nil {
		return 0
	}
	dialStarted := time.Now()
	socket := filepath.Join(state, "control.sock")
	if pause, ok := ctx.Value(beforeDialKey{}).(func()); ok {
		pause()
	}
	// Decision 32: a Unix-socket connect completes or fails at once (a full Linux
	// backlog is EAGAIN); it never waits. A dial timer could only expire on time
	// this process spent unscheduled, turning a healthy socket into ETIMEDOUT.
	conn, err := (&net.Dialer{}).DialContext(work, "unix", socket)
	dialErr := err
	if err != nil && prescanErrno(err) {
		// Decision 22: retain the single Python failure row, without fsync. Identity
		// remains unobserved: no transcript scan, claim or DB access precedes it.
		// Decision 32: the small create-once write is bounded by the 5 s absolute
		// deadline (bookkeeping), so a late-scheduled invocation still leaves its row.
		record = unreachableRecord(record, err, time.Since(dialStarted))
		record = set(record, "adapterOutcome", "guard_unreachable")
		record = set(record, "detail", "the configured runtime could not be run: "+store.StoredOSErrorText(&os.PathError{Op: "connect", Path: socket, Err: err}))
		record = set(record, "elapsedMs", time.Since(started).Milliseconds())
		_, _ = bounded(bookkeeping, func() (string, error) { return Journal(bookkeeping, settings.config, record, slot) })
		return 0
	} // No retry, daemon start, writer lock or synchronous diagnostic fsync.
	if conn != nil {
		defer conn.Close()
	}
	scanStarted := time.Now()
	type identityResult struct {
		key      string
		identity Object
	}
	identified, err := bounded(work, func() (identityResult, error) {
		key, identity := EventIdentity(work, stop)
		return identityResult{key, identity}, nil
	})
	if err != nil {
		recordFault(err)
		return 0
	}
	record = set(record, "identityScanMs", time.Since(scanStarted).Milliseconds())
	record = set(record, "eventKey", nullable(identified.key))
	record = set(record, "eventIdentity", identified.identity)
	if identified.key == "" {
		record = set(record, "acceptance", "unestablished")
	} else {
		host, _ := hostLedger()
		type claimResult struct {
			acceptance string
			where      any
		}
		claim, err := bounded(work, func() (claimResult, error) {
			a, w := ClaimEvent(work, settings.config, identified.key, identified.identity, stop, slot, host)
			return claimResult{a, w}, nil
		})
		if err != nil {
			recordFault(err)
			return 0
		}
		record = set(record, "acceptance", claim.acceptance)
		record = set(record, "acceptedAs", claim.where)
		switch claim.acceptance {
		case "duplicate":
			finish(DuplicateInvocation, DuplicateDetail, "")
			return 0
		case "unarbitrated":
			finish(ArbitrationFailed, UnarbitratedDetail, "")
			return 0
		case "accepted":
			claimed = identified.key
		}
	}
	record = set(record, "guardInvoked", true)
	if dialErr != nil {
		// Other transport failures use Python's ordinary post-identity failure
		// shape, not a broader exception in the journal reader.
		record = unreachableRecord(record, dialErr, time.Since(dialStarted))
		finish("guard_unreachable", "the configured runtime could not be run: "+store.StoredOSErrorText(&os.PathError{Op: "connect", Path: socket, Err: dialErr}), "")
		return 0
	}
	guardStarted := time.Now()
	guardCtx, guardCancel := context.WithDeadline(work, guardStarted.Add(3500*time.Millisecond))
	defer guardCancel()
	verdict, err := bounded(guardCtx, func() (Object, error) {
		options := GuardOptions{Root: text(get(settings.config, "markerRoot")), Mode: text(get(settings.config, "mode")), DBPath: text(get(settings.config, "dbPath")), SocketPath: text(get(settings.config, "socketPath")), Program: text(get(settings.config, "relayExecutable"))}
		options.DefaultDBPath = ownerFallback(state, options.SocketPath, options.Program)
		if evaluator != nil {
			return evaluator(guardCtx, stop, options)
		}
		if ownsGuard(guardCtx, state, options.DBPath) {
			return evaluateOwner(guardCtx, stop, options)
		}
		return RequestGuard(guardCtx, conn, stop, options)
	})
	if err != nil {
		var auth *peerAuthError
		if errors.As(err, &auth) {
			record = unreachableRecord(record, syscall.EACCES, time.Since(guardStarted))
			finish("guard_unreachable", auth.Error(), "")
			return 0
		}
		var response *responseError
		var network net.Error
		timedOut := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &network) && network.Timeout())
		if !timedOut && !errors.As(err, &response) {
			recordFault(err)
			return 0
		}
	}
	record = set(record, "guardElapsedMs", time.Since(guardStarted).Milliseconds())
	record = set(record, "exitCode", nil)
	record = set(record, "signal", nil)
	record = set(record, "errno", nil)
	record = set(record, "guardStderr", "")
	if err != nil {
		outcome := "adapter_faulted"
		ending := "exited"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			outcome = "guard_timed_out"
			ending = "timed_out"
		} else {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				outcome = "guard_timed_out"
				ending = "timed_out"
			}
		}
		record = set(record, "processEnding", ending)
		record = set(record, "stdoutReading", "said_nothing")
		var response *responseError
		if errors.As(err, &response) {
			record = set(record, "stdoutReading", response.reading)
			exit := int64(0)
			if response.outcome == "guard_rejected_the_call" {
				exit = 2
			}
			record = set(record, "exitCode", exit)
			finish(response.outcome, nil, "")
			return 0
		}
		detail := err.Error()
		if outcome == "guard_timed_out" {
			deadline, _ := guardCtx.Deadline()
			detail = fmt.Sprintf("the native guard did not answer within its %dms remaining budget; the request was cancelled", max(0, deadline.Sub(guardStarted).Milliseconds()))
		}
		finish(outcome, detail, "")
		return 0
	}
	record = set(record, "processEnding", "exited")
	record = set(record, "exitCode", int64(0))
	record = set(record, "stdoutReading", "said_a_verdict")
	if errorKind, present := evidence.Lookup(verdict, "error"); present {
		outcome, exit := "guard_ended_unexpectedly", int64(0)
		switch errorKind {
		case "refused":
			outcome, exit = "guard_refused", 2
		case "host":
			outcome, exit = "guard_host_error", 3
		case "usage":
			outcome, exit = "guard_usage_error", 4
		}
		record = set(record, "stdoutReading", "said_an_error_record")
		record = set(record, "exitCode", exit)
		finish(outcome, nil, "")
		return 0
	}
	if _, present := evidence.Lookup(verdict, "decision"); !present {
		record = set(record, "stdoutReading", "said_something_unreadable")
		finish("guard_output_unreadable", nil, "")
		return 0
	}
	answer, valid := VerdictOutput(verdict)
	if !valid {
		finish("guard_verdict_incomplete", nil, "")
		return 0
	}
	for _, pair := range [][2]string{{"guardState", "state"}, {"guardDecision", "decision"}, {"observation", "observation"}, {"assignmentId", "assignmentId"}, {"guardRecordedAs", "recordedAs"}, {"counters", "counters"}} {
		record = set(record, pair[0], get(verdict, pair[1]))
	}
	finish("guard_answered", nil, answer)
	return 0
}

func unreachableRecord(record Object, err error, elapsed time.Duration) Object {
	var errno syscall.Errno
	var name any = "ETIMEDOUT"
	if errors.As(err, &errno) {
		name = pythonErrnoName(errno)
		if name == "" {
			name = int64(errno)
		} // Python errno.errorcode.get(e, e).
	}
	for _, f := range (Object{{Key: "guardInvoked", Value: true}, {Key: "processEnding", Value: "not_started"}, {Key: "stdoutReading", Value: "said_nothing"}, {Key: "exitCode", Value: nil}, {Key: "signal", Value: nil}, {Key: "errno", Value: name}, {Key: "guardElapsedMs", Value: elapsed.Milliseconds()}, {Key: "guardStderr", Value: ""}}) {
		record = set(record, f.Key, f.Value)
	}
	return record
}

func ownsGuard(ctx context.Context, state, configuredDB string) bool {
	raw, err := readRegular(ctx, filepath.Join(state, "takeover.json"), 1<<20)
	if err != nil {
		return false
	}
	// json.loads(bytes), as socket_guard reads the mirror: UTF-8 behind its byte order mark or
	// not, UTF-16 or UTF-32 (pyjson.DecodeBytes).
	text, err := pyjson.DecodeBytes(raw)
	if err != nil {
		return false
	}
	v, err := Decode([]byte(text))
	if err != nil {
		return false
	}
	record, ok := evidence.Object(v)
	if !ok || get(record, "owner") != "go" || get(record, "phase") != "active" {
		return false
	}
	path := configuredDB
	if path == "" {
		path = filepath.Join(state, "relay.sqlite3")
	}
	// The read-only Stop path reads the durable half in place: no copy and no SQLite sidecar
	// (store.OpenStopRead, Python's ownership.stop_metadata).
	ro, err := store.OpenStopRead(ctx, path, 0)
	if err != nil {
		return false
	}
	defer ro.Close()
	var owner string
	if err = ro.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='owner'").Scan(&owner); err != nil {
		return false
	}
	return owner == "go"
}

// HandleControl serves a single bounded newline-framed guard request for the owner.
// It is exported for the daemon's control.sock handler; it never starts a listener.
// ownerState is the serving owner's selected state directory, not client input or
// a fresh environment-based discovery. Its store is used only when neither the
// request nor the coordinator's intent pins one, as in the in-process path.
//
// Nothing a peer sends fails the owner (control.py GuardServer._serve, PR #185 4128954449): a
// request it cannot serve, a guard that failed and a handler panic are all answered with the
// host record, and the answered request is no handler failure. So is a request line that has
// not arrived whole by ControlAnswerGrace before ctx's deadline, however it was sent: the bound
// is on the whole line, and the grace is the time kept to answer its expiry. The error it returns is a transport failure only: the
// peer went away before an answer.
func HandleControl(ctx context.Context, conn net.Conn, ownerState string) (err error) {
	defer conn.Close()
	defer func() {
		if p := recover(); p != nil {
			err = answerHost(conn, fmt.Sprintf("guard control panic: %v", p))
		}
	}()
	deadline, ok := ctx.Deadline()
	if !ok {
		return fmt.Errorf("guard control handler requires an absolute deadline")
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(deadline.Add(-ControlAnswerGrace)); err != nil {
		return err
	}
	request, refused, err := readRequest(conn)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return answerHost(conn, "guard request line not received in time")
	}
	if err != nil {
		return err
	}
	if refused != "" {
		return answerHost(conn, refused)
	}
	if get(request, "method") != "guard-evaluate" || get(request, "protocol") != int64(1) {
		return rejectControl(conn)
	}
	params, stop, at, refused := guardParams(request, time.Now())
	if refused != "" {
		return answerHost(conn, refused)
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithDeadline(ctx, at)
	defer cancel()
	deadline, _ = ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	root, db, refused, err := ownerPaths(ownerState, params)
	if err != nil {
		// An owner that cannot resolve its own marker root answers the host record, as
		// control.py answers the same failure, rather than closing without a word.
		refused = err.Error()
	}
	if refused != "" {
		// Answered before anything is read or recorded, as a host error: the request asked this
		// owner to evaluate somewhere it does not, which is no Stop refusal (control.py owner_paths).
		return answerHost(conn, refused)
	}
	v, err := evaluateOwner(ctx, stop, GuardOptions{Root: root, Now: text(get(params, "now")), Mode: text(get(params, "mode")), DBPath: db, NoRecord: get(params, "noRecord") == true, DefaultDBPath: ownerFallback(ownerState, text(get(params, "socketPath")), text(get(params, "program")))})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The requester's deadline has passed: nobody is left to read an answer.
			return err
		}
		// A guard that failed is answered with the relay's host record, as the CLI answers
		// the same error and control.py answers a guard that raised, never with silence:
		// the live-state guard's refusal under test isolation (cutover.md) reaches the
		// requester this way, which journals guard_host_error. The answered request is not a
		// handler failure.
		return answerHost(conn, err.Error())
	}
	_, err = io.WriteString(conn, pyjson.Dumps(v, pyjson.Options{Compact: true})+"\n")
	return err
}

// ownerPaths is control.py owner_paths: a control request may not choose where the owner writes
// (PR #185 thread 4127894432). The owner evaluates only under its own marker root, resolved from
// its own configuration as the relay resolves one without --marker-root (the root the installer
// records as the hook's markerRoot), and reads only its own store: a request's dbPath, when
// present, must name ownerState/relay.sqlite3. Either is then used as the owner's own path, never
// the request's spelling. journalRoot and the other hook settings are not request parameters;
// the adapter journals in its own process. refused is the host detail for any other path.
func ownerPaths(ownerState string, params Object) (root, db, refused string, err error) {
	selected, err := delivery.ResolveMarkerRoot("")
	if err != nil {
		return "", "", "", err
	}
	requested, present := evidence.Lookup(params, "markerRoot")
	if !namesOwnPath(requested, selected.Path) {
		return "", "", "control.sock evaluates Stops only under this owner's marker root " + selected.Path + "; the request named " + requestedPath(requested, present), nil
	}
	own := filepath.Join(ownerState, "relay.sqlite3")
	requested, present = evidence.Lookup(params, "dbPath")
	if !present || requested == nil {
		return selected.Path, "", "", nil
	}
	if !namesOwnPath(requested, own) {
		return "", "", "control.sock reads only this owner's store " + own + "; the request named " + requestedPath(requested, present), nil
	}
	return selected.Path, own, "", nil
}

// namesOwnPath is control.py _names: only an absolute path names anything, by the same
// normalized spelling or by being the same existing file.
func namesOwnPath(requested any, own string) bool {
	path, ok := requested.(string)
	if !ok || !filepath.IsAbs(path) {
		return false
	}
	if filepath.Clean(path) == filepath.Clean(own) {
		return true
	}
	left, err := os.Stat(path)
	if err != nil {
		return false
	}
	right, err := os.Stat(own)
	return err == nil && os.SameFile(left, right)
}

func requestedPath(requested any, present bool) string {
	if path, ok := requested.(string); ok && present {
		return path
	}
	return "no path"
}
