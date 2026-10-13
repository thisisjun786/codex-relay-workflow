package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1265. A relay delivery resumes a thread and starts a turn in it. The thread's cwd, sandbox and
// writable roots come from the resume parameters, from what the host reports, or from the recorded
// settings. CRW-1265 refused a sandboxed run in the account home before thread/resume was sent; since
// CRW-1308 the rule's verdict is a recorded warning and the resume goes out.

// sandboxSettings is a recorded settings row for a workspace-write thread at cwd.
func sandboxSettings(cwd string, roots []string, sandbox map[string]any) map[string]any {
	s := authorized()
	r := make([]any, len(roots))
	for i, root := range roots {
		r[i] = root
	}
	s["cwd"], s["runtimeWorkspaceRoots"] = cwd, r
	s["environments"] = []any{map[string]any{"environmentId": "local", "cwd": cwd, "runtimeWorkspaceRoots": r}}
	if sandbox != nil {
		s["sandbox"] = sandbox
	}
	return s
}

func workspaceWrite(writable ...string) map[string]any {
	roots := make([]any, len(writable))
	for i, root := range writable {
		roots[i] = root
	}
	return map[string]any{"type": "workspaceWrite", "writableRoots": roots, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
}

// relayChildSandbox is the workspace-write sandbox the fixtures create a relay child in: with network access, so the child
// reaches the relay daemon's socket, as bridge creation requires (CRW-1296).
func relayChildSandbox() map[string]any {
	sandbox := workspaceWrite()
	sandbox["networkAccess"] = true
	return sandbox
}

// sandboxedSend runs one relay send over a scripted host and returns the receipt and the host calls.
func sandboxedSend(t *testing.T, settings map[string]any, settingsFree bool, hostThread map[string]any, hostTop map[string]any, answers ...map[string]any) (delivery.Obj, []string) {
	t.Helper()
	read := map[string]any{"thread": hostThread}
	for k, v := range hostTop {
		read[k] = v
	}
	rpc := &scriptRPC{answers: append([]map[string]any{read}, answers...), calls: []any{}}
	root := t.TempDir()
	db, err := ledger.OpenWithOptions(filepath.Join(root, "operations.sqlite3"), ledger.Options{Now: func() float64 { return 1700000000.125 }, Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: rpc, Ledger: db, Clock: delivery.NewFakeClock()})
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("close adapter: %v", err)
		}
	})
	receipt, err := a.Send(context.Background(), "sandbox-send", "thread-1", "work", &delivery.TaskSettings{Data: ordered(settings).(contract.OrderedObject), SettingsFreeResume: settingsFree}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	methods := []string{}
	for _, call := range rpc.calls {
		methods = append(methods, call.([]any)[0].(string))
	}
	return receipt, methods
}

func idleThread(cwd string) map[string]any {
	thread := map[string]any{"status": map[string]any{"type": "idle"}}
	if cwd != "" {
		thread["cwd"] = cwd
	}
	return thread
}

// warnedAndResumed checks a send the rule's verdict did not stop (CRW-1308): no sandbox_root_refused in the receipt, the
// resume went out after thread/read, the verdict is in the warning record, and the send ended as ends says. An empty ends
// is a started turn; otherwise it is the code of the later check, outside the rule, that stopped the send after the resume
// (the host's answer does not keep the recorded settings, or a loaded thread differs from its record).
func warnedAndResumed(t *testing.T, w *sandboxroottest.World, receipt delivery.Obj, methods []string, rule, ends string) {
	t.Helper()
	text, _ := json.Marshal(receipt)
	want, code := "[thread/read thread/resume turn/start]", ""
	if ends != "" {
		want = "[thread/read thread/resume]"
	}
	if rpc, ok := receipt.Get("rpcError").(delivery.Obj); ok {
		code = fmt.Sprint(rpc.Get("code"))
	}
	if strings.Contains(string(text), sandboxroot.Code) || fmt.Sprint(methods) != want || code != ends {
		t.Fatalf("want the calls %s and the end %q after a %s warning; calls %v, end %q; receipt %s", want, ends, rule, methods, code, text)
	}
	w.Warned(t, "relay/send_message_to_thread", rule, "")
}

// warnedSend is sandboxedSend with the host's answers to the resume and the turn.
// The warning record is emptied first, so the send's own verdict is the one checked.
func warnedSend(t *testing.T, w *sandboxroottest.World, settings map[string]any, settingsFree bool, hostCwd string, hostTop map[string]any) (delivery.Obj, []string) {
	t.Helper()
	w.ClearWarnings(t)
	return sandboxedSend(t, settings, settingsFree, idleThread(hostCwd), hostTop,
		resumeAt(settings, hostCwd, workspaceWrite()), map[string]any{"turn": map[string]any{"id": "fake-turn-1"}})
}

