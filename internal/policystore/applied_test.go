package policystore

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestAppliedIsUnverifiableWithoutARunningDigest is C2: a running digest that cannot be read is
// unverifiable with a reason, never applied.
func TestAppliedIsUnverifiableWithoutARunningDigest(t *testing.T) {
	file := Reading{State: Registered, Digest: "a", RegisteredDigest: "a"}
	running := Running{State: RunningUnavailable, Reason: "worker_policy_unreadable"}
	if got := Applied(file, running); got != AppliedUnverifiable {
		t.Fatalf("applied = %q, want %q", got, AppliedUnverifiable)
	}
}

// TestAppliedIsAppliedWhenTheServiceHoldsTheRegisteredBytes is C1.
func TestAppliedIsAppliedWhenTheServiceHoldsTheRegisteredBytes(t *testing.T) {
	file := Reading{State: Registered, Digest: "a", RegisteredDigest: "a"}
	running := Running{State: RunningObserved, Digest: "a"}
	if got := Applied(file, running); got != AppliedApplied {
		t.Fatalf("applied = %q, want %q", got, AppliedApplied)
	}
	if actions := AppliedActions(file, AppliedApplied); len(actions) != 0 {
		t.Fatalf("actions = %v, want none", actions)
	}
}

// TestAppliedNeedsARestartWhenTheServiceHoldsTheOldBytes is C1.
func TestAppliedNeedsARestartWhenTheServiceHoldsTheOldBytes(t *testing.T) {
	file := Reading{State: Registered, Digest: "b", RegisteredDigest: "b"}
	running := Running{State: RunningObserved, Digest: "a"}
	if got := Applied(file, running); got != AppliedNeedsAction {
		t.Fatalf("applied = %q, want %q", got, AppliedNeedsAction)
	}
	actions := AppliedActions(file, AppliedNeedsAction)
	if len(actions) != 1 || actions[0] != AppliedActionRestart {
		t.Fatalf("actions = %v, want the restart", actions)
	}
}

// TestAppliedIsNeverAppliedForUnregisteredBytes is the review finding: bytes the wiring record does
// not name are refused by the bridge launcher, so they are not applied however well the service
// agrees with them.
func TestAppliedIsNeverAppliedForUnregisteredBytes(t *testing.T) {
	file := Reading{State: Registered, Digest: "b", RegisteredDigest: "a"}
	running := Running{State: RunningObserved, Digest: "b"}
	if got := Applied(file, running); got != AppliedNeedsAction {
		t.Fatalf("applied = %q, want %q", got, AppliedNeedsAction)
	}
	actions := AppliedActions(file, AppliedNeedsAction)
	if len(actions) != 1 || actions[0] != AppliedActionReregister {
		t.Fatalf("actions = %v, want the registration repair", actions)
	}
}

// TestRunningDigestNamesItsFailures is C2: every way the reading can fail is a named state with a
// reason, and none of them is a digest.
func TestRunningDigestNamesItsFailures(t *testing.T) {
	original := runningSeams
	t.Cleanup(func() { runningSeams = original })
	runningSeams.scope = func() (*service.ScopeRegistry, error) {
		return &service.ScopeRegistry{Root: "/tmp/scope", Authority: "isolated"}, nil
	}
	runningSeams.executable = func() (string, error) { return "/usr/local/bin/crw", nil }
	runningSeams.stateDir = func(string, string) (store.StateSelection, error) {
		return store.StateSelection{Path: "/tmp/state"}, nil
	}
	runningSeams.readManage = func(func(string) string) (manageRelay, error) { return manageRelay{}, nil }
	runningSeams.observe = func(context.Context, store.StateSelection, string, string, *service.ScopeRegistry) service.Object {
		return service.Object{{Key: "observed", Value: false}, {Key: "reason", Value: "worker_policy_unreadable"}, {Key: "policy", Value: nil}}
	}
	env := envOf(map[string]string{"HOME": "/tmp/home", "CODEX_HOME": "/tmp/home/.codex"})
	reading := RunningDigest(context.Background(), env)
	if reading.State != RunningUnavailable || reading.Reason == "" || reading.Digest != "" {
		t.Fatalf("reading = %+v", reading)
	}
	// A configured state directory is passed to the resolver rather than a discovered one.
	seen := ""
	runningSeams.readManage = func(func(string) string) (manageRelay, error) {
		return manageRelay{Socket: "/tmp/app.sock", State: "/srv/relay"}, nil
	}
	runningSeams.stateDir = func(state, socket string) (store.StateSelection, error) {
		seen = state + "|" + socket
		return store.StateSelection{Path: "/srv/relay"}, nil
	}
	RunningDigest(context.Background(), env)
	if seen != "/srv/relay|/tmp/app.sock" {
		t.Fatalf("the resolver was called with %q, want the configured state and socket", seen)
	}
}
