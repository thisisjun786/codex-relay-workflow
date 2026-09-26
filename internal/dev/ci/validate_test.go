//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validateRepo is a Git repository shaped like this one for validate.py: the script copied
// in (its root is its own checkout), a manifest declaring ./skills/, and one skill.
func validateRepo(t *testing.T) *fixtureRepo {
	r := newRepo(t)
	script, err := os.ReadFile(filepath.Join(repoRoot(), "scripts/ci/validate.py"))
	if err != nil {
		t.Fatal(err)
	}
	r.write("scripts/ci/validate.py", string(script))
	r.write("plugins/crw/.codex-plugin/plugin.json", `{"skills": "./skills/"}`+"\n")
	r.write("plugins/crw/skills/example/SKILL.md", "---\nname: example\ndescription: \"Do useful work\"\n---\n")
	r.write("plugins/crw/skills/example/agents/openai.yaml", "interface:\n  display_name: \"Example\"\n"+
		"  short_description: \"Do useful work\"\n  default_prompt: \"$example work\"\n")
	return r
}

// validateParity runs the fixture's validate.py and `crw-dev ci validate` there.
func validateParity(t *testing.T, r *fixtureRepo, label string) result {
	t.Helper()
	py := runCommand(t, r.root, nil, "python3", filepath.Join(r.root, "scripts/ci/validate.py"))
	got := goCheck(t, r.root, nil, "validate")
	sameResult(t, label, py, got)
	return got
}

func Test47_VAL_1_SkillFrontmatterAndInterface(t *testing.T) {
	r := validateRepo(t)
	expectEqual(t, "valid", validateParity(t, r, "valid"),
		result{0, "Validated 1 skills, local link paths and Python syntax.\n", ""})
	skill := "plugins/crw/skills/example/SKILL.md"
	for _, row := range []struct{ header, message string }{
		{"name: another\ndescription: \"Do work\"", "Skill name must match its directory; description is required"},
		{"name: example", "Skill name must match its directory; description is required"},
		{"name: example\ndescription: \"\"", "Expected a nonempty string scalar"},
		{"name: example\nname: example\ndescription: \"Do work\"", "Expected one name and one description field"},
		{"name: 'example'\ndescription: 'it''s work'", ""},
		{"name: example\ndescription: \"unterminated", ""},
		{"title: example", "Expected one name and one description field"},
	} {
		r.write(skill, "---\n"+row.header+"\n---\n")
		got := validateParity(t, r, row.header)
		if row.message != "" {
			want := skill + ": " + row.message + "\nNo skills validated\n"
			expectEqual(t, row.header, got, result{1, "", want})
		}
	}
	r.write(skill, "---\nname: example\ndescription: \"Do work\"\n---\n")
	r.write("plugins/crw/skills/example/agents/openai.yaml", "interface:\n  display_name: \"Example\"\n"+
		"  short_description: \"Do useful work\"\n  default_prompt: \"$another work\"\n")
	expectEqual(t, "prompt", validateParity(t, r, "prompt"), result{1, "", skill + ": Default prompt must name this skill\nNo skills validated\n"})
	r.write("plugins/crw/skills/example/agents/openai.yaml", "interface:\n  display_name: \"Example\"\n  display_name: \"Again\"\n")
	expectEqual(t, "duplicate", validateParity(t, r, "duplicate"), result{1, "", skill + ": Malformed interface metadata\nNo skills validated\n"})
	os.Remove(filepath.Join(r.root, "plugins/crw/skills/example/agents/openai.yaml"))
	validateParity(t, r, "no openai.yaml")
	// Python syntax is still checked while Python sources exist.
	r.write("scripts/broken.py", "def (:\n")
	got := validateParity(t, r, "syntax")
	if got.code != 1 || !strings.Contains(got.stderr, "scripts/broken.py: ") {
		t.Errorf("syntax: %+v", got)
	}
}

