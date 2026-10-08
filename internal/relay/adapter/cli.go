package adapter

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// testClock is an optional link-time fixture seam. Release builds leave it empty;
// the built-binary oracle supplies a clock without environment-dependent behavior.
var testClock string

// Register installs the production path at executable composition time, not package
// initialization: importing this library never changes another package's test defaults.
func Register(configure ...func(*appserver.Client)) {
	f := hostFactory{}
	if len(configure) > 0 {
		f.configure = configure[0]
	}
	delivery.HostCommand = f.hostCommand
	delivery.ObserveTurn = f.observeTurn
	delivery.EmitConfirmTurn = f.confirmTurn
	cli.SupervisorHostCommand = f.supervisorHostCommand
	cli.DaemonFactory = f.daemonFactory
	managed.HostStart = f.managedStart
	if testClock != "" {
		now, err := strconv.ParseFloat(testClock, 64)
		if err != nil {
			panic(err)
		}
		delivery.CommandClock = &delivery.FakeClock{T: now}
		cli.SupervisorClock = func() float64 { return now }
		supervisor.TokenSource = bytes.NewReader(make([]byte, 1<<20))
	}
}

func (f hostFactory) observeTurn(ctx context.Context, state, socket, thread, turn string) (status string, err error) {
	selection, err := store.ResolveStateDir("", socket)
	if err != nil {
		return "", err
	}
	a, err := f.open(socket, selection.Path, Options{})
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, a.Close()) }()
	observed, err := a.ReadTurn(ctx, thread, turn)
	if err != nil {
		return "", unconfirmedTurn(turn, err)
	}
	if observed == nil {
		return "", &store.RefusedError{Reason: "unassigned_turn", Detail: "turn " + strconv.Quote(turn) + " does not exist on " + strconv.Quote(thread)}
	}
	status, _ = observed.Status.(string)
	return status, nil
}

// confirmTurn answers emit's existence check without --socket: it reads the turn read-only through
// the host the store recorded, on the socket the caller resolved, and selects no store of its own.
// Like observeTurn it takes the caller's state directory without reading it. It resolves the socket
// the way adapter.Open does, but opens no operations ledger: the read keeps no ledger, so neither a
// refused nor a staged emit leaves one behind (CRW-680). The adapter is built without a ledger, so
// it has no transport, and Adapter.Close leaves the RPC client alone; the client is closed here.
// confirmed is false whenever the read could not be made (no connection, a failed call, or
// HostUnavailable from a listing the page budget ran out on), which leaves emit to stage the receipt
// as before (CRW-675).
func (f hostFactory) confirmTurn(ctx context.Context, state, socket, thread, turn string) (found, confirmed bool, err error) {
	canonical, err := ledger.CanonicalEndpoint(socket)
	if err != nil {
		return false, false, err
	}
	// No Ledger and no LedgerPath: the adapter is read-only, so ReadTurn never opens one. The
	// client is closed explicitly, because a ledger-free adapter has no transport to close it.
	client := appserver.New(canonical, appserver.DefaultBounds)
	defer func() { _ = client.Close() }()
	a := New(Options{ConfigureClient: f.configure, RPC: client})
	defer func() { _ = a.Close() }()
	observed, err := a.ReadTurn(ctx, thread, turn)
	if err != nil {
		return false, false, err
	}
	if observed == nil {
		return false, true, nil
	}
	return true, true, nil
}

// unconfirmedTurn is _observed_turn_status's answer to a failed turn read: a host that could
// not confirm the turn refuses the receipt (unassigned_turn) `from` the HostUnavailable, which
// stays reachable; any other failure is the read's own.
func unconfirmedTurn(turn string, err error) error {
	var unavailable *HostUnavailable
	if errors.As(err, &unavailable) {
		return store.RefusedBecause("unassigned_turn", "the host could not confirm turn "+strconv.Quote(turn)+": "+err.Error(), err)
	}
	return err
}

