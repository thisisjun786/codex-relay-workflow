package role

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// dispatchHostDialTimeout bounds the connection the dispatch CLI opens. A socket that accepts a
// connection but never finishes the App Server handshake must not hold the CLI, and a socket that
// nobody listens on must fail fast; either way the command runs without a host.
const dispatchHostDialTimeout = 2 * time.Second

// The two reads the created check makes, and the only methods this host forwards.
const (
	dispatchHostThreadRead = "thread/read"
	dispatchHostTurnsList  = "thread/turns/list"
)

// dispatchHost is the read-only App Server host the dispatch CLI passes to CheckedDispatch. It
// reuses the bridge's client (internal/bridge/appserver) and opens no creation path: the CLI's
// stopped close can read the child's newest turn, and nothing here can create, start or steer one.
type dispatchHost struct {
	client *appserver.Client
}

// dispatchHostSocket is the control socket the bridge and the relay default to:
// <CODEX_HOME>/app-server-control/app-server-control.sock, with CODEX_HOME when it is set and
// non-empty, else ~/.codex (internal/relay/store DefaultSocket and internal/bridge/mcp Defaults).
func dispatchHostSocket(env host.LookupEnv) (string, error) {
	if env == nil {
		env = os.LookupEnv
	}
	codexHome, _ := env("CODEX_HOME")
	if codexHome == "" {
		home, err := host.Home(env)
		if err != nil {
			return "", err
		}
		codexHome = filepath.Join(home, ".codex")
	}
	return filepath.Join(codexHome, "app-server-control", "app-server-control.sock"), nil
}

// dispatchHostOpen opens the read-only host for one dispatch command and returns the close the
// caller defers. Any failure to resolve or dial the socket returns a nil host and a no-op close, so
// the command keeps the native-database path it has when no host is passed.
func dispatchHostOpen(ctx context.Context, env host.LookupEnv) (DispatchHost, func()) {
	dialCtx, cancel := context.WithTimeout(ctx, dispatchHostDialTimeout)
	defer cancel()
	socket, err := dispatchHostSocket(env)
	if err != nil {
		return nil, func() {}
	}
	client, err := appserver.Dial(dialCtx, socket)
	if err != nil {
		return nil, func() {}
	}
	open := &dispatchHost{client: client}
	return open, func() { _ = open.client.Close() }
}

// Call forwards only the two reads the created check makes and refuses everything else, so a caller
// of this host cannot reach a creation, start or steer path. thread/read is always asked without
// turns and thread/turns/list with one entry.
func (h *dispatchHost) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	switch method {
	case dispatchHostThreadRead:
		if value, ok := params["includeTurns"]; !ok || value != false {
			return nil, fmt.Errorf("dispatch host refuses %s without includeTurns false", method)
		}
	case dispatchHostTurnsList:
		if !dispatchHostSingleTurn(params["limit"]) {
			return nil, fmt.Errorf("dispatch host refuses %s without limit 1", method)
		}
	default:
		return nil, fmt.Errorf("dispatch host refuses method %s", method)
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
