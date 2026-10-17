package reviewadapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRepo1603(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("the readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("init")
	run("add", "README.md", "sub/f.go")
	run("-c", "user.name=ReadProof", "-c", "user.email=readproof@example.invalid", "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("the readme\nmore lines\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.go"), []byte("package sub\nvar changed = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runShell1603(t *testing.T, dir, command string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%q failed: %v\n%s", command, err, out)
	}
	return string(out)
}

func inputTree1603(t *testing.T) (root, outside string) {
	t.Helper()
	root = t.TempDir()
	outside = t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "README.md"), "proof1603-marker\n")
	write(filepath.Join(root, "sub", "nested.go"), "package sub\n")
	write(filepath.Join(outside, "secret.txt"), "outside1603-marker\n")
	return root, outside
}

func TestReadProof1603GitSeparators(t *testing.T) {
	repo := gitRepo1603(t)
	for name, tc := range map[string]struct{ command, marker string }{
		"show patch trailing separator":    {"git --no-pager show HEAD --", "the readme"},
		"diff HEAD trailing separator":     {"git --no-pager diff HEAD --", "+more lines"},
		"worktree diff trailing separator": {"git --no-pager diff --", "+more lines"},
		"show path with separator":         {"git --no-pager show HEAD -- README.md", "the readme"},
	} {
		t.Run("reads: "+name, func(t *testing.T) {
			if out := runShell1603(t, repo, tc.command); !strings.Contains(out, tc.marker) {
				t.Fatalf("%q produced %q, want marker %q", tc.command, out, tc.marker)
			}
			if !commandReadsCheckout(tc.command, repo) {
				t.Fatalf("%q is a read of the checkout", tc.command)
			}
		})
	}
	for name, command := range map[string]string{
		"stat then separator":     "git --no-pager show --stat HEAD --",
		"no-patch then separator": "git --no-pager show --no-patch HEAD --",
		"status":                  "git status --short",
		"log":                     "git --no-pager log --",
	} {
		t.Run("not a read: "+name, func(t *testing.T) {
			if commandReadsCheckout(command, repo) {
				t.Fatalf("%q proves no read of the checkout", command)
			}
		})
	}
}

func TestReadProof1603GitOptionalValues(t *testing.T) {
	repo := gitRepo1603(t)
	for name, command := range map[string]string{
		"diff unified":          "git --no-pager diff --unified",
		"diff unified attached": "git --no-pager diff --unified=3",
		"diff color":            "git --no-pager diff --color",
		"diff color always":     "git --no-pager diff --color=always",
		"diff color never":      "git --no-pager diff --color=never",
		"diff abbrev":           "git --no-pager diff --abbrev",
		"diff abbrev attached":  "git --no-pager diff --abbrev=7",
		"diff find-renames":     "git --no-pager diff --find-renames",
		"diff find-renames pct": "git --no-pager diff --find-renames=50%",
		"diff find-copies":      "git --no-pager diff --find-copies",
		"diff find-copies pct":  "git --no-pager diff --find-copies=50%",
		"diff relative":         "git --no-pager diff --relative",
		"diff relative subdir":  "git --no-pager diff --relative=sub",
		"diff -U attached":      "git --no-pager diff -U3",
		"show unified":          "git --no-pager show HEAD --unified",
		"show color always":     "git --no-pager show HEAD --color=always",
		"show abbrev attached":  "git --no-pager show HEAD --abbrev=7",
		"show find-renames":     "git --no-pager show HEAD --find-renames",
		"show relative":         "git --no-pager show HEAD --relative",
	} {
		t.Run("reads: "+name, func(t *testing.T) {
			if out := runShell1603(t, repo, command); !strings.Contains(out, "diff --git") {
				t.Fatalf("%q produced %q, want a patch", command, out)
			}
			if !commandReadsCheckout(command, repo) {
				t.Fatalf("%q is a read of the checkout", command)
			}
		})
	}
	outside := filepath.Join(t.TempDir(), "nope")
	for name, command := range map[string]string{
		"optional then stat":         "git --no-pager diff --color --stat",
		"optional then no-patch":     "git --no-pager show HEAD --relative --no-patch",
		"optional then outside path": "git --no-pager diff --color " + outside,
		"required value missing":     "git --no-pager diff --src-prefix",
		"stat":                       "git --no-pager diff --stat",
		"status":                     "git status --short",
	} {
		t.Run("not a read: "+name, func(t *testing.T) {
			if commandReadsCheckout(command, repo) {
				t.Fatalf("%q proves no read of the checkout", command)
			}
		})
	}
}

