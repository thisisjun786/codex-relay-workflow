package configguard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func activationHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	return home
}
func activationWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func activationRead(t *testing.T, path string) string {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func activationRun(t *testing.T, home string, state map[string]bool, calls *[][]string) CodexRunner {
	return func(args []string) CodexRunResult {
		*calls = append(*calls, append([]string(nil), args...))
		if args[1] == "list" {
			var rows []string
			for _, k := range DeclaredFeatures() {
				rows = append(rows, string(k)+" stable "+map[bool]string{true: "true", false: "false"}[state[string(k)]])
			}
			rows = append(rows, "multi_agent_v2 under-development false", "plugin_hooks removed false")
			return CodexRunResult{Stdout: strings.Join(rows, "\n")}
		}
		state[args[2]] = true
		path := filepath.Join(home, "config.toml")
		content, _ := os.ReadFile(path)
		out := SetTableKey(string(content), "features", args[2], true)
		activationWrite(t, path, out.Content)
		return CodexRunResult{}
	}
}
func activationDeps(t *testing.T, home string, state map[string]bool, calls *[][]string) ActivateDeps {
	return ActivateDeps{CodexHome: home, Run: activationRun(t, home, state, calls), Now: func() string { return "2026-06-30T00:00:00.000Z" }}
}
func allActivationFlags() map[string]bool {
	return map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": true}
}

func TestActivateSelectedFlagsBackupAndManifest(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	original := "# foreign\n[features]\nmulti_agent = true\ngoals = true\nhooks = false\n[model]\nname = \"keep\"\n"
	activationWrite(t, path, original)
	var calls [][]string
	m, e := Activate(activationDeps(t, home, map[string]bool{"multi_agent": true, "goals": true}, &calls))
	if e != nil {
		t.Fatal(e)
	}
	want := [][]string{{"features", "list"}, {"features", "enable", "hooks"}, {"features", "enable", "default_mode_request_user_input"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v", calls)
	}
	if m.Flags["multi_agent"].EnabledByCodexclaw || !m.Flags["multi_agent"].PriorEnabled || !m.Flags["hooks"].EnabledByCodexclaw {
		t.Fatalf("flags=%+v", m.Flags)
	}
	if _, ok := m.Flags["multi_agent_v2"]; ok {
		t.Fatal("undeclared feature recorded")
	}
	if m.BackupPath == nil || *m.BackupPath != path+".crw-2026-06-30T00-00-00-000Z.bak" || activationRead(t, *m.BackupPath) != original {
		t.Fatal("backup is not byte exact")
	}
	info, e := os.Stat(*m.BackupPath)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup mode: %v %v", info, e)
	}
	content := activationRead(t, path)
	if !strings.Contains(content, "name = \"keep\"") || !strings.Contains(content, "dedicated_tools = true") {
		t.Fatalf("config=%q", content)
	}
	sum := sha256.Sum256([]byte(content))
	if m.PostActivateHash == nil || *m.PostActivateHash != hex.EncodeToString(sum[:]) {
		t.Fatal("post hash mismatch")
	}
	raw := activationRead(t, manifestPath(home))
	if !strings.HasSuffix(raw, "\n") || !strings.HasPrefix(raw, "{\n  \"version\": 2,\n") {
		t.Fatalf("manifest=%q", raw)
	}
	parsed := parseInstallManifest(raw)
	if parsed == nil || !reflect.DeepEqual(parsed.Flags, m.Flags) || !reflect.DeepEqual(parsed.TableKeys, m.TableKeys) {
		t.Fatalf("manifest roundtrip=%+v", parsed)
	}
	var oracle struct {
		Config, Backup, Manifest string
		Calls                    [][]string
	}
	recorded, err := os.ReadFile("testdata/oracle-fresh-activation.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(recorded, &oracle); err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(raw, home, "<HOME>") != oracle.Manifest || content != oracle.Config || activationRead(t, *m.BackupPath) != oracle.Backup || !reflect.DeepEqual(calls, oracle.Calls) {
		t.Fatal("activation differs from the recorded byte-exact manifest/config/backup or runner calls")
	}
}
func TestActivateRerunKeepsManagedPriorAndOwnership(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "[memories]\ndedicated_tools = false # original\n")
	var calls [][]string
	deps := activationDeps(t, home, map[string]bool{}, &calls)
	first, e := Activate(deps)
	if e != nil {
		t.Fatal(e)
	}
	calls = nil
	second, e := Activate(deps)
	if e != nil {
		t.Fatal(e)
	}
	key := second.TableKeys["memories.dedicated_tools"]
	if len(calls) != 1 || key.PriorValue == nil || *key.PriorValue != "false" || !key.SetByCodexclaw {
		t.Fatalf("rerun=%+v calls=%v", key, calls)
	}
	if !first.Flags["goals"].EnabledByCodexclaw || second.Flags["goals"].EnabledByCodexclaw || !second.Flags["goals"].PriorEnabled {
		t.Fatal("oracle flag-rerun quirk changed")
	}
}
func TestActivateManagedValues(t *testing.T) {
	for _, v := range []string{"true", "false", "[true]", "\"\"\"text\"\"\""} {
		t.Run(v, func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, "[memories]\ndedicated_tools = "+v+"\n")
			var calls [][]string
			m, e := Activate(activationDeps(t, home, allActivationFlags(), &calls))
			if e != nil {
				t.Fatal(e)
			}
			key, ok := m.TableKeys["memories.dedicated_tools"]
			if strings.HasPrefix(v, "[") || strings.HasPrefix(v, "\"\"\"") {
				if ok {
					t.Fatal("unsupported value recorded")
				}
				return
			}
			if !ok || key.PriorValue == nil || *key.PriorValue != v || key.SetByCodexclaw != (v != "true") {
				t.Fatalf("key=%+v", key)
			}
			if len(calls) != 1 {
				t.Fatal(calls)
			}
		})
	}
}
func TestActivateFailurePaths(t *testing.T) {
	for _, kind := range []string{"list", "hard", "soft"} {
		t.Run(kind, func(t *testing.T) {
			home := activationHome(t)
			path := filepath.Join(home, "config.toml")
			activationWrite(t, path, "# saved\n")
			var calls [][]string
			deps := activationDeps(t, home, map[string]bool{}, &calls)
			base := deps.Run
			deps.Run = func(a []string) CodexRunResult {
				if kind == "list" && a[1] == "list" || kind == "hard" && a[1] == "enable" && a[2] == "goals" || kind == "soft" && a[1] == "enable" && a[2] == "default_mode_request_user_input" {
					return CodexRunResult{ExitCode: 7, Stderr: " \ufefffailure\n "}
				}
				return base(a)
			}
			m, e := Activate(deps)
			if kind != "soft" {
				if e == nil || m != nil || !strings.Contains(e.Error(), "failed (exit 7): failure") {
					t.Fatalf("result=%+v error=%v", m, e)
				}
				if _, e := os.Stat(manifestPath(home)); !os.IsNotExist(e) {
					t.Fatal("hard failure published manifest")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			f := m.Flags["default_mode_request_user_input"]
			if !f.EnableFailed || f.EnabledByCodexclaw || f.Failure == nil || f.Failure.ExitCode != 7 || f.Failure.Message != "failure" {
				t.Fatalf("soft=%+v", f)
			}
		})
	}
}
func TestActivateMissingConfigAndTickingClock(t *testing.T) {
	home := activationHome(t)
	var calls [][]string
	deps := activationDeps(t, home, allActivationFlags(), &calls)
	m, e := Activate(deps)
	if e != nil {
		t.Fatal(e)
	}
	if m.BackupPath != nil || m.TableKeys["memories.dedicated_tools"].PriorValue != nil {
		t.Fatalf("manifest=%+v", m)
	}
	ticks := 0
	deps.Now = func() string {
		ticks++
		if ticks == 1 {
			return "backup"
		}
		return "activation"
	}
	m, e = Activate(deps)
	if e != nil {
		t.Fatal(e)
	}
	if ticks != 2 || m.ActivatedAt != "activation" || !strings.HasSuffix(*m.BackupPath, ".crw-backup.bak") {
		t.Fatalf("clock=%+v ticks=%d", m, ticks)
	}
}

func TestActivationRecordedOracle(t *testing.T) {
	var rows struct {
		Parses []struct {
			Input      string
			Expected   json.RawMessage
			Serialized string
		}
		Preserve []struct {
			Pre, Post string
			Enabled   bool
			Expected  *string
		}
	}
	b, e := os.ReadFile("testdata/oracle-activation.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &rows); e != nil {
		t.Fatal(e)
	}
	for i, r := range rows.Parses {
		m := parseInstallManifest(r.Input)
		if string(r.Expected) == "null" {
			if m != nil {
				t.Errorf("parse %d accepted", i)
			}
			continue
		}
		if m == nil {
			t.Errorf("parse %d rejected", i)
			continue
		}
		b, e := manifestBytes(m)
		if e != nil {
			t.Fatal(e)
		}
		if r.Serialized != "" && string(b) != r.Serialized {
			t.Errorf("parse %d exact bytes:\ngot %s\nwant %s", i, b, r.Serialized)
		}
		var got, want any
		if e = json.Unmarshal(b, &got); e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(r.Expected, &want); e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parse %d: got%v want%v", i, got, want)
		}
	}
	for i, r := range rows.Preserve {
		got, ok := PreserveMultiAgentV2Table(r.Pre, r.Post, r.Enabled)
		if r.Expected == nil {
			if ok {
				t.Errorf("preserve%d unexpected%q", i, got)
			}
		} else if !ok || got != *r.Expected {
			t.Errorf("preserve%d pre%q post%q enabled%v: got%q/%v want%q", i, r.Pre, r.Post, r.Enabled, got, ok, *r.Expected)
		}
	}
}
func TestPreserveMultiAgentV2TableCRLF(t *testing.T) {
	pre := "[features.multi_agent_v2]\r\nenabled = false\r\nmax_concurrent_threads_per_session = 7\r\n"
	got, ok := PreserveMultiAgentV2Table(pre, "[features]\r\nmulti_agent_v2 = true\r\n")
	want := "[features]\r\n\r\n[features.multi_agent_v2]\r\nenabled = true\r\nmax_concurrent_threads_per_session = 7\r\n"
	if !ok || got != want {
		t.Fatalf("got%q/%v want%q", got, ok, want)
	}
}
