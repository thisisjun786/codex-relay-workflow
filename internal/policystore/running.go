package policystore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/manage"
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
// of internal/relay, which are the same ones crw manage and relay doctor use, and the management
// command itself, which is the reader crw manage config is.
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

// manageRelay is the relay block crw manage config reports: the socket and the state directory,
// either of which may be empty.
type manageRelay struct {
	Socket string `json:"socket"`
	State  string `json:"state"`
}

// manageRelaySettings is the relay block crw manage config reports, read by running that command in
// this process through the package public entry point. The management command owns every judgement
// about the configuration file: which fields it must carry, which values it refuses and what the
// install layout defaults are. So this package reads no part of that file itself, and the relay the
// policy API observes is exactly the one the management command names.
//
// A non-zero exit is the command own refusal and its stderr is the reason, so the caller reports the
// running digest unavailable with that sentence rather than falling back to a relay the command
// never named. A report this reader cannot decode is refused for the same reason: fail closed rather
// than answer a relay this package inferred.
//
// The command reads the process environment itself (Run builds its Env with os.Getenv), so the
// environment this reader is handed is not what it runs with, and the parameter is unused. It stays
// in the signature because the seam type is shared with the stubs the tests in this package install,
// and this issue does not change them. config is a local file read with no work a context could
// cancel, so the call runs on a background context rather than the reading own.
func manageRelaySettings(_ func(string) string) (manageRelay, error) {
	var stdout, stderr bytes.Buffer
	if code := manage.Run(context.Background(), []string{"config"}, nil, &stdout, &stderr); code != 0 {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = fmt.Sprintf("crw manage config exited with status %d", code)
		}
		return manageRelay{}, errors.New(reason)
	}
	var report struct {
		Relay manageRelay `json:"relay"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return manageRelay{}, fmt.Errorf("crw manage config printed no readable report: %w", err)
	}
	return report.Relay, nil
}

// relaySettings is the socket and the configured state directory of the running service: exactly
// what crw manage config reports. The command fills the install layout defaults, so an absent file
// and a file that names no relay answer with the App Server default socket here as well.
//
// The guard below covers a report that names no socket at all, which the management command does
// not produce: it fills the App Server default itself. It is kept so a report that named nothing
// can never hand an empty socket to the state-directory resolver, and it never overrides a socket
// the command named.
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
//
// The relay it observes is the one crw manage config names, and that command reads this process own
// environment, so env does not decide which relay is observed. It stays in the signature because the
// GUI handler passes the process environment and the exported shape is not this issue to change, and
// relaySettings still reads it for the guard it describes.
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
