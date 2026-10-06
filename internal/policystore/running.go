package policystore

import (
	"context"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The two answers of a running-digest reading. Unavailable always carries a reason, so a reader
// can tell "the service holds the old bytes" from "the question could not be asked".
const (
	RunningObserved    = "observed"
	RunningUnavailable = "unavailable"
)

// Running is the running relay service's worker policy digest, or why it could not be read.
type Running struct {
	State  string
	Digest string
	Reason string
}

// runningSeams are the three host readers a test replaces. The production values are the exported
// readers of internal/relay, which are the same ones crw manage and relay doctor use.
var runningSeams = struct {
	scope      func() (*service.ScopeRegistry, error)
	observe    func(context.Context, store.StateSelection, string, string, *service.ScopeRegistry) service.Object
	executable func() (string, error)
	stateDir   func(string, string) (store.StateSelection, error)
	readManage func(func(string) string) (string, error)
}{
	scope:      service.ResolveScope,
	observe:    service.ObserveWorkerPolicy,
	executable: os.Executable,
	stateDir:   store.ResolveStateDir,
	readManage: manageRelaySocket,
}

// manageRelaySocket is the relay socket the management session's configuration names, read through
// crwconfig the way crw manage reads it. An absent or unusable file leaves "" and the caller falls
// back to the App Server's default socket.
func manageRelaySocket(getenv func(string) string) (string, error) {
	file, err := crwconfig.Load(getenv, "")
	if err != nil {
		return "", err
	}
	var manage struct {
		Relay struct {
			Socket string `json:"socket"`
		} `json:"relay"`
	}
	if err := file.Section("manage", &manage); err != nil {
		return "", err
	}
	return manage.Relay.Socket, nil
}

// relaySocket is the socket the running service is observed on: the management configuration's,
// else the App Server control socket below the Codex home.
func relaySocket(env LookupEnv) (string, error) {
	getenv := func(key string) string { value, _ := env(key); return value }
	named, _ := runningSeams.readManage(getenv)
	if named != "" {
		return named, nil
	}
	home, ok := codexHome(env)
	if !ok {
		return "", errNoCodexHome
	}
	return filepath.Join(home, "app-server-control", "app-server-control.sock"), nil
}

// RunningDigest reads the digest the running relay service's worker published. Every failure is a
// named state with a reason; it is never reported as the file's digest, and never as an empty
// success.
func RunningDigest(ctx context.Context, env LookupEnv) Running {
	socket, err := relaySocket(env)
	if err != nil {
		return Running{State: RunningUnavailable, Reason: err.Error()}
	}
	selection, err := runningSeams.stateDir("", socket)
	if err != nil {
		return Running{State: RunningUnavailable, Reason: "the relay state directory could not be resolved: " + err.Error()}
	}
	scope, err := runningSeams.scope()
	if err != nil {
		return Running{State: RunningUnavailable, Reason: "the relay scope could not be resolved: " + err.Error()}
	}
	executable, err := runningSeams.executable()
	if err != nil {
		return Running{State: RunningUnavailable, Reason: "this executable's directory could not be resolved: " + err.Error()}
	}
	observation := runningSeams.observe(ctx, selection, socket, filepath.Dir(executable), scope)
	if reason, _ := observation.Get("reason").(string); reason != "" {
		return Running{State: RunningUnavailable, Reason: reason}
	}
	policy, ok := observation.Get("policy").(service.Object)
	if !ok {
		return Running{State: RunningUnavailable, Reason: "worker_policy_unreadable"}
	}
	digest, _ := policy.Get("digest").(string)
	if digest == "" {
		return Running{State: RunningUnavailable, Reason: "worker_policy_unconfigured"}
	}
	return Running{State: RunningObserved, Digest: digest}
}

// Applied is the applied field of a read: whether the running service holds this file's bytes.
func Applied(file Reading, running Running) string {
	switch {
	case file.State != Registered:
		return AppliedUnverifiable
	case running.State != RunningObserved:
		return AppliedUnverifiable
	case running.Digest == file.Digest:
		return AppliedApplied
	default:
		return AppliedNeedsAction
	}
}

// AppliedActions is the work a caller must do for the applied state, or nil when none is needed.
func AppliedActions(applied string) []string {
	if applied == AppliedNeedsAction {
		return []string{AppliedActionRestart}
	}
	return nil
}
