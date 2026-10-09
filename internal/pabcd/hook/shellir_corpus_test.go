package hook

import (
	"testing"
)

// shellCorpusRow is one command from the issue reproductions (CRW-765, 851, 894, 917, 941, 989, 998, 1012, 1014) with
// the verdict each command gate must give. memory is the memory write gate: A no write destination, W a known
// destination, G a destination it cannot evaluate, U a command the reader refuses. github is the GitHub post guard: A
// allowed, D denied. The rows are the complete commands the issue texts show; prose and fragments are not rows.
type shellCorpusRow struct {
	src    string
	cmd    string
	memory string
	github string
}

func shellCorpusRows() []shellCorpusRow {
	return []shellCorpusRow{
		{"CRW-1012", `python3 -c "print(f'{len(\"abc\")}')"`, "A", "A"},
		{"CRW-1014", `python3 -c 'open("mem/a","w")'`, "W", "A"},
		{"CRW-1014", `python3 -c 'a='"'"'q'"'"'; open("mem/a","w")'`, "W", "A"},
		{"CRW-1014", `python3 -c 'open("mem/a",'""'"w")'`, "W", "A"},
		{"CRW-1014", `sh -c 'a='"'"'q'"'"'; echo hi > mem/a'`, "W", "A"},
		{"CRW-1014", `echo hi > 'mem'/a`, "W", "A"},
		{"CRW-1014", `python3 -c $'open("mem/a","w")'`, "W", "A"},
		{"CRW-765", "python3 - <<'EOF'\nopen('mem/a','w')\nEOF", "W", "A"},
		{"CRW-765", "python3 <<EOF\nopen(file='mem/a', mode='w')\nEOF", "W", "A"},
		{"CRW-765", "node <<'EOF'\nrequire('fs').writeFileSync('mem/a','x')\nEOF", "W", "A"},
		{"CRW-765", "bash <<'EOF'\necho x > mem/a\nEOF", "W", "A"},
		{"CRW-765", "python3 <<EOF\nopen('$P','w')\nEOF", "U", "D"},
		{"CRW-765", "cat > note.md <<'EOF'\nmem/a 경로를 인용한 글\nEOF", "W", "A"},
		{"CRW-765", "go test -count=1 ./internal/pabcd/hook ./cmd/crw", "A", "A"},
		{"CRW-765", "git commit -F -", "A", "A"},
		{"CRW-765", "hash -p /bin/bash b", "U", "D"},
		{"CRW-765", "ln -sf /bin/bash X", "A", "A"},
		{"CRW-765", "alias b=bash", "U", "D"},
		{"CRW-765", "python3 -m json.tool", "U", "D"},
		{"CRW-765", "sed -n$F -", "U", "D"},
		{"CRW-765", "jq --from-file /dev/stdin", "A", "A"},
		{"CRW-765", "grep -f /dev/stdin", "A", "A"},
		{"CRW-765", "cat <<<hello", "A", "A"},
		{"CRW-765", "jq . <<<'{}'", "A", "A"},
		{"CRW-851", `python3 -c 'import builtins; builtins .exec("""open(file="mem/a",mode="w")""")'`, "W", "A"},
		{"CRW-851", `python3 -c 'import sys; pad="\""; src=sys.argv[1]; exec(src)' 'open(file="mem/a",mode="w")'`, "U", "A"},
		{"CRW-851", `python3 - <<< "open('mem/a','w')"`, "W", "A"},
		{"CRW-851", `printf '%s' "open('mem/a','w')" | python3 -`, "U", "D"},
		{"CRW-851", "curl -s x | python3 -", "U", "D"},
		{"CRW-851", "echo hi | python3 script.py", "A", "A"},
		{"CRW-851", "printf hi | cat", "A", "A"},
		{"CRW-894", "printf 'rm -rf ../repo' | bash </dev/stdin", "U", "D"},
		{"CRW-894", "printf 'echo hi' | bash </dev/null", "U", "D"},
		{"CRW-894", "printf x | (cd sub; cat)", "A", "A"},
		{"CRW-894", "printf x | nohup cat", "A", "A"},
		{"CRW-894", "bash -c 'echo hi'", "A", "A"},
		{"CRW-894", "exec -a x true", "A", "A"},
		{"CRW-894", "printf 'echo x > memories/a' | exec -a x bash", "W", "A"},
		{"CRW-894", "printf 'rm -rf ../repo' | bash 3</dev/fd/0 </dev/null <&3", "U", "D"},
		{"CRW-894", "exec -a x >/dev/null rm -rf ../repo", "A", "A"},
		{"CRW-894", "php -f script.php", "A", "A"},
		{"CRW-917", "gh pr comment 1 '--body' 'MY_API_KEY=sentinel'", "A", "D"},
		{"CRW-917", "gh pr create --title 'MY_API_KEY=sentinel' --body-file /tmp/clean.md", "A", "D"},
		{"CRW-917", "git checkout -b topic && git status", "A", "A"},
		{"CRW-941", "sed -e 'w mem/a' x.txt", "W", "A"},
		{"CRW-941", "sed -n 'w mem/a' x.txt", "W", "A"},
		{"CRW-941", "sort -o mem/a x.txt", "W", "A"},
		{"CRW-941", "sed 's/a/b/w mem/a' x.txt", "W", "A"},
		{"CRW-941", "sed -e p -e 'W mem/a' x.txt", "W", "A"},
		{"CRW-941", "sed -f script.sed x.txt", "U", "D"},
		{"CRW-941", "sed 's/a/b/' x.txt", "A", "A"},
		{"CRW-941", "sed -n p x.txt", "A", "A"},
		{"CRW-941", "sed 's/w/x/' x.txt", "A", "A"},
		{"CRW-941", "sed 'w /dev/stdout' x.txt", "W", "A"},
		{"CRW-941", "sort x.txt", "A", "A"},
		{"CRW-941", "sort -o /tmp/out x.txt", "W", "A"},
		{"CRW-989", "printf 'rm -rf ../repo' | exec -a x &>/dev/null bash", "U", "D"},
		{"CRW-989", "printf 'echo x > memories/a' | exec -a x &>/dev/null bash", "U", "D"},
		{"CRW-989", "printf x | sh -c 'cat' _ -s", "A", "A"},
		{"CRW-989", "printf x | dash -c 'cat' _ -s", "A", "A"},
		{"CRW-998", `python3 -c 'from pathlib import Path; p=Path("/fixture/.codex/memories/n.md"); p.open("w".strip()).write("x")'`, "G", "A"},
		{"CRW-998", `python3 -c 'from functools import partial; m="/fixture/.codex/memories/n.md"; partial(open,m,mode="w")().write("x")'`, "G", "A"},
		{"CRW-998", `python3 -c 'from pathlib import Path; p=Path("/w/a"); m="/fixture/.codex/memories/n.md"; p.replace(m,**{})'`, "G", "A"},
		{"CRW-998", `env 2>/dev/null python3 -c 'import os; open(os.path.join("/fixture/.codex/memories","n.md"),"w").write("x")'`, "G", "A"},
		{"zsh", "=ls", "U", "D"},
		{"zsh", "repeat 2 echo x > m/x", "U", "D"},
		{"zsh", "setopt extendedglob", "U", "D"},
		{"CRW-765", "PATH=/x:$PATH ls", "U", "D"},
		{"CRW-765", "BASH_ENV=/tmp/x bash -c 'echo'", "U", "D"},
		{"CRW-765", "git -c core.editor=vim commit", "U", "D"},
		{"CRW-765", "git -c alias.x=!sh x", "U", "D"},
		{"CRW-765", "npm --script-shell=sh run x", "U", "D"},
		{"CRW-765", "make", "A", "A"},
		{"CRW-765", `bash -c "$X"`, "U", "D"},
		{"CRW-765", `eval 'echo x > m/n.md'`, "W", "A"},
		{"CRW-765", "tee m/n.md < w/a", "W", "A"},
		{"CRW-765", "cp w/a m/b", "W", "A"},
		{"CRW-765", "mv w/a m/c", "W", "A"},
		{"CRW-765", "dd if=w/a of=m/e", "W", "A"},
		{"CRW-765", "sed -i 's/a/b/' m/d", "W", "A"},
		{"CRW-765", "echo x >| m/j", "W", "A"},
		{"CRW-765", "echo x &> m/k", "W", "A"},
		{"CRW-917", "gh pr comment 1 --body hi", "A", "D"},
		{"CRW-917", "gh api repos/o/r/issues -X POST -f body=x", "A", "D"},
	}
}

// shellCorpusMemory is the memory gate's code for one command, read by the shared reader.
func shellCorpusMemory(cmd string) string {
	dests, ok := shellIRWriteDests(cmd, "/work", nil)
	if _, bad := shellIRFStringUnreadable(cmd); bad {
		return "U" // the gate reads a Python program the walk cannot finish as a write attempt of its own
	}
	switch {
	case !ok:
		return "U"
	case slicesContainsUnknown(dests):
		return "G"
	case len(dests) > 0:
		return "W"
	}
	return "A"
}

// TestShellCorpusVerdicts holds every corpus row to its verdict in the memory gate and the GitHub post guard.
func TestShellCorpusVerdicts(t *testing.T) {
	githubPostTempHome(t)
	for _, row := range shellCorpusRows() {
		if got := shellCorpusMemory(row.cmd); got != row.memory {
			t.Errorf("%s memory gate on %q: %s, want %s", row.src, row.cmd, got, row.memory)
		}
		got := "A"
		if HandleGitHubPostGuard(githubPostShell(t, t.TempDir(), row.cmd)) != "" {
			got = "D"
		}
		if got != row.github {
			t.Errorf("%s GitHub post guard on %q: %s, want %s", row.src, row.cmd, got, row.github)
		}
	}
}
