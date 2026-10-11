package engine

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
)

// RecoverEnvRequest is what `crw flow recover` asks RecoverEnv to open: the run's stored journal and the host the run ran on.
// The binary installs RecoverEnv (cmd/crw): it opens the relay store the journal is persisted in and connects to the App
// Server. A build that installs nothing cannot recover a call of another process and says so.
type RecoverEnvRequest struct {
	// RunID is the run the call belongs to; empty asks the journal which run holds CallID.
	RunID  string
	CallID string
	// State and Socket are the relay state directory and the App Server control socket the operator named; empty takes the
	// relay's own selection (CODEX_SESSION_RELAY_STATE or discovery, and the default control socket).
	State, Socket string
}

// RecoverSetup is what RecoverEnv opened: the run's journal, the host, the run the call belongs to, and the close that ends
// both (it commits the rows the recovery appended).
type RecoverSetup struct {
	Journal Journal
	Host    ReconHost
	RunID   string
	Close   func() error
}

// RecoverEnv opens what `crw flow recover` works on for a call.
var RecoverEnv func(ctx context.Context, req RecoverEnvRequest) (RecoverSetup, error)

const recoverUsage = `usage: crw flow recover [--run RUN] [--state DIR] [--socket PATH] CALL (--adopt THREAD|TURN | --absent)`

// recoverCmd is `crw flow recover CALL --adopt THREAD|TURN | --absent`: the only manual way out of a recovery_hold. --adopt
// names the thread or turn that is the call's; --absent reads the host again, with a complete listing and history, and
// accepts the call only when the proofs of the automatic reconcile hold. It prints what it did as JSON and exits 0, exits 1
// when the proof does not hold (nothing is appended), and exits 2 for a command line it cannot use.
func recoverCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs_ := flag.NewFlagSet("crw flow recover", flag.ContinueOnError)
	fs_.SetOutput(stderr)
	runID := fs_.String("run", "", "the run the call belongs to (default: the run that holds the call)")
	stateDir := fs_.String("state", "", "the relay state directory the journal is stored in (default: the relay's own selection)")
	socket := fs_.String("socket", "", "the App Server control socket the run's host listens on (default: the default control socket)")
	adopt := fs_.String("adopt", "", "the thread or turn that is the call's")
	absent := fs_.Bool("absent", false, "accept the call as absent when the host shows the proof")
	fs_.Usage = func() { fmt.Fprintln(stderr, recoverUsage) }
	var positional []string
	rest := args
	for {
		if err := fs_.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs_.NArg() == 0 {
			break
		}
		positional = append(positional, fs_.Arg(0))
		rest = fs_.Args()[1:]
	}
	switch {
	case len(positional) != 1:
		fmt.Fprintln(stderr, recoverUsage)
		fmt.Fprintln(stderr, "crw flow recover: error: exactly one call id is required")
		return 2
	case *absent == (*adopt != ""):
		fmt.Fprintln(stderr, recoverUsage)
		fmt.Fprintln(stderr, "crw flow recover: error: exactly one of --adopt and --absent is required")
		return 2
	case RecoverEnv == nil:
		fmt.Fprintln(stderr, "crw flow recover: error: this build installs no journal store and no host, so a call of another process cannot be recovered")
		return 2
	}
	setup, err := RecoverEnv(ctx, RecoverEnvRequest{RunID: *runID, CallID: positional[0], State: *stateDir, Socket: *socket})
	if err != nil {
		fmt.Fprintf(stderr, "crw flow recover: error: %v\n", err)
		return 2
	}
	res, err := Recover(ctx, setup.Journal, ReconcileConfig{RunID: setup.RunID, Host: setup.Host}, RecoverRequest{CallID: positional[0], Adopt: *adopt, Absent: *absent})
	if setup.Close != nil {
		if cerr := setup.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("the rows of the recovery could not be stored: %w", cerr)
		}
	}
	var refused *ProofNotMet
	switch {
	case errors.As(err, &refused):
		fmt.Fprintf(stderr, "crw flow recover: %v\n", err)
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "crw flow recover: error: %v\n", err)
		return 2
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		fmt.Fprintf(stderr, "crw flow recover: error: %v\n", err)
		return 2
	}
	return 0
}
