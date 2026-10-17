package reviewadapter

import (
	"encoding/json"
	"fmt"
	"os"
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
			if u, ok := astra.Usage([]byte(strings.Join(lines, "\n")), req); !ok || u.Commands != 0 {
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
		"git -C show a file":                "git -C " + q(root) + " show HEAD:source.go",
		"git -C chains from the last -C":    "git -C " + q(filepath.Dir(root)) + " -C " + filepath.Base(root) + " show HEAD:source.go",
		"git -C a subdir show a file":       "git -C " + q(filepath.Join(root, "sub")) + " show HEAD:source.go",
		"git show a historical file":        "git show HEAD:old.go",
		"git show a file":                   "git show HEAD:source.go",
		"git show a revision":               "git show HEAD",
		"git show a patch of a path":        "git show HEAD -- source.go",
		"git diff a path":                   "git diff -- source.go",
		"git diff a range":                  "git diff HEAD~1",
		"git diff the worktree":             "git diff",
	} {
		t.Run("reads: "+name, func(t *testing.T) {
			if !commandReadsCheckout(command, root) {
				t.Fatalf("%q is a read of the checkout", command)
			}
		})
	}
	for name, command := range map[string]string{
		"pwd":                         "pwd",
		"date":                        "date",
		"true":                        "true",
		"ls":                          "ls",
		"git status":                  "git status --short",
		"an echoed command":           "echo 'cat README.md'",
		"an outside path":             "cat " + q(filepath.Join(outside, "secret.txt")),
		"a parent escape":             "cat " + filepath.Join("..", filepath.Base(outside), "secret.txt"),
		"a symlink out":               "cat link.txt",
		"cd outside then read":        "cd " + q(outside) + " && cat secret.txt",
		"cd nowhere then relative":    "cd $d; cat README.md",
		"bare cd then read":           "cd; cat README.md",
		"bare popd then read":         "popd; cat README.md",
		"an unknown call then read":   "somefunchange; cat README.md",
		"a git env override":          "GIT_DIR=" + filepath.Join(root, ".git") + " git show HEAD:source.go",
		"a standalone assignment":     "GIT_DIR=" + q(outside) + "; git show HEAD:source.go",
		"an override prefix":          "HOME=" + q(outside) + " cat README.md",
		"a function shadowing cat":    "cat() { :; }; cat README.md",
		"exit before the read":        "exit 0; cat " + q(filepath.Join(root, "README.md")),
		"a shell that exits first":    "/bin/sh -c 'exit 0; cat " + filepath.Join(root, "README.md") + "'",
		"a failed ls prefix":          "ls /definitely-missing && cat README.md; true",
		"a failed reader prefix":      "cat missing.go && cat README.md; true",
		"a neutral prefix then true":  "printf 'x\n' && cat README.md; true",
		"an unproven && then read":    "maybe && cd " + q(outside) + "; cat README.md",
		"a later stdin redirect wins": "cat < README.md < /dev/null",
		"a here-string overrides":     "cat < README.md <<< 'text'",
		"a glob is not a read":        "cat *.md",
		"a quoted glob":               "cat '*.md'; true",
		"an escaped glob":             "cat \\*.md; true",
		"a dash operand is stdin":     "cat -",
		"git -C outside":              "git -C " + q(outside) + " show HEAD:source.go",
		"git -C a subdir show a dir":  "git -C " + q(filepath.Join(root, "sub")) + " show HEAD:sub",
		"git diff outside paths":      "git diff " + q(filepath.Join(outside, "secret.txt")) + " " + q(filepath.Join(outside, "secret.txt")),
		"rg on an outside dir":        "rg 'README' " + q(outside),
		"rg --files":                  "rg --files",
		"rg a type list":              "rg --type-list",
		"cat --help":                  "cat --help README.md",
		"sed in place":                "sed -i 's/x/y/' source.go",
		"a failed && prefix":          "false && cat README.md; true",
		"an || right side":            "true || cat README.md",
		"an if":                       "if false; then cat README.md; fi",
		"an uncalled function":        "f() { cat README.md; }",
		"cat on plain stdin":          "cat",
		"a piped echo":                "echo hi | cat",
		"a substitution operand":      "cat $f",
		"a command substitution":      "cat $(echo README.md)",
		"a relative script":           "./cat README.md",
		"eval":                        "eval 'cat README.md'",
		"source":                      ". ./lib.sh",
		"a non-file":                  "cat /dev/null",
		"a background read":           "cat README.md &",
		"a negated read":              "! cat README.md",
		"a git blob hash":             "git show 8be04d7a1b2c3d4e5f60718293a4b5c6d7e8f901",
		"git show a tree":             "git show HEAD:",
		"git show a directory":        "git show HEAD:sub",
		"git show a tree object":      "git show 'HEAD^{tree}'",
		"git show a blob object":      "git show 'HEAD^{blob}'",
		"git show stats only":         "git show --stat HEAD",
		"git show names only":         "git show --name-only HEAD",
		"git show --no-patch":         "git show --no-patch HEAD",
		"git show a format":           "git show --format=%s HEAD",
		"git diff stats":              "git diff --stat",
		"git diff without an index":   "git diff --no-index source.go README.md",
		"git log":                     "git log -1",
		"git ls-files":                "git ls-files",
		"a git repo override":         "git --git-dir " + q(filepath.Join(root, ".git")) + " show HEAD:source.go",
	} {
		t.Run("not a read: "+name, func(t *testing.T) {
			if commandReadsCheckout(command, root) {
				t.Fatalf("%q proves no read of the checkout", command)
			}
		})
	}
}
