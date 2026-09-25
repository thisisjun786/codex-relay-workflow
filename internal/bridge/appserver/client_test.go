package appserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func bounded(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCall_correlates_interleaved_responses(t *testing.T) {
	// Given: a real unix websocket where earlier requests answer later.
	host := fakehost.Start(t)
	host.Script("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": "slow"}, Delay: 80 * time.Millisecond}, fakehost.Reply{Result: map[string]any{"goal": "fast"}})
	ctx := bounded(t)
	client, err := appserver.Dial(ctx, host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var wg sync.WaitGroup
	results := make([]string, 2)
	errorsByCall := make([]error, 2)
	wg.Go(func() {
		var raw json.RawMessage
		raw, errorsByCall[0] = client.Call(ctx, "thread/goal/get", map[string]any{"threadId": "a"})
		if errorsByCall[0] == nil {
			results[0] = string(raw)
		}
	})
	wg.Go(func() {
		var raw json.RawMessage
		raw, errorsByCall[1] = client.Call(ctx, "thread/goal/get", map[string]any{"threadId": "b"})
		if errorsByCall[1] == nil {
			results[1] = string(raw)
		}
	})
	wg.Wait()
	for _, err := range errorsByCall {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, request := range host.Requests() {
		if request.Method != "thread/goal/get" {
			continue
		}
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(request.Params, &p); err != nil {
			t.Fatal(err)
		}
		want := `{"goal":"slow"}`
		if i == 3 {
			want = `{"goal":"fast"}`
		}
		index := 0
		if p.ThreadID == "b" {
			index = 1
		}
		if results[index] != want {
			t.Fatalf("crossed replies: %q", results)
		}
	}
}

func TestCall_returns_ack_timeout_without_retry(t *testing.T) {
	// Given: the host delays its acknowledgement beyond the phase bound.
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Delay: time.Second})
	client := appserver.New(host.SocketPath, appserver.PhaseBounds{Establish: time.Second, Transmit: time.Second, Ack: 20 * time.Millisecond})
	ctx := bounded(t)
	defer client.Close()
	// When
	_, err := client.Call(ctx, "thread/read", map[string]any{"threadId": "a"})
	// Then
	var timeout *appserver.PhaseTimeout
	if !errors.As(err, &timeout) || timeout.Phase != "ack" {
		t.Fatalf("want ack timeout: %v", err)
	}
	if got := host.Count("thread/read"); got != 1 {
		t.Fatalf("retried: %d", got)
	}
	// A stuck close handshake cannot extend the two-second close budget.
	started := time.Now()
	_ = client.Close()
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("close exceeded its 2s bound: %s", elapsed)
	}
}

func TestCall_rejects_oversized_frame(t *testing.T) {
	// Given
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{PadBytes: appserver.MaxFrameBytes + 1})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// When
	_, err = client.Call(bounded(t), "thread/read", map[string]any{})
	// Then
	var oversized *appserver.ResponseTooLarge
	if !errors.As(err, &oversized) || oversized.Limit != 16*1024*1024 || oversized.FrameBytes <= oversized.Limit {
		t.Fatalf("want 16MiB response limit: %v", err)
	}
	if got := host.Count("thread/read"); got != 1 {
		t.Fatalf("oversized request retried: %d", got)
	}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"ok": true}})
	response, err := client.Call(bounded(t), "thread/read", map[string]any{})
	if err != nil || string(response) != `{"ok":true}` {
		t.Fatalf("next read: %s, %v", response, err)
	}
}

func TestCall_leaves_approvals_and_refuses_other_requests(t *testing.T) {
	// Given
	host := fakehost.Start(t)
	methods := append(append([]string{}, fakehost.ApprovalMethods[:]...), "item/tool/call")
	host.Script("thread/read", fakehost.Reply{Result: map[string]any{"ok": true}, ServerRequests: methods}, fakehost.Reply{Result: map[string]any{"ok": true}})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	mark := client.RequestMark()
	// When
	_, err = client.Call(bounded(t), "thread/read", map[string]any{"threadId": "a"})
	if err != nil {
		t.Fatal(err)
	}
	// A subsequent round-trip fences delivery of the prior client response on the host.
	_, err = client.Call(bounded(t), "thread/read", map[string]any{"threadId": "b"})
	if err != nil {
		t.Fatal(err)
	}
	// Then
	report := client.RequestsSince(mark, "a")
	if report.ApprovalsLeftForThisThread != 7 || report.RefusedForThisThread != 1 {
		t.Fatalf("routing: %+v", report)
	}
	answers := host.Answers()
	if len(answers) != 1 {
		t.Fatalf("want one refusal, got %d", len(answers))
	}
	var answer struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answers[0].Raw, &answer); err != nil || answer.Error.Code != -32601 {
		t.Fatalf("wrong refusal: %s: %v", answers[0].Raw, err)
	}
	if refusals := client.RefusalsSince(mark, "a"); len(refusals) != 1 || refusals[0].Method != "item/tool/call" {
		t.Fatalf("compatibility refusal stream: %+v", refusals)
	}
}

func TestRefusalsSince_retains_refusal_after_approval_burst(t *testing.T) {
	// Given: a refusal followed by more than a ring's worth of unanswered approvals.
	host := fakehost.Start(t)
	methods := []string{"item/tool/call"}
	for range 67 {
		methods = append(methods, "execCommandApproval")
	}
	host.Respond("thread/read", fakehost.Reply{ServerRequests: methods})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	mark := client.RequestMark()
	// When
	if _, err := client.Call(bounded(t), "thread/read", map[string]any{"threadId": "a"}); err != nil {
		t.Fatal(err)
	}
	// Then: approvals evict only the shared history, not the separate refusal ring.
	refusals := client.RefusalsSince(mark, "a")
	if len(refusals) != 1 || refusals[0].Method != "item/tool/call" {
		t.Fatalf("lost refusal: %+v", refusals)
	}
	if report := client.RequestsSince(mark, "a"); report.NotRetained != 4 {
		t.Fatalf("shared ring did not overflow: %+v", report)
	}
}

func TestRequestsSince_reports_evictions(t *testing.T) {
	// Given
	host := fakehost.Start(t)
	methods := make([]string, 69)
	for i := range methods {
		methods[i] = "execCommandApproval"
	}
	host.Respond("thread/read", fakehost.Reply{ServerRequests: methods})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	mark := client.RequestMark()
	// When
	_, err = client.Call(bounded(t), "thread/read", map[string]any{"threadId": "a"})
	if err != nil {
		t.Fatal(err)
	}
	// Then
	report := client.RequestsSince(mark, "a")
	if len(report.ThisThread) != 64 || report.NotRetained != 5 {
		t.Fatalf("bad ring: %+v", report)
	}
}
