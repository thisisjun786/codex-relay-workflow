package sandboxroot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefusesAccountHome(t *testing.T) {
	runNamed(t, "account_home", "project_list_ignores_home_key", "project_list_home_key_does_not_allow_home_child")
}

func TestRefusesAncestorOfHome(t *testing.T) {
	runNamed(t, "home_ancestor", "dotdot_to_home_ancestor")
}

func TestRefusesFilesystemRoot(t *testing.T) { runNamed(t, "filesystem_root") }

func TestRefusesSymlinkToHome(t *testing.T) { runNamed(t, "symlink_to_home") }

func TestRefusesDotDotToHome(t *testing.T) { runNamed(t, "dotdot_to_home") }

func TestRefusesAncestorOfProjectRoot(t *testing.T) {
	runNamed(t, "project_ancestor_code", "project_ancestor_names_one_project", "explicit_project_root_ancestor_refused")
}

func TestRefusesAncestorOfProjectUnderSandboxRoot(t *testing.T) {
	runNamed(t, "project_ancestor_under_sandbox_root", "project_ancestor_git_file_under_sandbox_root", "project_ancestor_intermediate_under_sandbox_root")
}

func TestRefusesRelativePath(t *testing.T) { runNamed(t, "relative", "relative_dot") }

func TestRefusesMissingPath(t *testing.T) {
	runNamed(t, "missing", "missing_is_not_created")
	// The rule never makes the path it refuses.
	c := loadCases(t)[0]
	w := materialize(t, c)
	missing := filepath.Join(w.root, "scratch", "crw", "never-made")
	if err := Check(missing, "workspace-write"); err == nil {
		t.Fatal("a missing path was allowed")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused path was made: %v", err)
	}
}

func TestRefusesOutsideDedicatedRoot(t *testing.T) {
	runNamed(t, "outside_dedicated_root", "outside_dedicated_root_home_child", "outside_sandbox_root_parent",
		"refuse_worktree_root_dir_without_git_file", "refuse_clone_with_git_dir_under_worktree_root", "refuse_worktree_root_itself", "refuse_worktree_container_dir")
}

func TestAllowsPathUnderSandboxRoot(t *testing.T) {
	runNamed(t, "allow_sandbox_root_itself", "allow_path_under_sandbox_root")
}

func TestAllowsNonProjectDirUnderSandboxRoot(t *testing.T) {
	runNamed(t, "allow_non_project_dir_with_trust_key", "allow_sandbox_root_with_only_nonrepo_keys", "allow_other_dir_beside_project_under_sandbox_root")
}

func TestAllowsProjectRootItself(t *testing.T) {
	runNamed(t, "allow_project_root_itself", "allow_inside_project_root", "explicit_project_root_allowed_itself", "project_list_symlink_key_resolves_to_its_real_path")
}

func TestAllowsProjectUnderSandboxRoot(t *testing.T) {
	runNamed(t, "allow_project_under_sandbox_root", "allow_inside_project_under_sandbox_root")
}

func TestAllowsLinkedWorktreeUnderWorktreeRoot(t *testing.T) {
	runNamed(t, "allow_linked_worktree_under_worktree_root", "allow_inside_linked_worktree", "allow_linked_worktree_under_codex_home")
}

func TestExplicitProjectRootUnderSandboxRootIsKept(t *testing.T) {
	runNamed(t, "explicit_project_root_under_sandbox_root_is_kept")
}

func TestProjectListIgnoresHomeMissingAndNonRepoSandboxKeys(t *testing.T) {
	runNamed(t, "project_list_ignores_home_key", "project_list_ignores_missing_key", "allow_non_project_dir_with_trust_key",
		"allow_sandbox_root_with_only_nonrepo_keys", "explicit_project_roots_drop_missing_root_and_home")
}

func TestFakeHomeUnderSandboxRootIsNotProtected(t *testing.T) {
	runPrefixed(t, "fake_home_", "env_home_")
}

func TestWritableRootsGetRefusalRules(t *testing.T) {
	runPrefixed(t, "writable_", "unusable_sandbox_root_refuses_writable")
}

func TestDestinationPreflightAllowsNewWorktreeUnderContainers(t *testing.T) {
	runNamed(t, "destination_new_under_worktree_root", "destination_nested_new_under_worktree_root", "destination_new_under_worktree_root_missing_root",
		"destination_new_under_codex_home_worktrees", "destination_new_under_sandbox_root", "destination_new_in_project_root")
}

