package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1265. A sandboxed Codex never starts or resumes in the account home, above it, or above a
// project root: every path that hands a cwd to the App Server asks internal/sandboxroot first, and a
// refusal leaves the fake host with no call at all.

func refusalOf(t *testing.T, err error, rule string) *sandboxroot.Refusal {
	t.Helper()
	var refusal *sandboxroot.Refusal
	if !errors.As(err, &refusal) || refusal.Rule != rule {
		t.Fatalf("want a %s refusal, got %v", rule, err)
	}
	return refusal
}

func TestCreateThreadRefusesHomeCwdBeforeHostCall(t *testing.T) {
	w := sandboxroottest.Fake(t)
	b, host := testBridge(t)
	for _, sandbox := range []string{"read-only", "workspace-write"} {
		id := "home-" + sandbox
		_, err := b.CreateThread(context.Background(), CreateThread{RequestID: id, CWD: w.Home, Sandbox: sandbox, Prompt: "hi", Model: "explicit-model", Effort: "high"})
		refusalOf(t, err, sandboxroot.RuleAccountHome)
		if methods := hostMethods(host); len(methods) != 0 {
			t.Fatalf("%s: the host was called: %v", sandbox, methods)
		}
		if receipt, err := b.GetOperation(context.Background(), id); err == nil {
			t.Fatalf("%s: a refused creation left a receipt: %v", sandbox, receipt)
		}
	}
	// The home's parent and a link to the home are refused the same way.
	link := filepath.Join(w.Sandbox, "to-home")
	if err := os.Symlink(w.Home, link); err != nil {
		t.Fatal(err)
	}
	for cwd, rule := range map[string]string{filepath.Dir(w.Home): sandboxroot.RuleHomeAncestor, link: sandboxroot.RuleAccountHome} {
		_, err := b.CreateThread(context.Background(), CreateThread{RequestID: "other-" + rule, CWD: cwd, Sandbox: "workspace-write", Model: "explicit-model", Effort: "high"})
		refusalOf(t, err, rule)
	}
	if methods := hostMethods(host); len(methods) != 0 {
		t.Fatalf("the host was called: %v", methods)
	}
}

func TestCreateThreadRefusesProjectAncestorCwd(t *testing.T) {
	w := sandboxroottest.Fake(t)
	project := w.Mkdir(t, w.Home, "code", "aas")
	w.Project(t, project)
	b, host := testBridge(t)
	_, err := b.CreateThread(context.Background(), CreateThread{RequestID: "above-project", CWD: filepath.Dir(project), Sandbox: "workspace-write", Model: "explicit-model", Effort: "high"})
	if refusal := refusalOf(t, err, sandboxroot.RuleProjectAncestor); refusal.Project != project {
		t.Fatalf("project %q, want %q", refusal.Project, project)
	}
	if methods := hostMethods(host); len(methods) != 0 {
		t.Fatalf("the host was called: %v", methods)
	}
	// The project itself and a directory under the sandbox base run as before.
	for _, cwd := range []string{project, w.Mkdir(t, w.Sandbox, "run")} {
		host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}, "cwd": cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"}}})
		receipt, err := b.CreateThread(context.Background(), CreateThread{RequestID: "ok-" + filepath.Base(cwd), CWD: cwd, Sandbox: "read-only", Model: "explicit-model", Effort: "high"})
		if err != nil || receipt["status"] != "accepted" {
			t.Fatalf("%s: receipt=%v err=%v", cwd, receipt, err)
		}
	}
}

func TestCreateThreadKeepsHomeCwdWithoutSandbox(t *testing.T) {
	w := sandboxroottest.Fake(t)
	b, host := testBridge(t)
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}, "cwd": w.Home, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "dangerFullAccess"}}})
	receipt, err := b.CreateThread(context.Background(), CreateThread{RequestID: "no-sandbox", CWD: w.Home, Sandbox: "danger-full-access", Model: "explicit-model", Effort: "high"})
	if err != nil || receipt["status"] != "accepted" || host.Count("thread/start") != 1 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
	}
}

func worktreeAt(t *testing.T, destination string) (*Bridge, *fakehost.Server, CreateWorktree) {
	t.Helper()
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.Destination = destination
	worktreeHost(host, destination)
	return b, host, input
}

func TestCreateWorktreeThreadAllowsDefaultWorktreeRoot(t *testing.T) {
	w := sandboxroottest.Fake(t)
	b, host, input := worktreeAt(t, filepath.Join(w.Worktrees, "isolated"))
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || host.Count("thread/start") != 1 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
	}
	if _, err := os.Lstat(filepath.Join(input.Destination, ".git")); err != nil {
		t.Fatalf("the worktree was not made: %v", err)
	}
}

func TestCreateWorktreeThreadAllowsCodexHomeWorktree(t *testing.T) {
	w := sandboxroottest.Fake(t)
	parent := w.Mkdir(t, w.CodexHome, "worktrees", "abc123")
	b, host, input := worktreeAt(t, filepath.Join(parent, "repo"))
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || host.Count("thread/start") != 1 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
	}
}