func (f hostFactory) supervisorHostCommand(ctx context.Context, command, state, socket, program string, args map[string]string, now float64) (out any, err error) {
	s, err := store.Open(ctx, state+"/relay.sqlite3", socket)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	ledgerSelection, err := store.ResolveStateDir("", socket)
	if err != nil {
		return nil, err
	}
	a, err := f.open(socket, ledgerSelection.Path, Options{Store: s, Clock: &delivery.FakeClock{T: now}})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, a.Close()) }()
	channel := &supervisor.Channel{Store: s, Linkage: supervisor.StoreLinkage{Store: s}, Program: program, Socket: socket}
	channel.SettingsLoader = func(ctx context.Context, task string) (*delivery.TaskSettings, error) {
		r := &registry.Registry{Store: s, Now: (&delivery.FakeClock{T: now}).ISO, Policy: registry.EnvironmentRolePolicy()}
		settings, role, free, err := r.AuthorizedSettingsBound(ctx, task)
		if err != nil {
			return nil, err
		}
		return &delivery.TaskSettings{Data: delivery.Obj(settings.Data), SettingsFreeResume: free, BoundRole: role}, nil
	}
	switch command {
	case "supervisor-send":
		record, err := channel.Attempt(ctx, args["message"], a, now)
		if err != nil {
			return nil, err
		}
		if record == nil {
			row, err := channel.Get(ctx, args["message"])
			if err != nil {
				return nil, err
			}
			var hold, next any
			if row.HoldReason.Valid {
				hold = row.HoldReason.String
			}
			if row.NextEligibleAt.Valid {
				next = row.NextEligibleAt.Float64
			}
			return contract.OrderedObject{{Key: "attempted", Value: false}, {Key: "sent", Value: false}, {Key: "messageId", Value: args["message"]}, {Key: "state", Value: row.State}, {Key: "holdReason", Value: hold}, {Key: "nextEligibleAt", Value: next}, {Key: "detail", Value: "nothing was sent and nothing is wrong: a busy recipient, a backoff still running, or a message already sent"}}, nil
		}
		out := map[string]any{"attempted": true, "sent": record["deliveryState"] == "dispatched"}
		for key, value := range record {
			out[key] = value
		}
		return out, nil
	case "supervisor-read":
		return channel.ReadBack(ctx, args["message"], args["turn"], args["proof"], args["as"], a, now)
	default:
		return nil, &HostUnavailable{"unknown supervisor host command: " + command}
	}
}

func (f hostFactory) hostCommand(ctx context.Context, command, state, socket string, args map[string]any, clock delivery.Clock) (out any, err error) {
	s, err := store.Open(ctx, state+"/relay.sqlite3", socket)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	ledgerSelection, err := store.ResolveStateDir("", socket)
	if err != nil {
		return nil, err
	}
	a, err := f.open(socket, ledgerSelection.Path, Options{Store: s, Clock: clock})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, a.Close()) }()
	d := delivery.NewService(s, clock)
	ack := delivery.NewAck(d)
	rc := delivery.NewReconciler(d)
	switch command {
	case "deliver":
		if id := pyjson.Text(args["--event"]); id != "" {
			record, e := d.Attempt(ctx, id, a, nil, "")
			if e != nil {
				return nil, e
			}
			if _, e = ack.BindPendingAnchors(ctx); e != nil {
				return nil, e
			}
			var value any
			if record != nil {
				value = record
			}
			return contract.OrderedObject{{Key: "attempt", Value: value}}, nil
		}
		limit := hostLimit(args["--limit"])
		rows, e := d.Eligible(ctx, clock.Now(), limit, limit, 0)
		if e != nil {
			return nil, e
		}
		attempts := []any{}
		for _, row := range rows {
			r, e := d.Attempt(ctx, row.S("event_id"), a, nil, "")
			if e != nil {
				return nil, e
			}
			if r == nil {
				attempts = append(attempts, nil)
			} else {
				attempts = append(attempts, r)
			}
		}
		if _, e = ack.BindPendingAnchors(ctx); e != nil {
			return nil, e
		}
		return contract.OrderedObject{{Key: "attempts", Value: attempts}}, nil
	case "reconcile":
		out, e := rc.ReconcileAttempt(ctx, pyjson.Text(args["--request-id"]), a, nil)
		if e != nil {
			if strings.HasPrefix(e.Error(), "KeyError: ") {
				return nil, &dispatch.PayloadExit{Code: contract.ExitHost, Payload: contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: e.Error()}}}
			}
			return nil, e
		}
		if _, e = ack.BindPendingAnchors(ctx); e != nil {
			return nil, e
		}
		return out, nil
	case "recover":
		out, e := rc.RecoverOnStart(ctx, a, nil)
		if e != nil {
			return nil, e
		}
		bound, e := ack.BindPendingAnchors(ctx)
		if e != nil {
			return nil, e
		}
		return append(out, contract.Field{Key: "anchorsBound", Value: bound}), nil
	case "ack":
		return delivery.AckCommand(ctx, ack, rc, a, pyjson.Text(args["--event"]), pyjson.Text(args["--ack-turn"]), pyjson.Text(args["--ack-proof"]), args["--reject"])
	case "verify-acks":
		return delivery.VerifyAcksCommand(ctx, ack, rc, a, hostLimit(args["--limit"]))
	}
	return nil, &HostUnavailable{"unknown host command: " + command}
}

// hostLimit is a host command's --limit: the int64 the parser read from the line, else the
// command's int64 default. A value past int64 is refused by the parser before this runs.
func hostLimit(value any) int {
	return int(value.(int64))
}
