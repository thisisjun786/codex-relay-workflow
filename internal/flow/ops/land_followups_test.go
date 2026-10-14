package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1397: the follow-ups of the gate-only land (CRW-1354).

// TestAnEndedLandAsksAgainAfterATransientClose (item 1): only the relay's refusal to close an execution whose head may have landed
// (ErrCloseConflict) is the row's close_refused. Any other close error is asked again in place: a close that succeeds on a later try
// closes the execution, and one that keeps failing is the op's error, so the land does not end with its execution current under a run
// that has ended; the escalation row is written once.
func TestAnEndedLandAsksAgainAfterATransientClose(t *testing.T) {
	w := newWorld(t)
	busy := errors.New("database is locked")
	w.relay.closeErrs = []error{busy, busy, nil}
	w.must("escalate", "n1", EscalateInfo{Reason: "land_review_needs_changes", EndLand: true})
	row := w.j.Events()[len(w.j.Events())-1]
	if len(w.relay.closes) != 3 || row.Data["closed"] != true || row.Data["close_refused"] != nil {
		t.Fatalf("a close that failed twice and then closed: %d closes, row %v", len(w.relay.closes), row.Data)
	}

	w.relay.closes = nil
	w.relay.closeErr = busy
	if _, err := w.op("escalate", "n2", EscalateInfo{Reason: "land_verification_failed", EndLand: true}); err == nil || !strings.Contains(err.Error(), busy.Error()) {
		t.Fatalf("a close that keeps failing is the op's error, got %v", err)
	}
	if len(w.relay.closes) < 2 {
		t.Errorf("a failing close was asked %d times, want it asked again", len(w.relay.closes))
	}
	escalations := 0
	for _, ev := range w.j.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == engine.EvEscalation && dataStr(ev, "node") == "n2" {
			escalations++
		}
	}
	if escalations != 1 {
		t.Errorf("%d escalation rows of n2, want 1", escalations)
	}

	w.relay.closes = nil
	w.relay.closeErr = fmt.Errorf("%w: disposition_conflict: node n3 landed", ErrCloseConflict)
	w.must("escalate", "n3", EscalateInfo{Reason: "integration_refused", EndLand: true})
	row = w.j.Events()[len(w.j.Events())-1]
	if len(w.relay.closes) != 1 || row.Data["closed"] != false || !strings.Contains(fmt.Sprint(row.Data["close_refused"]), "landed") {
		t.Errorf("a close the relay refuses for a head that may have landed: %d closes, row %v", len(w.relay.closes), row.Data)
	}
}

// configThreads is a host whose threads read back the pair as the host spells it.
type configThreads struct{ cfg map[string][2]string }

func (configThreads) ReadTurns(context.Context, string, []string) ([]appserver.TurnReading, error) {
	return nil, nil
}
func (configThreads) ArchiveThread(context.Context, string) error { return nil }
func (h configThreads) ThreadConfig(_ context.Context, id string) (*string, *string, error) {
	c, ok := h.cfg[id]
	if !ok {
		return nil, nil, errors.New("thread not found: " + id)
	}
	return &c[0], &c[1], nil
}

// TestReadAuthorsTrimsThePair (item 9): the pair the host reads back is trimmed, so a model or an effort with blanks around it is the
// pair it names (and not a malformed receipt of the relay's release), and a model of blanks only is no model.
func TestReadAuthorsTrimsThePair(t *testing.T) {
	w := newWorld(t)
	w.lane.d.Threads = configThreads{cfg: map[string][2]string{"t1": {" claude-fake-sonnet \n", "\thigh "}, "t2": {"  ", "high"}}}
	authors, warnings := w.lane.readAuthors(context.Background(), []string{"t1", "t2"})
	if len(authors) != 2 || authors[0].Model != "claude-fake-sonnet" || authors[0].Effort != "high" || !authors[0].Observed {
		t.Fatalf("the authors read back: %+v", authors)
	}
	if authors[1].Observed || authors[1].Reason != landAuthorNullModel || len(warnings) != 1 {
		t.Fatalf("a model of blanks only: %+v, warnings %q", authors[1], warnings)
	}
}
