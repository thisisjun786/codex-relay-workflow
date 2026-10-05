package doctor

// This file replays testdata/harness/install/oracle.json over the harness install checks and ports
// the B tests of CXC v0.2.40 cxc-ops/test/cxc-ops.test.ts (install-root FAIL/PASS/WARN, the clean
// pabcd PASS, the corrupt-file WARN with its repair) and the D6/D7/D8 integration tests of
// manifest-targets.test.ts. The recorded answers keep the oracle spelling; before comparing, an
// expectation goes through the names decision (decision 1,
// contract/schema/cxc/name-substitution.json), the same renames the corpus replayer applies to a
// fixture: R32 (codexclaw -> crw) and the cli table rows (cxc doctor -> crw doctor harness, cxc
// enable -> crw install features enable, cxc reset -> crw pabcd reset). The recorder's temp root
// token is substituted with this test's root.
//
// Two recorded paths are not compared byte for byte, because the oracle text cannot be
// reproduced and the difference is disclosed in the PR body instead: a malformed plugin.json
// (V8's SyntaxError message) is asserted by severity and a non-empty evidence only, and the
// non-iterable TypeError shapes the CRW-332 validator cannot carry keep their FAIL severity with
// the validator's own message.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// harnessInstallTempToken is the recorder's placeholder for its temporary root.
const harnessInstallTempToken = "$" + "{TEMP}"

// harnessInstallPluginRootToken is the oracle's manifest placeholder, kept as the validator reads
// it.
const harnessInstallPluginRootToken = "$" + "{PLUGIN_ROOT}"

type harnessInstallCheckRecorded struct {
	Name     string
	Severity string
	Evidence string
	Repair   string
}

type harnessInstallPabcdRecorded struct {
	Name     string
	Severity string
	Evidence string
	Repair   string
	Threw    string
}

type harnessInstallManifestRecorded struct {
	Name   string
	Checks []harnessInstallCheckRecorded
}

// harnessInstallOracle is testdata/harness/install/oracle.json; the field names match its keys
// case-insensitively.
type harnessInstallOracle struct {
	Oracle            string
	Dist              string
	Node              string
	Pabcd             []harnessInstallPabcdRecorded
	ManifestTargets   []harnessInstallManifestRecorded
	InstallRoot       []harnessInstallCheckRecorded
	MalformedManifest harnessInstallCheckRecorded
}

func harnessInstallOracleRecorded(t *testing.T) harnessInstallOracle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "harness", "install", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded harnessInstallOracle
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded.Oracle, "3c1459ac") {
		t.Fatalf("oracle.json names %q, want the CXC v0.2.40 commit", recorded.Oracle)
	}
	if len(recorded.Pabcd) == 0 || len(recorded.ManifestTargets) == 0 || len(recorded.InstallRoot) == 0 {
		t.Fatalf("oracle.json holds an empty group: %d pabcd, %d manifest, %d install-root",
			len(recorded.Pabcd), len(recorded.ManifestTargets), len(recorded.InstallRoot))
	}
	return recorded
}

// harnessInstallRenamed is the oracle text with the names decision applied, the renames the
// corpus replayer applies to an expectation (contract/schema/cxc/name-substitution.json).
func harnessInstallRenamed(text string) string {
	return strings.NewReplacer(
		"codexclaw", "crw",
		"cxc doctor", "crw doctor harness",
		"cxc enable", "crw install features enable",
		"cxc reset", "crw pabcd reset",
	).Replace(text)
}

// harnessInstallExpected substitutes this test's temp root for the recorder's token and applies
// the names decision.
func harnessInstallExpected(recorded, root string) string {
	return harnessInstallRenamed(strings.ReplaceAll(recorded, harnessInstallTempToken, root))
}

// harnessInstallGot is one ported check in the recorded shape.
func harnessInstallGot(check HarnessCheck) harnessInstallCheckRecorded {
	return harnessInstallCheckRecorded{Name: check.Name, Severity: string(check.Severity), Evidence: check.Evidence, Repair: check.Repair}
}

