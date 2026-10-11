package engine

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
)

// RecoverEnv opens what `crw flow recover` works on for a run: the run's journal and the host it ran on. This build keeps
// the journal in memory (store persistence is F06), so a second process has no journal to open and nothing installs the
// hook; the command then says so. The persistent journal and the App Server connection of a real run install it.
var RecoverEnv func(ctx context.Context, runID string) (Journal, ReconHost, error)

const recoverUsage = `usage: crw flow recover [--run RUN] CALL (--adopt THREAD|TURN | --absent)`

// recoverCmd is `crw flow recover CALL --adopt THREAD|TURN | --absent`: the only manual way out of a recovery_hold. --adopt
// names the thread or turn that is the call's; --absent reads the host again, with a complete listing and history, and
// accepts the call only when the proofs of the automatic reconcile hold. It prints what it did as JSON and exits 0, exits 1
// when the proof does not hold (nothing is appended), and exits 2 for a command line it cannot use.
func recoverCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs_ := flag.NewFlagSet("crw flow recover", flag.ContinueOnError)
	fs_.SetOutput(stderr)
	runID := fs_.String("run", "", "the run the call belongs to")
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
		fmt.Fprintln(stderr, "crw flow recover: error: this build keeps a run's journal in memory, so a call of another process cannot be recovered (the journal is stored by F06)")
		return 2
	}
	j, host, err := RecoverEnv(ctx, *runID)
	if err != nil {
		fmt.Fprintf(stderr, "crw flow recover: error: %v\n", err)
		return 2
	}
	res, err := Recover(ctx, j, ReconcileConfig{RunID: *runID, Host: host}, RecoverRequest{CallID: positional[0], Adopt: *adopt, Absent: *absent})
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
