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

const validated = "Validated 1 skills, local link paths and no Python outside skill assets.\n"

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

// Python stays out of the product (CRW-483): a .py file, tracked or not, and a script a python
// shebang runs are refused anywhere but a skill's asset directories, whose helper scripts are
// original assets an agent runs and no runtime or CI step does. A shell script, a directory link
// and a file that names python outside a first-line shebang are not named; a link is judged by
// the file it leads to. The bodies parse as Python, so only the rule can refuse them.
func Test47_VAL_3_PythonFilesAreRefusedOutsideSkillAssets(t *testing.T) {
	r := validateRepo(t)
	r.write("scripts/fake-gh", "#!/usr/bin/env python3\nprint()\n")
	r.write("scripts/sh-tool", "#!/bin/sh\nexec python3 \"$@\"\n") // names python, but its shebang is a shell's
	r.write("scripts/notes", "python3 appears in this line, which is no shebang\n#!/usr/bin/env python3\n")
	r.write("scripts/tool.py", "print()\n")
	r.write("internal/pkg/gen.py", "print()\n")
	// Skill assets, which are allowed: a .py file at depth, an extension-less python script, a sample.
	r.write("plugins/crw/skills/example/scripts/fixtures/case/input.py", "print()\n")
	r.write("plugins/crw/skills/example/scripts/helper", "#!/usr/bin/env python3\nprint()\n")
	r.write("plugins/crw/skills/example/examples/demo.py", "print()\n")
	// Names that only look like assets.
	r.write("plugins/crw/skills/example/scripts.py", "print()\n")
	r.write("plugins/crw/skills/example/references/helper.py", "print()\n")
	r.write("plugins/crw/wiring/scripts/run.py", "print()\n")
	symlink := func(name, target string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(r.root, name)); err != nil {
			t.Fatal(err)
		}
	}
	symlink("scripts/link", "fake-gh")                                                            // leads to a python script
	symlink("scripts/run", "../plugins/crw/skills/example/scripts/fixtures/case/input.py")        // carries an asset into the product
	symlink("plugins/crw/skills/example/scripts/escape.py", "../../../../../internal/pkg/gen.py") // leaves the assets
	symlink("plugins/crw/skills/example/scripts/linkdir", "../../../../../internal")              // a directory link is no Python file
	r.commit()
	r.write("scripts/new.py", "print()\n") // untracked: refused before it is added
	const tail = "; Python is allowed only in skill assets (plugins/crw/skills/*/{scripts,examples}/, port/cxc/skills/*/{scripts,examples}/)\n"
	expectEqual(t, "refused", validate(t, r), result{1, "", "internal/pkg/gen.py: a Python file" + tail +
		"plugins/crw/skills/example/references/helper.py: a Python file" + tail +
		"plugins/crw/skills/example/scripts.py: a Python file" + tail +
		"plugins/crw/skills/example/scripts/escape.py: a Python file" + tail +
		"plugins/crw/wiring/scripts/run.py: a Python file" + tail +
		"scripts/fake-gh: a script with a python shebang" + tail +
		"scripts/link: a script with a python shebang" + tail +
		"scripts/new.py: a Python file" + tail +
		"scripts/run: a Python file" + tail +
		"scripts/tool.py: a Python file" + tail})
}

// Only skill assets carry Python: the check passes, and says so, for a repository whose Python
// sits in a skill's scripts and examples directories.
func Test47_VAL_3_PythonInSkillAssetsIsAllowed(t *testing.T) {
	r := validateRepo(t)
	r.write("plugins/crw/skills/example/scripts/tool.py", "print()\n")
	r.write("plugins/crw/skills/example/scripts/fixtures/case/input.py", "print()\n")
	r.write("plugins/crw/skills/example/scripts/helper", "#!/usr/bin/env python3\nprint()\n")
	r.write("plugins/crw/skills/example/examples/demo.py", "print()\n")
	expectEqual(t, "untracked", validate(t, r), result{0, validated, ""})
	r.commit()
	expectEqual(t, "tracked", validate(t, r), result{0, validated, ""})
}