// harnessInstallRecover runs one check and answers the panic value as an error, the port's shape
// of the oracle's uncaught throw.
func harnessInstallRecover(run func()) (thrown error) {
	defer func() {
		if value := recover(); value != nil {
			if err, ok := value.(error); ok {
				thrown = err
				return
			}
			thrown = errors.New(fmt.Sprint(value))
		}
	}()
	run()
	return nil
}

// harnessInstallValue is the JSON text of one value, as JSON.stringify writes it.
func harnessInstallValue(value any) string {
	// json.Marshal of these test documents cannot fail.
	raw, _ := json.Marshal(value)
	return string(raw)
}

func harnessInstallSessions(t *testing.T, ws string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(ws, ".crw", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// harnessInstallPabcdWorkspace rebuilds the tree of one recorded pabcd case.
func harnessInstallPabcdWorkspace(t *testing.T, root, name string) string {
	t.Helper()
	ws := filepath.Join(root, "ws-"+name)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	switch name {
	case "clean":
		harnessInstallSessions(t, ws, map[string]string{"rec-s1.json": harnessInstallValue(map[string]string{"phase": "P"})})
		if err := os.WriteFile(filepath.Join(ws, ".crw", "sessions", "note.txt"), []byte("not a session"), 0o644); err != nil {
			t.Fatal(err)
		}
	case "corrupt_one":
		harnessInstallSessions(t, ws, map[string]string{"bad.json": "{oops}"})
	case "corrupt_two":
		harnessInstallSessions(t, ws, map[string]string{"a.json": "]", "b.json": "{"})
	case "entry_is_directory":
		if err := os.MkdirAll(filepath.Join(ws, ".crw", "sessions", "dir.json"), 0o755); err != nil {
			t.Fatal(err)
		}
	case "deep_10001":
		harnessInstallSessions(t, ws, map[string]string{"deep.json": strings.Repeat("[", 10001) + strings.Repeat("]", 10001)})
	case "invalid_session_name":
		// One ill-formed byte in the entry name, as Node's readdirSync sees it.
		dir := filepath.Join(ws, ".crw", "sessions")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, string([]byte{'b', 0xff})+".json"), []byte("{oops}"), 0o644); err != nil {
			t.Fatal(err)
		}
	case "unreadable":
		harnessInstallSessions(t, ws, map[string]string{"rec-s1.json": harnessInstallValue(map[string]string{"phase": "P"})})
		if err := os.Chmod(filepath.Join(ws, ".crw", "sessions"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(ws, ".crw", "sessions"), 0o755) })
		if _, err := os.ReadDir(filepath.Join(ws, ".crw", "sessions")); err == nil {
			// root or CAP_DAC_OVERRIDE: mode 000 does not make the directory unreadable, so the
			// recorded panic is unreachable in this environment.
			t.Skip("the sessions directory stays readable despite mode 000 (privileged user)")
		}
	}
	return ws
}

func TestHarnessInstallPabcdRecorded(t *testing.T) {
	for _, recorded := range harnessInstallOracleRecorded(t).Pabcd {
		t.Run(recorded.Name, func(t *testing.T) {
			root := t.TempDir()
			ws := harnessInstallPabcdWorkspace(t, root, recorded.Name)
			if recorded.Threw != "" {
				want := harnessInstallExpected(recorded.Threw, root)
				thrown := harnessInstallRecover(func() { HarnessPabcdCheck(ws) })
				if thrown == nil {
					t.Fatalf("HarnessPabcdCheck(%s) did not throw, want %q", recorded.Name, want)
				}
				if thrown.Error() != want {
					t.Fatalf("HarnessPabcdCheck(%s) thrown = %q, want %q", recorded.Name, thrown.Error(), want)
				}
				return
			}
			want := harnessInstallCheckRecorded{
				Name:     "pabcd-state",
				Severity: recorded.Severity,
				Evidence: harnessInstallExpected(recorded.Evidence, root),
				Repair:   harnessInstallExpected(recorded.Repair, root),
			}
			if got := harnessInstallGot(HarnessPabcdCheck(ws)); got != want {
				t.Fatalf("HarnessPabcdCheck(%s) = %+v, want %+v", recorded.Name, got, want)
			}
		})
	}
}

