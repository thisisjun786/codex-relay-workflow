package policystore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The two answers of a running-digest reading. Unavailable always carries a reason, so a reader can
// tell "the service holds the old bytes" from "the question could not be asked".
const (
	RunningObserved    = "observed"
	RunningUnavailable = "unavailable"
)

// Running is the running relay service worker policy digest, or why it could not be read.
type Running struct {
	State  string
	Digest string
	Reason string
}

// runningSeams are the host readers a test replaces. The production values are the exported readers
// of internal/relay, which are the same ones crw manage and relay doctor use.
var runningSeams = struct {
	scope      func() (*service.ScopeRegistry, error)
	observe    func(context.Context, store.StateSelection, string, string, *service.ScopeRegistry) service.Object
	executable func() (string, error)
	stateDir   func(string, string) (store.StateSelection, error)
	readManage func(func(string) string) (manageRelay, error)
}{
	scope:      service.ResolveScope,
	observe:    service.ObserveWorkerPolicy,
	executable: os.Executable,
	stateDir:   store.ResolveStateDir,
	readManage: manageRelaySettings,
}

// manageRelay is the relay block of the management session configuration: the socket and the state
// directory, either of which may be empty.
type manageRelay struct {
	Socket string `json:"socket"`
	State  string `json:"state"`
}

// manageRelaySettings is the relay block the management session configuration names, read through
// crwconfig the way crw manage reads it. An absent file leaves both empty and the caller falls back
// to the App Server default socket; a file that is there but cannot be read or is not a JSON object
// is an error, because which relay to observe could not be established and the default relay is not
// the answer to that question. A manage key that is there but is not an object is the same kind of
// error: the management reader refuses it, so reading it as "names nothing" would answer about the
// default relay for a host whose intended relay was never established.
func manageRelaySettings(getenv func(string) string) (manageRelay, error) {
	file, err := crwconfig.Load(getenv, "")
	if err != nil {
		return manageRelay{}, err
	}
	// The section is read raw first, because json.Unmarshal reads a JSON null into a struct without
	// an error and would leave the zero value indistinguishable from an absent key. The distinction
	// is the one coreManageSection makes: an absent key names nothing, and anything else that is not
	// an object is refused.
	var raw json.RawMessage
	if err := file.Section("manage", &raw); err != nil {
		return manageRelay{}, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return manageRelay{}, nil
	}
	if trimmed[0] != '{' {
		return manageRelay{}, errors.New("the manage section is not a JSON object")
	}
	manage := map[string]json.RawMessage{}
	if err := json.Unmarshal(trimmed, &manage); err != nil {
		return manageRelay{}, fmt.Errorf("the manage section is not a JSON object: %w", err)
	}
	relay := manageRelay{}
	if raw, present := manage["relay"]; present && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &relay); err != nil {
			return manageRelay{}, fmt.Errorf("manage.relay is not an object: %w", err)
		}
	}
	return relay, nil
}

// relaySettings is the socket and the configured state directory of the running service.
func relaySettings(env LookupEnv) (string, string, error) {
	getenv := func(key string) string { value, _ := env(key); return value }
	configured, err := runningSeams.readManage(getenv)
	if err != nil {
		return "", "", fmt.Errorf("the management configuration could not be read: %w", err)
	}
	socket := configured.Socket
	if socket == "" {
		home, ok := codexHome(env)
		if !ok {
			return "", "", errNoCodexHome
		}
		socket = filepath.Join(home, "app-server-control", "app-server-control.sock")
	}
	return socket, configured.State, nil
}

// RunningDigest reads the digest the running relay service worker published. Every failure is a
// named state with a reason; it is never reported as the file digest, and never as an empty
// success.
func RunningDigest(ctx context.Context, env LookupEnv) Running {
	socket, state, err := relaySettings(env)
	if err != nil {
		return Running{State: RunningUnavailable, Reason: err.Error()}
	}
	selection, err := runningSeams.stateDir(state, socket)
	if err != nil {
		return Running{State: RunningUnavailable, Reason: "the relay state directory could not be resolved: " + err.Error()}
	}
	scope, err := runningSeams.scope()
	if err != nil {
		return Running{State: RunningUnavailable, Reason: "the relay scope could not be resolved: " + err.Error()}
	}
	installation, err := installationDirectory()
	if err != nil {
		return Running{State: RunningUnavailable, Reason: err.Error()}
	}
	observation := runningSeams.observe(ctx, selection, socket, installation, scope)
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

// installationDirectory is this executable directory with its symbolic links resolved, which is
// the spelling the running service records in its installation id (service.InstallationID resolves
// the executable before hashing it). Without that resolution a crw started through the installer
// pointer would compute a different id and never match the worker it is observing.
func installationDirectory() (string, error) {
	executable, err := runningSeams.executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return filepath.Dir(executable), nil
}

// Applied is the applied field of a read: whether the policy this host enforces is the file bytes,
// and whether the running service has loaded them.
//
// A file whose bytes no longer match the digest the wiring record names is NOT applied, however
// well the running service agrees with it: the bridge launcher refuses those bytes against the
// record, so a bridge started from them would not run. That case is a registration repair, and it
// is reported as needs_user_action with the repair named.
func Applied(file Reading, running Running) string {
	switch {
	case file.State != Registered:
		return AppliedUnverifiable
	case file.RegisteredDigest != "" && file.Digest != "" && file.RegisteredDigest != file.Digest:
		return AppliedNeedsAction
	case running.State != RunningObserved:
		return AppliedUnverifiable
	case running.Digest == file.Digest:
		return AppliedApplied
	default:
		return AppliedNeedsAction
	}
}

// AppliedActions is the work a caller must do for the applied state, or nil when none is needed.
func AppliedActions(file Reading, applied string) []string {
	switch {
	case applied == AppliedNeedsAction && file.RegisteredDigest != "" && file.Digest != "" && file.RegisteredDigest != file.Digest:
		return []string{AppliedActionReregister}
	case applied == AppliedNeedsAction:
		return []string{AppliedActionRestart}
	}
	return nil
}
