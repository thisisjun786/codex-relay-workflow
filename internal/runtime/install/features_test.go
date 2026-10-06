package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// The fake reexecutes this Go test binary; no Node or real Codex is involved.
// Its target survives TestMain's nested isolation through this explicit test-only env.
func TestFeatureCodexHelper(t *testing.T) {
	t.Parallel()
	home := os.Getenv("CRW499_FAKE_HOME")
	if home == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	log, err := os.OpenFile(filepath.Join(home, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(90)
	}
	_ = json.NewEncoder(log).Encode(args)
	_ = log.Close()
	if os.Getenv("CRW499_FAKE_MODE") == "overflow" {
		fmt.Print(strings.Repeat("x", 2<<20))
		os.Exit(0)
	}
	if os.Getenv("CRW499_FAKE_MODE") == "utf8" {
		_, _ = os.Stderr.Write([]byte{'a', 0xe2, 0x82})
		os.Exit(3)
	}
	if len(args) < 2 || args[0] != "features" {
		os.Exit(91)
	}
	if args[1] == "list" && os.Getenv("CRW499_FAKE_LIST_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "codex: config parse error")
		os.Exit(3)
	}
	path := filepath.Join(home, "config.toml")
	b, _ := os.ReadFile(path)
	content := string(b)
	if args[1] == "list" {
		for _, key := range []string{"multi_agent", "goals", "hooks", "default_mode_request_user_input"} {
			v, _ := configguard.ReadTableKey(content, "features", key)
			fmt.Printf("%s stable %t\n", key, v == "true")
		}
		os.Exit(0)
	}
	if len(args) != 3 {
		os.Exit(92)
	}
	if args[2] == os.Getenv("CRW499_FAKE_FAIL_KEY") || args[1] == "disable" && os.Getenv("CRW499_FAKE_DISABLE_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "error: unknown feature key")
		os.Exit(2)
	}
	content = configguard.SetTableKey(content, "features", args[2], args[1] == "enable").Content
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		os.Exit(93)
	}
	os.Exit(0)
}

type featureHome struct {
	t    *testing.T
	home string
	env  scope.Env
}

func newFeatureHome(t *testing.T, content string) featureHome {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(exe, "'", "'\\''") + "'"
	script := "#!/bin/sh\nexec " + quoted + " -test.run='^TestFeatureCodexHelper$' -- \"$@\"\n"
	if err := writeExecutable(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env := scope.Env(os.Environ()).With("HOME", t.TempDir()).With("CODEX_HOME", home).With("PATH", bin).With("CRW499_FAKE_HOME", home)
	for _, key := range []string{"CRW499_FAKE_MODE", "CRW499_FAKE_LIST_FAIL", "CRW499_FAKE_FAIL_KEY", "CRW499_FAKE_DISABLE_FAIL"} {
		env = env.Without(key)
	}
	return featureHome{t, home, env}
}

func (h featureHome) run(args ...string) (int, string, string) {
	h.t.Helper()
	var out, err bytes.Buffer
	code := Main(context.Background(), append([]string{"features"}, args...), h.env, &out, &err)
	return code, out.String(), err.String()
}

func (h featureHome) read(name string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.home, name))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func (h featureHome) success(args ...string) string {
	h.t.Helper()
	code, out, err := h.run(args...)
	if code != 0 || err != "" {
		h.t.Fatalf("%v: exit %d stdout=%q stderr=%q", args, code, out, err)
	}
	return out
}