// harnessInstallManifestPayload rebuilds the payload of one recorded manifest-target case.
func harnessInstallManifestPayload(t *testing.T, root, name string) string {
	t.Helper()
	payload := filepath.Join(root, "payload-"+name)
	if err := os.MkdirAll(filepath.Join(payload, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(manifest string) {
		if err := os.WriteFile(filepath.Join(payload, ".codex-plugin", "plugin.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile := func(rel, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(payload, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(payload, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hookDoc := func(command string) string {
		return harnessInstallValue(map[string]any{"hooks": map[string]any{"Stop": []any{
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command}}},
		}}})
	}
	mcpDoc := func(arg string) string {
		return harnessInstallValue(map[string]any{"mcpServers": map[string]any{"bridge": map[string]any{"command": "node", "args": []any{arg}}}})
	}
	manifestHooks := harnessInstallValue(map[string]any{"name": "crw", "version": "0.4.0", "hooks": []any{"./hooks.json"}})
	manifestMCP := harnessInstallValue(map[string]any{"name": "crw", "version": "0.4.0", "mcpServers": "./.mcp.json"})
	switch name {
	case "missing_hook_target":
		write(manifestHooks)
		writeFile("hooks.json", hookDoc("node "+harnessInstallPluginRootToken+"/dist/missing.js"))
	case "missing_mcp_target":
		write(manifestMCP)
		writeFile(".mcp.json", mcpDoc(harnessInstallPluginRootToken+"/dist/missing.js"))
	case "empty_target":
		write(manifestHooks)
		writeFile("dist/empty.js", "")
		writeFile("hooks.json", hookDoc("node "+harnessInstallPluginRootToken+"/dist/empty.js"))
	case "escapes_root":
		write(manifestHooks)
		writeFile("hooks.json", hookDoc("node "+harnessInstallPluginRootToken+"/../outside.js"))
	case "unparseable_hook":
		write(manifestHooks)
		writeFile("hooks.json", "not json")
	case "unparseable_mcp":
		write(manifestMCP)
		writeFile(".mcp.json", "not json")
	case "nonparse_failure":
		write(manifestHooks)
		if err := os.MkdirAll(filepath.Join(payload, "hooks.json"), 0o755); err != nil {
			t.Fatal(err)
		}
	case "null_manifest":
		write("null")
	default:
		t.Fatalf("no payload builder for %q", name)
	}
	return payload
}

func TestHarnessInstallManifestTargetsRecorded(t *testing.T) {
	for _, recorded := range harnessInstallOracleRecorded(t).ManifestTargets {
		t.Run(recorded.Name, func(t *testing.T) {
			root := t.TempDir()
			payload := harnessInstallManifestPayload(t, root, recorded.Name)
			got := HarnessManifestTargetChecks(payload)
			if len(got) != len(recorded.Checks) {
				t.Fatalf("HarnessManifestTargetChecks(%s) = %d checks, want %d: %+v", recorded.Name, len(got), len(recorded.Checks), got)
			}
			for i, want := range recorded.Checks {
				expected := harnessInstallCheckRecorded{
					Name:     want.Name,
					Severity: want.Severity,
					Evidence: harnessInstallExpected(want.Evidence, root),
					Repair:   harnessInstallExpected(want.Repair, root),
				}
				if check := harnessInstallGot(got[i]); check != expected {
					t.Fatalf("check %d = %+v, want %+v", i, check, expected)
				}
			}
		})
	}
}

// harnessInstallPayloadAt writes one payload root; an empty manifest leaves the file absent.
func harnessInstallPayloadAt(t *testing.T, root, name, manifest string) string {
	t.Helper()
	dir := filepath.Join(root, "root-"+name)
	if err := os.MkdirAll(filepath.Join(dir, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, ".codex-plugin", "plugin.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// harnessInstallHomeAt writes one codex home whose cache holds the named roots.
func harnessInstallHomeAt(t *testing.T, root, name string, entries [][]string) string {
	t.Helper()
	home := filepath.Join(root, "home-"+name)
	if err := os.MkdirAll(filepath.Join(home, "plugins", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.MkdirAll(filepath.Join(home, "plugins", "cache", entry[0], entry[1], entry[2]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// harnessInstallUnsetEnv removes one variable and restores its presence on cleanup, so the
// "variable absent" branch is reachable without touching the host.
func harnessInstallUnsetEnv(t *testing.T, key string) {
	t.Helper()
	if value, ok := os.LookupEnv(key); ok {
		t.Cleanup(func() { _ = os.Setenv(key, value) })
	} else {
		t.Cleanup(func() { _ = os.Unsetenv(key) })
	}
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessInstallRootRecorded(t *testing.T) {
	for _, recorded := range harnessInstallOracleRecorded(t).InstallRoot {
		t.Run(recorded.Name, func(t *testing.T) {
			root := t.TempDir()
			name := recorded.Name
			manifest := harnessInstallValue(map[string]any{"name": "crw", "version": "0.4.0"})
			payload := func(body string) string { return harnessInstallPayloadAt(t, root, name, body) }
			home := func(cache string, entries [][]string) HarnessOptions {
				return HarnessOptions{CodexHome: harnessInstallHomeAt(t, root, cache, entries)}
			}
			var check HarnessCheck
			switch name {
			case "matching_root":
				check = HarnessInstalledRootCheck(payload(manifest), home(name, [][]string{{"local", "crw", "0.4.0"}}))
			case "stale_root":
				check = HarnessInstalledRootCheck(payload(manifest), home(name, [][]string{{"local", "crw", "0.1.0"}}))
			case "matching_and_stale":
				check = HarnessInstalledRootCheck(payload(manifest), home(name, [][]string{{"local", "crw", "0.1.0"}, {"mkt", "crw", "0.4.0"}}))
			case "two_stale":
				check = HarnessInstalledRootCheck(payload(manifest), home(name, [][]string{{"mkt1", "crw", "0.1.0"}, {"mkt2", "crw", "0.2.0"}}))
			case "no_cache":
				check = HarnessInstalledRootCheck(payload(manifest), HarnessOptions{CodexHome: filepath.Join(root, "empty-home")})
			case "not_installed":
				check = HarnessInstalledRootCheck(payload(manifest), home(name, [][]string{{"mkt", "other", "1.0.0"}}))
			case "no_name_version":
				check = HarnessInstalledRootCheck(payload("{}"), home(name, nil))
			case "manifest_absent":
				check = HarnessInstalledRootCheck(payload(""), home(name, nil))
			case "manifest_null":
				check = HarnessInstalledRootCheck(payload("null"), home(name, nil))
			case "linked_root":
				options := home(name, [][]string{{"mkt", "crw", "0.4.0"}})
				elsewhere := filepath.Join(root, "elsewhere-"+name)
				if err := os.MkdirAll(elsewhere, 0o755); err != nil {
					t.Fatal(err)
				}
				entry := filepath.Join(options.CodexHome, "plugins", "cache", "mkt", "crw", "0.4.0")
				if err := os.Remove(entry); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(elsewhere, entry); err != nil {
					t.Fatal(err)
				}
				check = HarnessInstalledRootCheck(payload(manifest), options)
			case "entry_is_file":
				options := home(name, nil)
				if err := os.MkdirAll(filepath.Join(options.CodexHome, "plugins", "cache", "mkt2"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(options.CodexHome, "plugins", "cache", "mkt2", "crw"), []byte("not a dir"), 0o644); err != nil {
					t.Fatal(err)
				}
				check = HarnessInstalledRootCheck(payload(manifest), options)
			case "env_empty":
				t.Setenv("CODEX_HOME", "")
				check = HarnessInstalledRootCheck(payload(manifest), HarnessOptions{})
			case "home_empty":
				harnessInstallUnsetEnv(t, "CODEX_HOME")
				t.Setenv("HOME", "")
				check = HarnessInstalledRootCheck(payload(manifest), HarnessOptions{})
			case "option_wins":
				t.Setenv("CODEX_HOME", harnessInstallHomeAt(t, root, name+"_env", [][]string{{"mkt", "crw", "0.4.0"}}))
				check = HarnessInstalledRootCheck(payload(manifest), HarnessOptions{CodexHome: filepath.Join(root, "option-empty-home")})
			case "env_wins_over_home":
				t.Setenv("CODEX_HOME", harnessInstallHomeAt(t, root, name, [][]string{{"mkt", "crw", "0.4.0"}}))
				t.Setenv("HOME", filepath.Join(root, "home-no-cache"))
				check = HarnessInstalledRootCheck(payload(manifest), HarnessOptions{})
			case "surrogate_name", "surrogate_missing":
				// The manifest text holds the JSON escape for a lone surrogate; Go source cannot spell it
				// as a unicode escape, so it is assembled. The cache directory is named with U+FFFD, which
				// is the name Node asks the filesystem for.
				surrogate := harnessInstallValue(map[string]string{"name": "SURROGATE", "version": "0.4.0"})
				surrogate = strings.Replace(surrogate, "SURROGATE", "x"+string([]byte{0x5c})+"ud800", 1)
				entries := [][]string{{"mkt", "x\uFFFD", "0.4.0"}}
				if name == "surrogate_missing" {
					entries = [][]string{{"mkt", "other", "0.4.0"}}
				}
				check = HarnessInstalledRootCheck(payload(surrogate), home(name, entries))
			case "invalid_market_name":
				// One ill-formed byte in the market segment: the oracle decodes it to U+FFFD and then
				// misses the directory, so the plugin reads as not installed.
				options := home(name, nil)
				if err := os.MkdirAll(filepath.Join(options.CodexHome, "plugins", "cache", string([]byte{'m', 0xff}), "crw", "0.1.0"), 0o755); err != nil {
					t.Fatal(err)
				}
				check = HarnessInstalledRootCheck(payload(manifest), options)
			default:
				t.Fatalf("no builder for %q", name)
			}
			got := harnessInstallGot(check)
			want := harnessInstallCheckRecorded{
				Name:     "install-root",
				Severity: recorded.Severity,
				Evidence: harnessInstallExpected(recorded.Evidence, root),
				Repair:   harnessInstallExpected(recorded.Repair, root),
			}
			if got != want {
				t.Fatalf("HarnessInstalledRootCheck(%s) = %+v, want %+v", name, got, want)
			}
		})
	}
}

// ---- the direct cases: the injected seams and the disclosed differences -------------------

func TestHarnessInstallCodexHomePort(t *testing.T) {
	lookup := func(values map[string]string) record.Environ {
		return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	}
	passwdHome := func() (string, error) { return "/synthetic", nil }
	passwdFail := func() (string, error) { return "", errors.New("home not set and passwd unreadable") }
	for _, test := range []struct {
		name      string
		codexHome string
		values    map[string]string
		passwd    func() (string, error)
		want      string
		wantErr   bool
	}{
		{"option wins verbatim", "/option", map[string]string{"CODEX_HOME": "/env", "HOME": "/home"}, passwdHome, "/option", false},
		{"env wins verbatim", "", map[string]string{"CODEX_HOME": "/env", "HOME": "/home"}, passwdHome, "/env", false},
		{"empty env is used", "", map[string]string{"CODEX_HOME": "", "HOME": "/home"}, passwdHome, "", false},
		{"home gets the codex suffix", "", map[string]string{"HOME": "/home"}, passwdHome, "/home/.codex", false},
		{"empty home stays relative", "", map[string]string{"HOME": ""}, passwdHome, ".codex", false},
		{"passwd gets the codex suffix", "", map[string]string{}, passwdHome, "/synthetic/.codex", false},
		{"passwd failure is the error", "", map[string]string{}, passwdFail, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := harnessInstallCodexHome(test.codexHome, lookup(test.values), test.passwd)
			if test.wantErr {
				if err == nil {
					t.Fatalf("harnessInstallCodexHome = %q, want the passwd error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("harnessInstallCodexHome = %q, want %q", got, test.want)
			}
		})
	}
}

// The resolver error must panic before any manifest read, as the oracle's homedir throw
// propagates before runInstalledRootCheck reads its payload (audit round 3).
func TestHarnessInstallResolverPanicsBeforeManifest(t *testing.T) {
	read := false
	thrown := harnessInstallRecover(func() {
		harnessInstallInstalledRootCheck("/payload", func() (string, error) { return "", errors.New("no home") },
			func(string) ([]byte, error) { read = true; return nil, errors.New("the manifest must not be read") })
	})
	if thrown == nil || thrown.Error() != "no home" {
		t.Fatalf("thrown = %v, want the resolver error", thrown)
	}
	if read {
		t.Fatal("the manifest was read before the resolver error propagated")
	}

	// The positive control proves the readFile seam is the one the check body uses: a failing read
	// is the catch clause's WARN, as the oracle answers one.
	check := harnessInstallInstalledRootCheck("/payload", func() (string, error) { return "/home", nil },
		func(string) ([]byte, error) { read = true; return nil, errors.New("boom") })
	if !read {
		t.Fatal("the check body did not read the manifest through its seam")
	}
	if check.Name != "install-root" || check.Severity != HarnessWarn {
		t.Fatalf("check = %+v, want the install-root WARN", check)
	}
}

// A malformed install manifest is the one recorded case whose oracle message (V8's SyntaxError
// text) the port does not reproduce; the severity and the check shape stay parity-identical.
func TestHarnessInstallMalformedManifestPort(t *testing.T) {
	recorded := harnessInstallOracleRecorded(t).MalformedManifest
	root := t.TempDir()
	check := HarnessInstalledRootCheck(harnessInstallPayloadAt(t, root, "malformed_manifest", "not json"), HarnessOptions{CodexHome: filepath.Join(root, "home")})
	if string(check.Severity) != recorded.Severity || check.Name != "install-root" || check.Evidence == "" {
		t.Fatalf("malformed manifest check = %+v, want the recorded %s install-root check", check, recorded.Severity)
	}
	if check.Evidence == harnessInstallExpected(recorded.Evidence, root) {
		t.Fatalf("evidence %q equals the recorded V8 message; the port keeps its own parse text (disclosed difference)", check.Evidence)
	}
}

// The corrupt-session WARN renders its repair line, as cxc-ops.test.ts expects of a non-PASS
// check (the report text is CRW-346's; this pins the ported check's contribution to it).
func TestHarnessInstallCorruptRepairRendersPort(t *testing.T) {
	root := t.TempDir()
	ws := harnessInstallPabcdWorkspace(t, root, "corrupt_one")
	check := HarnessPabcdCheck(ws)
	if check.Severity != HarnessWarn || check.Repair != "crw pabcd reset --state" {
		t.Fatalf("check = %+v, want the corrupt-session WARN with the reset repair", check)
	}
	text := RenderHarnessReport(HarnessReport{SchemaVersion: HarnessSchemaVersion, Overall: HarnessWarn, Checks: []HarnessCheck{check}})
	if !strings.Contains(text, "    repair: crw pabcd reset --state") {
		t.Fatalf("rendered report misses the repair line:\n%s", text)
	}
}
