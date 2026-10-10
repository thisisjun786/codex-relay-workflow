package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
)

func configCommandHome(t *testing.T, content string, manifest bool) featureHome {
	t.Helper()
	h := newFeatureHome(t, content)
	h.env = h.env.With("CRW_HOME", t.TempDir())
	if manifest {
		m := map[string]any{"version": 1, "activatedAt": "2026-08-29T00:00:00.000Z", "configPath": filepath.Join(h.home, "config.toml"), "backupPath": nil, "postActivateHash": nil, "flags": map[string]any{}, "tableKeys": map[string]any{}}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.home, configguard.InstallManifestName), b, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func runConfigCommand(t *testing.T, h featureHome, args ...string) (int, string, string) {
	t.Helper()
	var out, err bytes.Buffer
	code := Main(context.Background(), append([]string{"config"}, args...), h.env, &out, &err)
	if _, e := os.Stat(filepath.Join(h.home, "calls")); !os.IsNotExist(e) {
		t.Fatalf("config invoked Codex: %v", e)
	}
	return code, out.String(), err.String()
}

func TestConfigCommandArgumentsAndReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		args     []string
		code     int
		out, err string
	}{
		{"bare", nil, 2, "Usage:\n  crw install config list", ""},
		{"help", []string{"--help"}, 0, "Usage:\n  crw install config list", ""},
		{"short help", []string{"-h"}, 0, "Usage:\n  crw install config list", ""},
		{"word help", []string{"help"}, 0, "Usage:\n  crw install config list", ""},
		{"list", []string{"list"}, 0, "memories.dedicated_tools = true\n  memories/", ""},
		{"list trailing help", []string{"list", "--help"}, 0, "Usage:\n  crw install config list", ""},
		{"get", []string{"get", "memories.dedicated_tools"}, 0, "memories.dedicated_tools = true\n", ""},
		{"padded get", []string{"get", " memories.dedicated_tools "}, 0, "memories.dedicated_tools = true\n", ""},
		{"missing id", []string{"get"}, 2, "", "config get: a <table.key> argument is required\n"},
		{"unknown missing id", []string{"frob"}, 2, "", "config frob: a <table.key> argument is required\n"},
		{"unknown action", []string{"frob", "x"}, 2, "", "config: unknown action 'frob'\n"},
		{"unknown get", []string{"get", "features.hooks"}, 2, "", "'features.hooks' is not a crw-managed key."},
		{"get help as id", []string{"get", "--help"}, 0, "Usage:\n  crw install config list", ""},
		{"set help as id", []string{"set", "--help"}, 0, "Usage:\n  crw install config list", ""},
		{"unset help as id", []string{"unset", "--help"}, 0, "Usage:\n  crw install config list", ""},
		{"invalid bool", []string{"set", "memories.dedicated_tools", "yes"}, 2, "", "config set: the value must be true or false, got 'yes'\n"},
		{"unknown set", []string{"set", "features.hooks", "true"}, 2, "", "'features.hooks' is not a crw-managed key."},
		{"unknown unset", []string{"unset", "features.hooks"}, 1, "", "config unset: 'features.hooks' is not a crw-managed key."},
		{"unrecorded unset", []string{"unset", "memories.dedicated_tools"}, 1, "", "config unset: memories.dedicated_tools is not recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := configCommandHome(t, "[memories]\ndedicated_tools = true\n", true)
			before := h.read(configguard.InstallManifestName)
			code, out, err := runConfigCommand(t, h, tc.args...)
			if code != tc.code || tc.out == "" && out != "" || tc.out != "" && !strings.HasPrefix(out, tc.out) || tc.err == "" && err != "" || tc.err != "" && !strings.HasPrefix(err, tc.err) {
				t.Fatalf("exit %d stdout=%q stderr=%q", code, out, err)
			}
			if h.read(configguard.InstallManifestName) != before || h.read("config.toml") != "[memories]\ndedicated_tools = true\n" {
				t.Fatal("read/refusal wrote settings or manifest")
			}
		})
	}
}

func TestConfigCommandSetRepeatUnset(t *testing.T) {
	t.Parallel()
	h := configCommandHome(t, "[memories]\ndedicated_tools = false # user\nforeign = true\n", true)
	for i, args := range [][]string{{"set", "memories.dedicated_tools", "true"}, {"set", "memories.dedicated_tools", "true"}, {"unset", "memories.dedicated_tools"}} {
		code, out, err := runConfigCommand(t, h, args...)
		if code != 0 || err != "" {
			t.Fatalf("step %d exit %d stdout=%q stderr=%q", i, code, out, err)
		}
		if i < 2 && !strings.HasPrefix(out, "주의: ") || i == 0 && !strings.Contains(out, "false -> true\nbackup: ") || i == 1 && !strings.Contains(out, "true -> true (already set; recorded)\n") || i == 2 && !strings.HasPrefix(out, "memories.dedicated_tools: restored to false\n") {
			t.Fatal(out)
		}
	}
	if h.read("config.toml") != "[memories]\ndedicated_tools = false # user\nforeign = true\n" {
		t.Fatal("original value/comment or foreign key lost")
	}
	var m struct {
		Version   int
		TableKeys map[string]json.RawMessage
	}
	if err := json.Unmarshal([]byte(h.read(configguard.InstallManifestName)), &m); err != nil || m.Version != 2 || len(m.TableKeys) != 0 {
		t.Fatalf("manifest %+v %v", m, err)
	}
}