// Command cases of auto-enable.test.ts:85-185, driven through the public CLI.
func TestFeaturesManagedKeyRoundTrips(t *testing.T) {
	t.Parallel()
	for _, prior := range []string{"", "false", "true", `"oops`} {
		t.Run(prior, func(t *testing.T) {
			content := "[memories]\ngenerate_memories = true\n"
			if prior != "" {
				content += "dedicated_tools = " + prior + "\n"
			}
			h := newFeatureHome(t, content)
			out := h.success("enable")
			if !strings.HasPrefix(out, "crw: enabled [multi_agent, goals, hooks, default_mode_request_user_input]\n") {
				t.Fatal(out)
			}
			var manifest struct {
				TableKeys map[string]struct {
					PriorValue     *string
					SetByCodexclaw bool
				}
			}
			if err := json.Unmarshal([]byte(h.read(configguard.InstallManifestName)), &manifest); err != nil {
				t.Fatal(err)
			}
			rec, exists := manifest.TableKeys["memories.dedicated_tools"]
			if prior == `"oops` {
				if exists {
					t.Fatal("unsupported value recorded")
				}
			} else if !exists || rec.SetByCodexclaw != (prior != "true") || prior == "" && rec.PriorValue != nil || prior != "" && (rec.PriorValue == nil || *rec.PriorValue != prior) {
				t.Fatalf("record %+v", rec)
			}
			if prior == "false" {
				h.success("enable")
			} // The original prior value survives rerun.
			out = h.success("disable")
			after := h.read("config.toml")
			if !strings.Contains(after, "generate_memories = true") || !strings.Contains(after, "[memories]") {
				t.Fatal(after)
			}
			if prior == "" && strings.Contains(after, "dedicated_tools") || prior != "" && !strings.Contains(after, "dedicated_tools = "+prior) {
				t.Fatal(after)
			}
			if prior == "" && !strings.Contains(out, "restored keys: memories.dedicated_tools") {
				t.Fatal(out)
			}
			marker, err := configguard.ReadSelfHealMarkerFile(h.home)
			if err != nil || marker.OptedOut == nil || !*marker.OptedOut {
				t.Fatalf("marker %+v, %v", marker, err)
			}
			h.success("enable")
			marker, err = configguard.ReadSelfHealMarkerFile(h.home)
			if err != nil || marker.OptedOut != nil || marker.OptedOutAt != nil {
				t.Fatalf("marker %+v, %v", marker, err)
			}
		})
	}
}

func TestFeaturesSoftAndHardFailures(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"goals", "default_mode_request_user_input"} {
		t.Run(key, func(t *testing.T) {
			h := newFeatureHome(t, "[features]\n")
			h.env = h.env.With("CRW499_FAKE_FAIL_KEY", key)
			code, out, err := h.run("enable")
			if key == "goals" {
				if code != 1 || out != "" || !strings.Contains(err, "codex features enable goals failed (exit 2)") {
					t.Fatalf("%d %q %q", code, out, err)
				}
				if _, err := os.Stat(filepath.Join(h.home, configguard.InstallManifestName)); !os.IsNotExist(err) {
					t.Fatal("manifest written after hard failure")
				}
				return
			}
			if code != 0 || !strings.HasPrefix(out, "crw: enabled [multi_agent, goals, hooks]\n") || strings.Contains(out, "경고") {
				t.Fatalf("%d %q %q", code, out, err)
			}
			want := "crw: 경고 — 'default_mode_request_user_input' 를 켤 수 없었다 (exit 2)\n  영향: Default 모드에서 질문선택지 UI(request_user_input)가 모델에게 노출되지 않는다. Plan 모드에서는 계속 동작한다.\n  codex: error: unknown feature key\n  확인: codex features list | grep default_mode_request_user_input\n  수동: codex features enable default_mode_request_user_input\n"
			if err != want {
				t.Fatalf("warning %q", err)
			}
			var m struct {
				Flags map[string]struct {
					EnableFailed bool
					Failure      struct {
						ExitCode int
						Message  string
					}
				}
			}
			if err := json.Unmarshal([]byte(h.read(configguard.InstallManifestName)), &m); err != nil {
				t.Fatal(err)
			}
			f := m.Flags[key]
			if !f.EnableFailed || f.Failure.ExitCode != 2 || f.Failure.Message != "error: unknown feature key" {
				t.Fatalf("failure %+v", f)
			}
		})
	}
}

func TestFeaturesHelpUsageAndStatus(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"enable", "disable", "status", "unknown"} {
		for _, help := range []string{"--help", "-h", "help"} {
			h := newFeatureHome(t, "unchanged\n")
			out := h.success(action, "--force", help)
			if !strings.HasPrefix(out, "Usage:\n  crw install features enable") || strings.Contains(out, "uninstall") {
				t.Fatal(out)
			}
			if h.read("config.toml") != "unchanged\n" {
				t.Fatal("help wrote settings")
			}
			entries, _ := os.ReadDir(h.home)
			if len(entries) != 1 {
				t.Fatalf("help wrote files: %v", entries)
			}
		}
	}
	h := newFeatureHome(t, "[features]\nhooks = true\n")
	want := "multi_agent: disabled\ngoals: disabled\nhooks: enabled\ndefault_mode_request_user_input: disabled\n"
	if out := h.success("status", "--ignored"); out != want {
		t.Fatalf("status %q", out)
	}
	for _, args := range [][]string{nil, {"unknown"}, {"uninstall"}, {"config"}, {"hook"}} {
		code, out, err := h.run(args...)
		if code != 2 || out != "" || err != "usage: crw install features <enable|disable|status>\n" {
			t.Fatalf("%v: %d %q %q", args, code, out, err)
		}
	}
	h.env = h.env.With("CRW499_FAKE_LIST_FAIL", "1")
	code, out, err := h.run("status")
	if code != 1 || out != "" || err != "crw: codex features list failed (exit 3): codex: config parse error\n" {
		t.Fatalf("%d %q %q", code, out, err)
	}
}

