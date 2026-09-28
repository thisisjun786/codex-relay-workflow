package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// ExecuteTakeover is the native-only command surface, separate from the frozen
// Python argparse tree. Global routing flags retain their before-command form.
func ExecuteTakeover(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, bool) {
	index := -1
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--state" || a == "--socket" || a == "--kind-module" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--state=") || strings.HasPrefix(a, "--socket=") || strings.HasPrefix(a, "--kind-module=") || a == "--json" {
			continue
		}
		if a == "takeover" {
			index = i
		}
		break
	}
	if index < 0 {
		return 0, false
	}
	root := flag.NewFlagSet("takeover", flag.ContinueOnError)
	root.SetOutput(io.Discard)
	state := root.String("state", "", "")
	socket := root.String("socket", "", "")
	root.Bool("json", false, "")
	fail := func(err error) (int, bool) { return emit(stdout, stderr, nil, err), true }
	if err := root.Parse(argv[:index]); err != nil {
		return fail(&UsageError{err.Error(), 4})
	}
	args := argv[index+1:]
	if len(args) == 0 {
		return fail(&UsageError{"takeover requires status, begin, drain, transfer, activate, rollback, or commit", 4})
	}
	action := args[0]
	flags := flag.NewFlagSet("takeover "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	to := flags.String("to", "", "")
	flags.Bool("json", false, "")
	if err := flags.Parse(args[1:]); err != nil {
		return fail(&UsageError{err.Error(), 4})
	}
	if len(flags.Args()) != 0 {
		return fail(&UsageError{"unexpected takeover arguments", 4})
	}
	if (action == "begin" && *to != "go") || (action == "rollback" && *to != "python") || ((action != "begin" && action != "rollback") && *to != "") {
		return fail(&UsageError{"begin requires --to go; rollback requires --to python", 4})
	}
	selection, err := store.ResolveStateDir(*state, *socket)
	if err != nil {
		return fail(err)
	}
	if action == "candidate" {
		code := runTakeoverCandidate(ctx, selection, *socket, stdout, stderr)
		return code, true
	}
	switch action {
	case "status", "begin", "drain", "transfer", "activate", "rollback", "commit":
	default:
		return fail(&UsageError{"unknown takeover action: " + action, 4})
	}
	build := Version
	if Build != "" {
		build = Build
	}
	c, err := service.NewTakeover(ctx, selection, *socket, build)
	if err != nil {
		return fail(takeoverError(err))
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch action {
	case "begin":
		err = c.Begin(bounded, *to)
	case "drain":
		err = c.Drain(bounded)
	case "transfer":
		err = c.Transfer(bounded)
	case "activate":
		err = c.Activate(bounded)
	case "rollback":
		err = c.Rollback(bounded)
	case "commit":
		err = c.Commit(bounded)
	}
	if err != nil {
		return fail(takeoverError(err))
	}
	status, err := c.Status(bounded)
	if err != nil {
		return fail(takeoverError(err))
	}
	if err = json.NewEncoder(stdout).Encode(status); err != nil {
		return fail(err)
	}
	return 0, true
}
func takeoverError(err error) error {
	var refused *ownership.Refused
	if errors.As(err, &refused) {
		return &store.RefusedError{Reason: "store_owned_by_other", Detail: refused.Detail}
	}
	return err
}
func runTakeoverCandidate(ctx context.Context, selection store.StateSelection, socket string, stdout, stderr io.Writer) int {
	candidateCtx, channel, err := service.ReceiveCandidate(ctx)
	if err != nil {
		return emit(stdout, stderr, nil, takeoverError(err))
	}
	defer channel.Close()
	if channel.Record.Database.RealPath != selection.DBPath() || channel.Record.AppServerSocket == nil || *channel.Record.AppServerSocket != socket {
		return emit(stdout, stderr, nil, &UsageError{"candidate routing disagrees", 4})
	}
	// Invoke the same daemon entry point, not an ownership-only placeholder.
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	daemonCommand.Flags(flags)
	if err = flags.Set("allow-isolated-scope", "true"); err != nil {
		return emit(stdout, stderr, nil, err)
	}
	// Reuse the service's finite worker segment. A stopped segment remains Go
	// owned; supervision/restart is an explicit service action, not owner reset.
	if err = flags.Set("deadline", "3600"); err != nil {
		return emit(stdout, stderr, nil, err)
	}
	candidateCtx = context.WithValue(candidateCtx, activationKey{}, channel)
	services := Services{Selection: selection, SocketPath: socket, AdapterRequested: true, Program: program(os.Args[0])}
	answer, err := runDaemon(candidateCtx, services, Args{Flags: flags, Set: map[string]bool{"deadline": true}})
	return emit(stdout, stderr, answer, err)
}

type activationKey struct{}

func activateCandidate(ctx context.Context) error {
	channel, ok := ctx.Value(activationKey{}).(*service.CandidateChannel)
	if !ok {
		return nil
	}
	build := Version
	if Build != "" {
		build = Build
	}
	if err := channel.Ready(ctx, build); err != nil {
		return fmt.Errorf("candidate activation: %w", err)
	}
	return nil
}
