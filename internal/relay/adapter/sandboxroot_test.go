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
// settings, and a sandboxed run in the account home is refused before thread/resume is sent.

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

func refusedNothingSent(t *testing.T, receipt delivery.Obj, methods []string, rule string) {
	t.Helper()
	rpcError, _ := receipt.Get("rpcError").(contract.OrderedObject)
	text, _ := json.Marshal(receipt)
	if receipt.Get("status") != "not_attempted" || rpcError.Get("code") != sandboxroot.Code || receipt.Get("retrySafe") != true {
		t.Fatalf("want a nothing-sent %s refusal, got %s", rule, text)
	}
	if message, _ := rpcError.Get("message").(string); !strings.Contains(message, rule) {
		t.Fatalf("the refusal does not name %s: %s", rule, message)
	}
	if fmt.Sprint(methods) != "[thread/read]" {
		t.Fatalf("calls after the refusal: %v", methods)
	}
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
	}{
		{"recorded cwd in the home, resume carries cwd and sandbox", false, sandboxSettings(w.Home, []string{w.Home}, nil), run, nil, sandboxroot.RuleAccountHome},
		{"cwd override in the home, host thread elsewhere", false, sandboxSettings(w.Home, []string{run}, nil), run, nil, sandboxroot.RuleAccountHome},
		{"cwd override above the home", false, sandboxSettings(filepath.Dir(w.Home), []string{run}, nil), run, nil, sandboxroot.RuleHomeAncestor},
		{"runtimeWorkspaceRoots holds the home", false, sandboxSettings(run, []string{run, w.Home}, nil), run, nil, sandboxroot.RuleAccountHome},
		{"policy writable root is the home", false, sandboxSettings(run, []string{run}, workspaceWrite(w.Home)), run, nil, sandboxroot.RuleAccountHome},
		{"settings-free, host reports the home cwd", true, sandboxSettings(run, []string{run}, nil), w.Home, nil, sandboxroot.RuleAccountHome},
		{"settings-free, home cwd, host reports a sandbox", true, sandboxSettings(run, []string{run}, nil), w.Home, map[string]any{"sandbox": map[string]any{"type": "workspaceWrite"}}, sandboxroot.RuleAccountHome},
		{"settings-free, home cwd, sandbox from the record", true, sandboxSettings(w.Home, []string{w.Home}, workspaceWrite()), w.Home, nil, sandboxroot.RuleAccountHome},
		{"read-only record in the home", false, sandboxSettings(w.Home, []string{w.Home}, map[string]any{"type": "readOnly", "networkAccess": false}), w.Home, nil, sandboxroot.RuleAccountHome},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			receipt, methods := sandboxedSend(t, c.settings, c.settingsFree, idleThread(c.hostCwd), c.hostTop)
			refusedNothingSent(t, receipt, methods, c.rule)
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
	}{
		{"recorded danger-full-access in the home", false, sandboxSettings(w.Home, []string{w.Home}, full), nil},
		{"settings-free, the host reports danger-full-access", true, sandboxSettings(w.Home, []string{w.Home}, workspaceWrite()), map[string]any{"sandbox": full}},
	} {
		t.Run(c.name, func(t *testing.T) {
			receipt, methods := sandboxedSend(t, c.settings, c.settingsFree, idleThread(w.Home), c.hostTop,
				resumeAt(c.settings, w.Home, full), map[string]any{"turn": map[string]any{"id": "fake-turn-1"}})
			if fmt.Sprint(methods) != "[thread/read thread/resume turn/start]" {
				text, _ := json.Marshal(receipt)
				t.Fatalf("calls %v; receipt %s", methods, text)
			}
		})
	}
}

// resumeAt is the host's answer to a resume of a thread at cwd with the sandbox.
func resumeAt(settings map[string]any, cwd string, sandbox map[string]any) map[string]any {
	r := resume()
	r["cwd"], r["runtimeWorkspaceRoots"], r["sandbox"] = cwd, []any{cwd}, sandbox
	r["model"], r["reasoningEffort"] = settings["model"], settings["reasoningEffort"]
	return r
}
