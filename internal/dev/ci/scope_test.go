//go:build dev

package ci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scopeRepo is test_scope.py's setUp: one commit holding README.md.
func scopeRepo(t *testing.T) (*fixtureRepo, string) {
	r := newRepo(t)
	r.write("README.md", "initial\n")
	r.commit()
	return r, r.head()
}

// scopeArgs are the CLI arguments of test_scope.py's select() helper.
func scopeArgs(base string, overrides map[string]string) []string {
	values := map[string]string{"base": base, "head": "HEAD", "event": "pull_request",
		"base-ref": "dev", "ref": "refs/pull/1/merge"}
	for key, value := range overrides {
		values[key] = value
	}
	var args []string
	for _, name := range []string{"base", "head", "event", "base-ref", "ref"} {
		args = append(args, "--"+name, values[name])
	}
	return args
}

// scopeParity runs scope.py and `crw-dev ci scope` on the same repository and arguments,
// requires identical exit, stdout and stderr, and returns the decoded selection on success.
func scopeParity(t *testing.T, r *fixtureRepo, base string, overrides map[string]string) map[string]any {
	t.Helper()
	args := scopeArgs(base, overrides)
	py := python(t, r.root, nil, "scripts/ci/scope.py", args...)
	got := goCheck(t, r.root, nil, "scope", args...)
	sameResult(t, strings.Join(args, " "), py, got)
	if got.code != 0 {
		return nil
	}
	var selection map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &selection); err != nil {
		t.Fatalf("selection JSON: %v\n%s", err, got.stdout)
	}
	return selection
}

func jobs(tests, packages bool) map[string]any {
	return map[string]any{"tests": tests, "packages": packages}
}

func strs(items ...string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

func expectEqual(t *testing.T, label string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", label, got, want)
	}
}

func Test47_SCOPE_1_SelectionFollowsPathClass(t *testing.T) {
	t.Run("prose", func(t *testing.T) {
		r, base := scopeRepo(t)
		r.write("docs/releases.md", "release procedure\n")
		r.commit()
		s := scopeParity(t, r, base, nil)
		expectEqual(t, "selected", s["selected"], jobs(false, false))
		expectEqual(t, "reason", s["reason"], "paths")
		expectEqual(t, "changed", s["changed"], strs("docs/releases.md"))
	})
	t.Run("skill", func(t *testing.T) {
		r, base := scopeRepo(t)
		r.write("plugins/crw/skills/crw-run/SKILL.md", "instructions\n")
		r.commit()
		expectEqual(t, "selected", scopeParity(t, r, base, nil)["selected"], jobs(true, false))
	})
	t.Run("skill plus manifest", func(t *testing.T) {
		r, base := scopeRepo(t)
		r.write("plugins/crw/skills/crw-run/SKILL.md", "instructions\n")
		r.write("plugins/crw/.codex-plugin/plugin.json", "{}\n")
		r.commit()
		expectEqual(t, "selected", scopeParity(t, r, base, nil)["selected"], jobs(true, true))
	})
	t.Run("full paths", func(t *testing.T) {
		for _, path := range []string{"packages/bridge/src/a.py", "scripts/runtime_install.py",
			"plugins/crw/wiring/launch.py", "plugins/crw/.codex-plugin/plugin.json",
			".github/workflows/ci.yml", "pyproject.toml", "conftest.py",
			"contract/runner/core.py", "contract/fixtures/records/a.json", "docs/port/test-map.md"} {
			if got := Classify(path); got != "full" {
				t.Errorf("Classify(%q) = %q, want full", path, got)
			}
		}
	})
	t.Run("go product paths", func(t *testing.T) {
		r, base := scopeRepo(t)
		paths := []string{".goreleaser.yaml", "Makefile", "cmd/crw/main.go", "contract/schema/relay-cli.json",
			"docs/port/inventory.md", "go.mod", "go.sum", "internal/contract/emit.go",
			"scripts/port/check_inventory.py", "tools.go"}
		for _, path := range paths {
			r.write(path, "x\n")
		}
		r.commit()
		s := scopeParity(t, r, base, nil)
		expectEqual(t, "unknown", s["unknown"], strs())
		expectEqual(t, "changed", s["changed"], strs(paths...))
		expectEqual(t, "selected", s["selected"], jobs(true, true))
		data, _ := json.Marshal(s)
		if _, err := ValidateSelection(decodeNumbers(t, string(data))); err != nil {
			t.Errorf("ValidateSelection: %v", err)
		}
	})
	t.Run("unregistered lookalikes", func(t *testing.T) {
		for _, path := range []string{"go.work", "cmdx/main.go", "docs/portable.md.txt"} {
			if got := Classify(path); got != "unknown" {
				t.Errorf("Classify(%q) = %q, want unknown", path, got)
			}
		}
	})
}

