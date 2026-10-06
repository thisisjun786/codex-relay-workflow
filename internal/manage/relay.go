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

// relayHelperErrStateUnresolved is the refusal of a relay run whose state directory the
// relay's own doctor answer did not supply. It is a sentinel so a caller can test for it
// with errors.Is while the error still carries the reason it happened.
var relayHelperErrStateUnresolved = errors.New("relay_state_unresolved")

// relayHelperMemoMu guards relayHelperMemo. The mutex is held across the doctor call in
// relayHelperState, so two concurrent first calls resolve the state once.
var relayHelperMemoMu sync.Mutex

// relayHelperMemo is the state directory each Env resolved from the relay's own doctor
// answer, remembered for the rest of that Env's life. Env is declared in another issue's
// file and may not gain a field here, and Run builds one Env per invocation, so the map
// holds one live entry per run; entries are never removed, which is bounded in practice.
var relayHelperMemo = map[*Env]string{}

// Relay runs the same executable's relay mode for cfg and returns its stdout and exit
// status. The argument order is fixed: relay --state <state> --socket <socket> <args...>,
// and both flags are passed even to a command that needs no socket.
//
// A command that ran and failed reports its exit status with no error, so the caller
// decides what the status means; the corollary is that such a command's stderr is
// unreachable, and an error's stderr excerpt covers an execution failure only (a command
// that could not be started, a doctor that failed, or a cancelled context).
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
		return stdout, 0, relayHelperError(err, stderr)
	}
	return stdout, code, nil
}

// relayHelperState is the state directory Relay runs with: the configured one, else the
// one this Env already resolved, else the relay doctor's answer, which is remembered.
func (e *Env) relayHelperState(ctx context.Context, cfg *Config) (string, error) {
	if cfg.Relay.State != "" {
		return cfg.Relay.State, nil
	}
	relayHelperMemoMu.Lock()
	defer relayHelperMemoMu.Unlock()
	if state, ok := relayHelperMemo[e]; ok {
		return state, nil
	}
	state, err := e.relayHelperDoctorState(ctx, cfg.Relay.Socket)
	if err != nil {
		return "", relayHelperUnresolved(err)
	}
	relayHelperMemo[e] = state
	return state, nil
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
		return "", fmt.Errorf("relay doctor: %w", err)
	}
	if answer.StateSelection.Path == "" {
		return "", errors.New("relay doctor reported no state directory")
	}
	return answer.StateSelection.Path, nil
}

// relayHelperRun runs one relay command line of the same executable and reports its
// stdout, exit status and stderr excerpt. A command that ran and failed is an exit status
// with no error; one that could not be started is an error.
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

// relayHelperUnresolved marks a state resolution failure as relay_state_unresolved while
// keeping the reason it carries.
func relayHelperUnresolved(detail error) error {
	if detail == nil {
		return relayHelperErrStateUnresolved
	}
	return fmt.Errorf("%w: %v", relayHelperErrStateUnresolved, detail)
}