// The allow-list is judged by where a file really is: only a regular file below
// <skills root>/<skill>/{scripts,examples}/ is a skill asset, and a name that leaves that place by
// `..`, a link at the end or on the way, or by only looking like one stays refused.
func TestSkillAssetPaths(t *testing.T) {
	root := t.TempDir()
	const py, shebang = "print()\n", "#!/usr/bin/env python3\nprint()\n"
	put := func(name, text string) {
		t.Helper()
		target := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(name, target string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	const skill = "plugins/crw/skills/example/"
	for _, name := range []string{
		skill + "scripts/a.py", skill + "scripts/deep/er/b.py", skill + "examples/c.py",
		"port/cxc/skills/crw-example/scripts/a.py", "port/cxc/skills/crw-example/examples/b.py",
		"plugins/crw/skills/scripts/scripts/a.py", // a skill that is called scripts
	} {
		put(name, py)
	}
	put(skill+"scripts/helper", shebang)
	// Where Python is refused: outside the skills directories, in a skill but outside its asset
	// directories, and one level too deep or too shallow.
	for _, name := range []string{
		"cmd/a.py", "internal/a.py", "plugins/crw/wiring/a.py", "plugins/crw/wiring/scripts/a.py", "scripts/a.py",
		"docs/scripts/a.py", "internal/scripts/a.py", "plugins/other/skills/example/scripts/a.py",
		skill + "scripts.py", skill + "a.py", skill + "references/a.py", skill + "Scripts/a.py",
		skill + "sub/scripts/a.py", skill + "examples.py/a.py", "plugins/crw/skills/scripts/a.py",
		"plugins/crw/skills/a.py", "port/cxc/records/crw-example/scripts/a.py", "port/cxc/skills/a.py",
		"port/cxc/skills/crw-example/SKILL.py",
	} {
		put(name, py)
	}
	put("cmd/helper", shebang)
	put(skill+"references/helper", shebang)
	put("README.md", "# Readme\n")
	// Links: at the end of the name, on the way, and from outside the assets in.
	link(skill+"scripts/to-product.py", "../../../../../cmd/a.py")
	link(skill+"scripts/to-sibling.py", "a.py")
	link(skill+"scripts/dangling.py", "missing.py")
	link(skill+"scripts/linkdir", "../../../../../cmd")
	link("scripts/run", "../"+skill+"scripts/a.py")
	link("scripts/run-helper", "../"+skill+"scripts/helper")
	link("scripts/doc", "../README.md")
	link("scripts/assets", "../"+skill+"scripts")
	for _, row := range []struct {
		name    string
		refused bool
	}{
		{skill + "scripts/a.py", false}, {skill + "scripts/deep/er/b.py", false}, {skill + "examples/c.py", false},
		{skill + "scripts/helper", false}, {"port/cxc/skills/crw-example/scripts/a.py", false},
		{"port/cxc/skills/crw-example/examples/b.py", false}, {"plugins/crw/skills/scripts/scripts/a.py", false},
		{"cmd/a.py", true}, {"internal/a.py", true}, {"plugins/crw/wiring/a.py", true}, {"plugins/crw/wiring/scripts/a.py", true},
		{"scripts/a.py", true}, {"docs/scripts/a.py", true}, {"internal/scripts/a.py", true},
		{"plugins/other/skills/example/scripts/a.py", true}, {"cmd/helper", true}, {skill + "references/helper", true},
		{skill + "scripts.py", true}, {skill + "a.py", true}, {skill + "references/a.py", true}, {skill + "Scripts/a.py", true},
		{skill + "sub/scripts/a.py", true}, {skill + "examples.py/a.py", true}, {"plugins/crw/skills/scripts/a.py", true},
		{"plugins/crw/skills/a.py", true}, {"port/cxc/records/crw-example/scripts/a.py", true}, {"port/cxc/skills/a.py", true},
		{"port/cxc/skills/crw-example/SKILL.py", true},
		// a name that climbs out of the place it starts in, or is not written plainly
		{skill + "scripts/../../../../../cmd/a.py", true}, {skill + "scripts/../scripts/a.py", true},
		{"./" + skill + "scripts/a.py", true}, {"plugins//crw/skills/example/scripts/a.py", true},
		{skill + "scripts/./a.py", true}, {"/" + skill + "scripts/a.py", true},
		// links: judged by where they lead
		{skill + "scripts/to-product.py", true}, {skill + "scripts/to-sibling.py", true}, {skill + "scripts/dangling.py", true},
		{skill + "scripts/linkdir/a.py", true}, {"scripts/run", true}, {"scripts/run-helper", true},
		// not Python: a link to a document, a directory link, a missing file, a file that only names python
		{"scripts/doc", false}, {"scripts/assets", false}, {skill + "scripts/linkdir", false},
		{"README.md", false}, {skill + "scripts/missing.txt", false},
	} {
		got := pythonFileErrors(root, []string{row.name})
		if (len(got) == 1) != row.refused || len(got) > 1 {
			t.Errorf("%s: refused = %v, want %v (%q)", row.name, got, row.refused, got)
		}
	}
	want := []string{"cmd/a.py: a Python file; Python is allowed only in skill assets (plugins/crw/skills/*/{scripts,examples}/, port/cxc/skills/*/{scripts,examples}/)",
		"cmd/helper: a script with a python shebang; Python is allowed only in skill assets (plugins/crw/skills/*/{scripts,examples}/, port/cxc/skills/*/{scripts,examples}/)"}
	expectEqual(t, "messages", pythonFileErrors(root, []string{"cmd/a.py", skill + "scripts/a.py", "cmd/helper"}), want)
}
