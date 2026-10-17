package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// followPages1602 observes the actual JSON pages watch prints, without sleeping
// or reading a bytes.Buffer while watch is still writing it.
type followPages1602 struct {
	ctx   context.Context
	pages chan Page
}

func (w *followPages1602) Write(b []byte) (int, error) {
	var p Page
	if err := json.Unmarshal(b, &p); err != nil {
		return 0, err
	}
	select {
	case w.pages <- p:
		return len(b), nil
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

func TestCRW1602WatchFollowsWriterBarrier(t *testing.T) {
	for _, state := range []string{StateCompleted, StateDead, StateHeld, StatePaused} {
		for _, writer := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/writer=%t", state, writer), func(t *testing.T) {
				w := newWorld(t)
				r := w.newRun("run-follow-1602", "thread-owner")
				t.Cleanup(r.close)
				switch state {
				case StateCompleted:
					r.finish(engine.RunCompleted, "")
				case StateHeld:
					r.hold("c-held", "executor", "A")
					r.finish(engine.RunCompleted, "")
				case StatePaused:
					r.endPaused()
				}
				rows := r.j.Events()
				cursor := rows[len(rows)-1].Seq
				var lock *Lock
				if writer {
					lock = holdLock(t, w, r.id, "audit")
					defer lock.Release()
				}
				cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				out := &followPages1602{ctx: cctx, pages: make(chan Page, 16)}
				var errOut bytes.Buffer
				var code int
				done := make(chan struct{})
				td := w.deps()
				go func() {
					defer close(done)
					defer close(out.pages)
					code = RunWith(cctx, td.Deps, []string{"watch", r.id, "--after", itoa(cursor), "--limit", "1", "--follow", "--json", "--wait", "1", "--state", w.state}, strings.NewReader(""), out, &errOut)
				}()
				t.Cleanup(func() { cancel(); <-done })
				var pages []Page
				if writer {
					// The second empty page proves watch read again while the
					// writer still held the barrier, rather than ending on the first.
					for i := 0; i < 2; i++ {
						select {
						case p, ok := <-out.pages:
							if !ok {
								t.Fatal("watch ended before the writer committed its final rows")
							}
							if len(p.Rows) != 0 || p.More || !p.Live || p.State != state || p.Cursor != cursor {
								t.Fatalf("writer-held empty page: %+v, want state %s, live, cursor %d", p, state, cursor)
							}
							pages = append(pages, p)
						case <-cctx.Done():
							t.Fatalf("watch did not keep reading the writer-held run: %v", cctx.Err())
						}
					}
					r.phase("writer-first")
					r.phase("writer-last")
					if err := r.j.Err(); err != nil {
						t.Fatal(err)
					}
					lock.Release()
				}
				select {
				case <-done:
				case <-cctx.Done():
					t.Fatalf("watch did not end after the barrier was released: %v", cctx.Err())
				}
				if code != 0 || cctx.Err() != nil {
					t.Fatalf("watch exit %d, context %v, stderr %s", code, cctx.Err(), errOut.String())
				}
				for p := range out.pages {
					pages = append(pages, p)
				}
				var got []Row
				for _, p := range pages {
					got = append(got, p.Rows...)
				}
				wantCursor := cursor
				if writer {
					wantCursor += 2
					if len(got) != 2 || got[0].Seq != cursor+1 || got[1].Seq != cursor+2 || got[0].Data["title"] != "writer-first" || got[1].Data["title"] != "writer-last" {
						t.Fatalf("watch missed or repeated the writer's final rows: %+v", got)
					}
				} else if len(got) != 0 || len(pages) != 1 {
					t.Fatalf("an unheld stopped run should end on its first empty page: %+v", pages)
				}
				if len(pages) == 0 {
					t.Fatal("watch printed no final page")
				}
				last := pages[len(pages)-1]
				if len(last.Rows) != 0 || last.More || last.Live || last.State != state || last.Cursor != wantCursor {
					t.Errorf("final page %+v, want empty, not live, state %s, cursor %d", last, state, wantCursor)
				}
			})
		}
	}
}
