//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validateRepo is a Git repository shaped like this one for the validate check: a manifest
// declaring ./skills/ and one skill.
func validateRepo(t *testing.T) *fixtureRepo {
	r := newRepo(t)
	r.write("plugins/crw/.codex-plugin/plugin.json", `{"skills": "./skills/"}`+"\n")
	r.write("plugins/crw/skills/example/SKILL.md", "---\nname: example\ndescription: \"Do useful work\"\n---\n")
	r.write("plugins/crw/skills/example/agents/openai.yaml", "interface:\n  display_name: \"Example\"\n"+
		"  short_description: \"Do useful work\"\n  default_prompt: \"$example work\"\n")
	return r
}

// validate runs `crw-dev ci validate` in the fixture.
func validate(t *testing.T, r *fixtureRepo) result {
	t.Helper()
	return goCheck(t, r.root, nil, "validate")
}

const validated = "Validated 1 skills, local link paths and Python syntax.\n"

func Test47_VAL_1_SkillFrontmatterAndInterface(t *testing.T) {
	r := validateRepo(t)
	expectEqual(t, "valid", validate(t, r), result{0, validated, ""})
	skill := "plugins/crw/skills/example/SKILL.md"
	for _, row := range []struct{ header, message string }{
		{"name: another\ndescription: \"Do work\"", "skill name must match its directory; description is required"},
		{"name: example", "skill name must match its directory; description is required"},
		{"name: example\ndescription: \"\"", "expected a nonempty string scalar"},
		{"name: example\nname: example\ndescription: \"Do work\"", "expected one name and one description field"},
		{"name: example\ndescription: \"unterminated", "unexpected end of JSON input"},
		{"title: example", "expected one name and one description field"},
	} {
		r.write(skill, "---\n"+row.header+"\n---\n")
		expectEqual(t, row.header, validate(t, r), result{1, "", skill + ": " + row.message + "\nNo skills validated\n"})
	}
	r.write(skill, "---\nname: 'example'\ndescription: 'it''s work'\n---\n")
	expectEqual(t, "single-quoted", validate(t, r), result{0, validated, ""})
	r.write(skill, "---\nname: example\ndescription: \"Do work\"\n---\n")
	r.write("plugins/crw/skills/example/agents/openai.yaml", "interface:\n  display_name: \"Example\"\n"+
		"  short_description: \"Do useful work\"\n  default_prompt: \"$another work\"\n")
	expectEqual(t, "prompt", validate(t, r), result{1, "", skill + ": default prompt must name this skill\nNo skills validated\n"})
	r.write("plugins/crw/skills/example/agents/openai.yaml", "interface:\n  display_name: \"Example\"\n  display_name: \"Again\"\n")
	expectEqual(t, "duplicate", validate(t, r), result{1, "", skill + ": malformed interface metadata\nNo skills validated\n"})
	os.Remove(filepath.Join(r.root, "plugins/crw/skills/example/agents/openai.yaml"))
	if got := validate(t, r); got.code != 1 || !strings.Contains(got.stderr, "agents/openai.yaml: no such file or directory") {
		t.Errorf("no openai.yaml: %+v", got)
	}
	// Python syntax is still checked while Python sources exist.
	r.write("scripts/broken.py", "def (:\n")
	got := validate(t, r)
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
		"~~~~\n[x](fenced.md)\n~~~\n[y](still-fenced.md)\n~~~~\n[title](missing2.md \"Title\")\n"+
		"[anchor](#existing)\n[host](//example.invalid/x.md)\n")
	errs, err := LinkErrors(r.root, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	expectEqual(t, "errors", errs, []string{"README.md:7: invalid local link missing.md",
		"README.md:8: invalid local link ../outside.md", "README.md:14: invalid local link missing2.md"})
	expectEqual(t, "cli", validate(t, r), result{1, "", strings.Join(errs, "\n") + "\n"})
}

// The skill name pattern is lowercase words joined by single hyphens: an underscore, an
// uppercase letter or a doubled/edge hyphen is refused.
func Test47_VAL_1_SkillNamePattern(t *testing.T) {
	for _, row := range []struct {
		name string
		ok   bool
	}{{"example", true}, {"ex-ample2", true}, {"ex_ample", false}, {"Example", false}, {"ex--ample", false}, {"-example", false}} {
		r := newRepo(t)
		r.write("plugins/crw/.codex-plugin/plugin.json", `{"skills": "./skills/"}`+"\n")
		dir := "plugins/crw/skills/" + row.name
		r.write(dir+"/SKILL.md", "---\nname: "+row.name+"\ndescription: \"Do useful work\"\n---\n")
		r.write(dir+"/agents/openai.yaml", "interface:\n  display_name: \"Example\"\n"+
			"  short_description: \"Do useful work\"\n  default_prompt: \"$"+row.name+" work\"\n")
		want := result{0, validated, ""}
		if !row.ok {
			want = result{1, "", dir + "/SKILL.md: invalid skill name\nNo skills validated\n"}
		}
		expectEqual(t, row.name, validate(t, r), want)
	}
}
