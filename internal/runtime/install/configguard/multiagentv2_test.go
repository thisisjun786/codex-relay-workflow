package configguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func multiAgentHome(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "codex")
	for key, dir := range map[string]string{"HOME": root, "CODEX_HOME": home, "CRW_HOME": filepath.Join(root, "crw")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, dir)
	}
	return home, filepath.Join(home, "config.toml")
}

func multiAgentFake(t *testing.T, path string, calls *[][]string) CodexRunner {
	t.Helper()
	return func(args []string) CodexRunResult {
		*calls = append(*calls, append([]string(nil), args...))
		value := "false"
		if args[1] == "enable" {
			value = "true"
		}
		activationWrite(t, path, "[features]\nmulti_agent_v2 = "+value+"\n")
		return CodexRunResult{}
	}
}

// The two B-class cases are multi-agent-v2.test.ts:35-58 and :60-97.
func TestMultiAgentV2ReadForms(t *testing.T) {
	_, path := multiAgentHome(t)
	for _, body := range []string{
		"[features.multi_agent_v2]\nenabled = true\n",
		"[features]\nmulti_agent_v2 = true\n",
		"[features]\nmulti_agent_v2 = { enabled = true, max_concurrent_threads_per_session = 4 }\n",
	} {
		activationWrite(t, path, body)
		if !IsMultiAgentV2Enabled(path) {
			t.Fatalf("not enabled: %q", body)
		}
	}
	activationWrite(t, path, "[features]\nmulti_agent_v2 = false\n")
	state := ReadMultiAgentV2State(MultiAgentV2Deps{ConfigPath: &path})
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":"v1","v2Enabled":false,"appliesTo":"flag-fallback models only","catalogPinned":{"v2":["gpt-5.6-sol","gpt-5.6-terra"],"v1":["gpt-5.6-luna"]},"effectiveFrom":"new sessions"}`
	if string(raw) != want {
		t.Fatalf("state = %s", raw)
	}
}

func TestMultiAgentV2TogglePreservesTable(t *testing.T) {
	home, path := multiAgentHome(t)
	activationWrite(t, path, "[features.multi_agent_v2]\nenabled = false\nmax_concurrent_threads_per_session = 7\n")
	var calls [][]string
	deps := MultiAgentV2Deps{CodexHome: home, Run: multiAgentFake(t, path, &calls)}
	for _, version := range []MultiAgentVersion{MultiAgentV2, MultiAgentV1} {
		state, err := SetMultiAgentV2State(deps, version)
		if err != nil || state.Version != version || state.V2Enabled != (version == MultiAgentV2) || !state.Changed {
			t.Fatalf("toggle %s = %+v, %v", version, state, err)
		}
		if state.MultiAgentV2Context != MultiAgentV2StatusContext() {
			t.Fatalf("context = %+v", state)
		}
		want := "[features]\n\n[features.multi_agent_v2]\nenabled = " + strconvBool(version == MultiAgentV2) + "\nmax_concurrent_threads_per_session = 7\n"
		if got := activationRead(t, path); got != want {
			t.Fatalf("config = %q, want %q", got, want)
		}
	}
	if !reflect.DeepEqual(calls, [][]string{{"features", "enable", "multi_agent_v2"}, {"features", "disable", "multi_agent_v2"}}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestMultiAgentV2OracleGrammar(t *testing.T) {
	cases := []struct {
		name, body string
		enabled    bool
	}{
		{"absent-key", "[features]\nother = true\n", false},
		{"table-shadows", "[features]\nmulti_agent_v2 = true\n[features.multi_agent_v2]\nmax = 7\n", false},
		{"first-scalar", "[features]\nmulti_agent_v2 = false\nmulti_agent_v2 = true\n", false},
		{"inline-substring", "[features]\nmulti_agent_v2 = { not_enabled = true }\n", true},
		{"inline-prefix", "[features]\nmulti_agent_v2 = { enabled = trueish }\n", true},
		{"scalar-prefix", "[features]\nmulti_agent_v2 = trueish\n", false},
		{"inside-string", "[features.multi_agent_v2]\nnote = \"\"\"\nenabled = true\n\"\"\"\n", true},
		{"header-in-string", "note = \"\"\"\n[features.multi_agent_v2]\nenabled = true\n\"\"\"\n", true},
		{"comment", "[features] # header\nmulti_agent_v2 = true # value\n", true},
		{"unicode", "[features]\nmulti_agent_v2\u00a0=\u00a0true\n", true},
		{"line-separator", "[features]\nother = 1\u2028multi_agent_v2 = true\n", true},
		{"inline-multiline", "[features]\nmulti_agent_v2 = {\n enabled = true\n}\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, path := multiAgentHome(t)
			for _, body := range []string{c.body, strings.ReplaceAll(c.body, "\n", "\r\n")} {
				activationWrite(t, path, body)
				if got := IsMultiAgentV2Enabled(path); got != c.enabled {
					t.Fatalf("enabled = %v for %q", got, body)
				}
			}
		})
	}
}

func TestMultiAgentV2PathsNoopAndCatalog(t *testing.T) {
	home, path := multiAgentHome(t)
	deps := MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult { t.Fatal("unexpected runner"); return CodexRunResult{} }}
	if IsMultiAgentV2Enabled(path) || ReadMultiAgentV2State(deps).Version != MultiAgentV1 {
		t.Fatal("missing file must mean v1")
	}
	if got, err := SetMultiAgentV2State(deps, MultiAgentV1); err != nil || got.Changed {
		t.Fatalf("missing no-op = %+v, %v", got, err)
	}
	activationWrite(t, path, "[features]\nmulti_agent_v2 = true\n")
	if got, err := SetMultiAgentV2State(deps, MultiAgentV2); err != nil || got.Changed {
		t.Fatalf("enabled no-op = %+v, %v", got, err)
	}
	empty, absent := "", path+".absent"
	for _, override := range []*string{&empty, &absent} {
		deps.ConfigPath = override
		if ReadMultiAgentV2State(deps).V2Enabled {
			t.Fatal("explicit path incorrectly defaulted")
		}
	}
	pins := MultiAgentV2CatalogPinned()
	pins.V2[0] = "caller-change"
	if MultiAgentV2CatalogPinned().V2[0] != "gpt-5.6-sol" {
		t.Fatal("catalog accessor leaked caller mutation")
	}
}

