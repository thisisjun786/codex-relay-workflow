package reviewadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAstraSuccessfulNonReadingCommandsAreNotRead(t *testing.T) {
	p := AstraProfile("/run/answer.strict.json")
	lines := []string{}
	for i, command := range []string{"pwd", "date", "true"} {
		b, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": fmt.Sprintf("cmd_%d", i), "type": "command_execution", "command": command, "exit_code": 0, "status": "completed"}})
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	res := checkFor(t, p, fakeRun{stdout: strings.Join(lines, "\n") + "\n" + astraStream(`{"verdict":"verified","findings":[]}`)}, 50*time.Second)
	wantState(t, res, StateNotRead)
	if res.Commands != 0 || res.Verdict != "" || len(res.Findings) != 0 {
		t.Fatalf("nonreading pass: %+v", res)
	}
}

func readProofCheckout(t *testing.T) (root, outside string) {
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
	write(filepath.Join(root, "README.md"), "the readme\n")
	write(filepath.Join(root, "source.go"), "package x\n")
	write(filepath.Join(root, "sub", "nested.go"), "package sub\n")
	write(filepath.Join(root, "with space", "inner.go"), "package s\n")
	write(filepath.Join(outside, "secret.txt"), "outside\n")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func astraCommandLine(t *testing.T, id, command string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{
		"id": id, "type": "command_execution", "command": command, "exit_code": 0, "status": "completed",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func astraCommandEvent(t *testing.T, id, command, status string, exit *int) string {
	t.Helper()
	item := map[string]any{"id": id, "type": "command_execution", "command": command, "status": status}
	if exit != nil {
		item["exit_code"] = *exit
	}
	b, err := json.Marshal(map[string]any{"type": "item.completed", "item": item})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAstraReadingCommandsSatisfyTheFloor(t *testing.T) {
	root, _ := readProofCheckout(t)
	astra := AstraProfile("/run/answer.strict.json")
	reads := []string{
		astraCommandLine(t, "read_0", "/usr/bin/zsh -lc 'cat README.md'"),
		astraCommandLine(t, "read_1", "/usr/bin/zsh -lc 'sed -n 1,20p source.go'"),
		astraCommandLine(t, "read_2", `/usr/bin/zsh -lc 'nl -ba source.go | sed -n "1,20p"'`),
	}
	stdout := strings.Join(reads, "\n") + "\n" + astraStream(`{"verdict":"verified","findings":[]}`)
	res := checkForWorktree(t, astra, root, fakeRun{stdout: stdout}, 50*time.Second)
	wantState(t, res, StateOK)
	if res.Commands != 3 || res.Verdict != VerdictVerified {
		t.Fatalf("result = %+v, want three reads and verified", res)
	}
	wantState(t, checkForWorktree(t, astra, root, fakeRun{stdout: stdout}, 44*time.Second), StateNotRead)
}

func TestAstraStreamsWithoutReadsStayVoid(t *testing.T) {
	root, _ := readProofCheckout(t)
	astra := AstraProfile("/run/answer.strict.json")
	res := checkForWorktree(t, astra, root, fakeRun{stdout: astraStream(`{"verdict":"verified","findings":[]}`)}, 50*time.Second)
	wantState(t, res, StateNotRead)
	if res.Commands != 0 || res.Verdict != "" {
		t.Fatalf("the real fixture: %+v, want no commands and no verdict", res)
	}
	t.Run("two reads beside a neutral command count two", func(t *testing.T) {
		stdout := strings.Join([]string{
			astraCommandLine(t, "r1", "cat README.md"),
			astraCommandLine(t, "r2", "pwd"),
			astraCommandLine(t, "r3", "sed -n 1,9p source.go"),
		}, "\n") + "\n" + astraStream(`{"verdict":"verified","findings":[]}`)
		res := checkForWorktree(t, astra, root, fakeRun{stdout: stdout}, 50*time.Second)
		wantState(t, res, StateNotRead)
		if res.Commands != 2 {
			t.Fatalf("result = %+v, want two commands", res)
		}
	})
	t.Run("one event of several reads is one command", func(t *testing.T) {
		stdout := astraCommandLine(t, "r1", "/usr/bin/zsh -lc 'cat README.md; sed -n 1,9p source.go; cat sub/nested.go'") +
			"\n" + astraStream(`{"verdict":"verified","findings":[]}`)
		res := checkForWorktree(t, astra, root, fakeRun{stdout: stdout}, 50*time.Second)
		wantState(t, res, StateNotRead)
		if res.Commands != 1 {
			t.Fatalf("result = %+v, want one command", res)
		}
	})
	t.Run("unproven prefixes hidden by a trailing true count nothing", func(t *testing.T) {
		for i, command := range []string{
			"ls /definitely-missing && cat README.md; true",
			"/bin/sh -c 'exit 0; cat " + filepath.Join(root, "README.md") + "'",
			"cat() { :; }; cat README.md",
		} {
			stdout := strings.Join([]string{
				astraCommandLine(t, "a0", command),
				astraCommandLine(t, "a1", command),
				astraCommandLine(t, "a2", command),
			}, "\n") + "\n" + astraStream(`{"verdict":"verified","findings":[]}`)
			res := checkForWorktree(t, astra, root, fakeRun{stdout: stdout}, 50*time.Second)
			wantState(t, res, StateNotRead)
			if res.Commands != 0 {
				t.Fatalf("event %d %q: commands = %d, want none", i, command, res.Commands)
			}
		}
	})
}

func TestAstraReadProofNeedsACompletedSuccess(t *testing.T) {
	root, _ := readProofCheckout(t)
	astra := AstraProfile("/run/answer.strict.json")
	req := Request{Worktree: root}
	one := 1
	turn := `{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4}}`
	for name, lines := range map[string][]string{
		"a failed read":       {astraCommandEvent(t, "c", "cat README.md", "failed", &one), turn},
		"a read with no exit": {astraCommandEvent(t, "c", "cat README.md", "completed", nil), turn},
		"a read in progress":  {astraCommandEvent(t, "c", "cat README.md", "in_progress", nil), turn},
	} {
		t.Run(name, func(t *testing.T) {
			if u, ok := astra.Usage(t.Context(), []byte(strings.Join(lines, "\n")), req); !ok || u.Commands != 0 {
				t.Fatalf("usage %+v %v, want no commands", u, ok)
			}
		})
	}
}

func TestCommandReadsCheckout(t *testing.T) {
	root, outside := readProofCheckout(t)
	q := func(p string) string { return "'" + p + "'" }
	for name, command := range map[string]string{
		"an absolute quoted path":           "cat " + q(filepath.Join(root, "README.md")),
		"a quoted path with a space":        "cat " + q(filepath.Join(root, "with space", "inner.go")),
		"cd into the checkout then read":    "cd " + q(root) + " && cat README.md",
		"cd into a spaced subdir":           "cd " + q(filepath.Join(root, "with space")) + " && cat inner.go",
		"cd then read in a list":            "cd " + q(root) + "; sed -n 1,20p source.go",
		"a zsh -lc recording":               "/usr/bin/zsh -lc 'cat README.md'",
		"a bash -c recording":               "/bin/bash -c 'sed -n 1,9p source.go'",
		"sed with a bare script":            "sed -n 1,20p source.go",
		"sed with a quoted script":          "sed -n '1,20p' source.go",
		"sed with -e":                       "sed -n -e '1,20p' source.go",
		"sed with combined flags":           "sed -nE '1,20p' source.go",
		"an input redirect":                 "cat < README.md",
		"the last input redirect":           "cat < /dev/null < README.md",
		"a pipeline":                        "nl -ba source.go | sed -n '1,20p'",
		"head":                              "head -20 source.go",
		"tail":                              "tail -n 5 source.go",
		"a nested file":                     "cat sub/nested.go",
		"a dotdot that stays inside":        "cat sub/../source.go",
		"rg on the checkout":                "rg -n 'package' .",
		"rg with -e and -g":                 "rg -e 'package' -g '*.go' .",
		"rg searching the cwd":              "rg 'package x'",
		"rg on a nested dir":                "rg --hidden 'package' sub",
		"a true prefix":                     "true && cat README.md",
		"a pipeline cd stays inside":        "cd " + q(outside) + " | cat; cat README.md",
		"a final && both ran":               "git status --short && sed -n 1,20p source.go",
		"a final && of a neutral then read": "printf 'header\n' && cat README.md",
		"git show a revision":               "git show HEAD",
		"git show a patch of a path":        "git show HEAD -- source.go",
		"git diff a path":                   "git diff -- source.go",
		"git diff a range":                  "git diff HEAD~1",
		"git diff the worktree":             "git diff",
	} {
		t.Run("reads: "+name, func(t *testing.T) {
			if !commandReadsCheckout(t.Context(), command, root) {
				t.Fatalf("%q is a read of the checkout", command)
			}
		})
	}
	for name, command := range map[string]string{
		"pwd":                                     "pwd",
		"date":                                    "date",
		"true":                                    "true",
		"ls":                                      "ls",
		"git status":                              "git status --short",
		"an echoed command":                       "echo 'cat README.md'",
		"an outside path":                         "cat " + q(filepath.Join(outside, "secret.txt")),
		"a parent escape":                         "cat " + filepath.Join("..", filepath.Base(outside), "secret.txt"),
		"a symlink out":                           "cat link.txt",
		"cd outside then read":                    "cd " + q(outside) + " && cat secret.txt",
		"cd nowhere then relative":                "cd $d; cat README.md",
		"bare cd then read":                       "cd; cat README.md",
		"bare popd then read":                     "popd; cat README.md",
		"an unknown call then read":               "somefunchange; cat README.md",
		"a git env override":                      "GIT_DIR=" + filepath.Join(root, ".git") + " git show HEAD:source.go",
		"a standalone assignment":                 "GIT_DIR=" + q(outside) + "; git show HEAD:source.go",
		"an override prefix":                      "HOME=" + q(outside) + " cat README.md",
		"a function shadowing cat":                "cat() { :; }; cat README.md",
		"exit before the read":                    "exit 0; cat " + q(filepath.Join(root, "README.md")),
		"a shell that exits first":                "/bin/sh -c 'exit 0; cat " + filepath.Join(root, "README.md") + "'",
		"a failed ls prefix":                      "ls /definitely-missing && cat README.md; true",
		"a failed reader prefix":                  "cat missing.go && cat README.md; true",
		"a neutral prefix then true":              "printf 'x\n' && cat README.md; true",
		"an unproven && then read":                "maybe && cd " + q(outside) + "; cat README.md",
		"a later stdin redirect wins":             "cat < README.md < /dev/null",
		"a here-string overrides":                 "cat < README.md <<< 'text'",
		"a glob is not a read":                    "cat *.md",
		"a quoted glob":                           "cat '*.md'; true",
		"an escaped glob":                         "cat \\*.md; true",
		"a dash operand is stdin":                 "cat -",
		"git -C outside":                          "git -C " + q(outside) + " show HEAD:source.go",
		"git -C a subdir show a dir":              "git -C " + q(filepath.Join(root, "sub")) + " show HEAD:sub",
		"git diff outside paths":                  "git diff " + q(filepath.Join(outside, "secret.txt")) + " " + q(filepath.Join(outside, "secret.txt")),
		"rg on an outside dir":                    "rg 'README' " + q(outside),
		"rg --files":                              "rg --files",
		"rg a type list":                          "rg --type-list",
		"cat --help":                              "cat --help README.md",
		"sed in place":                            "sed -i 's/x/y/' source.go",
		"a failed && prefix":                      "false && cat README.md; true",
		"an || right side":                        "true || cat README.md",
		"an if":                                   "if false; then cat README.md; fi",
		"an uncalled function":                    "f() { cat README.md; }",
		"cat on plain stdin":                      "cat",
		"a piped echo":                            "echo hi | cat",
		"a substitution operand":                  "cat $f",
		"a command substitution":                  "cat $(echo README.md)",
		"a relative script":                       "./cat README.md",
		"eval":                                    "eval 'cat README.md'",
		"source":                                  ". ./lib.sh",
		"a non-file":                              "cat /dev/null",
		"a background read":                       "cat README.md &",
		"a negated read":                          "! cat README.md",
		"a git blob hash":                         "git show 8be04d7a1b2c3d4e5f60718293a4b5c6d7e8f901",
		"git show a tree":                         "git show HEAD:",
		"git show a directory":                    "git show HEAD:sub",
		"git show a tree object":                  "git show 'HEAD^{tree}'",
		"git show a blob object":                  "git show 'HEAD^{blob}'",
		"git show stats only":                     "git show --stat HEAD",
		"git show names only":                     "git show --name-only HEAD",
		"git show --no-patch":                     "git show --no-patch HEAD",
		"git show a format":                       "git show --format=%s HEAD",
		"git diff stats":                          "git diff --stat",
		"git diff without an index":               "git diff --no-index source.go README.md",
		"git log":                                 "git log -1",
		"git ls-files":                            "git ls-files",
		"a git repo override":                     "git --git-dir " + q(filepath.Join(root, ".git")) + " show HEAD:source.go",
		"git -C show a file outside a repository": "git -C " + q(root) + " show HEAD:source.go",
		"git -C chains from the last -C outside a repository": "git -C " + q(filepath.Dir(root)) + " -C " + filepath.Base(root) + " show HEAD:source.go",
		"git -C a subdir show a file outside a repository":    "git -C " + q(filepath.Join(root, "sub")) + " show HEAD:source.go",
		"git show a historical file outside a repository":     "git show HEAD:old.go",
		"git show a file outside a repository":                "git show HEAD:source.go",
	} {
		t.Run("not a read: "+name, func(t *testing.T) {
			if commandReadsCheckout(t.Context(), command, root) {
				t.Fatalf("%q proves no read of the checkout", command)
			}
		})
	}
}

// revisionRepo1618 is a repository of three commits for TestReadProof1618RevisionObjectType: c0 holds the regular file p and
// old.go; c1 turns p into a directory with p/inner.go; c2 removes old.go and turns p into a symlink to README.md. The worktree
// is HEAD, so p is the symlink and old.go is gone.
func revisionRepo1618(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=ReadProof", "-c", "user.email=readproof@example.invalid"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(path, text string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init")
	write("README.md", "the readme\n")
	write("source.go", "package x\n")
	write("sub/nested.go", "package sub\n")
	write("old.go", "package old\n")
	write("p", "a regular file at c0\n")
	git("add", "README.md", "source.go", "sub/nested.go", "old.go", "p")
	git("commit", "-m", "c0")
	git("rm", "-q", "p")
	write("p/inner.go", "package inner\n")
	git("add", "p")
	git("commit", "-m", "c1")
	git("rm", "-r", "-q", "p", "old.go")
	if err := os.Symlink("README.md", filepath.Join(dir, "p")); err != nil {
		t.Fatal(err)
	}
	git("add", "p")
	git("commit", "-m", "c2")
	return dir
}

// fakeGit1618 puts a git first on PATH. It writes its arguments to a start marker, then runs tail. The marker is written under a
// temporary name and renamed, so it exists only once the fake has started; its path is returned.
func fakeGit1618(t *testing.T, tail string) string {
	t.Helper()
	dir := t.TempDir()
	if strings.Contains(dir, "'") {
		t.Fatalf("the fake git directory %q has a single quote", dir)
	}
	marker := filepath.Join(dir, "started")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" > '" + dir + "/started.tmp' && mv '" + dir + "/started.tmp' '" + marker + "'\n" +
		tail + "\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

// objectType1618 is the type git reports for object (a <rev>:<path> operand) in repo, or "" when git names no object.
func objectType1618(repo, object string) string {
	cmd := exec.Command("git", "cat-file", "-t", object)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestReadProof1618RevisionObjectType: a <rev>:<path> operand reads the checkout only when git names a blob for it at that revision.
// Each case first asks git for the object's type. A separator case also runs the command, and git's own output must show what the
// object is: a tree's header, a blob's content, or a failure.
func TestReadProof1618RevisionObjectType(t *testing.T) {
	repo := revisionRepo1618(t)
	q := func(p string) string { return "'" + p + "'" }
	parent, base := filepath.Dir(repo), filepath.Base(repo)
	for _, c := range []struct {
		name, command, object, typ string
		separator, read            bool
	}{
		{"not a read: a tree at an old revision that is a symlink now", "git show HEAD~1:p", "HEAD~1:p", "tree", false, false},
		{"not a read: a path deleted at HEAD", "git show HEAD:old.go", "HEAD:old.go", "", false, false},
		{"not a read: a missing path", "git show HEAD:missing.go", "HEAD:missing.go", "", false, false},
		{"not a read: a directory at HEAD", "git show HEAD:sub", "HEAD:sub", "tree", false, false},
		{"not a read: a tree with a pathspec", "git show HEAD:sub -- README.md", "HEAD:sub", "tree", true, false},
		{"not a read: an old tree with a pathspec", "git show HEAD~1:p -- README.md", "HEAD~1:p", "tree", true, false},
		{"not a read: a missing path with a pathspec", "git show HEAD:missing.go -- README.md", "HEAD:missing.go", "", true, false},
		{"not a read: an old tree before a trailing separator", "git show HEAD~1:p --", "HEAD~1:p", "tree", true, false},
		{"not a read: a tree before a trailing separator", "git show HEAD:sub --", "HEAD:sub", "tree", true, false},
		{"not a read: a missing path before a trailing separator", "git show HEAD:missing.go --", "HEAD:missing.go", "", true, false},
		{"read: a blob before a trailing separator", "git show HEAD:source.go --", "HEAD:source.go", "blob", true, true},
		{"read: a regular file at an old revision", "git show HEAD~2:p", "HEAD~2:p", "blob", false, true},
		{"read: a stored symlink", "git show HEAD:p", "HEAD:p", "blob", false, true},
		{"read: a file under an old directory", "git show HEAD~1:p/inner.go", "HEAD~1:p/inner.go", "blob", false, true},
		{"read: a file deleted since that revision", "git show HEAD~1:old.go", "HEAD~1:old.go", "blob", false, true},
		{"read: a file at HEAD", "git show HEAD:source.go", "HEAD:source.go", "blob", false, true},
		{"read: git -C the checkout", "git -C " + q(repo) + " show HEAD:source.go", "HEAD:source.go", "blob", false, true},
		{"read: git -C a parent, then the checkout", "git -C " + q(parent) + " -C " + base + " show HEAD:source.go", "HEAD:source.go", "blob", false, true},
		{"read: git -C a subdirectory of the checkout", "git -C " + q(filepath.Join(repo, "sub")) + " show HEAD:source.go", "HEAD:source.go", "blob", false, true},
		{"read: a blob with a pathspec", "git show HEAD:source.go -- source.go", "HEAD:source.go", "blob", true, true},
		{"read: an old blob with a pathspec", "git show HEAD~1:old.go -- README.md", "HEAD~1:old.go", "blob", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := objectType1618(repo, c.object); got != c.typ {
				t.Fatalf("git names %q as %q, want %q", c.object, got, c.typ)
			}
			if c.separator {
				cmd := exec.Command("/bin/sh", "-c", c.command)
				cmd.Dir = repo
				out, err := cmd.CombinedOutput()
				switch c.typ {
				case "":
					if err == nil {
						t.Fatalf("%q printed %q, want git to fail", c.command, out)
					}
				case "tree":
					if err != nil || !strings.HasPrefix(string(out), "tree "+c.object) {
						t.Fatalf("%q printed %q, %v; want the tree's header", c.command, out, err)
					}
				case "blob":
					want, werr := exec.Command("git", "-C", repo, "cat-file", "-p", c.object).Output()
					if err != nil || werr != nil || string(out) != string(want) {
						t.Fatalf("%q printed %q, %v; want the blob %q, %v", c.command, out, err, want, werr)
					}
				}
			}
			if got := commandReadsCheckout(t.Context(), c.command, repo); got != c.read {
				t.Fatalf("%q reads the checkout: %v, want %v", c.command, got, c.read)
			}
		})
	}
}

// TestReadProof1618EndedCallCountsNoRead: a git lookup belongs to the caller's call. Under a live call the blob counts. Under an
// ended call no lookup starts, so the fake git on PATH never runs and nothing counts.
func TestReadProof1618EndedCallCountsNoRead(t *testing.T) {
	repo := revisionRepo1618(t)
	const command = "git show HEAD:source.go"
	if !commandReadsCheckout(t.Context(), command, repo) {
		t.Fatalf("%q is a read of the checkout under a live call", command)
	}
	t.Run("an ended call starts no lookup", func(t *testing.T) {
		marker := fakeGit1618(t, "echo blob")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if commandReadsCheckout(ctx, command, repo) {
			t.Fatalf("%q counts under an ended call", command)
		}
		if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the ended call started a lookup: stat %q: %v", marker, err)
		}
	})
}

// TestReadProof1618StuckGitStopsWithTheCaller: a lookup that hangs ends with the caller's call. The fake git sleeps, and the call
// returns false within 10 s of the cancel, well before the probe's own 30 s bound.
func TestReadProof1618StuckGitStopsWithTheCaller(t *testing.T) {
	root, _ := readProofCheckout(t)
	marker := fakeGit1618(t, "exec sleep 60")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- commandReadsCheckout(ctx, "git show HEAD:README.md", root) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(marker); err == nil {
			if !strings.Contains(string(b), "cat-file -t HEAD:README.md") {
				t.Fatalf("the lookup was %q, want the probe of HEAD:README.md", b)
			}
			break
		}
		select {
		case got := <-done:
			t.Fatalf("the call returned %v before any lookup started", got)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no lookup started within 10 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case got := <-done:
		if got {
			t.Fatal("a lookup that ended with its caller counts a read")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not return within 10 s of the cancel")
	}
}
