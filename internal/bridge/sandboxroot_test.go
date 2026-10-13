package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1265 kept a sandboxed Codex out of the account home, the directories above it and the directories above a project
// root. CRW-1308 (Jun, 10-13) removes that restriction: every path that hands a cwd to the App Server still asks
// internal/sandboxroot, and its verdict is a recorded warning; the host is called as for any other root.

// createReply is the host's answer to a creation in cwd under a sandbox policy type.
func createReply(cwd, sandboxType string) fakehost.Reply {
	return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd}, "cwd": cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": sandboxType}}}
}

func TestCreateThreadRefusesHomeCwdBeforeHostCall(t *testing.T) {
	w := sandboxroottest.Fake(t)
	b, host := testBridge(t)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	for sandbox, kind := range map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite"} {
		id := "home-" + sandbox
		host.Respond("thread/start", createReply(w.Home, kind))
		receipt, err := b.CreateThread(context.Background(), CreateThread{RequestID: id, CWD: w.Home, Sandbox: sandbox, Prompt: "hi", Model: "explicit-model", Effort: "high"})
		if err != nil || receipt["status"] != "accepted" {
			t.Fatalf("%s: receipt=%v err=%v calls=%v", sandbox, receipt, err, hostMethods(host))
		}
		w.Warned(t, "bridge/create_thread", sandboxroot.RuleAccountHome, w.Home)
	}
	if host.Count("thread/start") != 2 || host.Count("turn/start") != 2 {
		t.Fatalf("the host was not called for each creation: %v", hostMethods(host))
	}
	// The home's parent and a link to the home pass the same way, each with its warning.
	link := filepath.Join(w.Sandbox, "to-home")
	if err := os.Symlink(w.Home, link); err != nil {
		t.Fatal(err)
	}
	for cwd, rule := range map[string]string{filepath.Dir(w.Home): sandboxroot.RuleHomeAncestor, link: sandboxroot.RuleAccountHome} {
		// The bridge runs the thread at the cwd's real path, and asks the rule about that path.
		real, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			t.Fatal(err)
		}
		host.Respond("thread/start", createReply(real, "workspaceWrite"))
		receipt, err := b.CreateThread(context.Background(), CreateThread{RequestID: "other-" + rule, CWD: cwd, Sandbox: "workspace-write", Model: "explicit-model", Effort: "high"})
		if err != nil || receipt["status"] != "accepted" {
			t.Fatalf("%s: receipt=%v err=%v", cwd, receipt, err)
		}
		w.Warned(t, "bridge/create_thread", rule, real)
	}
	if host.Count("thread/start") != 4 {
		t.Fatalf("the host was not called for each creation: %v", hostMethods(host))
	}
}