func Test47_VAL_2_LocalLinksResolveInsideTheRepository(t *testing.T) {
	r := validateRepo(t)
	r.write("has space.md", "# Existing\n")
	r.write("README.md", "[ok](has%20space.md#existing)\n[ok](<has space.md>)\n"+
		"[remote](https://example.invalid/no-network)\n"+
		"```md\n[example](missing-in-example.md)\n```\n"+
		"[bad](missing.md)\n[escape](../outside.md)\n"+
		"~~~~\n[x](fenced.md)\n~~~\n[y](still-fenced.md)\n~~~~\n[title](missing2.md \"Title\")\n")
	errs, err := LinkErrors(r.root, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "errors", errs, []string{"README.md:7: invalid local link missing.md",
		"README.md:8: invalid local link ../outside.md", "README.md:14: invalid local link missing2.md"})
	got := validateParity(t, r, "links")
	expectEqual(t, "cli", got, result{1, "", strings.Join(errs, "\n") + "\n"})
}

func TestURLNetlocNFKCParity(t *testing.T) {
	for _, delimiter := range []string{"／", "？", "＃", "＠", "：", "℀"} {
		t.Run(delimiter, func(t *testing.T) {
			url := "https://exa" + delimiter + "mple.com/path"
			r := validateRepo(t)
			r.write("README.md", "[remote]("+url+")\n")
			py := runCommand(t, r.root, nil, "python3", "-c", `
import importlib.util, pathlib, sys
spec = importlib.util.spec_from_file_location("validator", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
try:
    print(module.link_errors(pathlib.Path(sys.argv[2]), pathlib.Path(sys.argv[3])))
except ValueError as exc:
    print(str(exc))
`, filepath.Join(repoRoot(), "scripts/ci/validate.py"), filepath.Join(r.root, "README.md"), r.root)
			_, err := LinkErrors(r.root, "README.md")
			if err == nil || strings.TrimSpace(py.stdout) != err.Error() {
				t.Errorf("link %s: Python %q, Go %v", delimiter, py.stdout, err)
			}
			if py.code != 0 || !strings.Contains(py.stdout, "contains invalid characters under NFKC normalization") {
				t.Fatalf("Python did not reject URL: %+v", py)
			}
			manifestParity(t, testManifest().set("repository", url), "crw", nil)
			manifestParity(t, withIface("documentationUrl", url), "crw", nil)
		})
	}
}

// The skill name pattern is lowercase words joined by single hyphens: an underscore, an
// uppercase letter or a doubled/edge hyphen is refused exactly as validate.py refuses it.
func Test47_VAL_1_SkillNamePattern(t *testing.T) {
	for _, row := range []struct {
		name string
		ok   bool
	}{{"example", true}, {"ex-ample2", true}, {"ex_ample", false}, {"Example", false}, {"ex--ample", false}, {"-example", false}} {
		r := newRepo(t)
		script, err := os.ReadFile(filepath.Join(repoRoot(), "scripts/ci/validate.py"))
		if err != nil {
			t.Fatal(err)
		}
		r.write("scripts/ci/validate.py", string(script))
		r.write("plugins/crw/.codex-plugin/plugin.json", `{"skills": "./skills/"}`+"\n")
		dir := "plugins/crw/skills/" + row.name
		r.write(dir+"/SKILL.md", "---\nname: "+row.name+"\ndescription: \"Do useful work\"\n---\n")
		r.write(dir+"/agents/openai.yaml", "interface:\n  display_name: \"Example\"\n"+
			"  short_description: \"Do useful work\"\n  default_prompt: \"$"+row.name+" work\"\n")
		got := validateParity(t, r, row.name)
		want := result{0, "Validated 1 skills, local link paths and Python syntax.\n", ""}
		if !row.ok {
			want = result{1, "", dir + "/SKILL.md: Invalid skill name\nNo skills validated\n"}
		}
		expectEqual(t, row.name, got, want)
	}
}
