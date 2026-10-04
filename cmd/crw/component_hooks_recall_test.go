package main

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"modernc.org/sqlite"
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

func TestRecallHookSessionStartProduction(t *testing.T) {
	for _, store := range []string{"available", "missing", "unreadable"} {
		t.Run(store, func(t *testing.T) {
			home := recallHookComponentHome(t)
			if store == "available" {
				var output, errs bytes.Buffer
				if code := recall.Run([]string{"chat", "index", "--json"}, &output, &errs, time.Now()); code != 0 {
					t.Fatalf("seed index %d %s", code, errs.String())
				}
				// The ingress must read a real store, not a precomputed test notice.
				db, err := (&sqlite.Driver{}).Open(filepath.Join(home, "memories_1.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.(driver.ExecerContext).ExecContext(context.Background(), "CREATE TABLE jobs(kind,status,retry_remaining,last_error,finished_at); INSERT INTO jobs VALUES ('stage1','error',0,'capacity',NULL)", nil)
				closeErr := db.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("seed memory %v %v", err, closeErr)
				}
			}
			if store == "unreadable" {
				if err := os.Mkdir(filepath.Join(home, "memories_1.sqlite"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(home, "recall", "index.sqlite"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			raw := `{"hook_event_name":"SessionStart","cwd":"","source":"resume"}`
			claimed, code := runComponentHook(invocation{ctx: context.Background(), args: []string{"session-start", "--leg", "session-start-injecting-recall-context"}, stdout: &out}, strings.NewReader(raw), componentHooks())
			if !claimed || code != 0 || !strings.Contains(out.String(), "resumed after a pause") {
				t.Fatalf("%s %v %d %q", store, claimed, code, out.String())
			}
			if (strings.Contains(out.String(), "Index: 0 files")) != (store == "available") {
				t.Fatal(out.String())
			}
			if (strings.Contains(out.String(), "1 job(s) exhausted")) != (store == "available") {
				t.Fatal(out.String())
			}
			if store == "available" && strings.Index(out.String(), "1 job(s) exhausted") > strings.Index(out.String(), "recall is available") {
				t.Fatal("notice order")
			}
			if err := recall.AssertLegalHookResult(recall.HookResult{Stdout: out.String(), Code: code}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecallHookSessionStartCwdShapes(t *testing.T) {
	recallHookComponentHome(t)
	for _, row := range []struct {
		value       string
		unavailable bool
	}{{"false", false}, {"0", false}, {"true", true}, {"{}", true}, {"[]", true}, {"3", true}} {
		var out bytes.Buffer
		claimed, code := runComponentHook(invocation{ctx: context.Background(), args: []string{"session-start", "--leg", "session-start-injecting-recall-context"}, stdout: &out}, strings.NewReader(`{"cwd":`+row.value+`}`), componentHooks())
		if !claimed || code != 0 || !strings.Contains(out.String(), "recall is available") || strings.Contains(out.String(), "Recall unavailable") != row.unavailable {
			t.Fatalf("cwd=%s exit=%d out=%s", row.value, code, out.String())
		}
	}
}
