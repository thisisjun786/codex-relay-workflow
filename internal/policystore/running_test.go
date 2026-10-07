package policystore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// stubRunningSeams installs the host readers a running-digest reading uses and returns a pointer to
// the socket the resolver was called with. The configuration reader is left for the caller to set.
// Every reader a reading touches is stubbed, so no test opens a real relay state directory.
func stubRunningSeams(t *testing.T, digest string) *string {
	t.Helper()
	original := runningSeams
	t.Cleanup(func() { runningSeams = original })
	seen := new(string)
	runningSeams.scope = func() (*service.ScopeRegistry, error) {
		return &service.ScopeRegistry{Root: "/tmp/scope", Authority: "isolated"}, nil
	}
	runningSeams.executable = func() (string, error) { return "/usr/local/bin/crw", nil }
	runningSeams.stateDir = func(_, socket string) (store.StateSelection, error) {
		*seen = socket
		return store.StateSelection{Path: "/tmp/state"}, nil
	}
	runningSeams.observe = func(context.Context, store.StateSelection, string, string, *service.ScopeRegistry) service.Object {
		return service.Object{{Key: "policy", Value: service.Object{{Key: "digest", Value: digest}}}}
	}
	return seen
}

// TestRunningDigestReportsAnUnreadableManageConfig is C1: a management configuration that is there
// but cannot be read is a named failure, never the default relay's answer. Reporting the default
// socket's digest here would say the file bytes are applied when the question could not be asked.
func TestRunningDigestReportsAnUnreadableManageConfig(t *testing.T) {
	seen := stubRunningSeams(t, "same-digest")
	configError := errors.New("the configuration is not a JSON object")
	runningSeams.readManage = func(func(string) string) (manageRelay, error) { return manageRelay{}, configError }

	env := envOf(map[string]string{"HOME": "/tmp/home", "CODEX_HOME": "/tmp/home/.codex"})
	running := RunningDigest(context.Background(), env)
	if running.State != RunningUnavailable {
		t.Fatalf("state = %q, want %q", running.State, RunningUnavailable)
	}
	if running.Digest != "" {
		t.Fatalf("an unreadable configuration still answered a digest: %q", running.Digest)
	}
	if !strings.Contains(running.Reason, configError.Error()) {
		t.Fatalf("reason %q does not name the configuration failure", running.Reason)
	}
	if *seen != "" {
		t.Fatalf("the default socket %q was used although the configuration could not be read", *seen)
	}
	// The whole point: bytes the running service happens to agree with are not applied when the
	// question could not be asked.
	file := Reading{State: Registered, Digest: "same-digest", RegisteredDigest: "same-digest"}
	if got := Applied(file, running); got != AppliedUnverifiable {
		t.Fatalf("applied = %q, want %q", got, AppliedUnverifiable)
	}
}

// TestRunningDigestUsesTheDefaultSocketOnlyWithoutAFile is C1's other half: an absent configuration
// file is the one case that falls back to the App Server's default socket.
func TestRunningDigestUsesTheDefaultSocketOnlyWithoutAFile(t *testing.T) {
	seen := stubRunningSeams(t, "d")
	runningSeams.readManage = func(func(string) string) (manageRelay, error) { return manageRelay{}, nil }

	env := envOf(map[string]string{"HOME": "/tmp/home", "CODEX_HOME": "/tmp/home/.codex"})
	running := RunningDigest(context.Background(), env)
	if running.State != RunningObserved || running.Digest != "d" {
		t.Fatalf("reading = %+v", running)
	}
	if want := "/tmp/home/.codex/app-server-control/app-server-control.sock"; *seen != want {
		t.Fatalf("socket = %q, want the App Server default %q", *seen, want)
	}
}

// TestRunningDigestUsesTheConfiguredSocketWhenTheFileNamesOne is C1: a configuration that reads
// keeps naming the socket, so the fallback above is not reached on a configured host.
func TestRunningDigestUsesTheConfiguredSocketWhenTheFileNamesOne(t *testing.T) {
	seen := stubRunningSeams(t, "d")
	runningSeams.readManage = func(func(string) string) (manageRelay, error) {
		return manageRelay{Socket: "/srv/app.sock", State: "/srv/relay"}, nil
	}

	env := envOf(map[string]string{"HOME": "/tmp/home", "CODEX_HOME": "/tmp/home/.codex"})
	if running := RunningDigest(context.Background(), env); running.State != RunningObserved {
		t.Fatalf("reading = %+v", running)
	}
	if *seen != "/srv/app.sock" {
		t.Fatalf("socket = %q, want the configured one", *seen)
	}
}
