package bridge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// Every refusal a caller can provoke at an argument boundary, byte for byte with Python.
func Test_round3_argument_refusals_read_exactly_as_python(t *testing.T) {
	refusals := object(pythonRound3(t)["refusals"])
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}}}})
	host.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{}}})
	host.Respond("thread/list", fakehost.Reply{Result: map[string]any{"data": []any{}}})
	pair := func(extra map[string]any) SendMessage {
		expected := map[string]any{"model": "explicit-model", "reasoning_effort": "high"}
		for k, v := range extra {
			expected[k] = v
		}
		return SendMessage{RequestID: "s", ThreadID: "thread-1", Message: "hi", Expected: expected}
	}
	send := func(extra map[string]any) error {
		_, err := b.SendMessageToThread(context.Background(), pair(extra))
		return err
	}
	ww := func(roots any) map[string]any {
		return map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": roots, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
	}
	cases := map[string]func() error{
		"read_limit_0":        func() error { _, err := b.ReadThread(context.Background(), "thread-1", 0, nil, 4000); return err },
		"read_limit_101":      func() error { _, err := b.ReadThread(context.Background(), "thread-1", 101, nil, 4000); return err },
		"read_chars_99":       func() error { _, err := b.ReadThread(context.Background(), "thread-1", 20, nil, 99); return err },
		"read_chars_20001":    func() error { _, err := b.ReadThread(context.Background(), "thread-1", 20, nil, 20001); return err },
		"list_limit_0":        func() error { _, err := b.ListThreads(context.Background(), "", 0, nil); return err },
		"list_limit_101":      func() error { _, err := b.ListThreads(context.Background(), "", 101, nil); return err },
		"wait_-0.001":         func() error { _, err := b.WaitThread(context.Background(), "thread-1", "turn-1", -1e6); return err },
		"wait_50.001":         func() error { _, err := b.WaitThread(context.Background(), "thread-1", "turn-1", 50001e6); return err },
		"send_roots_relative": func() error { return send(map[string]any{"runtime_workspace_roots": []string{"relative"}}) },
		"send_sandbox_bogus":  func() error { return send(map[string]any{"sandbox": "bogus"}) },
		"send_cwd_blank":      func() error { return send(map[string]any{"cwd": " "}) },
		"send_policy_without_type": func() error {
			return send(map[string]any{"expected_sandbox_policy": map[string]any{"no_type": 1}})
		},
		"create_roots_relative": func() error {
			input := createInput(cwd, "cr")
			input.Roots = []string{"relative"}
			_, err := b.CreateThread(context.Background(), input)
			return err
		},
		"create_policy_relative_root": func() error {
			input := createInput(cwd, "cp")
			input.Sandbox, input.Policy = "workspace-write", ww([]any{"rel"})
			_, err := b.CreateThread(context.Background(), input)
			return err
		},
	}
	worktree := func(change func(*CreateWorktree)) func() error {
		return func() error {
			input := worktreeInput(t)
			input.RequestID = "wt-" + t.Name()
			change(&input)
			receipt, err := b.CreateWorktreeThread(context.Background(), input)
			if err == nil {
				return &Invalid{"no refusal: " + text(receipt["status"]) + " " + text(receipt["error"])}
			}
			return err
		}
	}
	flagged := ww([]any{"rel"})
	flagged["excludeSlashTmp"] = "no"
	for name, change := range map[string]func(*CreateWorktree){
		"wt_bogus_type":                  func(in *CreateWorktree) { in.Policy = map[string]any{"type": "bogus"} },
		"wt_flag_before_roots":           func(in *CreateWorktree) { in.Sandbox, in.Policy = "workspace-write", flagged },
		"wt_roots_relative":              func(in *CreateWorktree) { in.Sandbox, in.Policy = "workspace-write", ww([]any{"rel"}) },
		"wt_roots_not_list":              func(in *CreateWorktree) { in.Sandbox, in.Policy = "workspace-write", ww("rel") },
		"wt_roots_not_string":            func(in *CreateWorktree) { in.Sandbox, in.Policy = "workspace-write", ww([]any{7}) },
		"wt_mode_disagrees":              func(in *CreateWorktree) { in.Sandbox = "workspace-write" },
		"wt_blank_prompt":                func(in *CreateWorktree) { in.Prompt = " " },
		"wt_blank_title":                 func(in *CreateWorktree) { in.Title = " " },
		"wt_blank_app_server_project_id": func(in *CreateWorktree) { in.ProjectID = " " },
		"wt_blank_source_repository":     func(in *CreateWorktree) { in.Source = " " },
	} {
		cases[name] = worktree(change)
	}
	names := []string{}
	for name := range cases {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			want, ok := refusals[name].(string)
			if !ok {
				t.Fatalf("no Python refusal recorded for %s", name)
			}
			if err := cases[name](); err == nil || err.Error() != want {
				t.Fatalf("refusal\n got: %v\nwant: %s", err, want)
			}
		})
	}
	if methods := hostMethods(host); slices.ContainsFunc(methods, func(m string) bool {
		return strings.HasPrefix(m, "thread/start") || m == "turn/start" || m == "thread/resume"
	}) {
		t.Fatalf("a refused request reached the host: %v", methods)
	}
}

// The boundary values themselves are accepted, so the refusals above are the bounds and not a
// blanket rejection.
func Test_round3_limit_boundaries_are_inclusive(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "completed", "items": []any{}}}}})
	host.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{}}})
	host.Respond("thread/list", fakehost.Reply{Result: map[string]any{"data": []any{}}})
	for _, pair := range [][2]int{{1, 4000}, {100, 4000}, {20, 100}, {20, 20000}} {
		if _, err := b.ReadThread(context.Background(), "thread-1", pair[0], nil, pair[1]); err != nil {
			t.Fatalf("read %v: %v", pair, err)
		}
	}
	for _, limit := range []int{1, 100} {
		if _, err := b.ListThreads(context.Background(), "", limit, nil); err != nil {
			t.Fatalf("list %d: %v", limit, err)
		}
	}
	for _, seconds := range []int64{0, 50} {
		if _, err := b.WaitThread(context.Background(), "thread-1", "turn-1", time.Duration(seconds)*time.Second); err != nil {
			t.Fatalf("wait %d: %v", seconds, err)
		}
	}
}
