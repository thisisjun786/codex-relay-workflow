package dagsched

import (
	"context"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
)

// Script the host reads and final guard instead of relying on the optional
// Lifecycle implemented by the older release fake.
type interruptedReleaseHost struct {
	managed.Adapter
	host   *scriptedHost
	status string
	turns  []string
}

func (h *interruptedReleaseHost) CreateThread(ctx context.Context, in managed.CreateThreadRequest) (map[string]any, error) {
	receipt, err := h.host.CreateThread(ctx, in)
	if err == nil && receipt["status"] == "accepted" {
		h.turns = append(h.turns, pyjson.Text(receipt["turnId"]))
	}
	return receipt, err
}

func (h *interruptedReleaseHost) ReadTurn(_ context.Context, _, turn string) (*managed.Turn, error) {
	return &managed.Turn{ID: turn, Status: h.status}, nil
}

func (h *interruptedReleaseHost) HostCall(_ context.Context, method string, params map[string]any) (map[string]any, error) {
	switch method {
	case "thread/list":
		data := []any{}
		if params["archived"] == false {
			data = append(data, map[string]any{"id": "child-1"})
		}
		return map[string]any{"data": data}, nil
	case "thread/goal/get":
		return map[string]any{"goal": nil}, nil
	case "thread/read":
		return map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true}}, nil
	}
	return map[string]any{}, nil
}

func (h *interruptedReleaseHost) SendMessage(ctx context.Context, in managed.SendRequest) (map[string]any, error) {
	if withheld, err := in.BeforeStart(ctx); err != nil || withheld != nil {
		return map[string]any{"status": "not_attempted"}, err
	}
	receipt, err := h.host.SendMessage(ctx, in)
	if err == nil && receipt["status"] == "accepted" {
		h.turns = append(h.turns, pyjson.Text(receipt["turnId"]))
	}
	return receipt, err
}

func TestReleaseInterruptedStandbyContinuesFrozenRequest(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	h := &interruptedReleaseHost{Adapter: k.host, host: k.host, status: "inProgress"}
	var requests []string
	k.sched.Start = func(ctx context.Context, raw []byte) (StartAnswer, error) {
		requests = append(requests, string(raw))
		engine := &managed.Start{Store: k.s, Adapter: h, Now: k.clock, Socket: k.sched.Selectors.Socket, MarkerRoot: k.marker, StateSelector: k.state,
			Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
		answer, err := engine.Run(ctx, raw)
		return answerOf(answer), err
	}
	first := k.mustRelease("rp", "A")
	if first.Bound || first.Reason != "standby_incomplete" || k.heldSlots() != 1 {
		t.Fatalf("initial standby: %+v", first)
	}
	h.status = "interrupted"
	for i := 0; i < 5; i++ {
		got := k.mustRelease("rp", "A")
		if !got.Bound || got.State != "admitted" || got.RequestID != first.RequestID || got.ManifestDigest != first.ManifestDigest || got.SlotID != first.SlotID {
			t.Fatalf("same dag-release after interruption (repeat %d): bound=%v state=%s reason=%s", i, got.Bound, got.State, got.Reason)
		}
	}
	if len(requests) != 2 || requests[0] != requests[1] {
		t.Fatal("the repeated release did not use its original frozen request")
	}
	if created, sent := k.host.counts(); created != 1 || sent != 1 || !reflect.DeepEqual(h.turns, []string{"standby", "business"}) {
		t.Fatalf("duplicated turns: %v created=%d sent=%d", h.turns, created, sent)
	}
	if rows := k.rows(); rows != (rowCounts{releases: 1, requests: 1, manifests: 1, executions: 1, slots: 1}) {
		t.Fatalf("duplicate release rows: %+v", rows)
	}
}
