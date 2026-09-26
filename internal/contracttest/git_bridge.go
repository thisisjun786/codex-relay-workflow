package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

type gitBridgeRun struct {
	t                         *testing.T
	root, repo, base          string
	args                      bridge.CreateWorktree
	given                     map[string]any
	host                      *fakehost.Server
	client                    *appserver.Client
	store                     *ledger.Ledger
	b                         *bridge.Bridge
	receipts                  []any
	launchCalls               []any
	traces                    []any
	snapshots                 []any
	beforeStatus, beforeIndex string
	threads                   map[string]any
	calls                     []any
}

func runGitBridge(t *testing.T, scenario Scenario, root, repo, base string) (map[string]any, error) {
	t.Helper()
	r := &gitBridgeRun{t: t, root: root, repo: repo, base: base, given: scenario.Given, receipts: []any{}, launchCalls: []any{}, traces: []any{}, snapshots: []any{}, threads: map[string]any{}}
	r.args = bridge.CreateWorktree{RequestID: "isolated", Source: repo, Revision: base, Destination: filepath.Join(root, "isolated"), Mode: "bridge-managed-retained", Sandbox: "read-only", Policy: map[string]any{"type": "readOnly", "networkAccess": false}, Model: "anthropic/claude-opus-5-5", Effort: "xhigh"}
	r.args = r.arguments(asObject(scenario.Given["arguments"]))
	if scenario.Given["hostile_git"] == true {
		r.hostileGit()
	}
	host := fakehost.Start(t)
	r.host = host
	client, err := appserver.Dial(context.Background(), host.SocketPath)
	if err != nil {
		return nil, err
	}
	r.client = client
	t.Cleanup(func() { _ = client.Close() })
	store, err := ledger.Open(filepath.Join(root, "state", "operations.sqlite3"))
	if err != nil {
		return nil, err
	}
	r.store = store
	t.Cleanup(func() { _ = store.Close() })
	r.b = bridge.New(client, store, execution.Policy{})
	r.configureHost()
	r.beforeStatus = gitValue(t, context.Background(), repo, "-c", "core.fsmonitor=false", "status", "--porcelain=v1", "--ignored")
	r.beforeIndex = gitValue(t, context.Background(), repo, "-c", "core.fsmonitor=false", "diff", "--cached", "--binary")
	_ = os.Remove(filepath.Join(root, "unexpected-effect"))
	for _, raw := range scenario.Run["steps"].([]any) {
		if err := r.step(asObject(raw)); err != nil {
			return nil, err
		}
	}
	return r.observe(scenario), nil
}

func (r *gitBridgeRun) arguments(overrides map[string]any) bridge.CreateWorktree {
	input := r.args
	for key, value := range overrides {
		switch key {
		case "request_id":
			input.RequestID = stringValue(value)
		case "source_repository":
			input.Source = stringValue(value)
		case "starting_revision":
			input.Revision = stringValue(value)
		case "destination":
			input.Destination = stringValue(value)
		case "worktree_mode":
			input.Mode = stringValue(value)
		case "sandbox":
			input.Sandbox = stringValue(value)
		case "expected_sandbox_policy":
			input.Policy = asObject(value)
		case "model":
			input.Model = stringValue(value)
		case "reasoning_effort":
			input.Effort = stringValue(value)
		case "prompt":
			input.Prompt = stringValue(value)
		case "title":
			input.Title = stringValue(value)
		case "app_server_project_id":
			input.ProjectID = stringValue(value)
		}
	}
	return input
}

