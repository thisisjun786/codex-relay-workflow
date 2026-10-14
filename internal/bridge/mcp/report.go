package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

// ChildForward carries a child command to the relay daemon whose state directory is state and returns the daemon's answer
// byte for byte: the client a child's own command uses (hook.ForwardChild), which dials the state's control.sock and checks
// the daemon's peer and ownership. There is no other route and no fallback to a store of the bridge's own.
type ChildForward func(ctx context.Context, state, program string, argv []string, stdout, stderr io.Writer) (int, error)

// ChildState is the state directory of the relay daemon a bridge started on socket reaches, resolved the way the relay
// resolves a state (store.ResolveStateDir): "" is the default socket.
type ChildState func(socket string) (string, error)

var (
	forwardChild ChildForward
	childState   ChildState
)

// RegisterChildReports gives report_child its route to the relay daemon. It is called at executable composition time (the
// relay adapter's Register), not when this package loads: the package imports no relay package but the leaf argparse,
// which keeps the relay's own packages free to test against it (internal/relay/store's tests import it).
func RegisterChildReports(forward ChildForward, state ChildState) {
	forwardChild, childState = forward, state
}

// childReports is the bridge's route to the relay daemon (CRW-1297). The bridge runs outside the sandbox of the child
// that calls report_child, so a child that cannot connect to control.sock (a readOnly sandbox, or a workspace-write one
// without network access) reports through it. The state the daemon is found in is the bridge's own configuration: the
// App Server socket it was started with, resolved the way the relay resolves a state (CODEX_SESSION_RELAY_STATE, else the
// directory discovery scopes by that socket) in the bridge's own environment. No tool argument and nothing of the
// child's argv or environment reaches that choice, and the daemon runs the command on its own state in any case
// (dispatch.OwnedChildState, CRW-1299).
type childReports struct {
	// socket is the App Server socket the bridge was started with; "" when it was started with the default one.
	socket string
}

// ServerOption configures NewServer.
type ServerOption func(*handlers)

// WithChildReports gives report_child its route to the relay daemon. socket is the App Server socket the bridge was
// started with, "" for the default one: the relay's state discovery is scoped by it. A server built without this option
// answers report_child with an explicit refusal.
func WithChildReports(socket string) ServerOption {
	return func(h *handlers) { h.reports = &childReports{socket: socket} }
}

type reportChildIn struct {
	Command   string   `json:"command"`
	Arguments []string `json:"arguments"`
}

func (h handlers) reportChild(ctx context.Context, in reportChildIn) (map[string]any, error) {
	if h.reports == nil || forwardChild == nil || childState == nil {
		return h.explained("report_child", nil, &bridge.Invalid{Reason: "this bridge was built without a route to the relay daemon, so it cannot carry a child report"})
	}
	// The arguments are the command's own options and nothing else: the daemon reads them as it reads a child's
	// command line and answers a line it cannot read only with a rejected request, so the reason is given here. A
	// --state, --socket or --kind-module is no option of the command (they are the relay's root options), which is how
	// the call cannot name where the report goes.
	if parsed := argparse.Parse(in.Command, in.Arguments); parsed.Help || parsed.Message != "" {
		reason := parsed.Message
		if parsed.Help {
			reason = "--help asks for usage and reports nothing"
		}
		return h.explained("report_child", nil, &bridge.Invalid{Reason: fmt.Sprintf("arguments are not the options of `%s`: %s; the call names no state, socket or module: the relay daemon alone decides where a report is recorded", in.Command, reason)})
	}
	state, err := childState(h.reports.socket)
	if err != nil {
		return h.explained("report_child", nil, fmt.Errorf("the relay daemon's state directory cannot be resolved: %w", err))
	}
	var stdout, stderr bytes.Buffer
	code, err := forwardChild(ctx, state, "crw", append([]string{in.Command}, in.Arguments...), &stdout, &stderr)
	if err != nil {
		return h.explained("report_child", nil, err)
	}
	if code != 0 {
		answer := strings.TrimSpace(stdout.String() + " " + stderr.String())
		return h.explained("report_child", nil, fmt.Errorf("the relay daemon did not record the child report (exit %d): %s", code, answer))
	}
	out := map[string]any{"exitCode": code, "stdout": stdout.String(), "stderr": stderr.String()}
	var report map[string]any
	if json.Unmarshal(stdout.Bytes(), &report) == nil {
		out["report"] = report
	}
	return h.explained("report_child", out, nil)
}