func TestCreateThreadRefusesProjectAncestorCwd(t *testing.T) {
	w := sandboxroottest.Fake(t)
	project := w.Mkdir(t, w.Home, "code", "aas")
	w.Project(t, project)
	b, host := testBridge(t)
	host.Respond("thread/start", createReply(filepath.Dir(project), "workspaceWrite"))
	receipt, err := b.CreateThread(context.Background(), CreateThread{RequestID: "above-project", CWD: filepath.Dir(project), Sandbox: "workspace-write", Model: "explicit-model", Effort: "high"})
	if err != nil || receipt["status"] != "accepted" || host.Count("thread/start") != 1 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, hostMethods(host))
	}
	if record := w.Warned(t, "bridge/create_thread", sandboxroot.RuleProjectAncestor, filepath.Dir(project)); record.Project != project {
		t.Fatalf("project %q, want %q", record.Project, project)
	}
	before := len(w.Warnings())
	// The project itself and a directory under the sandbox base run as before, with no warning.
	for _, cwd := range []string{project, w.Mkdir(t, w.Sandbox, "run")} {
		host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}, "cwd": cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"}}})
		receipt, err := b.CreateThread(context.Background(), CreateThread{RequestID: "ok-" + filepath.Base(cwd), CWD: cwd, Sandbox: "read-only", Model: "explicit-model", Effort: "high"})
		if err != nil || receipt["status"] != "accepted" {
			t.Fatalf("%s: receipt=%v err=%v", cwd, receipt, err)
		}
	}
	if records := w.Warnings(); len(records) != before {
		t.Fatalf("an allowed root was warned about: %+v", records[before:])
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

// A destination in the account home or outside every base is made and its thread started (CRW-1308); the preflight and the
// finished worktree each leave a warning.
func TestCreateWorktreeThreadRefusesHomeDestination(t *testing.T) {
	w := sandboxroottest.Fake(t)
	for _, destination := range []string{filepath.Join(w.Home, "isolated"), filepath.Join(w.Root, "elsewhere", "isolated")} {
		w.Mkdir(t, filepath.Dir(destination))
		b, host, input := worktreeAt(t, destination)
		receipt, err := b.CreateWorktreeThread(context.Background(), input)
		if err != nil || receipt["status"] != "accepted" || host.Count("thread/start") != 1 {
			t.Fatalf("%s: receipt=%v err=%v calls=%v", destination, receipt, err, hostMethods(host))
		}
		if _, err := os.Lstat(filepath.Join(destination, ".git")); err != nil {
			t.Fatalf("%s: the worktree was not made: %v", destination, err)
		}
		if strings.Count(gitAt(t, input.Source, "worktree", "list", "--porcelain"), "worktree ") != 2 {
			t.Fatalf("%s: the worktree was not registered", destination)
		}
		w.Warned(t, "bridge/create_worktree_thread:preflight", sandboxroot.RuleOutsideDedicatedRoot, destination)
		w.Warned(t, "bridge/create_worktree_thread:check", sandboxroot.RuleOutsideDedicatedRoot, destination)
	}
}

// A thread the host reports in the account home is resumed there and the message delivered (CRW-1308), with a warning.
func TestSendMessageRefusesRecordedHomeCwdBeforeResume(t *testing.T) {
	w := sandboxroottest.Fake(t)
	for kind, sandbox := range map[string]map[string]any{"readOnly": nil, "workspaceWrite": {"sandbox": "workspace-write"}} {
		b, host := testBridge(t)
		host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}, "cwd": w.Home}}})
		host.Respond("thread/resume", createReply(w.Home, kind))
		host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
		expected := map[string]any{"model": "explicit-model", "reasoning_effort": "high"}
		for k, v := range sandbox {
			expected[k] = v
		}
		receipt, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "recorded-home", ThreadID: "thread-1", Message: "work", Expected: expected})
		if err != nil || receipt["status"] != "accepted" || host.Count("thread/resume") != 1 || host.Count("turn/start") != 1 {
			t.Fatalf("expected=%v receipt=%v err=%v calls=%v", expected, receipt, err, hostMethods(host))
		}
		w.Warned(t, "bridge/send_message_to_thread", sandboxroot.RuleAccountHome, w.Home)
		w.Warned(t, "bridge/send_message_to_thread:thread/resume", sandboxroot.RuleAccountHome, w.Home)
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
// with the account home among them is warned about before thread/resume, and the send goes on (CRW-1308).
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
			host.Respond("thread/resume", createReply(run, "workspaceWrite"))
			host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
			receipt, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "reported-roots", ThreadID: "thread-1", Message: "work", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
			if err != nil || receipt["status"] != "accepted" || host.Count("thread/resume") != 1 || host.Count("turn/start") != 1 {
				t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, hostMethods(host))
			}
			w.Warned(t, "bridge/send_message_to_thread", sandboxroot.RuleAccountHome, w.Home)
		})
	}
}

