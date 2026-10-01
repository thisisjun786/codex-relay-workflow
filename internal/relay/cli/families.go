package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/selection"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// familyDelegate runs a command of the families that still parse their own line (delivery,
// faults) with the selection refusal, check_start and admission cli.main applies
// first, handed to them as their check.
func familyDelegate(ctx context.Context, argv0, prog string, root argparse.Result, stdout, stderr io.Writer) (int, bool) {
	remaining := root.Remaining
	name := remaining[0]
	isFault, isDelivery := slices.Contains(faults.Names(), name), slices.Contains(delivery.CommandNames(), name)
	if !isFault && !isDelivery {
		return 0, false
	}
	kindModules := root.Values["kind-module"]
	argv := root.RootArgs()
	ctx = readOnlyContext(ctx, remaining)
	drains := !strings.HasPrefix(name, "intent-") && !store.ReadOnlyCommand(ctx)
	startChecked := !store.ReadOnlyCommand(ctx) && !strings.HasPrefix(name, "intent-")
	checkStart := func(selection store.StateSelection) error {
		if !startChecked || selection.Path == "" {
			return nil
		}
		return store.CheckStartLikeFence(ctx, selection.DBPath())
	}
	ctx, admitted := store.WithAdmitted(ctx)
	defer func() { _ = admitted.Release() }()
	admit := func(selection store.StateSelection, socket string) error {
		st, err := store.Open(ctx, selection.DBPath(), socket)
		if err != nil {
			return err
		}
		if err = kindModuleRefusal(kindModules); err == nil {
			admitted.Hold(st, selection.DBPath(), socket)
			return nil
		}
		if e := st.Close(); e != nil {
			err = errors.Join(err, e)
		}
		return err
	}
	program := selection.Program(argv0)
	refusal := func(selected store.StateSelection, socket string) error {
		if err := checkStart(selected); err != nil {
			return err
		}
		return dispatch.CheckSelection(dispatch.Services{Selection: selected, SocketPath: socket, AdapterRequested: socket != "", Program: program})
	}
	if isFault {
		code, _ := faults.ExecuteAs(ctx, prog, argv, stdout, stderr, func(selection store.StateSelection, socket string) error {
			if err := refusal(selection, socket); err != nil || !drains {
				return err
			}
			return admit(selection, socket)
		})
		return code, true
	}
	code, _ := delivery.ExecuteAs(ctx, prog, argv, stdout, stderr, func(selection store.StateSelection, socket string) error {
		if name != "ack-proof" {
			if err := refusal(selection, socket); err != nil {
				return err
			}
		}
		if drains && selection.Path != "" {
			return admit(selection, socket)
		}
		return kindModuleRefusal(kindModules)
	})
	return code, true
}

// kindModuleRefusal is _import_kind_modules as a whole answer, for the families' checks.
func kindModuleRefusal(names []string) error {
	for _, candidate := range names {
		if candidate == "codex_session_relay.projects" {
			faults.InstallProductDeclarations()
		}
		if candidate == "" {
			return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: "ValueError: Empty module name"}}, Code: contract.ExitHost}
		}
		if strings.HasPrefix(candidate, ".") {
			return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: "TypeError: the 'package' argument is required to perform a relative import for " + pyvalue.StrRepr(candidate)}}, Code: contract.ExitHost}
		}
		if faults.RegisteredModule(candidate) {
			continue
		}
		missing := candidate
		parts := strings.Split(candidate, ".")
		for i := 1; i < len(parts); i++ {
			prefix := strings.Join(parts[:i], ".")
			if prefix == "codex_session_relay" || faults.RegisteredModule(prefix) {
				continue
			}
			missing = prefix
			break
		}
		return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "usage"}, {Key: "detail", Value: "--kind-module " + pyvalue.StrRepr(candidate) + " could not be imported: No module named " + pyvalue.StrRepr(missing)}}, Code: contract.ExitUsage}
	}
	return nil
}

// Registered reports whether this build implements the relay command name.
func Registered(name string) bool {
	return dispatch.Registered(name) || slices.Contains(faults.Names(), name) || slices.Contains(delivery.CommandNames(), name)
}