func TestFeaturesDisableBranches(t *testing.T) {
	t.Parallel()
	h := newFeatureHome(t, "[features]\nhooks = true\n[memories]\ngenerate_memories = true\n")
	if out := h.success("disable"); out != "crw: no install manifest; nothing to revert\n" {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(h.home, "calls")); !os.IsNotExist(err) {
		t.Fatal("Codex called without manifest")
	}
	h.success("enable")
	content := h.read("config.toml") + "\n# user edit\n"
	if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	h.env = h.env.With("CRW499_FAKE_LIST_FAIL", "1")
	out := h.success("disable")
	for _, text := range []string{"disabled [multi_agent, goals, default_mode_request_user_input]; kept pre-existing [hooks]", "restored keys: memories.dedicated_tools", "note: config.toml changed since activation; reverted per key", "note: could not read 'codex features list'; reverted flags from the manifest alone"} {
		if !strings.Contains(out, text) {
			t.Fatal(out)
		}
	}
	if !strings.Contains(h.read("config.toml"), "# user edit") {
		t.Fatal("foreign edit lost")
	}
	h.env = h.env.Without("CRW499_FAKE_LIST_FAIL")
	h.success("enable")
	h.env = h.env.With("CRW499_FAKE_DISABLE_FAIL", "1")
	if out := h.success("disable"); !strings.Contains(out, "disabled [none]") {
		t.Fatal(out)
	}
}

func TestFeaturesUnreadableSettingsAndManifest(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"config.toml", configguard.InstallManifestName, configguard.SelfHealMarkerName} {
		t.Run(file, func(t *testing.T) {
			h := newFeatureHome(t, "[features]\n")
			path := filepath.Join(h.home, file)
			if file == "config.toml" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(h.home, "missing"), path); err != nil {
				t.Fatal(err)
			}
			code, out, err := h.run("enable")
			if file == configguard.SelfHealMarkerName {
				if code != 0 {
					t.Fatalf("optional marker gated enable: %d %q", code, err)
				}
			} else if code != 1 || out != "" || !strings.Contains(err, "left unchanged") {
				t.Fatalf("%d %q %q", code, out, err)
			}
			if target, err := os.Readlink(path); err != nil || target != filepath.Join(h.home, "missing") {
				t.Fatalf("link overwritten: %s %v", target, err)
			}
		})
	}
}

func TestFeaturesHomeTrimAndFallback(t *testing.T) {
	t.Parallel()
	h := newFeatureHome(t, "[features]\n")
	h.env = h.env.With("CODEX_HOME", "\ufeff \t"+h.home+"\n\u00a0")
	h.success("enable")
	if _, err := os.Stat(filepath.Join(h.home, configguard.InstallManifestName)); err != nil {
		t.Fatal(err)
	}
	h.env = h.env.With("CODEX_HOME", " \ufeff\t").With("HOME", filepath.Dir(h.home))
	h.success("disable")
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.home), ".codex", configguard.SelfHealMarkerName)); err != nil {
		t.Fatal(err)
	}
}

func TestFeaturesWarningFallback(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"default_mode_request_user_input", "some_future_flag"} {
		out := featureWarning(key, nil)
		if !strings.Contains(out, "경고") || strings.Contains(out, "exit") || !strings.Contains(out, "codex features enable "+key) {
			t.Fatal(out)
		}
		if key == "some_future_flag" && !strings.Contains(out, "이 플래그에 의존하는 기능이 비활성화된다.") {
			t.Fatal(out)
		}
	}
}

