package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-1313: the session that creates a thread names it right after thread/start with thread/name/set, by the
// thread title table of crw-canon/references/hierarchy.md, so the Codex app never shows the first prompt sentence
// as its title. create_thread creates all three: the repository session (relay role supervisor, the table's
// "[CRW] 부모 · <owner>/<repo>" from the checkout's origin when the caller gives no title), the project session
// (relay role parent, the caller's title by crw-plan's rule) and an issue child (relay role child, the managed
// start's "ISSUE-ID · title"). A naming failure is a warning in the receipt and on stderr, never a refusal.

// namingHost makes the fake host keep the names thread/name/set gives and answer thread/read with them.
func namingHost(host *fakehost.Server, cwd string, sandbox map[string]any, refuseName bool) {
	var mu sync.Mutex
	names := map[string]any{}
	start := startReply(cwd)
	start.Result["model"], start.Result["reasoningEffort"], start.Result["sandbox"] = pyModel, pyEffort, sandbox
	host.Respond("thread/start", start)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Handle("thread/name/set", func(raw json.RawMessage) fakehost.Reply {
		if refuseName {
			return fakehost.Reply{Error: &fakehost.RPCError{Code: -32603, Message: "name store unavailable"}}
		}
		var params map[string]any
		_ = json.Unmarshal(raw, &params)
		mu.Lock()
		names[pyjson.Text(params["threadId"])] = params["name"]
		mu.Unlock()
		return fakehost.Reply{Result: map[string]any{}}
	})
	host.Handle("thread/read", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		_ = json.Unmarshal(raw, &params)
		mu.Lock()
		defer mu.Unlock()
		id := pyjson.Text(params["threadId"])
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": id, "name": names[id], "status": map[string]any{"type": "idle"}}}}
	})
}

// readName is the name thread/read gives back for the thread.
func readName(t *testing.T, b *Bridge, thread string) any {
	t.Helper()
	read, err := b.call(context.Background(), "thread/read", map[string]any{"threadId": thread})
	if err != nil {
		t.Fatal(err)
	}
	return pyjson.Map(read["thread"])["name"]
}

// gitCheckout is a checkout whose origin is the given URL.
func gitCheckout(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func captureTitleWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := titleWarningOutput
	titleWarningOutput = &buf
	t.Cleanup(func() { titleWarningOutput = previous })
	return &buf
}

func TestCreatorsNameTheThreadByTheTitleTable(t *testing.T) {
	danger := map[string]any{"type": "dangerFullAccess"}
	for _, c := range []struct {
		name, role, title, origin, want string
	}{
		{"the repository session from its origin", "supervisor", "", "https://github.com/thisisjun786/codex-relay-workflow.git", "[CRW] 부모 · thisisjun786/codex-relay-workflow"},
		{"the repository session from an scp origin", "supervisor", "", "git@github.com:thisisjun786/crw-trial.git", "[CRW] 부모 · thisisjun786/crw-trial"},
		{"the project session by crw-plan's rule", "parent", "[CRW] 실사용 첫 시험 (P-CRW-140)", "", "[CRW] 실사용 첫 시험 (P-CRW-140)"},
		{"an issue child by the managed start's rule", "child", "CRW-1313 · 스레드 제목을 붙인다", "", "CRW-1313 · 스레드 제목을 붙인다"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, host := rolesBridge(t)
			cwd := t.TempDir()
			if c.origin != "" {
				cwd = gitCheckout(t, c.origin)
			}
			namingHost(host, cwd, danger, false)
			in := createInput(cwd, "title-"+c.role)
			in.Role, in.Model, in.Effort, in.Sandbox, in.Title, in.Prompt = c.role, pyModel, pyEffort, "danger-full-access", c.title, "첫 프롬프트 문장은 제목이 아니다."
			if c.role == "parent" {
				in.Model, in.Effort = parentModel, parentEffort
			}
			receipt, err := b.CreateThread(context.Background(), in)
			if err != nil || receipt["status"] != "accepted" {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			if got := readName(t, b, "thread-1"); got != c.want {
				t.Fatalf("thread/read name = %v, want %q", got, c.want)
			}
			if receipt["title"] != c.want || receipt["titleWarning"] != nil {
				t.Fatalf("receipt title = %v, warning = %v", receipt["title"], receipt["titleWarning"])
			}
			methods := hostMethods(host)
			if named, turned := indexOf(methods, "thread/name/set"), indexOf(methods, "turn/start"); named < 0 || named > turned || indexOf(methods, "thread/start") > named {
				t.Fatalf("the name is not set right after thread/start and before the first turn: %v", methods)
			}
		})
	}
}

