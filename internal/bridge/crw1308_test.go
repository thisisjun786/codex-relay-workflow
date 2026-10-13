package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// sandboxRootWarnings reads the sandbox-root warning record under a Codex home (CRW-1308): one JSON object a line in
// <CODEX_HOME>/crw/sandbox-root-warnings.jsonl.
func sandboxRootWarnings(t *testing.T, codexHome string) []map[string]any {
	t.Helper()
	file, err := os.Open(filepath.Join(codexHome, "crw", "sandbox-root-warnings.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var out []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("a record line is not JSON: %q: %v", scanner.Text(), err)
		}
		out = append(out, record)
	}
	return out
}

// CRW-1308 (Jun, 10-13): CRW removes every sandbox work-root restriction of CRW-1265. A sandboxed create_thread whose cwd is
// the account home, a plain folder under it, or a directory above a project root is not refused: the App Server call goes out
// and the rule's verdict is recorded as a warning.
func TestCreateThreadOutsideDedicatedRootsProceedsWithRecordedWarning(t *testing.T) {
	w := sandboxroottest.Fake(t)
	project := w.Mkdir(t, w.Home, "code", "aas")
	w.Project(t, project)
	cases := []struct {
		name, cwd, rule string
	}{
		{"the account home", w.Home, sandboxroot.RuleAccountHome},
		{"a plain folder under the home", w.Mkdir(t, w.Home, "tmp"), sandboxroot.RuleOutsideDedicatedRoot},
		{"a parent of a project root", filepath.Dir(project), sandboxroot.RuleProjectAncestor},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, host := testBridge(t)
			host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}, "cwd": c.cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"}}})
			receipt, err := b.CreateThread(context.Background(), CreateThread{RequestID: "crw1308-" + c.rule, CWD: c.cwd, Sandbox: "read-only", Model: "explicit-model", Effort: "high"})
			if err != nil || receipt["status"] != "accepted" {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			if host.Count("thread/start") != 1 {
				t.Fatalf("the App Server call did not go out: %v", hostMethods(host))
			}
			records := sandboxRootWarnings(t, w.CodexHome)
			if len(records) != i+1 {
				t.Fatalf("want %d warning records, got %v", i+1, records)
			}
			last := records[len(records)-1]
			if last["rule"] != c.rule || last["path"] != c.cwd || last["caller"] != "bridge/create_thread" || last["decision"] != "warn" {
				t.Fatalf("record %v, want rule %s for %s", last, c.rule, c.cwd)
			}
		})
	}
}
