// Package dispatchhost opens the read-only App Server host the role helper dispatch CLI uses for
// its stopped close. It is separate from package role because role is imported by offline relay
// packages (internal/relay/faults reaches it through internal/pabcd/hook) and must not pull a
// network client into them; the binary installs this opener into role.OpenDispatchHost instead.
package dispatchhost

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// dispatchHostDialTimeout bounds the connection the dispatch CLI opens. A socket that accepts a
// connection but never finishes the App Server handshake must not hold the CLI, and a socket that
// nobody listens on must fail fast; either way the command runs without a host.
const dispatchHostDialTimeout = 2 * time.Second

// Open is role.OpenDispatchHost: it dials the App Server control socket read-only and returns the
// host the stopped close reads through, with the close the caller defers. A socket that cannot be
// resolved or dialled returns an error and no host, and the CLI then passes nil.
func Open(env host.LookupEnv) (role.DispatchHost, func(), error) {
	socket, err := role.DispatchSocket(env)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatchHostDialTimeout)
	defer cancel()
	client, err := appserver.Dial(ctx, socket)
	if err != nil {
		return nil, nil, err
	}
	open := &dispatchHost{client: client}
	return open, func() { _ = open.client.Close() }, nil
}

// dispatchHost forwards the two reads the stopped close makes and refuses everything else, so a
// caller of this host cannot reach a creation, start or steer path. thread/read is always asked
// without turns and thread/turns/list with one entry.
type dispatchHost struct {
	client *appserver.Client
}

func (h *dispatchHost) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	switch method {
	case role.DispatchHostThreadRead:
		if value, ok := params["includeTurns"]; !ok || value != false {
			return nil, errors.New("dispatch host refuses " + method + " without includeTurns false")
		}
	case role.DispatchHostTurnsList:
		if !dispatchHostSingleTurn(params["limit"]) {
			return nil, errors.New("dispatch host refuses " + method + " without limit 1")
		}
	default:
		return nil, errors.New("dispatch host refuses method " + method)
	}
	return h.client.Call(ctx, method, params)
}

// dispatchHostSingleTurn reports whether a thread/turns/list limit is exactly one.
func dispatchHostSingleTurn(limit any) bool {
	switch value := limit.(type) {
	case int:
		return value == 1
	case int64:
		return value == 1
	case float64:
		return value == 1
	}
	return false
}
