package adapter

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
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
func Register() {
	delivery.HostCommand = hostCommand
	delivery.ObserveTurn = observeTurn
	cli.SupervisorHostCommand = supervisorHostCommand
	cli.DaemonFactory = daemonFactory
	managed.HostStart = managedStart
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

func observeTurn(ctx context.Context, state, socket, thread, turn string) (status string, err error) {
	selection, err := store.ResolveStateDir("", socket)
	if err != nil {
		return "", err
	}
	a, err := Open(socket, selection.Path, Options{})
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, a.Close()) }()
	observed, err := a.ReadTurn(thread, turn)
	if err != nil {
		var unavailable *HostUnavailable
		if errors.As(err, &unavailable) {
			return "", &store.RefusedError{Reason: "unassigned_turn", Detail: "the host could not confirm turn " + store.PythonRepr(turn) + ": " + err.Error()}
		}
		return "", err
	}
	if observed == nil {
		return "", &store.RefusedError{Reason: "unassigned_turn", Detail: "turn " + store.PythonRepr(turn) + " does not exist on " + store.PythonRepr(thread)}
	}
	status, _ = observed.Status.(string)
	return status, nil
}

func supervisorHostCommand(ctx context.Context, command, state, socket, program string, args map[string]string, now float64) (out any, err error) {
	s, err := store.Open(ctx, state+"/relay.sqlite3", socket)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	ledgerSelection, err := store.ResolveStateDir("", socket)
	if err != nil {
		return nil, err
	}
	a, err := Open(socket, ledgerSelection.Path, Options{Store: s, Clock: &delivery.FakeClock{T: now}})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, a.Close()) }()
	channel := &supervisor.Channel{Store: s, Linkage: supervisor.StoreLinkage{Store: s}, Program: program, Socket: socket}
	channel.SettingsLoader = func(ctx context.Context, task string) (*delivery.TaskSettings, error) {
		r := &registry.Registry{Store: s, Now: (&delivery.FakeClock{T: now}).ISO, Policy: registry.EnvironmentRolePolicy()}
		settings, free, err := r.AuthorizedSettings(ctx, task)
		if err != nil {
			return nil, err
		}
		return &delivery.TaskSettings{Data: delivery.Obj(settings.Data), SettingsFreeResume: free}, nil
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

func hostCommand(ctx context.Context, command, state, socket string, args map[string]any, clock delivery.Clock) (out any, err error) {
	s, err := store.Open(ctx, state+"/relay.sqlite3", socket)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	ledgerSelection, err := store.ResolveStateDir("", socket)
	if err != nil {
		return nil, err
	}
	a, err := Open(socket, ledgerSelection.Path, Options{Store: s, Clock: clock})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, a.Close()) }()
	d := delivery.NewService(s, clock)
	ack := delivery.NewAck(d)
	rc := delivery.NewReconciler(d)
	switch command {
	case "deliver":
		if id := text(args["--event"]); id != "" {
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
		limit := int(args["--limit"].(int64))
		rows, e := d.Eligible(ctx, clock.Now(), limit, limit, 0, nil)
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
		out, e := rc.ReconcileAttempt(ctx, text(args["--request-id"]), a, nil)
		if e != nil {
			if strings.HasPrefix(e.Error(), "KeyError: ") {
				return nil, &cli.PayloadExit{Code: contract.ExitHost, Payload: contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: e.Error()}}}
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
		return delivery.AckCommand(ctx, ack, rc, a, text(args["--event"]), text(args["--ack-turn"]), text(args["--ack-proof"]), args["--reject"])
	case "verify-acks":
		return delivery.VerifyAcksCommand(ctx, ack, rc, a, int(args["--limit"].(int64)))
	}
	return nil, &HostUnavailable{"unknown host command: " + command}
}
