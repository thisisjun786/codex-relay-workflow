package hook

import (
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// These are the CRW-778 cases for the memory gate: the state write that spends an authorization is the write argument
// memoryGateHandle already takes, so a write that published the new state and then failed the directory sync
// (state.Published) is a spent authorization, and the call it authorized goes ahead.

// publishedGateWrite publishes through state.WriteState and then reports the post-rename failure as *state.PublishedError,
// the shape a directory sync failure reaches the gate with.
func publishedGateWrite(cwd string, s state.State) error {
	if err := state.WriteState(cwd, s); err != nil {
		return err
	}
	return &state.PublishedError{Err: syscall.EIO}
}

// A grant whose consumption published is spent: the write it authorized is allowed, the grant is gone from the visible
// state, and the next call finds nothing to spend.
func TestMemoryGateAllowsAWriteWhoseGrantConsumptionPublished(t *testing.T) {
	cwd, _, env := gateScene(t)
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	if out := memoryGateHandle(gatePayload(t, cwd, nil), env, publishedGateWrite); out != "" {
		t.Errorf("a published consumption denied the write it authorized: %q", out)
	}
	if state.ReadState(cwd, gateSession).MemoryWriteGrant {
		t.Error("the grant is still in the published state")
	}
	if out := memoryGateHandle(gatePayload(t, cwd, nil), env, publishedGateWrite); out == "" {
		t.Error("the spent grant allowed a second write")
	}
}

// A failure before publication keeps today's behaviour: the authorization was not spent, so the write is denied and the
// grant stays for the retry.
func TestMemoryGateRefusesAWriteWhoseGrantWasNotPublished(t *testing.T) {
	cwd, _, env := gateScene(t)
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	failing := func(string, state.State) error { return syscall.EIO }
	reason := gateDeny(t, memoryGateHandle(gatePayload(t, cwd, nil), env, failing))
	if !strings.Contains(reason, "could not be spent") {
		t.Errorf("reason: %s", reason)
	}
	if !state.ReadState(cwd, gateSession).MemoryWriteGrant {
		t.Error("a pre-publication failure spent the grant")
	}
}