// CRW-1265 (verification of d90085b2): a send that states no policy resumes the thread under the roots
// the host's configuration gives it, and the host reports them in its thread/resume answer, not in
// thread/read. The answer is judged before turn/start: an account home among its roots is warned about
// and the turn starts (CRW-1308), for a notLoaded and an idle thread alike; a safe answer goes on.
func TestSendMessageRefusesHomeRootsOnlyTheResumeAnswerReports(t *testing.T) {
	w := sandboxroottest.Fake(t)
	run := w.Mkdir(t, w.Sandbox, "run")
	answer := func(extra map[string]any) fakehost.Reply {
		result := map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": run}, "cwd": run, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "workspaceWrite"}}
		for k, v := range extra {
			result[k] = v
		}
		return fakehost.Reply{Result: result}
	}
	home := map[string]map[string]any{
		"policy writable root":  {"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{w.Home}}},
		"runtimeWorkspaceRoots": {"runtimeWorkspaceRoots": []any{run, w.Home}},
		"thread runtime roots":  {"thread": map[string]any{"id": "thread-1", "cwd": run, "runtimeWorkspaceRoots": []any{w.Home}}},
		"home cwd":              {"cwd": w.Home},
	}
	for _, status := range []string{"notLoaded", "idle"} {
		for name, extra := range home {
			t.Run(status+"/"+name, func(t *testing.T) {
				b, host := testBridge(t)
				host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": status}, "cwd": run}}})
				host.Respond("thread/resume", answer(extra))
				host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
				receipt, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "resume-home", ThreadID: "thread-1", Message: "work", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
				if err != nil || receipt["status"] != "accepted" || host.Count("turn/start") != 1 {
					t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, hostMethods(host))
				}
				w.Warned(t, "bridge/send_message_to_thread:thread/resume", sandboxroot.RuleAccountHome, "")
			})
		}
		t.Run(status+"/a safe answer goes on", func(t *testing.T) {
			b, host := testBridge(t)
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": status}, "cwd": run}}})
			host.Respond("thread/resume", answer(map[string]any{"runtimeWorkspaceRoots": []any{run}, "sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{run}}}))
			host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
			receipt, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "resume-safe", ThreadID: "thread-1", Message: "work", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
			if err != nil || receipt["status"] != "accepted" || host.Count("turn/start") != 1 {
				t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, hostMethods(host))
			}
		})
	}
}

// CRW-1265 (pre-merge evaluation of f0691fdd, d2): a creation that states no policy and no roots leaves
// the writable roots to the host's configuration, and the host answers thread/start with the roots the
// thread got. An account home among them is warned about and the title and the initial prompt go out
// (CRW-1308); a safe answer goes on as before.
func TestCreateThreadsWithholdPromptWhenHostAnswersWithHomeWritableRoot(t *testing.T) {
	w := sandboxroottest.Fake(t)
	cwd := w.Mkdir(t, w.Sandbox, "run")
	answer := func(roots ...any) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd}, "cwd": cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": roots}}}
	}
	t.Run("create_thread", func(t *testing.T) {
		b, host := testBridge(t)
		host.Respond("thread/start", answer(w.Home))
		host.Respond("thread/name/set", fakehost.Reply{})
		host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
		receipt, err := b.CreateThread(context.Background(), CreateThread{RequestID: "inherited-home", CWD: cwd, Sandbox: "workspace-write", Prompt: "hi", Title: "t", Model: "explicit-model", Effort: "high"})
		if err != nil || receipt["status"] != "accepted" || host.Count("thread/start") != 1 || host.Count("thread/name/set") != 1 || host.Count("turn/start") != 1 {
			t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, hostMethods(host))
		}
		w.Warned(t, "bridge/create_thread:thread/start", sandboxroot.RuleAccountHome, w.Home)
	})
	t.Run("create_worktree_thread", func(t *testing.T) {
		// A worktree thread states its whole policy, so a root the host adds is a finding that withholds the prompt already.
		destination := filepath.Join(w.Worktrees, "isolated")
		b, host, input := worktreeAt(t, destination)
		policy := map[string]any{"type": "workspaceWrite", "writableRoots": []any{destination}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
		input.Sandbox, input.Policy = "workspace-write", policy
		host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": destination}, "cwd": destination, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{destination, w.Home}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}, "runtimeWorkspaceRoots": []any{destination}}})
		receipt, err := b.CreateWorktreeThread(context.Background(), input)
		if err != nil || receipt["status"] != "failed" || host.Count("turn/start") != 0 || host.Count("thread/name/set") != 0 {
			t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, hostMethods(host))
		}
	})
	t.Run("a safe answer goes on", func(t *testing.T) {
		b, host := testBridge(t)
		host.Respond("thread/start", answer(cwd))
		host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
		receipt, err := b.CreateThread(context.Background(), CreateThread{RequestID: "inherited-safe", CWD: cwd, Sandbox: "workspace-write", Prompt: "hi", Model: "explicit-model", Effort: "high"})
		if err != nil || receipt["status"] != "accepted" || host.Count("turn/start") != 1 {
			t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, hostMethods(host))
		}
	})
}
