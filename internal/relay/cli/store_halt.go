package cli

import (
	"context"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// store-halt-clear (CRW-885) is the operator's clear of the halt marker CRW-848 publishes. It takes
// the write gate exclusively, checks the two readings the operator made, removes S/corruption.json
// only when both exist and are not empty, and records the clear in one journal row. It never judges
// what the readings say.
//
// The command answers the start preflight itself, because that preflight refuses a halted store
// and this command exists to clear one (SelectsNoStore), and dispatch admits no writable store before
// the handler (OwnAdmission): the clear opens the store on its own connection, under the gate.
var storeHaltClearCommand = dispatch.Command{
	Name:           "store-halt-clear",
	OwnAdmission:   true,
	SelectsNoStore: func(dispatch.Args) bool { return true },
	Run:            runStoreHaltClear,
}

func runStoreHaltClear(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	if err := dispatch.CheckSelection(services); err != nil {
		return nil, err
	}
	if err := dispatch.CheckSocket(services); err != nil {
		return nil, err
	}
	input := store.HaltClearInput{
		RestorePath:   args.Text("restore-reading"),
		ReconcilePath: args.Text("reconcile-reading"),
		Actor:         args.Text("actor"),
		Reason:        args.Text("reason"),
	}
	if input.Actor == "" || input.Reason == "" {
		return nil, &dispatch.UsageError{Detail: "store-halt-clear needs a non-empty --actor and --reason", Code: contract.ExitUsage}
	}
	// The journal row is JSON, which replaces invalid UTF-8 with U+FFFD: a value that is not valid
	// UTF-8 would be recorded as different text, so it is refused before the marker is read.
	if !utf8.ValidString(input.Actor) || !utf8.ValidString(input.Reason) {
		return nil, &dispatch.UsageError{Detail: "store-halt-clear needs --actor and --reason as valid UTF-8", Code: contract.ExitUsage}
	}
	result, err := store.ClearHalt(ctx, services.Selection.DBPath(), input)
	if err != nil {
		return nil, err
	}
	if !result.Cleared {
		return contract.OrderedObject{
			{Key: "cleared", Value: false},
			{Key: "detail", Value: "no halt marker beside the store: nothing was changed and no journal row was written"},
		}, nil
	}
	var sequence, detectedAt any
	if result.Marker.Detail == "" {
		sequence = int64(result.Marker.Marker.Sequence)
		detectedAt = result.Marker.Marker.DetectedAt
	}
	return contract.OrderedObject{
		{Key: "cleared", Value: true},
		{Key: "markerSequence", Value: sequence},
		{Key: "markerDetectedAt", Value: detectedAt},
		{Key: "restoreSha256", Value: result.Restore.SHA256},
		{Key: "reconcileSha256", Value: result.Reconcile.SHA256},
		{Key: "actor", Value: input.Actor},
		{Key: "reason", Value: input.Reason},
	}, nil
}
