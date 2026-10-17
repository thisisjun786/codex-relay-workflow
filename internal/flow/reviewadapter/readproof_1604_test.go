package reviewadapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func aliasRepo1604(t *testing.T) (repo, outside string) {
	t.Helper()
	repo = gitRepo1603(t)
	outside = t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "README.md"), []byte("outside1604-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "nested"), filepath.Join(repo, "alias")); err != nil {
		t.Fatal(err)
	}
	return repo, outside
}

func TestReadProof1604GitObjectPaths(t *testing.T) {
	repo, outside := aliasRepo1604(t)
	for name, command := range map[string]string{
		"lexical dotdot through a symlink name": "git --no-pager show HEAD:./alias/../README.md",
		"lexical dotdot from a subdirectory":    "git --no-pager -C sub show HEAD:../alias/../README.md",
	} {
		t.Run("reads: "+name, func(t *testing.T) {
			if out := runShell1603(t, repo, command); out != "the readme\n" {
				t.Fatalf("%q produced %q, want the tracked readme", command, out)
			}
			if !commandReadsCheckout(command, repo) {
				t.Fatalf("%q is a read of the checkout", command)
			}
		})
	}
	t.Run("not a read: physical file through the symlink", func(t *testing.T) {
		if out := runShell1603(t, repo, "cat alias/../README.md"); out != "outside1604-marker\n" {
			t.Fatalf("cat alias/../README.md produced %q, want the outside file", out)
		}
		if commandReadsCheckout("cat alias/../README.md", repo) {
			t.Fatal("cat alias/../README.md proves no read of the checkout")
		}
	})
	for name, command := range map[string]string{
		"physical cd through the symlink": "cd -P alias/.. && cat README.md",
		"git -C a non-repo outside dir":   "git -C " + outside + " show HEAD:README.md",
		"a tree object":                   "git show HEAD:sub",
		"an empty object path":            "git show HEAD:",
		"an escaping object path":         "git show HEAD:../README.md",
		"stat then separator":             "git show --stat HEAD --",
	} {
		t.Run("not a read: "+name, func(t *testing.T) {
			if commandReadsCheckout(command, repo) {
				t.Fatalf("%q proves no read of the checkout", command)
			}
		})
	}
	t.Run("a stored symlink is blob content", func(t *testing.T) {
		run := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		run("add", "alias")
		run("-c", "user.name=ReadProof", "-c", "user.email=readproof@example.invalid", "commit", "-m", "store the symlink")
		target := filepath.Join(outside, "nested")
		if out := runShell1603(t, repo, "git --no-pager show HEAD:alias"); strings.TrimSpace(out) != target {
			t.Fatalf("git show HEAD:alias produced %q, want the stored target %q", out, target)
		}
		if !commandReadsCheckout("git show HEAD:alias", repo) {
			t.Fatal("git show HEAD:alias is a read of the checkout")
		}
		if commandReadsCheckout("cat alias", repo) {
			t.Fatal("cat alias proves no read of the checkout")
		}
	})
}

