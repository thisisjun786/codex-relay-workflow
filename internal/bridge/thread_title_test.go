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
	} {
		if got := repositorySessionTitle(origin); got != want {
			t.Errorf("repositorySessionTitle(%q) = %q, want %q", origin, got, want)
		}
	}
}
