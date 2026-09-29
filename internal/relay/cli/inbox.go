package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/inbox"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// replayQueued is the takeover inbox's handler table (decision 25): each queued command runs
// its existing handler on the drainer's store, with the drainer's App Server socket as its
// host, as inbox.replay runs args.handler(services, args) with the drainer's services.
func replayQueued(socket string) inbox.Apply {
	return func(ctx context.Context, st *store.Store, command string, argv []string) (any, int, error) {
		switch command {
		case "emit", "ack":
			return delivery.ApplyQueued(ctx, st, command, argv, socket)
		case "fault-notification-ack":
			return faults.ApplyQueued(ctx, st, argv)
		case "supervisor-read":
			return replayReadback(ctx, st, argv, socket)
		}
		return nil, 0, fmt.Errorf("no handler replays queued %s", command)
	}
}

// replayReadback is a legacy queued supervisor-read replayed as cmd_supervisor_read runs on the
// drainer's services: without a socket its no-host usage refusal; with one, the readback on the
// drainer's store (channel.ReadBack, inside the replay's transaction) against a host opened only
// when the readback first reads it, as Python's _LazyAdapter is, so a readback the store's own
// rows refuse (no such message, another recipient, a wrong proof) answers without the host.
func replayReadback(ctx context.Context, st *store.Store, argv []string, socket string) (any, int, error) {
	if socket == "" {
		return contract.OrderedObject{{Key: "error", Value: "usage"}, {Key: "detail", Value: "supervisor-read needs --socket: " + readbackNeedsHost}}, contract.ExitUsage, nil
	}
	parsed := argparse.Parse("supervisor-read", argv)
	if parsed.Message != "" || parsed.Help {
		return nil, 0, fmt.Errorf("queued supervisor-read arguments: %s", parsed.Message)
	}
	value := func(name string) string {
		values := parsed.Values[name]
		if len(values) == 0 {
			return ""
		}
		return values[len(values)-1]
	}
	now := SupervisorClock()
	var host supervisor.SendAdapter
	lazy := &lazyHost{open: func() (delivery.Adapter, func() error, error) {
		return delivery.QueuedAckHost(ctx, socket, &delivery.FakeClock{T: now})
	}}
	if delivery.QueuedAckHost != nil {
		host = lazy
	}
	channel := &supervisor.Channel{Store: st, Linkage: supervisor.StoreLinkage{Store: st}, Socket: socket}
	result, err := channel.ReadBack(ctx, value("message"), value("turn"), value("proof"), value("as"), host, now)
	if e := lazy.close(); err == nil && e != nil {
		err = e
	}
	if err = supervisorResult(err); err != nil {
		var refused *store.RefusedError
		if inbox.Retained(err) || !errors.As(err, &refused) {
			return nil, 0, err
		}
		return contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: nullableText(refused.Reason)}, {Key: "detail", Value: refused.Detail}}, contract.ExitRefused, nil
	}
	return supervisorOrdered(result), contract.ExitOk, nil
}

// lazyHost opens the replay's host on its first use (Python's _LazyAdapter): nothing is built
// for the host, and no ledger is opened beside it, unless a readback actually reads it. A host
// that cannot be opened fails that read, as the host's own failure would.
type lazyHost struct {
	open    func() (delivery.Adapter, func() error, error)
	host    delivery.Adapter
	release func() error
	err     error
	tried   bool
}

func (l *lazyHost) get() (delivery.Adapter, error) {
	if !l.tried {
		l.tried = true
		l.host, l.release, l.err = l.open()
	}
	return l.host, l.err
}

func (l *lazyHost) close() error {
	if l.release == nil {
		return nil
	}
	release := l.release
	l.release = nil
	return release()
}

func (l *lazyHost) ReadThread(thread string) (delivery.ThreadFacts, error) {
	h, err := l.get()
	if err != nil {
		return delivery.ThreadFacts{}, err
	}
	return h.ReadThread(thread)
}

func (l *lazyHost) IsArchived(thread string, cwd any) (*bool, error) {
	h, err := l.get()
	if err != nil {
		return nil, err
	}
	return h.IsArchived(thread, cwd)
}

func (l *lazyHost) ReadGoalStatus(thread string) (any, error) {
	h, err := l.get()
	if err != nil {
		return nil, err
	}
	return h.ReadGoalStatus(thread)
}

func (l *lazyHost) ListTurnIDs(thread string, limit int) ([]any, error) {
	h, err := l.get()
	if err != nil {
		return nil, err
	}
	return h.ListTurnIDs(thread, limit)
}

func (l *lazyHost) ReadTurn(thread, turn string) (*delivery.TurnInfo, error) {
	h, err := l.get()
	if err != nil {
		return nil, err
	}
	return h.ReadTurn(thread, turn)
}

func (l *lazyHost) SendMessage(requestID, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	h, err := l.get()
	if err != nil {
		return nil, err
	}
	return h.SendMessage(requestID, thread, message, settings)
}

func (l *lazyHost) GetOperation(requestID string) (delivery.Obj, error) {
	h, err := l.get()
	if err != nil {
		return nil, err
	}
	return h.GetOperation(requestID)
}

func (l *lazyHost) FindToken(thread, token string, limit int, messageOnly bool) (delivery.TokenScan, error) {
	h, err := l.get()
	if err != nil {
		return delivery.TokenScan{}, err
	}
	return h.FindToken(thread, token, limit, messageOnly)
}

func (l *lazyHost) FindDispatchedTurn(thread, turnID string, sentAt float64) (delivery.TurnPresence, error) {
	h, err := l.get()
	if err != nil {
		return delivery.TurnPresence{}, err
	}
	return h.FindDispatchedTurn(thread, turnID, sentAt)
}

func (l *lazyHost) FindTokenSince(thread, token string, older []string, limit int) (delivery.TokenScan, error) {
	h, err := l.get()
	if err != nil {
		return delivery.TokenScan{}, err
	}
	return h.FindTokenSince(thread, token, older, limit)
}

func (l *lazyHost) FindTokenInTurn(thread, token, turnID string, limit int) (delivery.TokenScan, error) {
	h, err := l.get()
	if err != nil {
		return delivery.TokenScan{}, err
	}
	return h.FindTokenInTurn(thread, token, turnID, limit)
}

func (l *lazyHost) RecipientFingerprint(thread string) (string, error) {
	h, err := l.get()
	if err != nil {
		return "", err
	}
	return h.RecipientFingerprint(thread)
}

// drainInbox applies S/takeover-inbox on the admitted writable store st of the state
// directory it lives in. A bare ownership refusal (the store's admission changed under the
// drain) answers as a refused admission does.
func drainInbox(ctx context.Context, st *store.Store, socket string) error {
	return store.AsOwnershipRefusal(inbox.Drain(ctx, st, filepath.Dir(st.Path), replayQueued(socket)))
}

// drainsBeforeHandler reports whether a writable command admits its store and drains the inbox
// at dispatch, before its handler reads its own arguments (cli.py main: _ownership_preflight
// opens services.store, then inbox.replay, then the handler). The service and daemon commands
// drain in their own recovery; managed-start and the intent commands open their own admitted
// connection, which the fence never replays on.
func drainsBeforeHandler(command string) bool {
	switch command {
	case "service", "daemon", "managed-start":
		return false
	}
	return !strings.HasPrefix(command, "intent-")
}