func TestDestinationPreflightRefusesProtectedDestination(t *testing.T) {
	runNamed(t, "destination_account_home", "destination_home_ancestor", "destination_filesystem_root", "destination_relative",
		"destination_worktree_root_itself", "destination_outside_dedicated_root", "destination_new_child_of_home",
		"destination_through_symlink_to_home", "destination_dotdot_out_of_worktree_root")
}

func TestDangerFullAccessIsOutsideTheRule(t *testing.T) {
	runPrefixed(t, "sandbox_off_", "destination_danger_full_access", "unusable_sandbox_root_does_not_stop")
}

func TestUnknownSandboxCountsAsOn(t *testing.T) {
	runPrefixed(t, "sandbox_on_", "sandbox_unspecified_", "explicit_sandbox_beats")
	for _, reported := range []any{nil, "", 7, map[string]any{}, map[string]any{"type": 3}} {
		if got := SandboxOf(reported); got != Unknown {
			t.Fatalf("SandboxOf(%v) = %q, want %q", reported, got, Unknown)
		}
	}
	if SandboxOf(map[string]any{"type": "dangerFullAccess"}) != "dangerFullAccess" || SandboxOf("workspace-write") != "workspace-write" {
		t.Fatal("SandboxOf drops a reported mode")
	}
}

func TestUnusableSandboxRootRefusesEveryStart(t *testing.T) {
	runPrefixed(t, "unusable_sandbox_root_")
	// The refusal says which configuration key to fix.
	c := loadCases(t)[0]
	c.CrwConfig = map[string]any{"paths": map[string]any{"sandbox_root": "{root}/gone"}}
	w := materialize(t, c)
	err := Check(w.root, "workspace-write")
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Rule != RuleSandboxRootUnusable || !strings.Contains(err.Error(), "paths.sandbox_root") {
		t.Fatalf("an unusable sandbox_root: %v", err)
	}
}

func TestSandboxRootContainingProjectStaysUsable(t *testing.T) {
	runNamed(t, "sandbox_root_containing_project_stays_usable_as_a_base", "project_ancestor_under_sandbox_root", "allow_project_under_sandbox_root")
}

func TestRefusalNamesPathAndRule(t *testing.T) {
	cases := map[string]Case{}
	for _, c := range loadCases(t) {
		cases[c.Name] = c
	}
	c := cases["project_ancestor_under_sandbox_root"]
	w := materialize(t, c)
	link := filepath.Join(w.root, "scratch", "crw", "via")
	if err := os.Symlink(w.root+"/scratch/crw", link); err != nil {
		t.Fatal(err)
	}
	err := Check(link, "workspace-write")
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("want a refusal, got %v", err)
	}
	if refusal.Path != link || refusal.Resolved != w.root+"/scratch/crw" || refusal.Rule != RuleProjectAncestor || refusal.Project != w.root+"/scratch/crw/repo" {
		t.Fatalf("refusal fields: %+v", refusal)
	}
	if len(refusal.Allowed) < 3 || refusal.Allowed[0] != w.root+"/scratch/crw" {
		t.Fatalf("allowed bases: %v", refusal.Allowed)
	}
	for _, want := range []string{"sandbox_root_refused", link, "project_ancestor", refusal.Project, w.root + "/scratch/crw"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal text %q lacks %q", err, want)
		}
	}
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != "sandbox_root_refused" {
		t.Fatal("the refusal has no code")
	}
}

func TestCheckAllJudgesWorkingRootThenWritableRoots(t *testing.T) {
	c := loadCases(t)[0]
	w := materialize(t, c)
	sandbox := w.root + "/scratch/crw"
	if err := CheckAll(sandbox, []string{w.root + "/opt/elsewhere"}, "workspace-write"); err != nil {
		t.Fatal(err)
	}
	var refusal *Refusal
	err := CheckAll(sandbox, []string{w.root + "/opt/elsewhere", w.root + "/home/jun"}, "workspace-write")
	if !errors.As(err, &refusal) || refusal.Rule != RuleAccountHome {
		t.Fatalf("a writable account home: %v", err)
	}
	if err := CheckAll(sandbox, []string{w.root + "/home/jun"}, "danger-full-access"); err != nil {
		t.Fatalf("no sandbox: %v", err)
	}
}

