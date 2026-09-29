//go:build dev

package skills

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

var skillNames = []string{"crw-alpha", "crw-beta"}

// checkout lays out a synthetic repository root the way this one is laid out: a plugin manifest
// declaring ./skills/, two skills, a stray file and a directory without SKILL.md (neither a
// skill), and the root `skills` alias pointing into the plugin.
func checkout(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	plugin := filepath.Join(root, "plugins", "crw")
	must(t, os.MkdirAll(filepath.Join(plugin, ".codex-plugin"), 0o755))
	must(t, os.WriteFile(filepath.Join(plugin, ".codex-plugin", "plugin.json"), []byte(`{"name": "crw", "skills": "./skills/"}`), 0o644))
	for _, name := range skillNames {
		must(t, os.MkdirAll(filepath.Join(plugin, "skills", name), 0o755))
		must(t, os.WriteFile(filepath.Join(plugin, "skills", name, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o644))
	}
	must(t, os.WriteFile(filepath.Join(plugin, "skills", "notes.md"), []byte("not a skill"), 0o644))
	must(t, os.MkdirAll(filepath.Join(plugin, "skills", "no-skill-file"), 0o755))
	must(t, os.Symlink("plugins/crw/skills", filepath.Join(root, "skills")))
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type result struct {
	code           int
	stdout, stderr string
}

func runLink(root, dest string, apply bool) result {
	var stdout, stderr bytes.Buffer
	code := link(root, dest, apply, &stdout, &stderr)
	return result{code, stdout.String(), stderr.String()}
}

// inodes identifies every entry in dir by name and lstat inode, so a replaced entry shows.
func inodes(t *testing.T, dir string) map[string]uint64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	found := map[string]uint64{}
	for _, entry := range entries {
		info, err := os.Lstat(filepath.Join(dir, entry.Name()))
		must(t, err)
		found[entry.Name()] = info.Sys().(*syscall.Stat_t).Ino
	}
	return found
}

func names(found map[string]uint64) []string {
	var out []string
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestCheckOnAMissingInstallWritesNothing(t *testing.T) {
	root := checkout(t)
	dest := filepath.Join(t.TempDir(), "nested", "skills")
	got := runLink(root, dest, false)
	if got.code != 1 || strings.Count(got.stdout, "MISSING ") != len(skillNames) || got.stderr != "" {
		t.Fatalf("check on an empty destination: %+v", got)
	}
	if _, err := os.Lstat(filepath.Dir(dest)); !os.IsNotExist(err) {
		t.Fatalf("--check created %s: %v", filepath.Dir(dest), err)
	}
}

func TestApplyLinksEverySkillAndARerunChangesNothing(t *testing.T) {
	root := checkout(t)
	dest := filepath.Join(t.TempDir(), "nested", "skills")
	got := runLink(root, dest, true)
	if got.code != 0 || strings.Count(got.stdout, "CREATED ") != len(skillNames) {
		t.Fatalf("apply: %+v", got)
	}
	before := inodes(t, dest)
	if strings.Join(names(before), ",") != strings.Join(skillNames, ",") {
		t.Fatalf("linked %v, want exactly the skills %v", names(before), skillNames)
	}
	for _, name := range skillNames {
		target, err := os.Readlink(filepath.Join(dest, name))
		must(t, err)
		want, err := filepath.EvalSymlinks(filepath.Join(root, "plugins", "crw", "skills", name))
		must(t, err)
		if target != want {
			t.Errorf("%s links to %s, want the declared source %s", name, target, want)
		}
	}
	for _, apply := range []bool{false, true} {
		again := runLink(root, dest, apply)
		if again.code != 0 || strings.Count(again.stdout, "LINKED ") != len(skillNames) || strings.Contains(again.stdout, "CREATED") {
			t.Fatalf("rerun (apply=%v) after apply: %+v", apply, again)
		}
	}
	after := inodes(t, dest)
	for name, inode := range before {
		if after[name] != inode {
			t.Errorf("%s was replaced by the rerun", name)
		}
	}
}

func TestAnyConflictWritesNothingAndLeavesThePathAsItWas(t *testing.T) {
	for _, kind := range []string{"file", "directory", "foreign-link", "dangling-link"} {
		t.Run(kind, func(t *testing.T) {
			root := checkout(t)
			folder := t.TempDir()
			dest := filepath.Join(folder, "skills")
			must(t, os.MkdirAll(dest, 0o755))
			conflict := filepath.Join(dest, skillNames[len(skillNames)-1])
			foreign := filepath.Join(folder, "foreign")
			switch kind {
			case "file":
				must(t, os.WriteFile(conflict, []byte("preserve me"), 0o644))
			case "directory":
				must(t, os.Mkdir(conflict, 0o755))
				must(t, os.WriteFile(filepath.Join(conflict, "owned"), []byte("preserve me"), 0o644))
			case "foreign-link":
				must(t, os.Mkdir(foreign, 0o755))
				must(t, os.Symlink(foreign, conflict))
			case "dangling-link":
				must(t, os.Symlink(foreign, conflict))
			}
			before := inodes(t, dest)
			for _, apply := range []bool{false, true} {
				got := runLink(root, dest, apply)
				if got.code != 1 || !strings.Contains(got.stderr, "CONFLICT "+conflict) {
					t.Fatalf("apply=%v: %+v", apply, got)
				}
				after := inodes(t, dest)
				if len(after) != 1 || after[filepath.Base(conflict)] != before[filepath.Base(conflict)] {
					t.Fatalf("apply=%v wrote beside or over a conflict: %v", apply, names(after))
				}
			}
			switch kind {
			case "file":
				data, err := os.ReadFile(conflict)
				must(t, err)
				if string(data) != "preserve me" {
					t.Fatalf("file content changed: %q", data)
				}
			case "directory":
				data, err := os.ReadFile(filepath.Join(conflict, "owned"))
				must(t, err)
				if string(data) != "preserve me" {
					t.Fatalf("directory content changed: %q", data)
				}
			default:
				target, err := os.Readlink(conflict)
				must(t, err)
				if target != foreign {
					t.Fatalf("link retargeted to %s", target)
				}
			}
		})
	}
}

func TestAPathThatAppearsAfterThePreflightIsNeverReplaced(t *testing.T) {
	root := checkout(t)
	dest := filepath.Join(t.TempDir(), "skills")
	late := filepath.Join(dest, skillNames[len(skillNames)-1])
	saved := symlink
	defer func() { symlink = saved }()
	// The path appears after the preflight judged it MISSING and before its own link is made.
	symlink = func(source, target string) error {
		if target != late {
			if err := os.WriteFile(late, []byte("written by someone else"), 0o644); err != nil {
				return err
			}
		}
		return saved(source, target)
	}
	got := runLink(root, dest, true)
	if got.code != 1 || !strings.Contains(got.stderr, "Installation failed") || !strings.Contains(got.stderr, "file exists") {
		t.Fatalf("a path created after the preflight: %+v", got)
	}
	if data, err := os.ReadFile(late); err != nil || string(data) != "written by someone else" {
		t.Fatalf("the late path was replaced: %q %v", data, err)
	}
	if _, err := os.Readlink(filepath.Join(dest, skillNames[0])); err != nil {
		t.Fatalf("the link made before the failure was not kept: %v", err)
	}
}

func TestALinkThroughTheRootSkillsAliasIsLinked(t *testing.T) {
	// Installations made before the skills moved under the plugin point at <root>/skills/<name>.
	// While the root keeps that alias they read the same source and must be left alone.
	root := checkout(t)
	dest := filepath.Join(t.TempDir(), "skills")
	must(t, os.MkdirAll(dest, 0o755))
	for _, name := range skillNames {
		must(t, os.Symlink(filepath.Join(root, "skills", name), filepath.Join(dest, name)))
	}
	before := inodes(t, dest)
	for _, apply := range []bool{false, true} {
		got := runLink(root, dest, apply)
		if got.code != 0 || strings.Count(got.stdout, "LINKED ") != len(skillNames) || strings.Contains(got.stdout, "CREATED") {
			t.Fatalf("apply=%v over alias links: %+v", apply, got)
		}
	}
	after := inodes(t, dest)
	for name, inode := range before {
		if after[name] != inode {
			t.Errorf("%s was replaced", name)
		}
	}
}

func TestTheDeclaredSkillsPathMustStayInsideThePlugin(t *testing.T) {
	for _, row := range []struct{ declared, message string }{
		{"../outside/", "./ relative path"},
		{"/abs/skills/", "./ relative path"},
		{"skills/", "./ relative path"},
		{"./../outside/", "stay inside the plugin root"},
		{"./escape/", "resolves outside"},
		{"./missing/", "does not exist"},
	} {
		t.Run(row.declared, func(t *testing.T) {
			root := checkout(t)
			outside := filepath.Join(root, "plugins", "outside")
			must(t, os.MkdirAll(filepath.Join(outside, "crw-gamma"), 0o755))
			must(t, os.WriteFile(filepath.Join(outside, "crw-gamma", "SKILL.md"), []byte("x"), 0o644))
			must(t, os.Symlink(outside, filepath.Join(root, "plugins", "crw", "escape")))
			must(t, os.WriteFile(filepath.Join(root, "plugins", "crw", ".codex-plugin", "plugin.json"), []byte(`{"skills": "`+row.declared+`"}`), 0o644))
			dest := filepath.Join(t.TempDir(), "skills")
			got := runLink(root, dest, true)
			if got.code != 2 || !strings.Contains(got.stderr, row.message) {
				t.Fatalf("%s: %+v", row.declared, got)
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatalf("%s: a refused manifest still wrote %s", row.declared, dest)
			}
		})
	}
}

func TestTheCommandLinksThisCheckoutIntoTheCodexHome(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "isolated-codex")
	env := map[string]string{"CODEX_HOME": codexHome, "HOME": filepath.Join(t.TempDir(), "home")}
	getenv := func(key string) string { return env[key] }
	for _, row := range []struct {
		args []string
		code int
		errs string
	}{
		{nil, 2, "one of the arguments --check --apply is required"},
		{[]string{"--check", "--apply"}, 2, "not allowed with argument --check"},
		{[]string{"--check", "--dest"}, 2, "argument --dest: expected one argument"},
		{[]string{"--check", "--force"}, 2, "unrecognized arguments: --force"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Link(row.args, getenv, &stdout, &stderr); code != row.code || !strings.Contains(stderr.String(), row.errs) {
			t.Errorf("%q: exit %d stderr %q", row.args, code, stderr.String())
		}
	}
	var noHome bytes.Buffer
	if code := Link([]string{"--check"}, func(string) string { return "" }, &noHome, &noHome); code != 2 || !strings.Contains(noHome.String(), "pass --dest") {
		t.Errorf("no CODEX_HOME and no HOME: exit %d %q", code, noHome.String())
	}
	if _, err := os.Lstat(codexHome); !os.IsNotExist(err) {
		t.Fatalf("a usage error wrote %s", codexHome)
	}
	var stdout, stderr bytes.Buffer
	if code := Link([]string{"--apply"}, getenv, &stdout, &stderr); code != 0 {
		t.Fatalf("apply into CODEX_HOME: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	linked := inodes(t, filepath.Join(codexHome, "skills"))
	if _, ok := linked["crw-run"]; !ok {
		t.Fatalf("the default destination lacks crw-run: %v", names(linked))
	}
	skill, err := os.ReadFile(filepath.Join(codexHome, "skills", "crw-run", "SKILL.md"))
	if err != nil || !strings.Contains(string(skill), "crw-run") {
		t.Fatalf("crw-run does not read this checkout's skill: %v", err)
	}
	if _, err := os.Lstat(env["HOME"]); !os.IsNotExist(err) {
		t.Fatalf("an explicit CODEX_HOME still wrote under HOME")
	}
}
