package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file is the host side of the reconcile pass (engine.ReconHost): the reads that find a creation or a turn on the host
// by the fields the 0.154 host returns. Every list is read to the end of its cursor, every turn in full; a read that cannot be
// completed is an error, never an empty answer.

// maxReconcilePages bounds the pages of one listing.
const maxReconcilePages = 500

// reconcilePageSize is the page size reconcile asks for.
const reconcilePageSize = 100

// threadWire is a Thread as thread/list and thread/read give it.
type threadWire struct {
	ID              string          `json:"id"`
	Cwd             string          `json:"cwd"`
	Ephemeral       bool            `json:"ephemeral"`
	Model           *string         `json:"model"`
	ReasoningEffort *string         `json:"reasoningEffort"`
	Source          json.RawMessage `json:"source"`
	ParentThreadID  *string         `json:"parentThreadId"`
	ForkedFromID    *string         `json:"forkedFromId"`
	CreatedAt       int64           `json:"createdAt"`
	Status          struct {
		Type string `json:"type"`
	} `json:"status"`
}

func (w threadWire) host() engine.HostThread {
	t := engine.HostThread{ID: w.ID, Cwd: w.Cwd, Ephemeral: w.Ephemeral, Status: w.Status.Type, CreatedAt: time.Unix(w.CreatedAt, 0)}
	// the source is a name for a plain kind and an object for a custom or sub-agent one
	var name string
	if json.Unmarshal(w.Source, &name) == nil {
		t.Source = name
	} else {
		t.Source = string(w.Source)
	}
	if w.ParentThreadID != nil {
		t.ParentThreadID = *w.ParentThreadID
	}
	if w.ForkedFromID != nil {
		t.ForkedFromID = *w.ForkedFromID
	}
	if w.Model != nil {
		t.Pair.Model = *w.Model
	}
	if w.ReasoningEffort != nil {
		t.Pair.Effort = *w.ReasoningEffort
	}
	return t
}

// ListThreads implements engine.ReconHost: thread/list filtered by the exact cwd and the backend's source kind, read to
// the end of the cursor. Threads of another source are dropped.
func (b *Backend) ListThreads(ctx context.Context, cwd string) ([]engine.HostThread, error) {
	var out []engine.HostThread
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxReconcilePages {
			return nil, fmt.Errorf("thread/list: more than %d pages", maxReconcilePages)
		}
		params := map[string]any{"cwd": cwd, "sourceKinds": []string{b.cfg.Source}, "limit": reconcilePageSize}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := b.client.Call(ctx, "thread/list", params)
		if err != nil {
			return nil, err
		}
		var decoded struct {
			Data       []threadWire `json:"data"`
			NextCursor *string      `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("thread/list: %w", err)
		}
		for _, w := range decoded.Data {
			if t := w.host(); t.Source == b.cfg.Source {
				out = append(out, t)
			}
		}
		if decoded.NextCursor == nil || *decoded.NextCursor == "" {
			return out, nil
		}
		cursor = *decoded.NextCursor
	}
}

// ReadThread implements engine.ReconHost: thread/read without turns.
func (b *Backend) ReadThread(ctx context.Context, id string) (engine.HostThread, error) {
	raw, err := b.client.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
	if err != nil {
		return engine.HostThread{}, err
	}
	var decoded struct {
		Thread threadWire `json:"thread"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Thread.ID == "" {
		return engine.HostThread{}, errors.New("thread/read: the answer names no thread")
	}
	return decoded.Thread.host(), nil
}

// Turns implements engine.ReconHost: thread/turns/list with itemsView "full", oldest first, every page. A turn the list did
// not return in full is read with thread/items/list; one that cannot be read is reported as not readable.
func (b *Backend) Turns(ctx context.Context, thread string) ([]engine.HostTurn, error) {
	type turnWire struct {
		ID        string            `json:"id"`
		Status    string            `json:"status"`
		ItemsView string            `json:"itemsView"`
		Items     []json.RawMessage `json:"items"`
	}
	var wires []turnWire
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxReconcilePages {
			return nil, fmt.Errorf("thread/turns/list: more than %d pages", maxReconcilePages)
		}
		params := map[string]any{"threadId": thread, "itemsView": "full", "sortDirection": "asc", "limit": reconcilePageSize}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := b.client.Call(ctx, "thread/turns/list", params)
		if err != nil {
			return nil, err
		}
		var decoded struct {
			Data       []turnWire `json:"data"`
			NextCursor *string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("thread/turns/list: %w", err)
		}
		wires = append(wires, decoded.Data...)
		if decoded.NextCursor == nil || *decoded.NextCursor == "" {
			break
		}
		cursor = *decoded.NextCursor
	}
	out := make([]engine.HostTurn, len(wires))
	var partial []string
	for i, w := range wires {
		out[i] = engine.HostTurn{ID: w.ID, Status: w.Status}
		if w.ItemsView == "full" || w.ItemsView == "" {
			out[i].Readable = true
			out[i].Marker = markerOf(w.Items)
		} else {
			partial = append(partial, w.ID)
		}
	}
	if len(partial) > 0 {
		readings, err := b.client.Bus().ReconcileTurns(ctx, thread, partial)
		if err != nil {
			return nil, err
		}
		for _, r := range readings {
			for i := range out {
				if out[i].ID != r.TurnID {
					continue
				}
				switch {
				case r.Readable:
					out[i].Readable = true
					out[i].Marker = markerOf(r.Items)
				case r.ReadError != nil:
					out[i].ReadError = r.ReadError
				default:
					out[i].ReadError = errors.New("the turn's items are not available")
				}
			}
		}
	}
	return out, nil
}

// markerOf is the operation a turn's items belong to ("" when they name none).
func markerOf(items []json.RawMessage) string {
	op, _, _ := MarkerOf(items)
	return op
}

// ArchiveThread implements engine.ReconHost.
func (b *Backend) ArchiveThread(ctx context.Context, id string) error {
	_, err := b.client.Call(ctx, "thread/archive", map[string]any{"threadId": id})
	return err
}

// InterruptTurn implements engine.ReconHost.
func (b *Backend) InterruptTurn(ctx context.Context, thread, turn string) error {
	_, err := b.client.Call(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	return err
}

// HarvestTurn implements engine.ReconHost: the turn read by its id, the way RunTurn would have ended it.
func (b *Backend) HarvestTurn(ctx context.Context, thread, turn, op string) (engine.TurnOutcome, bool, error) {
	out, err := b.ReadTurn(ctx, thread, turn, op)
	if errors.Is(err, ErrTurnInProgress) {
		return engine.TurnOutcome{}, true, nil
	}
	return out, false, err
}

// AwaitTurn implements engine.ReconHost: it follows the turn of a thread another process started to its end, on the
// bus and by reading it, and interrupts it if ctx ends first, the way a turn this process started is followed.
func (b *Backend) AwaitTurn(ctx context.Context, thread, turn, op string) (engine.TurnOutcome, error) {
	_ = b.hold(ctx, thread)
	feed := b.client.Bus().Subscribe(thread)
	defer feed.Close()
	w := &turnWatch{b: b, spec: engine.TurnSpec{ThreadID: thread, Op: op}, turnID: turn, feed: feed}
	return w.await(ctx)
}