func TestReadProof1603InheritedInput(t *testing.T) {
	root, outside := inputTree1603(t)
	for name, command := range map[string]string{
		"wrapper reads the redirect":      "/bin/sh -c 'cat' < README.md",
		"wrapper sed reads the redirect":  "/bin/sh -c 'sed -n 1p' < README.md",
		"block reads the redirect":        "{ cat; } < README.md",
		"subshell reads the redirect":     "(cat) < README.md",
		"nested wrapper reads the file":   `/bin/sh -c '/bin/sh -c "cat"' < README.md`,
		"cd inside keeps the opened file": "{ cd sub; cat; } < README.md",
		"wrapper dash operand":            "/bin/sh -c 'cat -' < README.md",
		"redirect beats the pipe":         "echo ignored | /bin/sh -c 'cat' < README.md",
		"other fd open keeps stdin":       "/bin/sh -c 'cat' < README.md 3</dev/null",
		"other fd close keeps stdin":      "/bin/sh -c 'cat' < README.md 3<&-",
	} {
		t.Run("reads: "+name, func(t *testing.T) {
			if out := runShell1603(t, root, command); !strings.Contains(out, "proof1603-marker") {
				t.Fatalf("%q produced %q, want the file content", command, out)
			}
			if !commandReadsCheckout(command, root) {
				t.Fatalf("%q is a read of the checkout", command)
			}
		})
	}
	for name, command := range map[string]string{
		"wrapper stdin is devnull":       "/bin/sh -c 'cat' < /dev/null",
		"inner redirect wins":            "/bin/sh -c 'cat < /dev/null' < README.md",
		"inner here-string wins":         "(cat <<< 'not repo') < README.md",
		"piped wrapper without a file":   "echo marker | /bin/sh -c 'cat'",
		"explicit operand ignores stdin": "{ cat /dev/null; } < README.md",
		"wrapper operand ignores stdin":  "/bin/sh -c 'cat /dev/null' < README.md",
		"inner local redirect no leak":   "{ true < README.md; cat; } < /dev/null",
		"heredoc overrides the file":     "cat < README.md <<'EOF'\nx\nEOF",
		"inner pipe stdin":               "/bin/sh -c 'echo hi | cat' < README.md",
		"outside file":                   "/bin/sh -c 'cat' < " + filepath.Join(outside, "secret.txt"),
		"pwd":                            "pwd",
		"date":                           "date",
		"true":                           "true",
	} {
		t.Run("not a read: "+name, func(t *testing.T) {
			if commandReadsCheckout(command, root) {
				t.Fatalf("%q proves no read of the checkout", command)
			}
		})
	}
}

