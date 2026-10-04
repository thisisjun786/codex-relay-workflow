package childcleanup

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func TestIncompleteReleaseAndRunningPrecedence(t *testing.T) {
	for _, mode := range []string{"root-interrupted", "root-failed", "root-empty", "root-unlistable", "root-notLoaded", "sub-failed", "sub-unreadable", "sub-running", "mixed-running", "unresolved-running"} {
		t.Run(mode, func(t *testing.T) {
			root := thread{id: "child", loaded: true, rollout: true}
			if mode == "root-notLoaded" {
				root.loaded, root.status = false, "notLoaded"
			}
			s := newScripted(t, root)
			statuses := map[string]string{}
			switch mode {
			case "root-interrupted":
				statuses["child"] = "interrupted"
			case "root-failed", "mixed-running":
				statuses["child"] = "failed"
			case "root-empty":
				statuses["child"] = ""
			}
			if mode[:3] == "sub" || mode == "mixed-running" || mode == "unresolved-running" {
				s.byID["sub"] = &thread{id: "sub", parent: "child", status: "idle", loaded: true, rollout: true, unreadable: mode == "sub-unreadable"}
				statuses["sub"] = "failed"
			}
			running := mode == "sub-running" || mode == "mixed-running" || mode == "unresolved-running"
			if running {
				statuses["sub"] = "inProgress"
			}
			if mode == "unresolved-running" {
				s.byID["unknown"] = &thread{id: "unknown", loaded: true, unreadable: true}
			}
			completedHistory(s, statuses)
			if mode == "root-unlistable" {
				s.srv.Handle("thread/turns/list", func(json.RawMessage) fakehost.Reply { return failure(-32601, "unlistable") })
			}
			s.srv.Respond("thread/unsubscribe", fakehost.Reply{})
			c := s.client(t)
			ConfigureSubscriptions(c)
			w, err := c.WatchTurn(context.Background(), "child")
			if err != nil {
				t.Fatal(err)
			}
			w.Finish("last", false)
			s.srv.Respond("probe/end", fakehost.Reply{Before: []fakehost.Notification{{Method: "turn/completed", Params: map[string]any{"threadId": "child", "turn": map[string]any{"id": "last"}}}}})
			if _, err = c.Call(context.Background(), "probe/end", nil); err != nil {
				t.Fatal(err)
			}
			bound := time.Second // Below the first five-second hold retry.
			if running {
				bound = 300 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), bound)
			if err = s.srv.WaitCount(ctx, "thread/turns/list", 1); err != nil {
				t.Fatal(err)
			}
			err = s.srv.WaitCount(ctx, "thread/unsubscribe", 1)
			cancel()
			if running {
				if err == nil || len(s.archived()) != 0 {
					t.Fatal("running subtree released")
				}
				report, err := Clean(context.Background(), c, "child", Options{DescendantsOnly: true})
				if err != nil || report.Complete() || report.Items[0].Outcome != OutcomeHeldActive {
					t.Fatalf("running precedence: %+v %v", report, err)
				}
				completedHistory(s, nil)
				// The five-second ticker can miss a due time just after its first tick.
				ctx, cancel = context.WithTimeout(context.Background(), 12*time.Second)
				defer cancel()
				if err = s.srv.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
					t.Fatal(err)
				}
				if mode != "unresolved-running" && len(s.archived()) != 1 {
					t.Fatalf("completed descendant not archived: %v", s.archived())
				}
			} else if err != nil || len(s.archived()) != 0 {
				t.Fatalf("non-running root pinned: %v archives=%v", err, s.archived())
			}
			if mode == "root-notLoaded" {
				r, err := Clean(context.Background(), c, "child", Options{DescendantsOnly: true})
				if err != nil || r.Complete() || outcomes(r) != "child:held_incomplete" {
					t.Fatalf("missing root anchor: %+v %v", r, err)
				}
			}
		})
	}
}

func TestUnprovedHistoryStillReadsActiveStatus(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		s := newScripted(t, thread{id: "child", loaded: true, status: "active", rollout: true})
		s.srv.Handle("thread/turns/list", func(json.RawMessage) fakehost.Reply {
			if malformed {
				return fakehost.Reply{Result: map[string]any{"data": "malformed"}}
			}
			return failure(-32601, "unlistable")
		})
		r, err := run(t, s, Options{DescendantsOnly: true})
		if err != nil || r.Complete() || outcomes(r) != "child:held_active" || s.srv.Count("thread/read") != 1 || len(s.archived()) != 0 {
			t.Fatalf("active status skipped: %+v %v reads=%d", r, err, s.srv.Count("thread/read"))
		}
	}
}