func TestReadProof1604StdinAliases(t *testing.T) {
	root, outside := inputTree1603(t)
	for _, alias := range []string{"/dev/stdin", "/dev/fd/0", "/proc/self/fd/0"} {
		for name, command := range map[string]string{
			"head reads the alias":    "head -n 1 " + alias + " < README.md",
			"cat reads the alias":     "cat " + alias + " < README.md",
			"sed reads the alias":     "sed -n 1p " + alias + " < README.md",
			"wrapper reads the alias": "/bin/sh -c 'head -n 1 " + alias + "' < README.md",
			"cd keeps the opened fd":  "{ cd sub; head -n 1 " + alias + "; } < README.md",
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
			"no redirect at all":       "head -n 1 " + alias,
			"pipe instead of a file":   "echo hello | head -n 1 " + alias,
			"redirected from devnull":  "head -n 1 " + alias + " < /dev/null",
			"redirected from outside":  "head -n 1 " + alias + " < " + filepath.Join(outside, "secret.txt"),
			"inner redirect overrides": "/bin/sh -c 'head -n 1 " + alias + " < /dev/null' < README.md",
		} {
			t.Run("not a read: "+name, func(t *testing.T) {
				if commandReadsCheckout(command, root) {
					t.Fatalf("%q proves no read of the checkout", command)
				}
			})
		}
	}
	t.Run("reads: an explicit file beside the alias", func(t *testing.T) {
		command := "cat /dev/stdin README.md < " + filepath.Join(outside, "secret.txt")
		if out := runShell1603(t, root, command); !strings.Contains(out, "proof1603-marker") {
			t.Fatalf("%q produced %q, want the checkout file", command, out)
		}
		if !commandReadsCheckout(command, root) {
			t.Fatalf("%q is a read of the checkout", command)
		}
	})
	for _, p := range []string{"-", "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0"} {
		if !stdinOperand(p) {
			t.Fatalf("%q is an explicit stdin operand", p)
		}
	}
	for _, p := range []string{"/dev/stdin-other", "/dev/stdin0", "/dev/fd/1", "/proc/self/fd/1", "/dev/fd", "/proc/self/fd", "/dev/stdout", "stdin", ""} {
		if stdinOperand(p) {
			t.Fatalf("%q is not an explicit stdin operand", p)
		}
	}
	for name, command := range map[string]string{
		"a prefixed alias": "head -n 1 /dev/stdin-other < README.md",
		"fd1 alias":        "head -n 1 /dev/fd/1 < README.md",
		"proc fd1 alias":   "head -n 1 /proc/self/fd/1 < README.md",
	} {
		t.Run("not a read: "+name, func(t *testing.T) {
			if commandReadsCheckout(command, root) {
				t.Fatalf("%q proves no read of the checkout", command)
			}
		})
	}
}

func TestReadProof1604AdapterFloors(t *testing.T) {
	repo, _ := inputTree1603(t)
	astra := AstraProfile("/run/answer.strict.json")
	reads := []string{
		astraCommandLine(t, "a0", "git --no-pager show HEAD:./alias/../README.md"),
		astraCommandLine(t, "a1", "head -n 1 /dev/stdin < README.md"),
		astraCommandLine(t, "a2", "/bin/sh -c 'cat /proc/self/fd/0' < README.md"),
	}
	stdout := strings.Join(reads, "\n") + "\n" + astraStream(`{"verdict":"verified","findings":[]}`)
	res := checkForWorktree(t, astra, repo, fakeRun{stdout: stdout}, 50*time.Second)
	wantState(t, res, StateOK)
	if res.Commands != 3 || res.Verdict != VerdictVerified {
		t.Fatalf("result = %+v, want three reads and verified", res)
	}
	wantState(t, checkForWorktree(t, astra, repo, fakeRun{stdout: stdout}, 44*time.Second), StateNotRead)
	nonreads := []string{
		astraCommandLine(t, "n0", "pwd"),
		astraCommandLine(t, "n1", "date"),
		astraCommandLine(t, "n2", "true"),
	}
	res = checkForWorktree(t, astra, repo, fakeRun{stdout: strings.Join(nonreads, "\n") + "\n" + astraStream(`{"verdict":"verified","findings":[]}`)}, 50*time.Second)
	wantState(t, res, StateNotRead)
	if res.Commands != 0 {
		t.Fatalf("result = %+v, want no commands", res)
	}
	req := Request{Worktree: repo}
	one := 1
	turn := `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4}}`
	for name, lines := range map[string][]string{
		"a failed corrected read":  {astraCommandEvent(t, "c", "head -n 1 /dev/stdin < README.md", "failed", &one), turn},
		"a corrected read no exit": {astraCommandEvent(t, "c", "git --no-pager show HEAD:./alias/../README.md", "completed", nil), turn},
	} {
		t.Run(name, func(t *testing.T) {
			if u, ok := astra.Usage([]byte(strings.Join(lines, "\n")), req); !ok || u.Commands != 0 {
				t.Fatalf("usage %+v %v, want no commands", u, ok)
			}
		})
	}
}
