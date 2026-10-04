package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/recall"
)

func recallHookComponentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(key, home)
	}
	t.Setenv("CRW_BIN", "crw")
	t.Setenv("PLUGIN_ROOT", "")
	return home
}
func TestRecallHookComponentIngress(t *testing.T) {
	recallHookComponentHome(t)
	for _, row := range []struct {
		event, leg, raw string
		want            bool
	}{
		{"user-prompt-submit", "user-prompt-submit-detecting-recall-intent", `{"hook_event_name":"UserPromptSubmit","prompt":"지난번 hook.ts"}`, true},
		{"user-prompt-submit", "user-prompt-submit-detecting-recall-intent", `{"hook_event_name":"UserPromptSubmit","prompt":"remember this"}`, false},
		{"user-prompt-submit", "user-prompt-submit-detecting-recall-intent", `{"hook_event_name":"Stop","prompt":"지난번"}`, false},
		{"user-prompt-submit", "user-prompt-submit-detecting-recall-intent", `null`, false},
		{"user-prompt-submit", "user-prompt-submit-detecting-recall-intent", `broken`, false},
		{"post-compact", "post-compact-injecting-recall-context", `{}`, false},
		{"post-compact", "post-compact-injecting-recall-context", ``, false},
	} {
		for _, args := range [][]string{{row.event, "--leg", row.leg}, {row.event, "--leg=" + row.leg}} {
			var out bytes.Buffer
			start := time.Now()
			claimed, code := runComponentHook(invocation{ctx: context.Background(), args: args, stdout: &out}, strings.NewReader(row.raw), componentHooks())
			if !claimed || code != 0 || (out.Len() > 0) != row.want {
				t.Fatalf("%v: %v %d %q", args, claimed, code, out.String())
			}
			if time.Since(start) >= 5*time.Second {
				t.Fatal("UPS hook exceeded 5s")
			}
			if err := recall.AssertLegalHookResult(recall.HookResult{Stdout: out.String(), Code: code}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, args := range [][]string{{"stop", "--leg", "post-compact-injecting-recall-context"}, {"user-prompt-submit", "--leg", "other"}} {
		if claimed, _ := runComponentHook(invocation{args: args}, unreadComponentInput{}, componentHooks()); claimed {
			t.Fatal(args)
		}
	}
}
func TestRecallHookComponentObservation(t *testing.T) {
	home := recallHookComponentHome(t)
	plugin := filepath.Join(t.TempDir(), "plugin")
	os.MkdirAll(filepath.Join(plugin, ".codex-plugin"), 0700)
	os.WriteFile(filepath.Join(plugin, ".codex-plugin", "plugin.json"), []byte(`{"version":"1.0.0"}`), 0600)
	t.Setenv("PLUGIN_ROOT", plugin)
	var out bytes.Buffer
	claimed, code := runComponentHook(invocation{ctx: context.Background(), args: []string{"post-compact", "--leg", "post-compact-injecting-recall-context"}, stdout: &out}, strings.NewReader(`{"session_id":"hook-fixture","hook_event_name":"PostCompact"}`), componentHooks())
	if !claimed || code != 0 || out.Len() != 0 {
		t.Fatal("PostCompact contract")
	}
	found := 0
	err := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".json") {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var rec map[string]any
			if err := json.Unmarshal(b, &rec); err != nil {
				return err
			}
			if rec["component"] != "recall" || rec["event"] != "post-compact" {
				t.Fatalf("record %s", b)
			}
			found++
		}
		return nil
	})
	if err != nil || found != 1 {
		t.Fatalf("observations=%d err=%v", found, err)
	}
}