// RootsOf lists every writable root a settings-shaped object carries, and nothing else.
func TestRootsOfListsEveryWritableRoot(t *testing.T) {
	object := map[string]any{
		"cwd":                   "/not/a/root",
		"runtimeWorkspaceRoots": []any{"/a", 7, "/b"},
		"writableRoots":         []string{"/c"},
		"sandbox":               map[string]any{"type": "workspaceWrite", "writableRoots": []any{"/d"}},
		"sandboxPolicy":         map[string]any{"writableRoots": []any{"/e"}},
		"config":                map[string]any{"sandbox_workspace_write": map[string]any{"writable_roots": []any{"/f"}}},
	}
	if got, want := strings.Join(RootsOf(object), ","), "/a,/b,/c,/d,/e,/f"; got != want {
		t.Fatalf("roots %s, want %s", got, want)
	}
	if got := RootsOf(nil); len(got) != 0 {
		t.Fatalf("roots of nothing: %v", got)
	}
	if got := RootsOf(map[string]any{"sandbox": "workspace-write"}); len(got) != 0 {
		t.Fatalf("roots of a sandbox mode: %v", got)
	}
}

// CRW-1265 (pre-merge evaluation of f0691fdd, d5). CODEX_HOME is a path the kernel resolves: a symbolic link
// followed by ".." names the link target's parent, not the link's own. The rule reads config.toml and derives
// the worktree base from that directory, so a project the Codex configuration registers and a linked worktree
// under the Codex worktrees are still allowed, and the decoy next to the link is not read.
func TestCodexHomeLinkFollowedByDotDotNamesTheKernelsDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(parts ...string) string {
		dir := filepath.Join(parts...)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	write := func(path, text string) {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	home, scratch := mk(root, "home", "jun"), mk(root, "scratch", "crw")
	codex := mk(root, "else", "codex")
	sub := mk(codex, "sub")
	project := mk(root, "projects", "app")
	repo := mk(codex, "worktrees", "abc", "repo")
	gitMeta(t, mk(root, "gitdir"))
	write(filepath.Join(repo, ".git"), "gitdir: "+filepath.Join(root, "gitdir")+"\n")
	write(filepath.Join(codex, "config.toml"), "[projects.\""+project+"\"]\ntrust_level = \"trusted\"\n")
	cfg := mk(root, "cfg")
	write(filepath.Join(cfg, "config.toml"), "# the decoy: no projects\n")
	mk(cfg, "worktrees", "abc", "repo") // not the Codex worktrees
	if err := os.Symlink(sub, filepath.Join(cfg, "link")); err != nil {
		t.Fatal(err)
	}
	crw := filepath.Join(root, "crw.json")
	write(crw, `{"paths":{"sandbox_root":"`+scratch+`","worktree_root":"`+filepath.Join(root, "data", "worktrees")+`"}}`)
	env := map[string]string{"HOME": home, "CODEX_HOME": cfg + "/link/..", "CRW_CONFIG": crw}
	t.Cleanup(Inject(func(name string) string { return env[name] }, home))
	if err := Check(project, "workspace-write"); err != nil {
		t.Fatalf("a project the Codex configuration registers: %v", err)
	}
	if err := Check(repo, "workspace-write"); err != nil {
		t.Fatalf("a linked worktree under the Codex worktrees: %v", err)
	}
	if err := CheckDestination(filepath.Join(codex, "worktrees", "def", "repo"), "workspace-write"); err != nil {
		t.Fatalf("a destination under the Codex worktrees: %v", err)
	}
	// The decoy directory next to the link is no worktree base.
	if err := CheckDestination(filepath.Join(cfg, "worktrees", "def", "repo"), "workspace-write"); err == nil {
		t.Fatal("the lexical spelling's worktrees directory was taken for a base")
	}
}

// CRW-1588: a .git file is a linked worktree's marker only when it resolves to a Git directory; an empty,
// garbage or dangling file does not authorize the directory below a worktree base.
func TestLinkedWorktreeRequiresAValidGitfile(t *testing.T) {
	base := t.TempDir()
	checkout := filepath.Join(base, "slot", "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &rules{worktreeBase: []string{base}}
	marker := filepath.Join(checkout, ".git")
	for _, content := range []string{"", "garbage\n", "gitdir: " + filepath.Join(base, "missing") + "\n"} {
		if err := os.WriteFile(marker, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if r.linkedWorktree(checkout) {
			t.Errorf("invalid marker %q counted as linked worktree", content)
		}
	}
	target := filepath.Join(base, "metadata")
	gitMeta(t, target)
	if err := os.WriteFile(marker, []byte("gitdir: "+target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !r.linkedWorktree(checkout) {
		t.Fatal("valid gitfile did not identify linked worktree")
	}
}