func (r *gitBridgeRun) record() {
	trace := []any{}
	for _, request := range r.host.Requests() {
		var params any
		if len(request.Params) > 0 {
			if err := json.Unmarshal(request.Params, &params); err != nil {
				r.t.Fatal(err)
			}
		}
		trace = append(trace, []any{request.Method, params})
	}
	r.calls = trace
}
func (r *gitBridgeRun) step(step map[string]any) error {
	switch step["kind"] {
	case "launch":
		input := r.arguments(asObject(step["arguments"]))
		if method := stringValue(step["fail_before_write"]); method != "" {
			r.client.FailBeforeWrite(method)
		}
		var receipt map[string]any
		result, err := r.b.CreateWorktreeThread(context.Background(), input)
		if err != nil {
			var invalid *bridge.Invalid
			var setting *settings.UntransmittableError
			switch {
			case errors.As(err, &setting):
				receipt = map[string]any{"exception": "UntransmittableSetting", "message": err.Error()}
			case errors.As(err, &invalid):
				receipt = map[string]any{"exception": "ValueError", "message": err.Error()}
			case errors.Is(err, ledger.ErrConflict):
				receipt = map[string]any{"exception": "ValueError", "message": "different arguments"}
			default:
				receipt = map[string]any{"exception": "ValueError", "message": err.Error()}
			}
		} else {
			receipt = result
		}
		r.receipts = append(r.receipts, receipt)
		r.record()
		r.launchCalls = append(r.launchCalls, float64(len(r.calls)))
		r.traces = append(r.traces, r.calls)
	case "host":
		if step["field"] == "drop_after" {
			r.given["host"] = map[string]any{"drop_after": step["value"]}
		}
		return nil
	case "restart":
		store, err := ledger.Open(filepath.Join(r.root, "state", "operations.sqlite3"))
		if err != nil {
			return err
		}
		r.t.Cleanup(func() { _ = store.Close() })
		r.b = bridge.New(r.client, store, execution.Policy{})
	case "snapshot":
		r.snapshots = append(r.snapshots, gitValue(r.t, context.Background(), r.repo, "worktree", "list", "--porcelain"))
	case "move":
		if err := os.Rename(filepath.Join(r.root, stringValue(step["from"])), filepath.Join(r.root, stringValue(step["to"]))); err != nil {
			return err
		}
	case "seed_legacy":
		return r.seedLegacy(step)
	case "concurrent":
		return r.concurrent(step)
	default:
		return fmt.Errorf("unsupported git bridge step %v", step["kind"])
	}
	return nil
}

func observedPaths(scenario Scenario) []string {
	out := []string{}
	for _, check := range scenario.Expect.Checks {
		if len(check.Path) == 2 && check.Path[0] == "paths" {
			if name, ok := check.Path[1].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}
func (r *gitBridgeRun) observe(scenario Scenario) map[string]any {
	r.record()
	root := r.root
	repo := r.repo
	if _, err := os.Stat(filepath.Join(root, "moved-source")); err == nil {
		repo = filepath.Join(root, "moved-source")
	}
	status := gitValue(r.t, context.Background(), repo, "-c", "core.fsmonitor=false", "-c", "filter.example.clean=", "-c", "filter.example.smudge=", "-c", "filter.example.process=", "-c", "filter.example.required=false", "status", "--porcelain=v1", "--ignored")
	index := gitValue(r.t, context.Background(), repo, "-c", "core.fsmonitor=false", "-c", "filter.example.clean=", "-c", "filter.example.smudge=", "-c", "filter.example.process=", "-c", "filter.example.required=false", "diff", "--cached", "--binary")
	worktreeList := gitValue(r.t, context.Background(), repo, "-c", "core.fsmonitor=false", "worktree", "list", "--porcelain")
	revision := gitValue(r.t, context.Background(), repo, "-c", "core.fsmonitor=false", "rev-parse", "HEAD")
	obs := map[string]any{"exit": float64(0), "base": r.base, "destination": r.args.Destination, "receipts": r.receipts, "threads": r.threads, "calls": r.calls, "launch_calls": r.launchCalls, "launch_traces": r.traces, "worktree_snapshots": r.snapshots, "status": status, "before_status": r.beforeStatus, "before_index": r.beforeIndex, "after_index": index, "worktrees": worktreeList, "revision": revision}
	paths := map[string]any{}
	for _, name := range observedPaths(scenario) {
		_, err := os.Stat(filepath.Join(root, name))
		paths[name] = err == nil
	}
	obs["paths"] = paths
	checkout := map[string]any{}
	for _, name := range observedNames(scenario, "checkout") {
		checkout[name] = fileText(filepath.Join(root, "isolated", name))
	}
	obs["checkout"] = checkout
	source := map[string]any{}
	for _, name := range observedNames(scenario, "source") {
		source[name] = fileText(filepath.Join(r.repo, name))
	}
	obs["source"] = source
	ledgerRows := map[string]any{}
	for _, item := range r.receipts {
		if receipt := asObject(item); receipt["requestId"] != nil {
			id := stringValue(receipt["requestId"])
			stored, err := r.store.Get(context.Background(), id)
			if err != nil {
				r.t.Fatalf("read stored receipt %s: %v", id, err)
			}
			ledgerRows[id] = stored
		}
	}
	obs["ledger"] = ledgerRows
	views := map[string]any{}
	for id := range r.threads {
		read, readErr := r.b.ReadThread(context.Background(), id, 20, nil, 4000)
		goal, goalErr := r.b.GetGoal(context.Background(), id)
		if readErr != nil || goalErr != nil {
			r.t.Fatalf("thread view %s: %v %v", id, readErr, goalErr)
		}
		views[id] = map[string]any{"read": read, "goal": goal}
	}
	obs["thread_views"] = views
	return jsonShape(obs)
}