func TestMultiAgentV2RunnerOutcomes(t *testing.T) {
	for _, mode := range []string{"failure", "absent", "stale", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			home, path := multiAgentHome(t)
			deps := MultiAgentV2Deps{CodexHome: home, Run: func(args []string) CodexRunResult {
				if !reflect.DeepEqual(args, []string{"features", "enable", "multi_agent_v2"}) {
					t.Fatalf("args = %v", args)
				}
				switch mode {
				case "failure":
					return CodexRunResult{ExitCode: 7, Stderr: "\u00a0bad flag\ufeff\n"}
				case "stale":
					activationWrite(t, path, "[features]\nmulti_agent_v2 = false\n")
				case "unreadable":
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				return CodexRunResult{}
			}}
			got, err := SetMultiAgentV2State(deps, MultiAgentV2)
			if mode == "failure" {
				if err == nil || err.Error() != "codex features enable multi_agent_v2 failed (exit 7): bad flag" {
					t.Fatalf("failure = %v", err)
				}
			} else if mode == "unreadable" {
				if err == nil {
					t.Fatal("post read failure swallowed")
				}
			} else if err != nil || !got.Changed || got.Version != MultiAgentV1 || got.V2Enabled {
				t.Fatalf("observed state = %+v, %v", got, err)
			}
		})
	}
}

// Intentionally changed: refuse lost pre-images before even the no-op/runner.
func TestMultiAgentV2UnreadableRefused(t *testing.T) {
	for _, kind := range []string{"directory", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			home, path := multiAgentHome(t)
			var err error
			if kind == "directory" {
				err = os.Mkdir(path, 0700)
			} else {
				err = os.Symlink("missing-target", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, version := range []MultiAgentVersion{MultiAgentV1, MultiAgentV2} {
				_, err := SetMultiAgentV2State(MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult { t.Fatal("runner reached unreadable config"); return CodexRunResult{} }}, version)
				if err == nil {
					t.Fatal("unreadable pre-image accepted")
				}
			}
			if IsMultiAgentV2Enabled(path) {
				t.Fatal("forgiving reader must mean v1")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("unreadable path changed", err)
			}
		})
	}
}

// Intentionally changed: repair publishes by rename, preserving old inode data.
func TestMultiAgentV2AtomicRepair(t *testing.T) {
	home, path := multiAgentHome(t)
	activationWrite(t, path, "[features.multi_agent_v2]\nenabled = false\nmax = 7\n")
	alias := path + ".hardlink"
	var prior os.FileInfo
	deps := MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult {
		activationWrite(t, path, "[features]\nmulti_agent_v2 = true\n")
		if err := os.Link(path, alias); err != nil {
			t.Fatal(err)
		}
		var err error
		prior, err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return CodexRunResult{}
	}}
	if _, err := SetMultiAgentV2State(deps, MultiAgentV2); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(prior, after) || after.Mode().Perm() != 0600 || activationRead(t, alias) != "[features]\nmulti_agent_v2 = true\n" {
		t.Fatal("repair rewrote the old inode or changed mode")
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary debris: %v, %v", entries, err)
	}
}