func TestCreateWorktreeThreadRefusesHomeDestination(t *testing.T) {
	w := sandboxroottest.Fake(t)
	for _, destination := range []string{filepath.Join(w.Home, "isolated"), filepath.Join(w.Root, "elsewhere", "isolated")} {
		w.Mkdir(t, filepath.Dir(destination))
		b, host, input := worktreeAt(t, destination)
		receipt, err := b.CreateWorktreeThread(context.Background(), input)
		if err != nil || receipt["status"] != "failed" || pyjson.Map(receipt["rpcError"])["code"] != sandboxroot.Code {
			t.Fatalf("%s: receipt=%v err=%v", destination, receipt, err)
		}
		if methods := hostMethods(host); len(methods) != 0 {
			t.Fatalf("%s: the host was called: %v", destination, methods)
		}
		if _, err := os.Lstat(destination); !os.IsNotExist(err) {
			t.Fatalf("%s: the destination was made: %v", destination, err)
		}
		if effects, _ := receipt["attemptedEffects"].([]string); len(effects) != 0 {
			t.Fatalf("%s: effects %v (reserved or created)", destination, effects)
		}
		if strings.Count(gitAt(t, input.Source, "worktree", "list", "--porcelain"), "worktree ") != 1 {
			t.Fatalf("%s: a worktree was registered", destination)
		}
	}
}

func TestSendMessageRefusesRecordedHomeCwdBeforeResume(t *testing.T) {
	w := sandboxroottest.Fake(t)
	for _, sandbox := range []map[string]any{nil, {"sandbox": "workspace-write"}} {
		b, host := testBridge(t)
		host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}, "cwd": w.Home}}})
		expected := map[string]any{"model": "explicit-model", "reasoning_effort": "high"}
		for k, v := range sandbox {
			expected[k] = v
		}
		receipt, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "recorded-home", ThreadID: "thread-1", Message: "work", Expected: expected})
		if err != nil || receipt["status"] != "failed" || pyjson.Map(receipt["rpcError"])["code"] != sandboxroot.Code || receipt["delivery"] != "not_delivered" {
			t.Fatalf("expected=%v receipt=%v err=%v", expected, receipt, err)
		}
		if host.Count("thread/resume") != 0 || host.Count("turn/start") != 0 {
			t.Fatalf("a refused send resumed or started a turn: %v", hostMethods(host))
		}
	}
	// A request that names another cwd is judged by that cwd, a thread without a sandbox mode that
	// the host reports counts as sandboxed, and a thread that reports danger-full-access is outside the rule.
	b, host := testBridge(t)
	run := w.Mkdir(t, w.Sandbox, "run")
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}, "cwd": w.Home}}})
	host.Respond("thread/resume", startReply(run))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	receipt, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "named-cwd", ThreadID: "thread-1", Message: "work", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high", "cwd": run}})
	if err != nil || receipt["status"] == "failed" && pyjson.Map(receipt["rpcError"])["code"] == sandboxroot.Code {
		t.Fatalf("a send naming an allowed cwd was refused: receipt=%v err=%v", receipt, err)
	}
	b2, host2 := testBridge(t)
	host2.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}, "cwd": w.Home}, "sandbox": map[string]any{"type": "dangerFullAccess"}}})
	host2.Respond("thread/resume", startReply(w.Home))
	host2.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	receipt, err = b2.SendMessageToThread(context.Background(), SendMessage{RequestID: "unsandboxed", ThreadID: "thread-1", Message: "work", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
	if err != nil || host2.Count("thread/resume") != 1 {
		t.Fatalf("a thread without a sandbox was refused: receipt=%v err=%v calls=%v", receipt, err, hostMethods(host2))
	}
}

// The roots the host reports for a thread are the ones a resume that names none keeps: a safe cwd
// with the account home among them is refused before thread/resume.
func TestSendMessageRefusesReportedHomeWritableRoots(t *testing.T) {
	w := sandboxroottest.Fake(t)
	run := w.Mkdir(t, w.Sandbox, "run")
	for name, top := range map[string]map[string]any{
		"runtimeWorkspaceRoots": {"runtimeWorkspaceRoots": []any{run, w.Home}, "sandbox": map[string]any{"type": "workspaceWrite"}},
		"policy writable root":  {"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{w.Home}}},
	} {
		t.Run(name, func(t *testing.T) {
			b, host := testBridge(t)
			result := map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}, "cwd": run}}
			for k, v := range top {
				result[k] = v
			}
			host.Respond("thread/read", fakehost.Reply{Result: result})
			receipt, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "reported-roots", ThreadID: "thread-1", Message: "work", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
			if err != nil || receipt["status"] != "failed" || pyjson.Map(receipt["rpcError"])["code"] != sandboxroot.Code || receipt["delivery"] != "not_delivered" {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			if host.Count("thread/resume") != 0 || host.Count("turn/start") != 0 {
				t.Fatalf("a refused send resumed or started a turn: %v", hostMethods(host))
			}
		})
	}
}
