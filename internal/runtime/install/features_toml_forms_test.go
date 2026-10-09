package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tomledit"
)

// CRW-1141: the managed-key editor reads the forms of TOML a user or Codex may have written (a quoted or spaced table
// header, a quoted key, a dotted key at the root, a multi-line nested array, a header-like line inside a string, an inline
// table) through one semantic lookup. The real commands either leave a config.toml that decodes and differs only in the keys
// they own, or refuse with config.toml, the manifest and every backup byte for byte as they were.

const tomlFormsPrefix = "# user comment\n[mcp_servers.docs]\ncommand = \"npx\" # keep this\nargs = [\"-y\", \"docs\"]\n\n"

func tomlFormsCases() map[string]string {
	return map[string]string{
		"quoted header":           tomlFormsPrefix + "[\"memories\"]\ngenerate_memories = true\n",
		"spaced header":           tomlFormsPrefix + "[ memories ]\ngenerate_memories = true\n",
		"quoted key":              tomlFormsPrefix + "[memories]\n\"dedicated_tools\" = false\n",
		"root dotted key":         "memories.dedicated_tools = false\n" + tomlFormsPrefix,
		"multi-line nested array": tomlFormsPrefix + "[memories]\nextra = [\n  [1, 2],\n  [3],\n]\n",
		"header in a string":      tomlFormsPrefix + "[model]\nnote = \"\"\"\n[memories]\ndedicated_tools = false\n\"\"\"\n",
		"inline v2 table":         tomlFormsPrefix + "[features]\nmulti_agent_v2 = {not_enabled = true, enabled = false}\n",
		// Every flag is on already, so the fake Codex (which rewrites the whole file) never writes and the line endings seen
		// afterwards are the editor's.
		"crlf and mixed endings": "# crlf\r\n[features]\r\nmulti_agent = true\r\ngoals = true\r\nhooks = true\r\ndefault_mode_request_user_input = true\r\n[mcp_servers.docs]\r\ncommand = \"npx\"\n[memories]\r\ngenerate_memories = true\r\n",
	}
}

// tomlFormsOwned are the keys the feature commands may change: the four flags (through the fake Codex) and the managed key.
var tomlFormsOwned = [][]string{
	{"features", "multi_agent"}, {"features", "goals"}, {"features", "hooks"}, {"features", "default_mode_request_user_input"},
	{"memories", "dedicated_tools"},
}

func tomlFormsSnapshot(t *testing.T, home string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.Name() == "calls" || strings.HasSuffix(e.Name(), ".crw-lock") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(home, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func tomlFormsStrip(doc map[string]any, path []string) {
	if len(path) == 1 {
		delete(doc, path[0])
		return
	}
	if child, ok := doc[path[0]].(map[string]any); ok {
		tomlFormsStrip(child, path[1:])
		if len(child) == 0 {
			delete(doc, path[0])
		}
	}
}

// tomlFormsCheck asserts the outcome of one command: a success leaves a decoding config.toml that differs from before only
// in the owned keys and keeps the user's comments and MCP entry; a refusal leaves every file as it was.
func tomlFormsCheck(t *testing.T, step string, before map[string]string, after map[string]string, code int, stdout, stderr string) {
	t.Helper()
	if code != 0 {
		for name, content := range before {
			if after[name] != content {
				t.Fatalf("%s refused (exit %d, %q) but changed %s:\n%q\nwas\n%q", step, code, stderr, name, after[name], content)
			}
		}
		if len(after) != len(before) {
			t.Fatalf("%s refused (exit %d, %q) but left new files: %v", step, code, stderr, after)
		}
		return
	}
	got, err := tomledit.Decode(after["config.toml"])
	if err != nil {
		t.Fatalf("%s (stdout %q) left config.toml that does not decode: %v\n%s", step, stdout, err, after["config.toml"])
	}
	was, err := tomledit.Decode(before["config.toml"])
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range tomlFormsOwned {
		tomlFormsStrip(got, path)
		tomlFormsStrip(was, path)
	}
	if !tomledit.Equal(got, was) {
		t.Fatalf("%s changed more than the owned keys:\n%v\nwas\n%v\n%s", step, got, was, after["config.toml"])
	}
	for _, keep := range []string{"# user comment", "# keep this", "# crlf\r\n"} {
		if strings.Contains(before["config.toml"], keep) && !strings.Contains(after["config.toml"], keep) {
			t.Fatalf("%s dropped %q:\n%s", step, keep, after["config.toml"])
		}
	}
}

func TestFeaturesAndConfigCommandsKeepEveryTomlFormValid(t *testing.T) {
	t.Parallel()
	for name, content := range tomlFormsCases() {
		t.Run(name, func(t *testing.T) {
			h := newFeatureHome(t, content)
			h.env = h.env.With("CRW_HOME", t.TempDir())
			config := func(args ...string) (int, string, string) {
				var out, err bytes.Buffer
				code := Main(context.Background(), append([]string{"config"}, args...), h.env, &out, &err)
				return code, out.String(), err.String()
			}
			before := tomlFormsSnapshot(t, h.home)
			code, out, errOut := h.run("enable")
			tomlFormsCheck(t, "enable", before, tomlFormsSnapshot(t, h.home), code, out, errOut)
			if code != 0 {
				return
			}
			for _, step := range [][]string{{"set", "memories.dedicated_tools", "false"}, {"set", "memories.dedicated_tools", "true"}, {"unset", "memories.dedicated_tools"}} {
				before = tomlFormsSnapshot(t, h.home)
				code, out, errOut = config(step...)
				tomlFormsCheck(t, strings.Join(step, " "), before, tomlFormsSnapshot(t, h.home), code, out, errOut)
			}
			before = tomlFormsSnapshot(t, h.home)
			code, out, errOut = h.run("disable")
			tomlFormsCheck(t, "disable", before, tomlFormsSnapshot(t, h.home), code, out, errOut)
			if strings.Contains(content, "\r\n") && !strings.Contains(h.read("config.toml"), "command = \"npx\"\n[memories]\r\n") {
				t.Fatalf("mixed line endings were rewritten: %q", h.read("config.toml"))
			}
		})
	}
}

func TestConfigGetShowsAFormItDoesNotEditAsSet(t *testing.T) {
	t.Parallel()
	h := newFeatureHome(t, "[memories]\ndedicated_tools = [true]\n")
	var out, errOut bytes.Buffer
	code := Main(context.Background(), []string{"config", "get", "memories.dedicated_tools"}, h.env, &out, &errOut)
	if code != 0 || !strings.HasPrefix(out.String(), "memories.dedicated_tools = (set in a form crw does not edit: ") {
		t.Fatalf("exit %d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	if err := os.WriteFile(filepath.Join(h.home, "config.toml"), []byte("[memories]\nx = \"oops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Main(context.Background(), []string{"config", "list"}, h.env, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "not valid TOML") {
		t.Fatalf("exit %d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}
