package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
)

// relayHelperStderrLimit is how much of a relay command's stderr an error carries: the
// first bytes, so a command that fails loudly cannot fill a log with its noise.
const relayHelperStderrLimit = 2000

// relayHelperUnresolvedError is the refusal of a relay run whose state directory the
// relay's own doctor answer did not supply. Its message names the refusal, and it keeps
// the reason it happened reachable. It is a type rather than a sentinel variable because
// every package-level name this issue adds starts with relayHelper.
type relayHelperUnresolvedError struct{ detail error }

// Error is the refusal, with the reason when there is one.
func (e *relayHelperUnresolvedError) Error() string {
	if e.detail == nil {
		return "relay_state_unresolved"
	}
	return "relay_state_unresolved: " + e.detail.Error()
}

// Unwrap exposes the reason, so errors.Is and errors.As still reach a doctor's failure or
// a context that ended.
func (e *relayHelperUnresolvedError) Unwrap() error { return e.detail }

// relayHelperMemoMu guards relayHelperMemo and every field of the entries in it. It covers the
// map's own reads and writes and is never held across the doctor call, so a doctor that hangs
// for one Env cannot hold up another Env's resolution.
var relayHelperMemoMu sync.Mutex

// relayHelperEntry is one Env's resolution of the relay state directory. While the doctor runs
// it is the resolution in flight: done is open, and a call that overlaps waits on it instead of
// asking the doctor again. When the doctor answers, done closes with state, err or retry set.
// A successful entry stays in the map as the remembered answer; a failed or abandoned one is
// removed, so a failure is never remembered.
type relayHelperEntry struct {
	done     chan struct{}
	state    string
	err      error
	resolved bool // the doctor named a state directory; state is the answer
	retry    bool // the resolving call's own context ended; a waiter resolves for itself
}

// relayHelperMemo is the state directory each Env resolved from the relay's own doctor
// answer, remembered for the rest of that Env's life, or the resolution still running for it.
// Env is declared in another issue's file and may not gain a field here, and Run builds one
// Env per invocation, so the map holds one live entry per run. Run calls relayHelperForget when
// the invocation ends, so a process that embeds Run and calls it many times does not grow a map
// entry per call; within the invocation the one resolution is still reused.
var relayHelperMemo = map[*Env]*relayHelperEntry{}

// relayHelperForget drops one Env's remembered state directory, or the resolution still running
// for it. Run calls it when the invocation ends, so a process that embeds Run and calls it many
// times keeps no Env, with the streams and the document it carries, reachable past its run. A
// resolution that ends after the entry was dropped does not bring it back: it publishes only
// while the map still holds its own entry. The memo's other behaviour is unchanged: within one
// invocation the same Env still asks the doctor once.
func relayHelperForget(e *Env) {
	relayHelperMemoMu.Lock()
	delete(relayHelperMemo, e)
	relayHelperMemoMu.Unlock()
}

// Relay runs the same executable's relay mode for cfg and returns its stdout and exit
// status. The argument order is fixed: relay --state <state> --socket <socket> <args...>,
// and both flags are passed even to a command that needs no socket.
//
// A command that ran and failed reports its exit status with no error, so the caller
// decides what the status means; the corollary is that such a command's stderr is
// unreachable, and an error's stderr excerpt covers an execution failure only (a command
// that could not be started, a doctor that failed, or a context that ended). A context
// that ends while a command runs is that context's error, and the half-written answer it
// left behind is not returned as a result.
//
// When cfg.Relay.State is empty the state directory is the one the relay's own doctor
// answer selects, asked once and remembered for this Env. A doctor that fails or names no
// state directory is relay_state_unresolved.
func (e *Env) Relay(ctx context.Context, cfg *Config, args ...string) ([]byte, int, error) {
	state, err := e.relayHelperState(ctx, cfg)
	if err != nil {
		return nil, 0, err
	}
	argv := append([]string{"relay", "--state", state, "--socket", cfg.Relay.Socket}, args...)
	stdout, code, stderr, err := e.relayHelperRun(ctx, argv...)
	if err != nil {
		return nil, 0, relayHelperError(err, stderr)
	}
	return stdout, code, nil
}

// relayHelperState is the state directory Relay runs with: the configured one, else the
// one this Env already resolved, else the relay doctor's answer, which is remembered. The
// resolution is single-flight per Env: the first call asks the doctor and a call that overlaps
// it waits for that answer, so one Env asks the doctor once and every call runs with the same
// state. Only a success is remembered. A waiter whose resolving call ended because that call's
// own context ended does not inherit the cancellation: it resolves again with its own context.
// Any other failure is shared with the waiters of that doctor and forgotten, so the next call
// asks again. The map is locked only around its own access, so one Env's slow doctor never holds
// another Env.
func (e *Env) relayHelperState(ctx context.Context, cfg *Config) (string, error) {
	if cfg.Relay.State != "" {
		return cfg.Relay.State, nil
	}
	for {
		relayHelperMemoMu.Lock()
		entry, ok := relayHelperMemo[e]
		if ok && entry.resolved {
			state := entry.state
			relayHelperMemoMu.Unlock()
			return state, nil
		}
		if !ok {
			entry = &relayHelperEntry{done: make(chan struct{})}
			relayHelperMemo[e] = entry
			relayHelperMemoMu.Unlock()
			return e.relayHelperResolve(ctx, cfg.Relay.Socket, entry)
		}
		relayHelperMemoMu.Unlock()
		select {
		case <-entry.done:
		case <-ctx.Done():
			return "", relayHelperUnresolved(ctx.Err())
		}
		relayHelperMemoMu.Lock()
		state, err, retry := entry.state, entry.err, entry.retry
		relayHelperMemoMu.Unlock()
		if !retry {
			return state, err
		}
	}
}

