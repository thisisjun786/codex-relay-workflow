package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// followingReader1602 observes a second read after the real Reader returned an
// empty writer-held page. It changes no page data; the writer commits before the
// test releases that second read.
type followingReader1602 struct {
	*flowcli.Reader
	calls    int
	writer   bool
	waiting  chan struct{}
	released chan struct{}
}

func (s *followingReader1602) Tail(ctx context.Context, run string, after, limit int, wait time.Duration) (flowcli.Page, error) {
	s.calls++
	if s.writer && s.calls == 2 {
		close(s.waiting)
		select {
		case <-s.released:
		case <-ctx.Done():
			return flowcli.Page{}, ctx.Err()
		}
	}
	return s.Reader.Tail(ctx, run, after, limit, wait)
}

func TestCRW1602SSEFollowsWriterBarrier(t *testing.T) {
	for _, state := range []string{flowcli.StateCompleted, flowcli.StateDead, flowcli.StateHeld, flowcli.StatePaused} {
		for _, writer := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/writer=%t", state, writer), func(t *testing.T) {
				stateDir := t.TempDir()
				st, err := store.Open(t.Context(), filepath.Join(stateDir, "relay.sqlite3"), "")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close() })
				const run = "run-sse-1602"
				j, err := journal.Open(t.Context(), st, run, journal.Identity{PlanID: "plan-1", PlanRevision: 3, ActorTaskID: "thread-owner", EpochNonce: "n"}, journal.Options{Immediate: true})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := j.Close(); err != nil {
						t.Error(err)
					}
				})
				j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
				if state == flowcli.StateHeld {
					for _, ev := range []string{engine.EvPrepared, engine.EvThreadRequested, engine.EvOutcomeUnknown} {
						j.Append(engine.Event{RunID: run, Kind: engine.KindCall, CallID: "c-held", Path: "root", Position: 1, Op: "agent", Label: "executor:A", Event: ev, Data: map[string]any{"role": "executor", "node": "A", "attempt": 1, "reason": engine.ReasonDispatchUnknown}})
					}
				}
				switch state {
				case flowcli.StateCompleted, flowcli.StateHeld:
					j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunCompleted, "error_kind": ""}})
				case flowcli.StatePaused:
					j.Append(engine.Event{RunID: run, Kind: engine.KindFlow, Event: engine.EvRunPaused, Data: map[string]any{"by": "operator", "reason": "crw flow pause"}})
					j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "paused", Data: map[string]any{"status": engine.RunPaused, "by": "operator", "reason": "crw flow pause"}})
				}
				if err := j.Err(); err != nil {
					t.Fatal(err)
				}
				rows := j.Events()
				cursor := rows[len(rows)-1].Seq
				var lock *flowcli.Lock
				if writer {
					lock, err = flowcli.TakeLock(stateDir, run, "audit", time.Now())
					if err != nil {
						t.Fatal(err)
					}
					defer lock.Release()
				}
				rd, err := flowcli.OpenReader(t.Context(), stateDir, "")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = rd.Close() })
				p, err := rd.Tail(t.Context(), run, cursor, 1, 0)
				if err != nil || p.State != state || p.Live != writer || len(p.Rows) != 0 || p.More {
					t.Fatalf("fixture page %+v, err %v; want %s, live %t, empty", p, err, state, writer)
				}
				src := &followingReader1602{Reader: rd, writer: writer, waiting: make(chan struct{}), released: make(chan struct{})}
				oldOpen, oldWait, oldLimit := runsOpen, runsEventWait, runsEventLimit
				runsOpen = func(context.Context) (runsSource, error) { return src, nil }
				runsEventWait, runsEventLimit = 0, 1
				t.Cleanup(func() { runsOpen, runsEventWait, runsEventLimit = oldOpen, oldWait, oldLimit })
				cctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				req := httptest.NewRequest("GET", "/api/runs/"+run+"/events", nil).WithContext(cctx)
				rec := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { defer close(done); streamRunEvents(rec, req, run, cursor) }()
				t.Cleanup(func() { cancel(); <-done })
				if writer {
					select {
					case <-src.waiting:
					case <-done:
						t.Fatalf("SSE ended before the writer committed its final rows: %s", rec.Body.String())
					case <-cctx.Done():
						t.Fatalf("SSE did not continue reading: %v", cctx.Err())
					}
					for _, title := range []string{"writer-first", "writer-last"} {
						j.Append(engine.Event{RunID: run, Kind: engine.KindFlow, Event: "phase", Data: map[string]any{"title": title}})
					}
					if err := j.Err(); err != nil {
						t.Fatal(err)
					}
					lock.Release()
					close(src.released)
				}
				select {
				case <-done:
				case <-cctx.Done():
					t.Fatalf("SSE did not end after unlock: %v", cctx.Err())
				}
				if cctx.Err() != nil {
					t.Fatalf("SSE was cancelled instead of ending: %v", cctx.Err())
				}
				text := rec.Body.String()
				got := readEvents(t, bufio.NewReader(strings.NewReader(text)), "row", -1)
				wantCursor := cursor
				if writer {
					wantCursor += 2
					if len(got) != 2 || got[0].ID != fmt.Sprint(cursor+1) || got[1].ID != fmt.Sprint(cursor+2) {
						t.Fatalf("SSE missed or repeated the final rows: %+v", got)
					}
					for i, title := range []string{"writer-first", "writer-last"} {
						var row flowcli.Row
						if err := json.Unmarshal([]byte(got[i].Data), &row); err != nil || row.Data["title"] != title {
							t.Errorf("row %d: %+v, err %v, want %s", i, row, err, title)
						}
					}
					if strings.LastIndex(text, "event: row") > strings.Index(text, "event: end") {
						t.Errorf("SSE ended before its final row: %s", text)
					}
				} else if len(got) != 0 || src.calls != 1 {
					t.Fatalf("unheld stopped run should end on its first empty page: rows %+v, reads %d", got, src.calls)
				}
				end := readEvents(t, bufio.NewReader(strings.NewReader(text)), "end", -1)
				if len(end) != 1 {
					t.Fatalf("want one end event: %s", text)
				}
				var final struct {
					State  string `json:"state"`
					Cursor int    `json:"cursor"`
				}
				if err := json.Unmarshal([]byte(end[0].Data), &final); err != nil || final.State != state || final.Cursor != wantCursor {
					t.Errorf("end %s: %v, want state %s cursor %d", end[0].Data, err, state, wantCursor)
				}
			})
		}
	}
}