func Test47_SCOPE_2_UnknownCoversTheWholeCandidateTree(t *testing.T) {
	r, _ := scopeRepo(t)
	r.write("new-component/source.py", "pass\n")
	r.commit()
	base := r.head()
	r.write("README.md", "changed\n")
	r.commit()
	s := scopeParity(t, r, base, nil)
	expectEqual(t, "unknown", s["unknown"], strs("new-component/source.py"))
	expectEqual(t, "selected", s["selected"], jobs(true, true))
}

func Test47_SCOPE_3_RenamesListBothSides(t *testing.T) {
	r, _ := scopeRepo(t)
	r.write("packages/bridge/guide.md", "same\n")
	r.commit()
	base := r.head()
	if err := os.Mkdir(filepath.Join(r.root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.git("mv", "packages/bridge/guide.md", "docs/guide.md")
	r.commit()
	s := scopeParity(t, r, base, nil)
	expectEqual(t, "changed", s["changed"], strs("docs/guide.md", "packages/bridge/guide.md"))
	expectEqual(t, "packages", s["selected"].(map[string]any)["packages"], true)
}

// decodeNumbers decodes JSON the way the gate reads a selection (numbers kept exact).
func decodeNumbers(t *testing.T, text string) any {
	t.Helper()
	value, err := pyJSONLoads(text)
	if err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	return value
}

func Test47_SCOPE_4_ModeChangesAreUnsafe(t *testing.T) {
	t.Run("chmod", func(t *testing.T) {
		r, base := scopeRepo(t)
		if err := os.Chmod(filepath.Join(r.root, "README.md"), 0o755); err != nil {
			t.Fatal(err)
		}
		r.commit()
		s := scopeParity(t, r, base, nil)
		expectEqual(t, "unsafe", s["unsafe"], strs("README.md"))
		expectEqual(t, "selected", s["selected"], jobs(true, true))
	})
	t.Run("symlink", func(t *testing.T) {
		r, base := scopeRepo(t)
		readme := filepath.Join(r.root, "README.md")
		if err := os.Remove(readme); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("LICENSE", readme); err != nil {
			t.Fatal(err)
		}
		r.commit()
		s := scopeParity(t, r, base, nil)
		expectEqual(t, "unsafe", s["unsafe"], strs("README.md"))
		expectEqual(t, "selected", s["selected"], jobs(true, true))
	})
}

func Test47_SCOPE_5_NoUsableComparisonSelectsEverything(t *testing.T) {
	r, base := scopeRepo(t)
	for _, row := range []struct {
		base, reason string
		overrides    map[string]string
	}{
		{base, "empty", nil},
		{strings.Repeat("0", 40), "base-unavailable", nil},
		{"absent", "base-unavailable", nil},
		{"", "base-unavailable", nil},
		{base, "dispatch", map[string]string{"event": "workflow_dispatch", "ref": "refs/heads/dev"}},
	} {
		t.Run(row.reason+"/"+row.base, func(t *testing.T) {
			s := scopeParity(t, r, row.base, row.overrides)
			expectEqual(t, "reason", s["reason"], row.reason)
			expectEqual(t, "selected", s["selected"], jobs(true, true))
		})
	}
}

func Test47_SCOPE_6_EventContext(t *testing.T) {
	r, base := scopeRepo(t)
	for _, row := range []struct {
		overrides map[string]string
		message   string
	}{
		{map[string]string{"base-ref": "main"}, "PRs must target a development branch; main is a release mirror"},
		{map[string]string{"base-ref": ""}, "PRs must target a development branch; main is a release mirror"},
		{map[string]string{"event": "push", "ref": "refs/heads/main"}, "Only dev push CI supplies release evidence"},
		{map[string]string{"event": "pull_request_target"}, "Unsupported CI event"},
	} {
		got := goCheck(t, r.root, nil, "scope", scopeArgs(base, row.overrides)...)
		scopeParity(t, r, base, row.overrides)
		expectEqual(t, "refusal", got, result{1, "", "Selection failed: " + row.message + "\n"})
	}
	r.write("README.md", "change\n")
	r.commit()
	push := scopeParity(t, r, base, map[string]string{"event": "push", "ref": "refs/heads/dev"})
	expectEqual(t, "push tests", push["selected"].(map[string]any)["tests"], false)
	parent := scopeParity(t, r, base, map[string]string{"base-ref": "codex/parent"})
	expectEqual(t, "parent tests", parent["selected"].(map[string]any)["tests"], false)
}

func Test47_SCOPE_7_CLIWritesJSONAndJobOutputs(t *testing.T) {
	r, base := scopeRepo(t)
	r.write("README.md", "changed\n")
	r.commit()
	args := scopeArgs(base, nil)
	pyOut := filepath.Join(t.TempDir(), "outputs")
	goOut := filepath.Join(t.TempDir(), "outputs")
	py := python(t, r.root, []string{"GITHUB_OUTPUT=" + pyOut}, "scripts/ci/scope.py", args...)
	got := goCheck(t, r.root, []string{"GITHUB_OUTPUT=" + goOut}, "scope", args...)
	sameResult(t, "cli", py, got)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	pyData, _ := os.ReadFile(pyOut)
	goData, err := os.ReadFile(goOut)
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "GITHUB_OUTPUT", string(goData), string(pyData))
	want := "scope=" + strings.TrimSuffix(got.stdout, "\n") + "\ntests=false\npackages=false\n"
	expectEqual(t, "GITHUB_OUTPUT content", string(goData), want)
	// A failure is exit 1 with the "Selection failed" prefix and nothing on stdout.
	bad := goCheck(t, r.root, nil, "scope", scopeArgs(base, map[string]string{"head": "no-such-rev"})...)
	sameResult(t, "bad head", python(t, r.root, nil, "scripts/ci/scope.py", scopeArgs(base, map[string]string{"head": "no-such-rev"})...), bad)
	if bad.code != 1 || !strings.HasPrefix(bad.stderr, "Selection failed: ") || bad.stdout != "" {
		t.Errorf("bad head: %+v", bad)
	}
}

// Every named root prose file is a docs path in both implementations, POLICY.md included:
// a POLICY.md-only diff selects no expensive suite.
func Test47_SCOPE_1_RootProseFilesAreDocs(t *testing.T) {
	for _, path := range []string{"README.md", "CONTRIBUTING.md", "POLICY.md", "SECURITY.md", "AGENTS.md", "LICENSE"} {
		if got := Classify(path); got != "docs" {
			t.Errorf("Classify(%q) = %q, want docs", path, got)
		}
		r, base := scopeRepo(t)
		r.write(path, "changed prose\n")
		r.commit()
		s := scopeParity(t, r, base, nil)
		expectEqual(t, path+" selected", s["selected"], jobs(false, false))
		expectEqual(t, path+" unknown", s["unknown"], strs())
	}
}
