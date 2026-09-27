package faults

// subset ported for todo 22; todo 24 owns omitted.py.
// This is faultsweep.managed_readings and its omitted.observe call boundary. The
// projection owner supplies the observer; the sweep never duplicates its classifier.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
)

// ManagedReadingRequest is the complete omitted.observe selector set. Selection
// belongs to the observer (the daemon supplies its selected read-only store).
type ManagedReadingRequest struct {
	Selection  any    `json:"selection"`
	Root       string `json:"root"`
	Workspace  string `json:"workspace"`
	Assignment string `json:"assignment"`
	Session    string `json:"session"`
	Turn       string `json:"turn"`
	Now        string `json:"now"`
}

// ManagedReadingObserver is the read-only reporting projection owned by todo 24.
// An observer returns a reporting-observation/1 object or an error carrying its
// Python-compatible exception type and message (for example OSError: ...).
type ManagedReadingObserver interface {
	Observe(context.Context, ManagedReadingRequest) (any, error)
}

type ManagedReadingPage struct {
	Readings []any `json:"readings"`
	Gaps     []any `json:"gaps"`
	Cursor   any   `json:"cursor"`
	Filled   bool  `json:"filled"`
	Complete bool  `json:"complete"`
}

func (sw *Sweeper) ManagedReadings(ctx context.Context, selection any, observer ManagedReadingObserver, limit int, cursor any, now string) (ManagedReadingPage, error) {
	answer := ManagedReadingPage{Readings: []any{}, Gaps: []any{}}
	if limit < 1 {
		return answer, fmt.Errorf("fault_observation_malformed: limit is a positive integer, not %d", limit)
	}
	if limit > 1000 {
		limit = 1000
	}
	after, until, err := sw.rotation(ctx, cursor, "SELECT MAX(rowid) FROM assignment_settlements", true)
	if err != nil {
		return answer, err
	}
	rows := []row{}
	if until != nil {
		at, _ := after.(int64)
		rows, err = sw.Store.All(ctx, `SELECT s.rowid AS seq,s.relationship_id,s.thread_id,s.turn_id,m.request_id,m.marker_root,m.workspace,m.dispatch_request_id
 FROM managed_start_requests m CROSS JOIN assignment_settlements s
 JOIN relationships r ON r.relationship_id=m.relationship_id
 WHERE m.state='attached' AND s.relationship_id=m.relationship_id AND s.thread_id=m.child_task_id
 AND r.execution_generation=m.execution_generation AND r.superseded_by IS NULL
 AND (m.standby_turn_id IS NULL OR s.turn_id!=m.standby_turn_id)
 AND s.rowid>? AND s.rowid<=?
 AND NOT EXISTS (SELECT 1 FROM assignment_settlements e WHERE e.relationship_id=s.relationship_id AND e.thread_id=s.thread_id AND e.turn_id=s.turn_id AND e.rowid<s.rowid)
 ORDER BY s.rowid LIMIT ?`, at, until, limit)
		if err != nil {
			return answer, err
		}
	}
	if now == "" {
		now = sw.Now()
	}
	for _, r := range rows {
		sum := sha256.Sum256([]byte(text(r, "dispatch_request_id")))
		reading, err := observer.Observe(ctx, ManagedReadingRequest{Selection: selection, Root: text(r, "marker_root"), Workspace: text(r, "workspace"), Assignment: fmt.Sprintf("%x", sum), Session: text(r, "thread_id"), Turn: text(r, "turn_id"), Now: now})
		var reason string
		object, ok := reading.(map[string]any)
		if err != nil {
			reason = err.Error()
		} else if !ok || object == nil {
			reason = "the observer returned no reading"
		}
		if reason != "" {
			answer.Gaps = append(answer.Gaps, map[string]any{"gap": "managed_reading_failed", "relationId": r.Get("relationship_id"), "reason": reason})
			continue
		}
		relationship, _ := object["relationshipId"].(string)
		if strings.TrimSpace(relationship) == "" {
			object = copyMap(object)
			object["relationshipId"] = r.Get("relationship_id")
		}
		answer.Readings = append(answer.Readings, object)
	}
	answer.Filled = len(rows) >= limit
	answer.Complete = after == nil && !answer.Filled
	if answer.Filled {
		answer.Cursor = map[string]any{"at": rows[len(rows)-1].Get("seq"), "until": until}
	}
	return answer, nil
}
