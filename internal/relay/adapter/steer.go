package adapter

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// ActiveTurn exposes the bridge's guarded-turn observation to the supervisor.
func (a *Adapter) ActiveTurn(ctx context.Context, thread string) (map[string]any, error) {
	if a.bridge == nil {
		return nil, &HostUnavailable{"this adapter has no steering transport"}
	}
	ctx, release, err := a.admitRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return a.bridge.ActiveTurn(ctx, thread)
}

// SteerMessage uses the same ledger and recipient exclusion as SendMessage. It
// never resumes a thread or changes its settings, and never falls back to start.
func (a *Adapter) SteerMessage(ctx context.Context, requestID, thread, turn, message string) (delivery.Obj, error) {
	if a.transport == nil {
		return nil, &HostUnavailable{"this adapter has no steering transport"}
	}
	t := a.transport
	run, release, ok := t.admit(ctx, false)
	if !ok {
		return nil, ErrTransportClosing
	}
	defer release()
	run, cancel := context.WithTimeout(run, a.timeout*3)
	defer cancel()
	t.mu.Lock()
	retained, err := a.ledger.Lookup(run, requestID, "steer_thread", map[string]any{"threadId": thread, "expectedTurnId": turn, "message": message}, nil)
	if err != nil {
		t.mu.Unlock()
		return nil, err
	}
	if retained != nil && retained["status"] != "not_attempted" {
		t.mu.Unlock()
		return a.receipt(run, requestID, true)
	}
	if t.recipients[thread] {
		t.mu.Unlock()
		return delivery.Obj{{Key: "status", Value: "failed"}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "thread_busy"}}}}, nil
	}
	t.recipients[thread] = true
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.recipients, thread)
		t.mu.Unlock()
	}()
	_, err = a.bridge.SteerThread(run, requestID, thread, turn, message)
	if err != nil {
		return nil, err
	}
	return a.receipt(context.WithoutCancel(run), requestID, false)
}
