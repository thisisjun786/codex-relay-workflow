package managed

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
)

// Expose only Adapter so readiness always goes through the production HostRPC seam.
type readinessApp struct {
	Adapter
	unarchived, archived int
	archiveChild         bool
	archiveMore          bool
	archiveReply         map[string]any
	readReply            map[string]any
	readErr, listErr     error
	goalStatus           any
	calls                []string
}

func (h *readinessApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	h.calls = append(h.calls, method)
	switch method {
	case "thread/list":
		if h.listErr != nil {
			return nil, h.listErr
		}
		archived := params["archived"] == true
		if archived && h.archiveReply != nil {
			return h.archiveReply, nil
		}
		count := h.unarchived
		if archived {
			count = h.archived
		}
		offset := 0
		if cursor, ok := params["cursor"].(string); ok {
			var err error
			offset, err = strconv.Atoi(cursor)
			if err != nil {
				return nil, err
			}
		}
		end := min(offset+50, count)
		data := []any{}
		for i := offset; i < end; i++ {
			id := fmt.Sprintf("unrelated-%d", i)
			if i == count-1 && (!archived || h.archiveChild) {
				id = "t-1"
			}
			data = append(data, map[string]any{"id": id})
		}
		page := map[string]any{"data": data}
		if end < count || archived && h.archiveMore {
			page["nextCursor"] = strconv.Itoa(end)
		}
		return page, nil
	case "thread/read":
		if h.readErr != nil {
			return nil, h.readErr
		}
		if h.readReply != nil {
			return h.readReply, nil
		}
	case "thread/goal/get":
		if h.goalStatus != nil {
			return map[string]any{"goal": map[string]any{"status": h.goalStatus}}, nil
		}
	}
	return h.Adapter.HostCall(ctx, method, params)
}

func TestHostReadySameRequestReadsUnlistedChild(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		count int
	}{
		{"omitted_preview", 0},
		{"beyond_200_unarchived", 251},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			k := newReconcileKit(t)
			h := &readinessApp{Adapter: k.host, unarchived: scenario.count}
			k.start.Adapter = h
			k.host.standby = "inProgress"
			first := k.run()
			k.expect(first, "incomplete", "standby_incomplete", "")
			k.host.standby = "interrupted"
			k.host.threads["t-1"].status = "notLoaded"
			guardStart := -1
			k.host.beforeSend = func(SendRequest) { guardStart = len(h.calls) }
			for repeat := 0; repeat < 2; repeat++ {
				got := k.run()
				if got["state"] != "admitted" || got["businessTurnId"] != "business" || got["childTaskId"] != first["childTaskId"] {
					t.Errorf("retry %d: want same child admitted/business, got %v/%v/%v", repeat, got["state"], got["stage"], got["reason"])
				}
			}
			k.effects(1, 1)
			if guardStart < 0 || !reflect.DeepEqual(h.calls[guardStart:][len(h.calls[guardStart:])-2:], []string{"thread/read", "thread/goal/get"}) {
				t.Fatalf("final guard did not use the direct-read fallback: %v", h.calls)
			}
			if !reflect.DeepEqual(k.host.threads["t-1"].turns, []string{"business"}) {
				t.Fatalf("business turn duplicated: %v", k.host.threads["t-1"].turns)
			}
		})
	}
}