func TestGuardedSendRefusesSandboxedHomeResume(t *testing.T) {
	w := sandboxroottest.Fake(t)
	run := w.Mkdir(t, w.Sandbox, "run")
	cases := []struct {
		name         string
		settingsFree bool
		settings     map[string]any
		hostCwd      string
		hostTop      map[string]any
		rule         string
		ends         string // "" when the turn starts, else the later settings check that stops the send
	}{
		{"recorded cwd in the home, resume carries cwd and sandbox", false, sandboxSettings(w.Home, []string{w.Home}, nil), run, nil, sandboxroot.RuleAccountHome, "settings_not_preserved"},
		{"cwd override in the home, host thread elsewhere", false, sandboxSettings(w.Home, []string{run}, nil), run, nil, sandboxroot.RuleAccountHome, "settings_not_preserved"},
		{"cwd override above the home", false, sandboxSettings(filepath.Dir(w.Home), []string{run}, nil), run, nil, sandboxroot.RuleHomeAncestor, "settings_not_preserved"},
		{"runtimeWorkspaceRoots holds the home", false, sandboxSettings(run, []string{run, w.Home}, nil), run, nil, sandboxroot.RuleAccountHome, ""},
		{"policy writable root is the home", false, sandboxSettings(run, []string{run}, workspaceWrite(w.Home)), run, nil, sandboxroot.RuleAccountHome, ""},
		{"settings-free, host reports the home cwd", true, sandboxSettings(run, []string{run}, nil), w.Home, nil, sandboxroot.RuleAccountHome, "settings_differ_after_load"},
		{"settings-free, home cwd, host reports a sandbox", true, sandboxSettings(run, []string{run}, nil), w.Home, map[string]any{"sandbox": map[string]any{"type": "workspaceWrite"}}, sandboxroot.RuleAccountHome, "settings_differ_after_load"},
		{"settings-free, home cwd, sandbox from the record", true, sandboxSettings(w.Home, []string{w.Home}, workspaceWrite()), w.Home, nil, sandboxroot.RuleAccountHome, ""},
		{"read-only record in the home", false, sandboxSettings(w.Home, []string{w.Home}, map[string]any{"type": "readOnly", "networkAccess": false}), w.Home, nil, sandboxroot.RuleAccountHome, "settings_not_preserved"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			receipt, methods := warnedSend(t, w, c.settings, c.settingsFree, c.hostCwd, c.hostTop)
			warnedAndResumed(t, w, receipt, methods, c.rule, c.ends)
		})
	}
}

func TestGuardedSendKeepsUnsandboxedHomeResume(t *testing.T) {
	w := sandboxroottest.Fake(t)
	full := map[string]any{"type": "dangerFullAccess"}
	for _, c := range []struct {
		name         string
		settingsFree bool
		settings     map[string]any
		hostTop      map[string]any
		wantTurn     bool
	}{
		{"recorded danger-full-access in the home", false, sandboxSettings(w.Home, []string{w.Home}, full), nil, true},
		{"settings-free, record and host say danger-full-access", true, sandboxSettings(w.Home, []string{w.Home}, full), map[string]any{"sandbox": full}, true},
		// The host's report decides the sandbox of a settings-free resume before the record does: the rule lets the
		// resume go out, and the later comparison of the answer with the record is another check's business.
		{"settings-free, the host reports danger-full-access", true, sandboxSettings(w.Home, []string{w.Home}, workspaceWrite()), map[string]any{"sandbox": full}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			receipt, methods := sandboxedSend(t, c.settings, c.settingsFree, idleThread(w.Home), c.hostTop,
				resumeAt(c.settings, w.Home, full), map[string]any{"turn": map[string]any{"id": "fake-turn-1"}})
			text, _ := json.Marshal(receipt)
			if strings.Contains(string(text), sandboxroot.Code) || len(methods) < 2 || methods[1] != "thread/resume" {
				t.Fatalf("calls %v; receipt %s", methods, text)
			}
			if c.wantTurn && fmt.Sprint(methods) != "[thread/read thread/resume turn/start]" {
				t.Fatalf("calls %v; receipt %s", methods, text)
			}
		})
	}
}

// resumeAt is the host's answer to a resume of a thread at cwd with the sandbox.
func resumeAt(settings map[string]any, cwd string, sandbox map[string]any) map[string]any {
	r := resume()
	r["cwd"], r["runtimeWorkspaceRoots"], r["sandbox"] = cwd, []any{cwd}, sandbox
	r["thread"] = map[string]any{"id": "thread-1", "environments": []any{map[string]any{"environmentId": "local", "cwd": cwd, "runtimeWorkspaceRoots": []any{cwd}}}}
	r["model"], r["reasoningEffort"] = settings["model"], settings["reasoningEffort"]
	return r
}

// A settings-free resume sends no settings, but the thread keeps the roots it was recorded with and
// the ones the host reports for it: a safe cwd with the account home among the writable roots is
// warned about before thread/resume, from the record or from the host's answer, and the resume goes out (CRW-1308).
func TestGuardedSendRefusesInheritedHomeWritableRoots(t *testing.T) {
	w := sandboxroottest.Fake(t)
	run := w.Mkdir(t, w.Sandbox, "run")
	cases := []struct {
		name     string
		settings map[string]any
		hostTop  map[string]any
	}{
		{"recorded runtimeWorkspaceRoots hold the home", sandboxSettings(run, []string{run, w.Home}, workspaceWrite()), nil},
		{"recorded policy writable root is the home", sandboxSettings(run, []string{run}, workspaceWrite(w.Home)), nil},
		{"host reports the home among the roots", sandboxSettings(run, []string{run}, workspaceWrite()), map[string]any{"runtimeWorkspaceRoots": []any{run, w.Home}}},
		{"host reports a policy writable root in the home", sandboxSettings(run, []string{run}, workspaceWrite()), map[string]any{"sandbox": workspaceWrite(w.Home)}},
	}
	for _, c := range cases {
		for _, free := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, settings-free=%v", c.name, free), func(t *testing.T) {
				receipt, methods := warnedSend(t, w, c.settings, free, run, c.hostTop)
				warnedAndResumed(t, w, receipt, methods, sandboxroot.RuleAccountHome, "")
			})
		}
	}
}