func TestANamingFailureWarnsAndRecordsAndNeverRefuses(t *testing.T) {
	danger := map[string]any{"type": "dangerFullAccess"}
	t.Run("the host refuses thread/name/set", func(t *testing.T) {
		warnings := captureTitleWarnings(t)
		b, host := rolesBridge(t)
		cwd := t.TempDir()
		namingHost(host, cwd, danger, true)
		in := createInput(cwd, "title-refused")
		in.Role, in.Model, in.Effort, in.Sandbox, in.Title, in.Prompt = "parent", parentModel, parentEffort, "danger-full-access", "[CRW] 시험", "work"
		receipt, err := b.CreateThread(context.Background(), in)
		if err != nil || receipt["status"] != "accepted" || receipt["turnId"] != "turn-1" {
			t.Fatalf("a naming failure refused the creation: receipt=%v err=%v", receipt, err)
		}
		warning := pyjson.Map(receipt["titleWarning"])
		if warning["code"] != "title_not_set" || !strings.Contains(pyjson.Text(warning["message"]), "name store unavailable") || receipt["title"] != nil {
			t.Fatalf("receipt titleWarning = %v, title = %v", receipt["titleWarning"], receipt["title"])
		}
		if !strings.Contains(warnings.String(), "title_not_set") {
			t.Fatalf("no warning on stderr: %q", warnings.String())
		}
	})
	t.Run("a repository session whose checkout has no origin", func(t *testing.T) {
		warnings := captureTitleWarnings(t)
		b, host := rolesBridge(t)
		cwd := t.TempDir()
		namingHost(host, cwd, danger, false)
		in := createInput(cwd, "title-no-origin")
		in.Role, in.Model, in.Effort, in.Sandbox, in.Prompt = "supervisor", pyModel, pyEffort, "danger-full-access", "work"
		receipt, err := b.CreateThread(context.Background(), in)
		if err != nil || receipt["status"] != "accepted" || receipt["turnId"] != "turn-1" {
			t.Fatalf("receipt=%v err=%v", receipt, err)
		}
		if code := pyjson.Map(receipt["titleWarning"])["code"]; code != "title_unset" || host.Count("thread/name/set") != 0 {
			t.Fatalf("titleWarning = %v, thread/name/set x%d", receipt["titleWarning"], host.Count("thread/name/set"))
		}
		if !strings.Contains(warnings.String(), "title_unset") {
			t.Fatalf("no warning on stderr: %q", warnings.String())
		}
	})
	t.Run("a project session or an issue child created without a title", func(t *testing.T) {
		for _, role := range []string{"parent", "child"} {
			captureTitleWarnings(t)
			b, host := rolesBridge(t)
			cwd := t.TempDir()
			namingHost(host, cwd, danger, false)
			in := createInput(cwd, "untitled-"+role)
			in.Role, in.Model, in.Effort, in.Sandbox, in.Prompt = role, pyModel, pyEffort, "danger-full-access", "work"
			if role == "parent" {
				in.Model, in.Effort = parentModel, parentEffort
			}
			receipt, err := b.CreateThread(context.Background(), in)
			if err != nil || receipt["status"] != "accepted" {
				t.Fatalf("%s: receipt=%v err=%v", role, receipt, err)
			}
			if code := pyjson.Map(receipt["titleWarning"])["code"]; code != "title_unset" {
				t.Fatalf("%s: titleWarning = %v", role, receipt["titleWarning"])
			}
		}
	})
	t.Run("a creation with no role and no title stays as it was", func(t *testing.T) {
		b, host := testBridge(t)
		cwd := t.TempDir()
		namingHost(host, cwd, map[string]any{"type": "readOnly"}, false)
		in := createInput(cwd, "legacy")
		in.Prompt = "work"
		receipt, err := b.CreateThread(context.Background(), in)
		if err != nil || receipt["titleWarning"] != nil || host.Count("thread/name/set") != 0 {
			t.Fatalf("receipt=%v err=%v name/set x%d", receipt, err, host.Count("thread/name/set"))
		}
	})
}

