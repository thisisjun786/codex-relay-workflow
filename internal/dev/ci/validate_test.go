//go:build dev

package ci

import (
	"archive/zip"
	"os"
	"os/exec"
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

// validate runs `crw-dev ci validate` in the fixture. The two variables the large-blob check reads
// are pinned to empty (a local run), because hosted CI sets them for the test process itself.
func validate(t *testing.T, r *fixtureRepo) result {
	t.Helper()
	return goCheck(t, r.root, []string{"GITHUB_EVENT_NAME=", "BLOB_RANGE_BASE="}, "validate")
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
	const tail = "; Python is allowed only in skill assets (plugins/crw/skills/*/{scripts,examples}/)\n"
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
	// A commit now exists, so the large-blob check judges its history and says so.
	expectEqual(t, "tracked", validate(t, r), result{0, validated + largeBlobTestFull, ""})
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
		"plugins/crw/skills/crw-example/scripts/a.py", "plugins/crw/skills/crw-example/examples/b.py",
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
		"port/cxc/skills/crw-example/scripts/a.py", // the staging root of ported skills before the activation move (CRW-392)
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
		{skill + "scripts/helper", false}, {"plugins/crw/skills/crw-example/scripts/a.py", false},
		{"plugins/crw/skills/crw-example/examples/b.py", false}, {"plugins/crw/skills/scripts/scripts/a.py", false},
		{"cmd/a.py", true}, {"internal/a.py", true}, {"plugins/crw/wiring/a.py", true}, {"plugins/crw/wiring/scripts/a.py", true},
		{"scripts/a.py", true}, {"docs/scripts/a.py", true}, {"internal/scripts/a.py", true},
		{"plugins/other/skills/example/scripts/a.py", true}, {"cmd/helper", true}, {skill + "references/helper", true},
		{skill + "scripts.py", true}, {skill + "a.py", true}, {skill + "references/a.py", true}, {skill + "Scripts/a.py", true},
		{skill + "sub/scripts/a.py", true}, {skill + "examples.py/a.py", true}, {"plugins/crw/skills/scripts/a.py", true},
		{"plugins/crw/skills/a.py", true}, {"port/cxc/records/crw-example/scripts/a.py", true}, {"port/cxc/skills/a.py", true},
		{"port/cxc/skills/crw-example/SKILL.py", true}, {"port/cxc/skills/crw-example/scripts/a.py", true},
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
	want := []string{"cmd/a.py: a Python file; Python is allowed only in skill assets (plugins/crw/skills/*/{scripts,examples}/)",
		"cmd/helper: a script with a python shebang; Python is allowed only in skill assets (plugins/crw/skills/*/{scripts,examples}/)"}
	expectEqual(t, "messages", pythonFileErrors(root, []string{"cmd/a.py", skill + "scripts/a.py", "cmd/helper"}), want)
}

// tidyOK is what validate adds to its success line when the module files are checked and match.
const tidyOK = "go.mod and go.sum match go mod tidy.\n"

// Test1188_GoModIsTidyForValidate: validate refuses a go.mod that go mod tidy would rewrite, and
// prints the difference; a tidy go.mod passes. The require of an unused module needs no download, so the
// fixture stays offline.
func Test1188_GoModIsTidyForValidate(t *testing.T) {
	r := validateRepo(t)
	r.write("go.mod", "module example.com/x\n\ngo 1.27\n")
	expectEqual(t, "tidy", validate(t, r), result{0, validated + tidyOK, ""})
	r.write("go.mod", "module example.com/x\n\ngo 1.27\n\nrequire example.com/nothere v1.0.0\n")
	got := validate(t, r)
	if got.code != 1 || got.stdout != "" ||
		!strings.Contains(got.stderr, "go.mod or go.sum is not what go mod tidy writes") ||
		!strings.Contains(got.stderr, "-require example.com/nothere v1.0.0") {
		t.Errorf("untidy go.mod: %+v", got)
	}
}

// Test1188_GoModTidyCheckSkipsAnIncompleteModuleCache: with no module cache the offline check cannot
// load the imported module, so it says it skipped the check and validate still passes.
func Test1188_GoModTidyCheckSkipsAnIncompleteModuleCache(t *testing.T) {
	r := validateRepo(t)
	r.write("go.mod", "module example.com/x\n\ngo 1.27\n\nrequire golang.org/x/mod v0.38.0\n")
	r.write("x.go", "package x\n\nimport _ \"golang.org/x/mod/semver\"\n")
	got := goCheck(t, r.root, []string{"GITHUB_EVENT_NAME=", "BLOB_RANGE_BASE=", "GOMODCACHE=" + t.TempDir()}, "validate")
	if got.code != 0 || got.stderr != "" || !strings.HasPrefix(got.stdout, validated) ||
		!strings.Contains(got.stdout, "go mod tidy check skipped: the module cache lacks a module") {
		t.Errorf("incomplete cache: %+v", got)
	}
}

// cachedModule builds a module cache holding example.com/dep v1.0.0 without any network: the module
// is served from a file:// proxy once into a fresh GOMODCACHE, so the cache has the module files but
// no go.sum and no checksum database entry. It returns the cache directory.
func cachedModule(t *testing.T) string {
	t.Helper()
	proxy := filepath.Join(t.TempDir(), "proxy", "example.com", "dep", "@v")
	if err := os.MkdirAll(proxy, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/dep\n\ngo 1.27\n"
	archive, err := os.Create(filepath.Join(proxy, "v1.0.0.zip"))
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(archive)
	for name, body := range map[string]string{"go.mod": gomod, "dep.go": "package dep\n"} {
		f, err := w.Create("example.com/dep@v1.0.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"v1.0.0.mod": gomod, "v1.0.0.info": `{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`, "list": "v1.0.0\n"} {
		if err := os.WriteFile(filepath.Join(proxy, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := t.TempDir()
	seed := t.TempDir()
	for name, body := range map[string]string{"go.mod": "module example.com/seed\n\ngo 1.27\n\nrequire example.com/dep v1.0.0\n",
		"seed.go": "package seed\n\nimport _ \"example.com/dep\"\n"} {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "mod", "download", "example.com/dep")
	cmd.Dir = seed
	cmd.Env = append(os.Environ(), "GOMODCACHE="+cache, "GOPROXY=file://"+filepath.Dir(filepath.Dir(filepath.Dir(proxy))),
		"GOSUMDB=off", "GOFLAGS=-mod=mod -modcacherw", "GOTOOLCHAIN=local", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seeding the module cache: %v\n%s", err, out)
	}
	return cache
}

// Test1188_GoModTidyCheckNeverAsksTheChecksumDatabase: a module whose files are cached but whose go.sum
// lines are missing must not make validate reach for the checksum database (here a refused local port).
// The missing go.sum lines are what go mod tidy would add, so validate reports that difference, not a
// network error; with the lines written the same fixture passes.
func Test1188_GoModTidyCheckNeverAsksTheChecksumDatabase(t *testing.T) {
	cache := cachedModule(t)
	r := validateRepo(t)
	r.write("go.mod", "module example.com/x\n\ngo 1.27\n\nrequire example.com/dep v1.0.0\n")
	r.write("x.go", "package x\n\nimport _ \"example.com/dep\"\n")
	env := []string{"GITHUB_EVENT_NAME=", "BLOB_RANGE_BASE=", "GOMODCACHE=" + cache, "GOSUMDB=sum.golang.org http://127.0.0.1:1", "GOPROXY=http://127.0.0.1:1"}
	got := goCheck(t, r.root, env, "validate")
	if got.code != 1 || strings.Contains(got.stderr, "127.0.0.1") ||
		!strings.Contains(got.stderr, "go.mod or go.sum is not what go mod tidy writes") ||
		!strings.Contains(got.stderr, "+example.com/dep v1.0.0 h1:") {
		t.Errorf("missing go.sum lines: %+v", got)
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = r.root
	tidy.Env = append(os.Environ(), "GOMODCACHE="+cache, "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOWORK=off")
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	got = goCheck(t, r.root, env, "validate")
	if got.code != 0 || got.stderr != "" || !strings.HasSuffix(got.stdout, tidyOK) {
		t.Errorf("tidy fixture: %+v", got)
	}
}

// Test1188_GoModTidyCheckIgnoresPrivateModulePatterns: GOPRIVATE and GONOPROXY inherited from the
// caller name modules go fetches directly, past GOPROXY=off; the check must not follow them to the
// network (here a refused local HTTPS proxy). With an empty module cache it skips with the cache note.
func Test1188_GoModTidyCheckIgnoresPrivateModulePatterns(t *testing.T) {
	r := validateRepo(t)
	r.write("go.mod", "module example.com/x\n\ngo 1.27\n\nrequire example.com/dep v1.0.0\n")
	r.write("x.go", "package x\n\nimport _ \"example.com/dep\"\n")
	env := []string{"GITHUB_EVENT_NAME=", "BLOB_RANGE_BASE=", "GOMODCACHE=" + t.TempDir(), "GOPRIVATE=example.com",
		"GONOPROXY=example.com", "GONOSUMDB=example.com", "HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1"}
	got := goCheck(t, r.root, env, "validate")
	if got.code != 0 || got.stderr != "" || !strings.HasPrefix(got.stdout, validated) ||
		!strings.Contains(got.stdout, "go mod tidy check skipped: the module cache lacks a module") {
		t.Errorf("private module patterns: %+v", got)
	}
}