func TestHostReadyArchiveBoundaryAndReadJudgment(t *testing.T) {
	for _, scenario := range []struct {
		name, code string
		configure  func(*readinessApp, *appThread)
	}{
		{"unknown", "lifecycle_unknown", func(h *readinessApp, _ *appThread) { h.readErr = errors.New("thread/read: thread not found") }},
		{"listed_then_unknown", "lifecycle_unknown", func(h *readinessApp, _ *appThread) {
			h.unarchived = 1
			h.readErr = errors.New("thread/read: thread not found")
		}},
		{"archived", "recipient_archived", func(h *readinessApp, _ *appThread) { h.archived = 1; h.archiveChild = true }},
		{"archive_over_200", "archived_listing_incomplete", func(h *readinessApp, _ *appThread) { h.archived = 201; h.unarchived = 1 }},
		{"archive_at_200_with_more", "archived_listing_incomplete", func(h *readinessApp, _ *appThread) { h.archived = 200; h.archiveMore = true; h.unarchived = 1 }},
		{"archive_at_200_complete", "", func(h *readinessApp, _ *appThread) { h.archived = 200 }},
		{"malformed_archive_data", "archived_listing_incomplete", func(h *readinessApp, _ *appThread) { h.archiveReply = map[string]any{"data": "unreadable"} }},
		{"malformed_archive_cursor", "archived_listing_incomplete", func(h *readinessApp, _ *appThread) { h.archiveReply = map[string]any{"data": []any{}, "nextCursor": 1} }},
		{"missing_thread", "lifecycle_unknown", func(h *readinessApp, _ *appThread) { h.readReply = map[string]any{} }},
		{"malformed_thread", "lifecycle_unknown", func(h *readinessApp, _ *appThread) { h.readReply = map[string]any{"thread": "unknown"} }},
		{"idle", "", func(_ *readinessApp, thread *appThread) { thread.status = "idle" }},
		{"not_loaded", "", func(_ *readinessApp, thread *appThread) { thread.status = "notLoaded" }},
		{"active", "recipient_not_idle", func(_ *readinessApp, thread *appThread) { thread.status = "active" }},
		{"cannot_accept", "recipient_cannot_accept_input", func(h *readinessApp, _ *appThread) {
			h.readReply = map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": false}}
		}},
		{"paused", "recipient_paused", func(h *readinessApp, _ *appThread) { h.goalStatus = "paused" }},
		{"usage_limited", "recipient_usage_limited", func(h *readinessApp, _ *appThread) { h.goalStatus = "usageLimited" }},
		{"budget_limited", "recipient_budget_limited", func(h *readinessApp, _ *appThread) { h.goalStatus = "budgetLimited" }},
		{"malformed_goal", "lifecycle_unknown", func(h *readinessApp, _ *appThread) { h.goalStatus = 1 }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			k := newReconcileKit(t)
			k.host.addThreadAs("t-1")
			h := &readinessApp{Adapter: k.host}
			scenario.configure(h, k.host.threads["t-1"])
			got, err := hostReady(context.Background(), h, "t-1")
			if err != nil || got != scenario.code {
				t.Fatalf("want %q, got %q err=%v calls=%v", scenario.code, got, err, h.calls)
			}
			if scenario.code == "archived_listing_incomplete" || scenario.code == "recipient_archived" {
				for _, method := range h.calls {
					if method != "thread/list" {
						t.Fatalf("archived decision fell through to %s", method)
					}
				}
			}
			if scenario.name == "unknown" && !reflect.DeepEqual(h.calls, []string{"thread/list", "thread/list", "thread/read"}) {
				t.Fatalf("unknown thread reached goal lookup: %v", h.calls)
			}
		})
	}
}

func TestHostReadyBudgetAndTransportErrors(t *testing.T) {
	k := newReconcileKit(t)
	k.host.addThreadAs("t-1")
	h := &readinessApp{Adapter: k.host, archived: 200, unarchived: 251}
	code, err := hostReady(context.Background(), h, "t-1")
	if err != nil || code != "" || len(h.calls) != 10 {
		t.Fatalf("want success in ten RPCs, got %q err=%v calls=%v", code, err, h.calls)
	}
	for _, method := range []string{"list", "read"} {
		t.Run(method, func(t *testing.T) {
			failure := errors.New("scripted transport unavailable")
			h := &readinessApp{Adapter: k.host}
			if method == "list" {
				h.listErr = failure
			} else {
				h.readErr = failure
			}
			if _, err := hostReady(context.Background(), h, "t-1"); !errors.Is(err, failure) {
				t.Fatalf("transport error lost: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h = &readinessApp{Adapter: k.host, readErr: errors.New("thread/read: thread not found")}
	if _, err := hostReady(ctx, h, "t-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation downgraded to unknown: %v", err)
	}
}

func TestHostReadyFinalGuardRechecksArchiveAndStatus(t *testing.T) {
	for _, code := range []string{"recipient_archived", "archived_listing_incomplete", "recipient_not_idle"} {
		t.Run(code, func(t *testing.T) {
			k := newReconcileKit(t)
			h := &readinessApp{Adapter: k.host}
			k.start.Adapter = h
			k.host.standby = "inProgress"
			k.expect(k.run(), "incomplete", "standby_incomplete", "")
			k.host.standby = "interrupted"
			k.host.threads["t-1"].status = "notLoaded"
			var verdict map[string]any
			k.host.beforeSend = func(in SendRequest) {
				switch code {
				case "recipient_archived":
					h.archived, h.archiveChild = 1, true
				case "archived_listing_incomplete":
					h.archived = 201
				case "recipient_not_idle":
					k.host.threads["t-1"].status = "active"
				}
				var err error
				verdict, err = in.BeforeStart(context.Background())
				if err != nil {
					t.Fatal(err)
				}
			}
			got := k.run()
			if got["reason"] != "business_failed" || verdict["code"] != code || verdict["message"] != "Managed turn withheld: "+code {
				t.Fatalf("guard did not recheck %s: receipt=%v verdict=%v", code, got, verdict)
			}
			k.effects(1, 0)
		})
	}
}
