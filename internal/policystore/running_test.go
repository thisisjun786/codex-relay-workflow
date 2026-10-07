package policystore

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/manage"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// stubRunningSeams installs the host readers a running-digest reading uses and returns the socket
// and the state directory the resolver was called with. The configuration reader is left at its
// production value, so the reading under test runs the real management command. Every other reader
// is stubbed, so no test opens a real relay state directory.
func stubRunningSeams(t *testing.T, digest string) (socket, state *string) {
	t.Helper()
	original := runningSeams
	t.Cleanup(func() { runningSeams = original })
	socket, state = new(string), new(string)
	runningSeams.scope = func() (*service.ScopeRegistry, error) {
		return &service.ScopeRegistry{Root: "/tmp/scope", Authority: "isolated"}, nil
	}
	runningSeams.executable = func() (string, error) { return "/usr/local/bin/crw", nil }
	runningSeams.stateDir = func(configuredState, configuredSocket string) (store.StateSelection, error) {
		*state, *socket = configuredState, configuredSocket
		return store.StateSelection{Path: "/tmp/state"}, nil
	}
	runningSeams.observe = func(context.Context, store.StateSelection, string, string, *service.ScopeRegistry) service.Object {
		return service.Object{{Key: "policy", Value: service.Object{{Key: "digest", Value: digest}}}}
	}
	return socket, state
}