func TestMultiAgentV2SymlinkAndRepairFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconvBool(fail), func(t *testing.T) {
			home, path := multiAgentHome(t)
			target := filepath.Join(home, "target.toml")
			activationWrite(t, target, "[features.multi_agent_v2]\nenabled = false\nmax = 7\n")
			if err := os.Symlink("target.toml", path); err != nil {
				t.Fatal(err)
			}
			deps := MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult {
				activationWrite(t, target, "[features]\nmulti_agent_v2 = true\n")
				if fail {
					if err := os.Chmod(target, 0400); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(target, 0600) })
				}
				return CodexRunResult{}
			}}
			_, err := SetMultiAgentV2State(deps, MultiAgentV2)
			if fail && os.Geteuid() == 0 {
				t.Skip("root bypasses read-only permissions")
			}
			if (err != nil) != fail {
				t.Fatalf("repair error = %v", err)
			}
			if link, err := os.Readlink(path); err != nil || link != "target.toml" {
				t.Fatal("symlink replaced", err)
			}
			if fail && activationRead(t, target) != "[features]\nmulti_agent_v2 = true\n" {
				t.Fatal("failed repair changed runner bytes")
			}
		})
	}
}

// Intentionally changed: string value bytes lost by the oracle repair survive.
func TestMultiAgentV2MultilineSettingsPreserved(t *testing.T) {
	for _, quote := range []string{`"""`, `'''`} {
		for _, postOnly := range []bool{false, true} {
			t.Run(quote+strconvBool(postOnly), func(t *testing.T) {
				home, path := multiAgentHome(t)
				value := "note = " + quote + "  \r\n  indented\n\r\n\r\nenabled = text\n# string text\n[fake]\n" + quote
				pre := "[features.multi_agent_v2]\nenabled = false\nmax = 7\n"
				post := "# CRW_MULTI_AGENT_STRING_ collision\n[features]\nmulti_agent_v2 = true\n"
				if postOnly {
					post += "[other]\n" + value + "\n"
				} else {
					pre += value + "\n"
				}
				activationWrite(t, path, pre)
				deps := MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult { activationWrite(t, path, post); return CodexRunResult{} }}
				state, err := SetMultiAgentV2State(deps, MultiAgentV2)
				if err != nil || !state.V2Enabled || !strings.Contains(activationRead(t, path), value) {
					t.Fatalf("string lost: %q, %v", activationRead(t, path), err)
				}
			})
		}
	}
}

func TestMultiAgentV2RepairBoundaries(t *testing.T) {
	for _, mode := range []string{"post-table", "opener-eof", "crlf-only-in-string"} {
		t.Run(mode, func(t *testing.T) {
			home, path := multiAgentHome(t)
			pre := "[features.multi_agent_v2]\nenabled = false\nmax = 7\n"
			post := "[features]\nmulti_agent_v2 = true\n"
			value := "note = \"\"\"  "
			switch mode {
			case "post-table":
				post = "[features.multi_agent_v2]\nenabled = true\nnote = \"\"\"\n\n\n# preserve\n\"\"\"\n"
			case "opener-eof":
				pre += value
			case "crlf-only-in-string":
				value = "note = \"\"\"\r\n text\r\n\"\"\""
				pre += value
			}
			activationWrite(t, path, pre)
			var inode os.FileInfo
			deps := MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult {
				activationWrite(t, path, post)
				var err error
				inode, err = os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				return CodexRunResult{}
			}}
			got, err := SetMultiAgentV2State(deps, MultiAgentV2)
			if err != nil || !got.V2Enabled {
				t.Fatalf("state = %+v, %v", got, err)
			}
			content := activationRead(t, path)
			if mode == "post-table" {
				after, err := os.Stat(path)
				if err != nil || !os.SameFile(inode, after) || content != post {
					t.Fatal("no-repair branch rewrote post", err)
				}
			} else if !strings.Contains(content, value) {
				t.Fatalf("value lost = %q", content)
			}
			if mode == "crlf-only-in-string" && !strings.Contains(content, "[features.multi_agent_v2]\r\nenabled = true\r\n") {
				t.Fatal("helper EOL choice changed")
			}
			raw, err := json.Marshal(got)
			if err != nil || !strings.HasPrefix(string(raw), `{"version":"v2","v2Enabled":true,"changed":true,"appliesTo":`) {
				t.Fatalf("change JSON = %s, %v", raw, err)
			}
		})
	}
}
