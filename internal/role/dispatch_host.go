package role

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// OpenDispatchHost opens the read-only App Server host the dispatch CLI uses for a stopped close.
// It is nil by default and the binary installs an implementation (internal/role/dispatchhost)
// before role.CLI runs. Package role names no network client: it is imported by offline relay
// packages, so the dialing lives in the injected opener and never here. A nil opener means the CLI
// runs without a host, exactly as it did before the host existed.
var OpenDispatchHost func(env host.LookupEnv) (DispatchHost, func(), error)

// dispatchHostSocket is the control socket the bridge and the relay default to:
// <CODEX_HOME>/app-server-control/app-server-control.sock, with CODEX_HOME when it is set and
// non-empty, else ~/.codex (internal/relay/store DefaultSocket and internal/bridge/mcp Defaults).
// It is exported to the opener through DispatchSocket, which names no client either.
func dispatchHostSocket(env host.LookupEnv) (string, error) {
	if env == nil {
		env = os.LookupEnv
	}
	codexHome, _ := env("CODEX_HOME")
	if codexHome == "" {
		home, err := dispatchHostHome(env)
		if err != nil {
			return "", err
		}
		codexHome = strings.TrimSuffix(home, "/") + "/.codex"
	}
	return filepath.Join(codexHome, "app-server-control", "app-server-control.sock"), nil
}

// DispatchSocket is dispatchHostSocket for the injected opener, which lives in another package and
// therefore cannot reach an unexported helper.
func DispatchSocket(env host.LookupEnv) (string, error) { return dispatchHostSocket(env) }

// dispatchHostHome is Path.home() as the bridge and the relay read it (internal/bridge/mcp Home and
// internal/relay/store ownership.UserHome): HOME whenever it is set, with trailing slashes dropped
// and an empty home or one of slashes being the root, else this user's passwd entry.
func dispatchHostHome(env host.LookupEnv) (string, error) {
	home, set := env("HOME")
	if !set {
		return host.Home(env)
	}
	if home = strings.TrimRight(home, "/"); home == "" {
		return "/", nil
	}
	return home, nil
}

// The two reads the created check makes, and the only methods the injected host may forward. They
// are declared here, next to the interface the opener satisfies, so the allowlist has one owner.
const (
	DispatchHostThreadRead = "thread/read"
	DispatchHostTurnsList  = "thread/turns/list"
)
