package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"path/filepath"
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
		return fail(&UsageError{"takeover requires status, begin, drain, transfer, activate, abort, rollback, commit, or repair-mirror", 4})
	}
	action := args[0]
	flags := flag.NewFlagSet("takeover "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	to := flags.String("to", "", "")
	pythonRelay := flags.String("python-relay", "", "")
	readyTimeout := flags.Float64("ready-timeout", service.DefaultReadyTimeout.Seconds(), "")
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
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })
	launches := action == "activate" || action == "rollback"
	if (given["python-relay"] || given["ready-timeout"]) && !launches {
		return fail(&UsageError{"--python-relay and --ready-timeout apply to activate and rollback only", 4})
	}
	if given["python-relay"] && !filepath.IsAbs(*pythonRelay) {
		return fail(&UsageError{"--python-relay must be the absolute path of the retained fence release's codex-session-relay", 4})
	}
	// The bound must also fit a time.Duration next to the 30-second step bound.
	if math.IsNaN(*readyTimeout) || math.IsInf(*readyTimeout, 0) || *readyTimeout <= 0 || *readyTimeout > float64(math.MaxInt64/4)/float64(time.Second) {
		return fail(&UsageError{"--ready-timeout must be a finite number of seconds greater than zero", 4})
	}
	selection, err := store.ResolveStateDir(*state, *socket)
	if err != nil {
		return fail(err)
	}
	switch action {
	case "status", "begin", "drain", "transfer", "activate", "abort", "rollback", "commit", "repair-mirror":
	default:
		return fail(&UsageError{"unknown takeover action: " + action, 4})
	}
	build := Version
	if Build != "" {
		build = Build
	}
	ready := time.Duration(*readyTimeout * float64(time.Second))
	c, err := service.NewTakeover(ctx, selection, *socket, build, service.TakeoverOptions{PythonRelay: *pythonRelay, ReadyTimeout: ready})
	if err != nil {
		return fail(takeoverError(err))
	}
	// Every step is bounded by 30 seconds; a launching action adds the readiness
	// bound, which spans the candidate's recovery (decision 30).
	bound := 30 * time.Second
	if launches {
		bound += ready
	}
	bounded, cancel := context.WithTimeout(ctx, bound)
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
	case "abort":
		err = c.Abort(bounded)
	case "rollback":
		err = c.Rollback(bounded)
	case "commit":
		err = c.Commit(bounded)
	case "repair-mirror":
		err = c.RepairMirror(bounded)
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
