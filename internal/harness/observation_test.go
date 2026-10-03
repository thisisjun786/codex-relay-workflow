package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func lookup(m map[string]string) host.LookupEnv {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// hookEnv is a CODEX_HOME and a plugin root (with its manifest) under a temporary directory.
func hookEnv(t *testing.T) (env map[string]string, codexHome, plugin string) {
	t.Helper()
	root := t.TempDir()
	codexHome, plugin = filepath.Join(root, "codex"), filepath.Join(root, "plugin")
	if err := os.MkdirAll(filepath.Join(plugin, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, ".codex-plugin", "plugin.json"), []byte(`{"name":"crw","version":"1.2.3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"CODEX_HOME": codexHome, "PLUGIN_ROOT": plugin}, codexHome, plugin
}

func records(t *testing.T, codexHome string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(codexHome, "crw", "hook-observations"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestRecordInvocationWritesTheOraclesRecord(t *testing.T) {
	env, home, plugin := hookEnv(t)
	if !RecordInvocation(`{"session_id":"s1","agent_id":"a\"1","agent_type":"worker"}`, "pabcd-state", "stop", lookup(env)) {
		t.Fatal("not recorded")
	}
	manifest := `{"name":"crw","version":"1.2.3"}`
	real, _ := filepath.EvalSymlinks(plugin)
	dir := filepath.Join(home, "crw", "hook-observations", sum("s1"), sum(`"a\"1"`))
	file := filepath.Join(dir, sum(`["pabcd-state","stop",".codex-plugin/plugin.json"]`)+".json")
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	want := `^\{"schemaVersion":1,"sessionId":"s1","agentId":"a\\"1","component":"pabcd-state","event":"stop","observedAt":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z","pluginRoot":"` +
		regexp.QuoteMeta(real) + `","pluginVersion":"1.2.3","manifestDigest":"` + sum(manifest) + `","entrypoint":".codex-plugin/plugin.json","entrypointDigest":"` + sum(manifest) + `","outcome":"invoked"\}\n$`
	if !regexp.MustCompile(want).Match(got) {
		t.Fatalf("record %q does not match %s", got, want)
	}
	for path, mode := range map[string]os.FileMode{file: 0o600, dir: 0o700, filepath.Join(home, "crw"): 0o700} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v %v, want %v", path, st, err, mode)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a temporary file stays behind: %v", entries)
	}
	if !RecordInvocation(`{"session_id":"s1","agent_id":"a\"1","agent_type":"worker"}`, "pabcd-state", "stop", lookup(env)) {
		t.Fatal("second call not recorded")
	}
	if len(records(t, home)) != 1 {
		t.Errorf("the latest record replaces the earlier one in its slot: %v", records(t, home))
	}
}

func TestRecordInvocationRefusesWhatItCannotAttribute(t *testing.T) {
	env, home, _ := hookEnv(t)
	long := strings.Repeat("x", 257)
	for name, raw := range map[string]string{
		"empty":                    "",
		"not JSON":                 "{nope",
		"array":                    "[]",
		"no session":               `{}`,
		"blank session":            `{"session_id":""}`,
		"space in session":         `{"session_id":"a b"}`,
		"control byte in session":  `{"session_id":"a\u007f"}`,
		"session over 256 units":   `{"session_id":"` + long + `"}`,
		"non-string agent":         `{"session_id":"s1","agent_id":7}`,
		"agent_type without agent": `{"session_id":"s1","agent_type":"worker"}`,
		"over 4 MiB":               `{"session_id":"s1","pad":"` + strings.Repeat(" ", MaxStdinBytes) + `"}`,
		"truthy agent_type":        `{"session_id":"s1","agent_type":1}`,
		"actor id with U+FFFD":     `{"session_id":"s1","agent_id":"a\ufffd"}`,
		"lone surrogate actor":     `{"session_id":"s1","agent_id":"a\ud800"}`, // arrives as U+FFFD, which cannot tell two actors apart
	} {
		if RecordInvocation(raw, "pabcd-state", "stop", lookup(env)) {
			t.Errorf("%s: recorded", name)
		}
	}
	if RecordInvocation(`{"session_id":"s1"}`, "Bad Slug", "stop", lookup(env)) || RecordInvocation(`{"session_id":"s1"}`, "pabcd-state", "9stop", lookup(env)) {
		t.Error("a component or event that is not a slug was recorded")
	}
	if _, err := os.Stat(filepath.Join(home, "crw")); err == nil {
		t.Error("a refused record created a directory")
	}
	for name, raw := range map[string]string{
		"falsy agent_type":          `{"session_id":"s1","agent_type":0}`,
		"empty agent_type":          `{"session_id":"s1","agent_type":""}`,
		"a number no float64 holds": `{"session_id":"s1","unused":1e400}`,
	} {
		if !RecordInvocation(raw, "pabcd-state", "stop", lookup(env)) {
			t.Errorf("%s: not recorded", name)
		}
	}
}

func TestRecordInvocationNeedsAReadablePlugin(t *testing.T) {
	for name, damage := range map[string]func(env map[string]string, manifest string){
		"no PLUGIN_ROOT":   func(env map[string]string, _ string) { delete(env, "PLUGIN_ROOT") },
		"missing manifest": func(_ map[string]string, m string) { _ = os.Remove(m) },
		"no version":       func(_ map[string]string, m string) { _ = os.WriteFile(m, []byte(`{"name":"crw"}`), 0o644) },
		"symlinked manifest": func(_ map[string]string, m string) {
			_ = os.Rename(m, m+".real") // a valid manifest, reached through a link
			_ = os.Symlink(m+".real", m)
		},
	} {
		env, home, plugin := hookEnv(t)
		damage(env, filepath.Join(plugin, ".codex-plugin", "plugin.json"))
		if RecordInvocation(`{"session_id":"s1"}`, "pabcd-state", "stop", lookup(env)) {
			t.Errorf("%s: recorded", name)
		}
		if _, err := os.Stat(filepath.Join(home, "crw")); err == nil {
			t.Fatalf("%s: created %s/crw before the plugin was read", name, home)
		}
	}
}

// An input over the limit is refused before it is parsed: the oracle checks its byte length first, and a
// parse of what the limit exists to refuse costs memory in proportion to its size.
func TestRecordInvocationRefusesAnOversizedInputBeforeParsingIt(t *testing.T) {
	env, _, _ := hookEnv(t)
	raw := `{"session_id":"s1","pad":[` + strings.Repeat("1,", MaxStdinBytes/2) + `1]}`
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	recorded := RecordInvocation(raw, "pabcd-state", "stop", lookup(env))
	runtime.ReadMemStats(&after)
	if recorded || after.TotalAlloc-before.TotalAlloc > 1<<20 {
		t.Errorf("recorded %v, allocated %d bytes", recorded, after.TotalAlloc-before.TotalAlloc)
	}
}