func TestRepositorySessionTitle(t *testing.T) {
	for origin, want := range map[string]string{
		"https://github.com/o/r.git":      "[CRW] 부모 · o/r",
		"https://github.com/o/r":          "[CRW] 부모 · o/r",
		"ssh://git@github.com/o/r.git":    "[CRW] 부모 · o/r",
		"git@github.com:o/r.git":          "[CRW] 부모 · o/r",
		"https://ghe.example.com/o/r":     "[CRW] 부모 · o/r",
		"/srv/git/r.git":                  "",
		"https://github.com/o":            "",
		"https://github.com/o/r/extra":    "",
		"":                                "",
		"git@github.com:o/r name.git":     "",
		"https://github.com/../r.git":     "",
		"https://github.com/o/r.git\n ":   "[CRW] 부모 · o/r",
		"file:///srv/git/o/r.git":         "",
		"git@github.com:/o/r.git":         "[CRW] 부모 · o/r",
		"https://user@github.com/o/r.git": "[CRW] 부모 · o/r",
		// CRW-1313 (Argon V5 K2): the scp form's host is a host, with an optional user, and nothing else.
		"github.com:o/r.git":       "[CRW] 부모 · o/r",
		"git@ghe.example.com:o/r":  "[CRW] 부모 · o/r",
		"bad host:o/r.git":         "",
		"git@bad host:o/r.git":     "",
		"a/b:o/r.git":              "",
		`a\b:o/r.git`:              "",
		"git@github.com\t:o/r.git": "",
		"@github.com:o/r.git":      "",
		"git@:o/r.git":             "",
	} {
		if got := repositorySessionTitle(origin); got != want {
			t.Errorf("repositorySessionTitle(%q) = %q, want %q", origin, got, want)
		}
	}
}

// CRW-1313 (Argon V5 on the CRW-1353 lane, K1): create_worktree_thread names its thread by the same table and warns
// and records the same way. Its save right before turn/start is the phase dispatching_initial_prompt whatever the
// naming outcome, so a process killed after turn/start leaves a receipt that says the prompt may have been sent.

// worktreeNamingHost is namingHost for create_worktree_thread: the started thread is placed in the destination and runs
// the role's pair with a danger-full-access sandbox.
func worktreeNamingHost(host *fakehost.Server, destination string, refuseName bool) {
	danger := map[string]any{"type": "dangerFullAccess"}
	namingHost(host, destination, danger, refuseName)
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": destination}, "cwd": destination, "model": pyModel, "reasoningEffort": pyEffort, "approvalPolicy": "never", "sandbox": danger, "runtimeWorkspaceRoots": []any{destination}}})
}

// roleWorktreeInput is a worktree creation for role with the given title, whose source checkout has origin when one is
// given (the worktree shares the source's configuration, so the destination reads the same origin).
func roleWorktreeInput(t *testing.T, role, title, origin string) CreateWorktree {
	t.Helper()
	input := worktreeInput(t)
	if origin != "" {
		gitAt(t, input.Source, "remote", "add", "origin", origin)
	}
	input.RequestID = "worktree-title-" + role
	input.Role, input.Title, input.Prompt = role, title, "첫 프롬프트 문장은 제목이 아니다."
	input.Sandbox, input.Policy = "danger-full-access", map[string]any{"type": "dangerFullAccess"}
	input.Model, input.Effort = pyModel, pyEffort
	if role == "parent" {
		input.Model, input.Effort = parentModel, parentEffort
	}
	return input
}

// rowAtTurnStart makes the host read the bridge's ledger row for request when turn/start arrives: what a process killed
// right after sending turn/start leaves.
func rowAtTurnStart(t *testing.T, b *Bridge, host *fakehost.Server, request string) <-chan map[string]any {
	t.Helper()
	rows := make(chan map[string]any, 1)
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		row, err := b.Ledger.Get(context.Background(), request)
		if err != nil {
			t.Errorf("reading the ledger at turn/start: %v", err)
		}
		rows <- row
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}}
	})
	return rows
}