// relayHelperResolve asks the doctor for the call that owns entry, publishes the outcome to
// the waiters. A success stays in the map as the remembered answer unless the Env was forgotten
// meanwhile; any other outcome leaves the map.
func (e *Env) relayHelperResolve(ctx context.Context, socket string, entry *relayHelperEntry) (state string, err error) {
	finished := false
	defer func() {
		relayHelperMemoMu.Lock()
		if !finished {
			// The doctor call did not return (a panic): let a waiter resolve for itself.
			entry.retry = true
		}
		if relayHelperMemo[e] == entry && !entry.resolved {
			delete(relayHelperMemo, e)
		}
		relayHelperMemoMu.Unlock()
		close(entry.done)
	}()
	resolved, doctorErr := e.relayHelperDoctorState(ctx, socket)
	relayHelperMemoMu.Lock()
	finished = true
	switch {
	case doctorErr == nil:
		// An entry forgotten while the doctor ran is no longer in the map: its waiters still
		// get the answer, and nothing puts the entry back.
		entry.state, entry.resolved = resolved, true
	case ctx.Err() != nil:
		entry.retry = true
		entry.err = relayHelperUnresolved(doctorErr)
	default:
		entry.err = relayHelperUnresolved(doctorErr)
	}
	relayHelperMemoMu.Unlock()
	if doctorErr != nil {
		return "", relayHelperUnresolved(doctorErr)
	}
	return resolved, nil
}

// relayHelperDoctorState is the state directory the relay's doctor answer selects. The
// doctor call carries the socket and no state, because choosing the state is its whole
// point.
func (e *Env) relayHelperDoctorState(ctx context.Context, socket string) (string, error) {
	stdout, code, stderr, err := e.relayHelperRun(ctx, "relay", "--socket", socket, "doctor")
	if err != nil {
		return "", relayHelperError(err, stderr)
	}
	if code != 0 {
		return "", relayHelperError(fmt.Errorf("relay doctor exited with status %d", code), stderr)
	}
	var answer struct {
		StateSelection struct {
			Path string `json:"path"`
		} `json:"stateSelection"`
	}
	if err := json.Unmarshal(stdout, &answer); err != nil {
		return "", relayHelperError(fmt.Errorf("relay doctor answer: %w", err), stderr)
	}
	if answer.StateSelection.Path == "" {
		return "", relayHelperError(errors.New("relay doctor named no state directory"), stderr)
	}
	return answer.StateSelection.Path, nil
}

// relayHelperRun runs one relay command line of the same executable and reports its
// stdout, exit status and stderr excerpt. A command that ran and failed is an exit status
// with no error; one that could not be started, or whose context ended, is an error.
func (e *Env) relayHelperRun(ctx context.Context, argv ...string) ([]byte, int, string, error) {
	var stdout bytes.Buffer
	stderr := &relayHelperStderr{}
	cmd := exec.CommandContext(ctx, e.Executable, argv...)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), 0, stderr.excerpt(), nil
	}
	// A context that ended kills the child, and Run reports that as an exit status. Report
	// the context's own error instead, so a caller can tell a command that was cut off from
	// one that ran and failed, and is not handed the answer it had half written.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, stderr.excerpt(), ctxErr
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return stdout.Bytes(), exit.ExitCode(), stderr.excerpt(), nil
	}
	return stdout.Bytes(), 0, stderr.excerpt(), err
}

// relayHelperStderr keeps the first relayHelperStderrLimit bytes written to it and
// discards the rest, so a command that writes an unbounded amount to stderr neither blocks
// on a full pipe nor grows the error.
type relayHelperStderr struct {
	buf bytes.Buffer
}

func (s *relayHelperStderr) Write(p []byte) (int, error) {
	if room := relayHelperStderrLimit - s.buf.Len(); room > 0 {
		s.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// excerpt is what the error carries of the command's stderr.
func (s *relayHelperStderr) excerpt() string { return s.buf.String() }

// relayHelperError attaches the command's stderr excerpt to err, keeping err's identity
// for errors.Is and errors.As.
func relayHelperError(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w (stderr: %s)", err, stderr)
}

// relayHelperUnresolved is the refusal with the reason it carries: a doctor's exit status,
// an unreadable answer, or a context that ended.
func relayHelperUnresolved(detail error) error { return &relayHelperUnresolvedError{detail: detail} }

// relayHelperIsUnresolved reports whether err is that refusal.
func relayHelperIsUnresolved(err error) bool {
	var refused *relayHelperUnresolvedError
	return errors.As(err, &refused)
}