// manageConfigHost writes the management configuration file into a temporary home and points both
// the process environment (which the management command reads) and the LookupEnv the reading takes
// at it. An empty body leaves the file absent, which is the "no file" case. HOME, CODEX_HOME,
// CRW_CONFIG and the XDG locations are all temporary, so no test reads the operator own
// configuration or state. It returns the home and the LookupEnv.
func manageConfigHost(t *testing.T, body string) (string, func(string) (string, bool)) {
	t.Helper()
	root := t.TempDir()
	codexHome := filepath.Join(root, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "crw-config.json")
	if body != "" {
		if err := os.WriteFile(config, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	values := map[string]string{
		"HOME": root, "CODEX_HOME": codexHome, "CRW_CONFIG": config,
		"XDG_CONFIG_HOME": filepath.Join(root, "config"), "XDG_STATE_HOME": filepath.Join(root, "state"),
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	return root, envOf(values)
}

// defaultSocket is the App Server socket the management command fills when the file names no relay.
func defaultSocket(root string) string {
	return filepath.Join(root, ".codex", "app-server-control", "app-server-control.sock")
}

// manageConfigRelay runs crw manage config the way the reading does and returns the relay block it
// reports, so an assertion is against the command own answer rather than a value this test
// re-derived.
func manageConfigRelay(t *testing.T) manageRelay {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := manage.Run(context.Background(), []string{"config"}, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("crw manage config exited %d: %s", code, stderr.String())
	}
	var report struct {
		Relay manageRelay `json:"relay"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("crw manage config report %q: %v", stdout.String(), err)
	}
	return report.Relay
}

// TestRunningDigestRefusesAConfigurationTheManageCommandRefuses is C2, and the issue red cases: a
// configuration crw manage config refuses must make the running digest unavailable with that
// reason, never the default relay digest. The reader at the base looked only at manage.relay, so it
// answered the default relay digest for a configuration the management command refuses.
func TestRunningDigestRefusesAConfigurationTheManageCommandRefuses(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"a field the management command refuses", "{\"manage\":{\"parents\":[]}}\n", "manage.parents"},
		{"another field of the wrong shape", "{\"manage\":{\"settings\":\"x\"}}\n", "manage.settings"},
		{"a section that is not an object", "{\"manage\":{\"bridge\":\"x\"}}\n", "manage.bridge"},
		{"a relay that is not an object", "{\"manage\":{\"relay\":\"x\"}}\n", "manage.relay"},
		{"a manage key that is not an object", "{\"manage\": null}\n", "manage"},
		{"a file that is not JSON", "{\"manage\":\n", "configuration"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			socket, state := stubRunningSeams(t, "same-digest")
			_, env := manageConfigHost(t, c.body)
			running := RunningDigest(context.Background(), env)
			if running.State != RunningUnavailable {
				t.Fatalf("state = %q, want %q", running.State, RunningUnavailable)
			}
			if running.Digest != "" {
				t.Fatalf("a refused configuration still answered a digest: %q", running.Digest)
			}
			if !strings.Contains(running.Reason, c.want) {
				t.Fatalf("reason %q does not name %q", running.Reason, c.want)
			}
			if *socket != "" || *state != "" {
				t.Fatalf("the relay %q/%q was used although the configuration was refused", *state, *socket)
			}
			// The whole point: bytes the running service happens to agree with are not applied when
			// the management command refused the configuration that names the relay.
			file := Reading{State: Registered, Digest: "same-digest", RegisteredDigest: "same-digest"}
			if got := Applied(file, running); got != AppliedUnverifiable {
				t.Fatalf("applied = %q, want %q", got, AppliedUnverifiable)
			}
		})
	}
}

// TestRunningDigestUsesTheRelayTheManageConfigNames is C1: the socket and the state directory are
// exactly the ones crw manage config reports.
func TestRunningDigestUsesTheRelayTheManageConfigNames(t *testing.T) {
	socket, state := stubRunningSeams(t, "d")
	relaySocket := filepath.Join(t.TempDir(), "app.sock")
	relayState := t.TempDir()
	body := "{\"manage\":{\"relay\":{\"socket\":\"" + relaySocket + "\",\"state\":\"" + relayState + "\"}}}\n"
	_, env := manageConfigHost(t, body)
	if running := RunningDigest(context.Background(), env); running.State != RunningObserved {
		t.Fatalf("reading = %+v", running)
	}
	if *socket != relaySocket {
		t.Fatalf("socket = %q, want the configured one %q", *socket, relaySocket)
	}
	if *state != relayState {
		t.Fatalf("state = %q, want the configured one %q", *state, relayState)
	}
	reported := manageConfigRelay(t)
	if *socket != reported.Socket || *state != reported.State {
		t.Fatalf("observed %q/%q, but crw manage config reports %q/%q", *state, *socket, reported.State, reported.Socket)
	}
}

// TestRunningDigestUsesTheDefaultSocketTheManageConfigFills is C1 for every configuration the
// management command accepts without naming a relay: the socket is the App Server default the
// command itself fills, compared with the command own report.
func TestRunningDigestUsesTheDefaultSocketTheManageConfigFills(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"a file that is not there", ""},
		{"an empty manage object", "{\"manage\":{}}\n"},
		{"a file naming no manage section", "{\"paths\":{}}\n"},
		{"a relay naming no socket", "{\"manage\":{\"relay\":{\"state\":\"/tmp/relay\"}}}\n"},
		{"a relay naming an empty socket", "{\"manage\":{\"relay\":{\"socket\":\"\"}}}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			socket, _ := stubRunningSeams(t, "d")
			root, env := manageConfigHost(t, c.body)
			running := RunningDigest(context.Background(), env)
			if running.State != RunningObserved || running.Digest != "d" {
				t.Fatalf("reading = %+v", running)
			}
			if want := defaultSocket(root); *socket != want {
				t.Fatalf("socket = %q, want the App Server default %q", *socket, want)
			}
			if reported := manageConfigRelay(t).Socket; *socket != reported {
				t.Fatalf("socket = %q, but crw manage config reports %q", *socket, reported)
			}
		})
	}
}

// TestRunningDigestReportsTheObservationFailure is C2 for the other half of the reading: the relay
// was established and the observation of the running service failed. The reason is the
// observation's own, never a digest, and the relay the resolver was called with is the one the
// management command named.
func TestRunningDigestReportsTheObservationFailure(t *testing.T) {
	socket, state := stubRunningSeams(t, "d")
	runningSeams.observe = func(context.Context, store.StateSelection, string, string, *service.ScopeRegistry) service.Object {
		return service.Object{{Key: "reason", Value: "worker_policy_unreadable"}}
	}
	runningSeams.readManage = func(func(string) string) (manageRelay, error) {
		return manageRelay{Socket: "/srv/app.sock", State: "/srv/relay"}, nil
	}
	env := envOf(map[string]string{"HOME": "/tmp/home", "CODEX_HOME": "/tmp/home/.codex"})
	running := RunningDigest(context.Background(), env)
	if running.State != RunningUnavailable || running.Reason != "worker_policy_unreadable" || running.Digest != "" {
		t.Fatalf("reading = %+v", running)
	}
	if *socket != "/srv/app.sock" || *state != "/srv/relay" {
		t.Fatalf("resolver got %q/%q, want the relay the management command named", *state, *socket)
	}
}