func TestWorktreeCreatorNamesTheThreadByTheTitleTable(t *testing.T) {
	for _, c := range []struct {
		name, role, title, origin, want string
	}{
		{"the repository session from its origin", "supervisor", "", "git@github.com:thisisjun786/crw-trial.git", "[CRW] 부모 · thisisjun786/crw-trial"},
		{"the project session by crw-plan's rule", "parent", "[CRW] 실사용 첫 시험 (P-CRW-140)", "", "[CRW] 실사용 첫 시험 (P-CRW-140)"},
		{"an issue child by the managed start's rule", "child", "CRW-1313 · 스레드 제목을 붙인다", "", "CRW-1313 · 스레드 제목을 붙인다"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, host := rolesBridge(t)
			input := roleWorktreeInput(t, c.role, c.title, c.origin)
			worktreeNamingHost(host, input.Destination, false)
			rows := rowAtTurnStart(t, b, host, input.RequestID)
			receipt, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil || receipt["status"] != "accepted" || receipt["turnId"] != "turn-1" {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			if got := readName(t, b, "thread-1"); got != c.want {
				t.Fatalf("thread/read name = %v, want %q", got, c.want)
			}
			if receipt["title"] != c.want || receipt["titleWarning"] != nil {
				t.Fatalf("receipt title = %v, warning = %v", receipt["title"], receipt["titleWarning"])
			}
			methods := hostMethods(host)
			if named, turned := indexOf(methods, "thread/name/set"), indexOf(methods, "turn/start"); named < 0 || named > turned || indexOf(methods, "thread/start") > named {
				t.Fatalf("the name is not set right after thread/start and before the first turn: %v", methods)
			}
			if row := <-rows; row["phase"] != "dispatching_initial_prompt" || row["title"] != c.want {
				t.Fatalf("the save before the first turn: phase %v, title %v", row["phase"], row["title"])
			}
		})
	}
}

func TestAWorktreeNamingFailureWarnsAndRecordsAndNeverRefuses(t *testing.T) {
	for _, c := range []struct {
		name, role, title, code string
		refuse                  bool
	}{
		{"the host refuses thread/name/set", "parent", "[CRW] 시험", "title_not_set", true},
		{"a repository session whose checkout has no origin", "supervisor", "", "title_unset", false},
		{"a project session created without a title", "parent", "", "title_unset", false},
		{"an issue child created without a title", "child", "", "title_unset", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			warnings := captureTitleWarnings(t)
			b, host := rolesBridge(t)
			input := roleWorktreeInput(t, c.role, c.title, "")
			worktreeNamingHost(host, input.Destination, c.refuse)
			rows := rowAtTurnStart(t, b, host, input.RequestID)
			receipt, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil || receipt["status"] != "accepted" || receipt["turnId"] != "turn-1" || receipt["phase"] != "complete" {
				t.Fatalf("a naming failure refused the creation: receipt=%v err=%v", receipt, err)
			}
			warning := pyjson.Map(receipt["titleWarning"])
			if warning["code"] != c.code || receipt["title"] != nil {
				t.Fatalf("receipt titleWarning = %v, title = %v", receipt["titleWarning"], receipt["title"])
			}
			if c.refuse != (host.Count("thread/name/set") == 1) {
				t.Fatalf("thread/name/set x%d", host.Count("thread/name/set"))
			}
			if c.refuse && !strings.Contains(pyjson.Text(warning["message"]), "name store unavailable") {
				t.Fatalf("the warning does not say what the host answered: %v", warning)
			}
			if !strings.Contains(warnings.String(), c.code) {
				t.Fatalf("no warning on stderr: %q", warnings.String())
			}
			row := <-rows
			if row["phase"] != "dispatching_initial_prompt" || pyjson.Map(row["initialPrompt"])["state"] != "outcome_unknown" || pyjson.Map(row["titleWarning"])["code"] != c.code {
				t.Fatalf("the save before the first turn does not say the prompt may have been sent: %v", row)
			}
		})
	}
}

// An scp-style origin whose host is not a host names no repository: the repository session gets no title and the
// creation warns and records, as for a checkout with no origin (CRW-1313, Argon V5 K2).
func TestAnScpOriginWithABadHostNamesNoRepository(t *testing.T) {
	for _, origin := range []string{"bad host:o/r.git", "a/b:o/r.git"} {
		t.Run(origin, func(t *testing.T) {
			warnings := captureTitleWarnings(t)
			b, host := rolesBridge(t)
			cwd := gitCheckout(t, origin)
			namingHost(host, cwd, map[string]any{"type": "dangerFullAccess"}, false)
			in := createInput(cwd, "title-bad-host")
			in.Role, in.Model, in.Effort, in.Sandbox, in.Prompt = "supervisor", pyModel, pyEffort, "danger-full-access", "work"
			receipt, err := b.CreateThread(context.Background(), in)
			if err != nil || receipt["status"] != "accepted" || receipt["turnId"] != "turn-1" {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			if code := pyjson.Map(receipt["titleWarning"])["code"]; code != "title_unset" || receipt["title"] != nil || host.Count("thread/name/set") != 0 {
				t.Fatalf("title = %v, titleWarning = %v, thread/name/set x%d", receipt["title"], receipt["titleWarning"], host.Count("thread/name/set"))
			}
			if !strings.Contains(warnings.String(), "title_unset") {
				t.Fatalf("no warning on stderr: %q", warnings.String())
			}
		})
	}
}