func TestConfigCommandRefusalsAndHome(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"[memories]\nforeign = true\n", "[memories]\ndedicated_tools = [true]\n"} {
		h := configCommandHome(t, content, strings.Contains(content, "[true]"))
		code, out, err := runConfigCommand(t, h, "set", "memories.dedicated_tools", "false")
		if code != 1 || !strings.HasPrefix(out, "주의: ") || err == "" || h.read("config.toml") != content {
			t.Fatalf("exit %d stdout=%q stderr=%q", code, out, err)
		}
	}
	h := configCommandHome(t, "", false)
	if err := os.Remove(filepath.Join(h.home, "config.toml")); err != nil {
		t.Fatal(err)
	}
	h.env = h.env.With("CODEX_HOME", " \t"+h.home+"\ufeff ")
	code, out, err := runConfigCommand(t, h, "get", "memories.dedicated_tools")
	if code != 0 || out != "memories.dedicated_tools = (unset)\n" || err != "" {
		t.Fatalf("%d %q %q", code, out, err)
	}
	if e := os.Symlink("missing-target", filepath.Join(h.home, "config.toml")); e != nil {
		t.Fatal(e)
	}
	code, _, err = runConfigCommand(t, h, "list")
	if code != 1 || !strings.Contains(err, "left unchanged") {
		t.Fatalf("%d %q", code, err)
	}
	if target, e := os.Readlink(filepath.Join(h.home, "config.toml")); e != nil || target != "missing-target" {
		t.Fatalf("link %q %v", target, e)
	}
}

// CRW-1148: help in any argument position of any action is a no-write, and a token past the
// action's arity is a usage error before anything is read or written.
func TestConfigCommandHelpAndExtraArgumentsWriteNothing(t *testing.T) {
	t.Parallel()
	const content = "[memories]\ndedicated_tools = false\n"
	const id = "memories.dedicated_tools"
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"list help", []string{"list", "--help"}, 0},
		{"list short help", []string{"list", "-h"}, 0},
		{"get trailing help", []string{"get", id, "--help"}, 0},
		{"set trailing help", []string{"set", id, "true", "--help"}, 0},
		{"set short help", []string{"set", id, "true", "-h"}, 0},
		{"set word help", []string{"set", id, "true", "help"}, 0},
		{"set help in id position", []string{"set", "--help", "true"}, 0},
		{"set help in value position", []string{"set", id, "--help"}, 0},
		{"unset trailing help", []string{"unset", id, "-h"}, 0},
		{"list extra", []string{"list", "x"}, 2},
		{"get extra", []string{"get", id, "x"}, 2},
		{"set extra", []string{"set", id, "true", "x"}, 2},
		{"unset extra", []string{"unset", id, "x"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := configCommandHome(t, content, true)
			// An unresolvable home would fail any command that got past argument handling.
			h.env = h.env.With("CODEX_HOME", filepath.Join(h.home, "missing", "home"))
			code, out, err := runConfigCommand(t, h, tc.args...)
			if code != tc.code {
				t.Fatalf("exit %d stdout=%q stderr=%q", code, out, err)
			}
			if tc.code == 0 && (!strings.HasPrefix(out, "Usage:\n  crw install config list") || err != "") {
				t.Fatalf("help: stdout=%q stderr=%q", out, err)
			}
			if tc.code == 2 && (out != "" || !strings.Contains(err, "unexpected argument")) {
				t.Fatalf("extra: stdout=%q stderr=%q", out, err)
			}
			h.env = h.env.With("CODEX_HOME", h.home)
			if h.read("config.toml") != content {
				t.Fatal("config.toml changed")
			}
			entries, e := os.ReadDir(h.home)
			if e != nil {
				t.Fatal(e)
			}
			for _, entry := range entries {
				if strings.Contains(entry.Name(), ".bak") || strings.Contains(entry.Name(), ".lock") {
					t.Fatalf("stray file %s", entry.Name())
				}
			}
		})
	}
}

func TestConfigCommandHelpLeavesManifestAndBackupsUntouched(t *testing.T) {
	t.Parallel()
	h := configCommandHome(t, "[memories]\ndedicated_tools = false\n", true)
	before := h.read(configguard.InstallManifestName)
	before2 := h.read("config.toml")
	names := func() []string {
		entries, err := os.ReadDir(h.home)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		return got
	}
	listing := strings.Join(names(), ",")
	for _, args := range [][]string{{"set", "memories.dedicated_tools", "true", "--help"}, {"unset", "memories.dedicated_tools", "--help"}} {
		if code, _, err := runConfigCommand(t, h, args...); code != 0 || err != "" {
			t.Fatalf("%v: %d %q", args, code, err)
		}
	}
	if h.read(configguard.InstallManifestName) != before || h.read("config.toml") != before2 || strings.Join(names(), ",") != listing {
		t.Fatal("help wrote the manifest, config or a backup")
	}
}

func TestConfigCommandPaddedIDUsesResolvedKey(t *testing.T) {
	t.Parallel()
	h := configCommandHome(t, "[memories]\ndedicated_tools = false\n", true)
	_, canonical, _ := runConfigCommand(t, h, "get", "memories.dedicated_tools")
	_, padded, _ := runConfigCommand(t, h, "get", " \tmemories.dedicated_tools\n ")
	if canonical != padded || canonical != "memories.dedicated_tools = false\n" {
		t.Fatalf("canonical %q padded %q", canonical, padded)
	}
	code, out, err := runConfigCommand(t, h, "set", " memories.dedicated_tools ", "true")
	if code != 0 || err != "" || !strings.Contains(out, "memories.dedicated_tools: false -> true\n") {
		t.Fatalf("%d %q %q", code, out, err)
	}
	code, out, err = runConfigCommand(t, h, "unset", " memories.dedicated_tools ")
	if code != 0 || err != "" || !strings.HasPrefix(out, "memories.dedicated_tools: restored to false\n") {
		t.Fatalf("%d %q %q", code, out, err)
	}
}