func TestFeaturesRunnerBoundaries(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"utf8", "overflow", "missing", "denied", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			h := newFeatureHome(t, "")
			h.env = h.env.With("CRW499_FAKE_MODE", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if mode == "missing" {
				h.env = h.env.With("PATH", t.TempDir())
			}
			if mode == "denied" {
				if err := os.Chmod(filepath.Join(h.env.Get("PATH"), "codex"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancelled" {
				cancel()
			}
			result := featureRunner(ctx, h.env)([]string{"features", "list"})
			if mode == "utf8" {
				if result.ExitCode != 3 || result.Stderr != "a\ufffd" {
					t.Fatalf("%+v", result)
				}
			} else if result.ExitCode != 1 {
				t.Fatalf("exit %d", result.ExitCode)
			}
			if mode == "overflow" && (len(result.Stdout) == 0 || len(result.Stdout) > 1<<20) {
				t.Fatalf("output size %d", len(result.Stdout))
			}
			if mode == "missing" && result.Stderr != "spawnSync codex ENOENT" || mode == "denied" && result.Stderr != "spawnSync codex EACCES" {
				t.Fatalf("%+v", result)
			}
		})
	}
}

func TestFeaturesExternalOwnership(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"missing", "changed", "unverifiable"} {
		t.Run(reason, func(t *testing.T) {
			h := newFeatureHome(t, "[memories]\ngenerate_memories = true\n")
			h.success("enable")
			content := h.read("config.toml")
			switch reason {
			case "missing":
				content = strings.ReplaceAll(content, "dedicated_tools = true\n", "")
			case "changed":
				content = strings.ReplaceAll(content, "dedicated_tools = true", "dedicated_tools = false")
			case "unverifiable":
				var m struct{ BackupPath string }
				if err := json.Unmarshal([]byte(h.read(configguard.InstallManifestName)), &m); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(m.BackupPath); err != nil {
					t.Fatal(err)
				}
				content += "# foreign edit\n"
			}
			content = strings.ReplaceAll(content, "multi_agent = true", "multi_agent = false")
			if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
			out := h.success("disable")
			if !strings.Contains(out, "memories.dedicated_tools ("+reason+")") || !strings.Contains(out, "multi_agent (missing)") {
				t.Fatal(out)
			}
			if reason == "changed" && !strings.Contains(h.read("config.toml"), "dedicated_tools = false") {
				t.Fatal("foreign value lost")
			}
		})
	}
}

func TestFeaturesBackupAndEmptySuccess(t *testing.T) {
	t.Parallel()
	original := "# keep\n[features]\nmulti_agent = true\ngoals = true\nhooks = true\ndefault_mode_request_user_input = true\n[memories]\ndedicated_tools = true\n"
	h := newFeatureHome(t, original)
	if out := h.success("enable"); !strings.HasPrefix(out, "crw: enabled [none]\nbackup: ") {
		t.Fatal(out)
	}
	backups, err := filepath.Glob(filepath.Join(h.home, "config.toml.crw-*.bak"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups %v %v", backups, err)
	}
	b, err := os.ReadFile(backups[0])
	if err != nil || string(b) != original {
		t.Fatalf("backup %q %v", b, err)
	}
	if out := h.success("disable"); out != "crw: disabled [none]; kept pre-existing [multi_agent, goals, hooks, default_mode_request_user_input]\nleft to their current owner: memories.dedicated_tools (changed)\n" {
		t.Fatal(out)
	}
}

func TestFeaturesEmptyHomeStaysAtWorkingDirectory(t *testing.T) {
	t.Parallel()
	// Assert the resolver alone: even the broken account-home fallback must never
	// reach a mutating command in this regression's RED state.
	home, err := resolveFeatureHome(scope.Env{"HOME=", "CODEX_HOME= \ufeff\t"})
	if err != nil || home != ".codex" {
		t.Fatalf("home %q, error %v", home, err)
	}
}

func TestFeaturesKeepsInstallerHelp(t *testing.T) {
	t.Parallel()
	var out, err bytes.Buffer
	code := Main(context.Background(), []string{"help"}, nil, &out, &err)
	want := "usage: crw install {install,update,rollback,remove,status,register-mcp,hook,register-service} ...\n"
	if code != 0 || out.String() != want || err.Len() != 0 {
		t.Fatalf("exit %d stdout=%q stderr=%q", code, out.String(), err.String())
	}
}