func TestReadProof1603SymlinkParents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "sub", "nested", "keep.go"), "package keep\n")
	write(filepath.Join(root, "sub", "only.go"), "only-marker\n")
	if err := os.Symlink(filepath.Join(root, "sub", "nested"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "sub", "nested"), filepath.Join(outside, "back")); err != nil {
		t.Fatal(err)
	}
	for name, command := range map[string]string{
		"relative through a symlink parent":    "cat alias/../only.go",
		"absolute through a symlink parent":    "cat " + root + "/alias/../only.go",
		"redirect through a symlink parent":    "cat < alias/../only.go",
		"physical cd through a symlink":        "cd -P alias/.. && cat only.go",
		"absolute outside landing back inside": "cat " + outside + "/back/../only.go",
	} {
		t.Run("reads: "+name, func(t *testing.T) {
			if out := runShell1603(t, root, command); !strings.Contains(out, "only-marker") {
				t.Fatalf("%q produced %q, want the file content", command, out)
			}
			if !commandReadsCheckout(command, root) {
				t.Fatalf("%q is a read of the checkout", command)
			}
		})
	}
	t.Run("reads: output through a symlink parent", func(t *testing.T) {
		runShell1603(t, root, "cat alias/../only.go > alias/../capture.txt")
		if !commandReadsCheckout("cat alias/../only.go > alias/../capture.txt", root) {
			t.Fatal("the output redirect form is a read of the checkout")
		}
	})
	if out, err := os.ReadFile(filepath.Join(root, "sub", "capture.txt")); err != nil || !strings.Contains(string(out), "only-marker") {
		t.Fatalf("the output redirect wrote %q, %v", out, err)
	}
	escape := t.TempDir()
	outside2 := t.TempDir()
	write(filepath.Join(escape, "only.go"), "decoy-marker\n")
	write(filepath.Join(outside2, "nested", "keep.txt"), "keep\n")
	write(filepath.Join(outside2, "only.go"), "outside-marker\n")
	if err := os.Symlink(filepath.Join(outside2, "nested"), filepath.Join(escape, "escape")); err != nil {
		t.Fatal(err)
	}
	for name, command := range map[string]string{
		"escape symlink parent":   "cat escape/../only.go",
		"physical cd outside":     "cd -P escape/.. && cat only.go",
		"git -C outside physical": "git -C " + escape + "/escape/.. show HEAD:only.go",
	} {
		t.Run("not a read: "+name, func(t *testing.T) {
			if command == "cat escape/../only.go" {
				if out := runShell1603(t, escape, command); !strings.Contains(out, "outside-marker") {
					t.Fatalf("%q produced %q, want the outside file", command, out)
				}
			}
			if commandReadsCheckout(command, escape) {
				t.Fatalf("%q proves no read of the checkout", command)
			}
		})
	}
}

func TestReadProof1603AdapterFloors(t *testing.T) {
	root, _ := readProofCheckout(t)
	astra := AstraProfile("/run/answer.strict.json")
	reads := []string{
		astraCommandLine(t, "fix_0", "/bin/sh -c 'cat' < README.md"),
		astraCommandLine(t, "fix_1", "git show HEAD --"),
		astraCommandLine(t, "fix_2", "{ cat; } < sub/nested.go"),
	}
	stdout := strings.Join(reads, "\n") + "\n" + astraStream(`{"verdict":"verified","findings":[]}`)
	res := checkForWorktree(t, astra, root, fakeRun{stdout: stdout}, 50*time.Second)
	wantState(t, res, StateOK)
	if res.Commands != 3 || res.Verdict != VerdictVerified {
		t.Fatalf("result = %+v, want three reads and verified", res)
	}
	wantState(t, checkForWorktree(t, astra, root, fakeRun{stdout: stdout}, 44*time.Second), StateNotRead)
	nonreads := []string{
		astraCommandLine(t, "n0", "pwd"),
		astraCommandLine(t, "n1", "date"),
		astraCommandLine(t, "n2", "true"),
	}
	res = checkForWorktree(t, astra, root, fakeRun{stdout: strings.Join(nonreads, "\n") + "\n" + astraStream(`{"verdict":"verified","findings":[]}`)}, 50*time.Second)
	wantState(t, res, StateNotRead)
	if res.Commands != 0 {
		t.Fatalf("result = %+v, want no commands", res)
	}
	req := Request{Worktree: root}
	one := 1
	turn := `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4}}`
	for name, lines := range map[string][]string{
		"a failed corrected read":  {astraCommandEvent(t, "c", "/bin/sh -c 'cat' < README.md", "failed", &one), turn},
		"a corrected read no exit": {astraCommandEvent(t, "c", "{ cat; } < sub/nested.go", "completed", nil), turn},
	} {
		t.Run(name, func(t *testing.T) {
			if u, ok := astra.Usage([]byte(strings.Join(lines, "\n")), req); !ok || u.Commands != 0 {
				t.Fatalf("usage %+v %v, want no commands", u, ok)
			}
		})
	}
}
