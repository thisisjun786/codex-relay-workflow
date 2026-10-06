package hook

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// CRW-783 red-first cases: the guard is CRW's own protection, so no oracle case is replayed here.

// githubPostWant is one command's judgement: the rule and the place, or no denial, with the fixed guidance.
func githubPostWant(t *testing.T, raw, label, wantRule, wantPlace string) {
	t.Helper()
	reason := githubPostReason(t, raw)
	if wantRule == "" {
		if reason != "" {
			t.Errorf("%q was denied: %q", label, reason)
		}
		return
	}
	if reason == "" {
		t.Fatalf("%q was not denied, want (%s) at %s", label, wantRule, wantPlace)
	}
	const prefix = "GitHub post blocked ("
	const guidance = ": write the text to a file, check it, and pass it with --body-file, -F body=@file or --input"
	if !strings.HasPrefix(reason, prefix) || !strings.HasSuffix(reason, guidance) {
		t.Fatalf("reason %q does not have the fixed shape", reason)
	}
	rule, place, ok := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(reason, prefix), guidance), ") at ")
	if !ok {
		t.Fatalf("reason %q names no place", reason)
	}
	if rule != wantRule || place != wantPlace {
		t.Errorf("%q denied as (%s) at %s, want (%s) at %s", label, rule, place, wantRule, wantPlace)
	}
}

// githubPostReason is the deny reason for one payload; githubPostAnswerReason for one answer envelope.
func githubPostReason(t *testing.T, raw string) string {
	t.Helper()
	return githubPostAnswerReason(t, HandleGitHubPostGuard(raw))
}

func githubPostAnswerReason(t *testing.T, answer string) string {
	t.Helper()
	if answer == "" {
		return ""
	}
	var envelope struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(answer), &envelope); err != nil {
		t.Fatalf("answer is not JSON: %v (%q)", err, answer)
	}
	if got := envelope.HookSpecificOutput.PermissionDecision; got != "deny" {
		t.Fatalf("permissionDecision is %q, want deny (%q)", got, answer)
	}
	return envelope.HookSpecificOutput.PermissionDecisionReason
}

// githubPostPayload is one PreToolUse payload, for the Bash tool unless another tool is named.
func githubPostPayload(t *testing.T, tool, cwd string, input map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": tool, "cwd": cwd, "tool_input": input,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
}

func githubPostShell(t *testing.T, cwd, command string) string {
	t.Helper()
	return githubPostPayload(t, "Bash", cwd, map[string]any{"command": command})
}

func githubPostWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// githubPostTempHome points the homes at temporary directories and checks the real ones are unchanged.
func githubPostTempHome(t *testing.T) {
	t.Helper()
	before := githubPostHomeListing()
	t.Cleanup(func() {
		if after := githubPostHomeListing(); after != before {
			t.Errorf("the guard changed the real Codex home: %q became %q", before, after)
		}
	})
	for _, name := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(name, t.TempDir())
	}
}

// githubPostHomeListing is what ~/.codex and ~/.crw hold now.
func githubPostHomeListing() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "no home"
	}
	out := []string{}
	for _, dir := range []string{".codex", ".crw"} {
		entries, err := os.ReadDir(filepath.Join(home, dir))
		if err != nil {
			out = append(out, dir+": absent")
			continue
		}
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		slices.Sort(names)
		out = append(out, dir+": "+strings.Join(names, ","))
	}
	return strings.Join(out, " | ")
}

// githubPostFake builds a key-shaped value from pieces, so no key-shaped literal sits in this file.
func githubPostFake(prefix string, n int) string { return prefix + strings.Repeat("a", n) }

// githubPostNested wraps a program in n shell -c levels, each double-quoted, so the guard has to read n
// programs one level down before it reaches the text.
func githubPostNested(n int, program string) string {
	for i := 0; i < n; i++ {
		program = "bash -c " + strconv.Quote(program)
	}
	return program
}

// TestGitHubPostGuardJudgements is every command-level case, including the shapes the reviews found.
func TestGitHubPostGuardJudgements(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "clean.md", "a clean body\nsecond line\n")
	githubPostWrite(t, cwd, "dump.md", "A=1\nB=2\nC=3\n")
	githubPostWrite(t, cwd, "prefix.md", "token: "+githubPostFake("ghp_", 20)+"\n")
	githubPostWrite(t, cwd, "credential.md", "MY_API_KEY="+strings.Repeat("a", 20)+"\n")
	githubPostWrite(t, cwd, "input.json", "{\"body\": \""+githubPostFake("glpat-", 20)+"\"}\n")
	githubPostWrite(t, cwd, "later.md", "clean\nclean\n"+githubPostFake("AK"+"IA", 0)+strings.Repeat("A", 16)+"\n")
	githubPostWrite(t, cwd, "secret.md", "GH_TOKEN="+strings.Repeat("a", 20)+"\n")
	if err := os.Mkdir(filepath.Join(cwd, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	clean := "gh pr create -F clean.md -t "
	for _, c := range []struct{ name, command, rule, place string }{
		// The incident, and every inline body.
		{"incident", "gh api repos/o/r/pulls/1/comments -f body=" + strconv.Quote("the reply shows `local` here"), githubPostRuleExpand, githubPostWhereCommand},
		{"backtick", "gh pr comment 1 -b \"shows `local` here\"", githubPostRuleExpand, githubPostWhereCommand},
		{"command substitution", "gh pr comment 1 -b \"shows $(date) here\"", githubPostRuleExpand, githubPostWhereCommand},
		{"newline", "gh pr comment 1 -b \"line one\nline two\"", githubPostRuleExpand, githubPostWhereCommand},
		{"single quote", "gh pr comment 1 -b \"it's fine\"", githubPostRuleInline, githubPostWhereCommand},
		{"double quote", "gh pr comment 1 -b 'say \"hi\"'", githubPostRuleInline, githubPostWhereCommand},
		{"plain body", "gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"body equals", "gh pr create --title Plain --body=plain text", githubPostRuleInline, githubPostWhereCommand},
		{"issue comment", "gh issue comment 3 --body plain", githubPostRuleInline, githubPostWhereCommand},
		{"issue create", "gh issue create --body plain", githubPostRuleInline, githubPostWhereCommand},
		{"review", "gh pr review 7 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"api raw field", "gh api repos/o/r/pulls/1/reviews -f body=plain", githubPostRuleInline, githubPostWhereCommand},
		{"api field", "gh api repos/o/r/pulls/1/reviews -F body=plain", githubPostRuleInline, githubPostWhereCommand},
		{"api field with a variable", "gh api repos/o/r/pulls/1/reviews -F body=\"$X\"", githubPostRuleExpand, githubPostWhereCommand},
		{"api attached short value", "gh api repos/o/r/pulls/1/reviews -fbody=plain", githubPostRuleInline, githubPostWhereCommand},
		{"attached short value", "gh pr comment 1 -bplain", githubPostRuleInline, githubPostWhereCommand},
		{"attached after a boolean bundle", "gh pr create -dbplain", githubPostRuleInline, githubPostWhereCommand},
		{"line continuation", "gh pr comment 1 \\\n-b plain", githubPostRuleInline, githubPostWhereCommand},
		// The prefixes a shell drops before the command, and the separators between commands.
		{"env prefix", "env GH_TOKEN=x gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"sudo prefix", "sudo gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"runner prefix timeout", "timeout 30 gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"runner prefix nice", "nice gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"runner prefix nohup", "nohup gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"assignment prefix", "X=1 gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"incident behind a runner prefix", "timeout 30 gh pr comment 1 -b \"shows `env` here\"", githubPostRuleExpand, githubPostWhereCommand},
		{"after a pipe", "echo x | gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"after a semicolon", "echo x; gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"after and", "true && gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"after a newline", "echo x\ngh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		// The title rule, and the field order the audit found unjudged.
		{"single quoted title", "gh pr create -t 'Plain title' --body-file clean.md", "", ""},
		{"single quoted dollar", "gh pr create -t 'v$X' --body-file clean.md", "", ""},
		{"single quoted backtick", "gh pr create -t 'Fix `make test`' --body-file clean.md", "", ""},
		{"bare title", "gh pr create --title Plain --body-file clean.md", "", ""},
		{"title backtick", "gh pr create -t \"Release `date`\" --body-file clean.md", githubPostRuleExpand, githubPostWhereCommand},
		{"title variable", "gh pr create -t \"Release $X\" --body-file clean.md", githubPostRuleExpand, githubPostWhereCommand},
		{"title secret", "gh pr create -t \"" + githubPostFake("sk-", 16) + "\" --body-file clean.md", githubPostRuleSecret, githubPostWhereCommand},
		{"title after a clean file", clean + "\"Release $X\"", githubPostRuleExpand, githubPostWhereCommand},
		{"secret title after a clean file", clean + "\"" + githubPostFake("sk-", 16) + "\"", githubPostRuleSecret, githubPostWhereCommand},
		{"api variable title", "gh api repos/o/r/pulls/1 -F title=\"$X\" -F body=@clean.md", githubPostRuleExpand, githubPostWhereCommand},
		{"api title after a clean file", "gh api repos/o/r -F body=@clean.md -f title=\"see $X\"", githubPostRuleExpand, githubPostWhereCommand},
		// The text a post takes from a file, or from a quoted heredoc.
		{"body-file", "gh pr comment 1 --body-file clean.md", "", ""},
		{"body-file equals", "gh pr comment 1 --body-file=clean.md", "", ""},
		{"short file flag", "gh pr comment 1 -F clean.md", "", ""},
		{"attached body file", "gh pr comment 1 -Fcredential.md", githubPostRuleSecret, "credential.md:1"},
		{"dump", "gh pr comment 1 --body-file dump.md", githubPostRuleSecret, "dump.md:1"},
		{"key prefix", "gh pr comment 1 --body-file prefix.md", githubPostRuleSecret, "prefix.md:1"},
		{"credential", "gh pr comment 1 --body-file credential.md", githubPostRuleSecret, "credential.md:1"},
		{"later line", "gh pr comment 1 --body-file later.md", githubPostRuleSecret, "later.md:3"},
		{"api file field", "gh api repos/o/r/pulls/1/reviews -F body=@credential.md", githubPostRuleSecret, "credential.md:1"},
		{"api attached file field", "gh api repos/o/r/pulls/1/reviews -Fbody=@credential.md", githubPostRuleSecret, "credential.md:1"},
		{"api clean file field", "gh api repos/o/r/pulls/1/reviews -F body=@clean.md", "", ""},
		{"api input", "gh api repos/o/r/pulls/1/reviews --input input.json", githubPostRuleSecret, "input.json:1"},
		{"api input equals", "gh api repos/o/r/pulls/1/reviews --input=clean.md", "", ""},
		{"missing file", "gh pr comment 1 --body-file nowhere.md", githubPostRuleUnread, "nowhere.md"},
		{"directory", "gh pr comment 1 --body-file adir", githubPostRuleUnread, "adir"},
		{"clean quoted heredoc", "gh pr comment 1 --body-file - <<'EOF'\na clean body\nEOF\n", "", ""},
		{"clean double quoted heredoc", "gh pr comment 1 -F - <<\"EOF\"\na clean body\nEOF\n", "", ""},
		{"dirty quoted heredoc", "gh pr comment 1 --body-file - <<'EOF'\nclean\n" + githubPostFake("xoxb-", 12) + "\nEOF\n", githubPostRuleSecret, "-:2"},
		{"no heredoc", "echo hi | gh pr comment 1 --body-file -", githubPostRuleUnread, githubPostWhereCommand},
		{"unquoted heredoc", "gh pr comment 1 --body-file - <<EOF\na clean body\nEOF\n", githubPostRuleUnread, githubPostWhereCommand},
		{"heredoc bound to another line", "echo x <<'EOF'\nclean\nEOF\ncat secret.md | gh pr comment 1 --body-file -", githubPostRuleUnread, githubPostWhereCommand},
		// A program the guard cannot read, and one it can.
		{"shell -c", "bash -c 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"shell -c clean file", "sh -c 'gh pr comment 1 --body-file clean.md'", "", ""},
		{"shell -c variable", "bash -c \"gh pr comment 1 -b \\\"$X\\\"\"", githubPostRuleExpand, githubPostWhereCommand},
		{"command substitution", "eval \"$(printf 'gh pr comment 1 -b x')\"", githubPostRuleUnread, githubPostWhereCommand},
		{"readable eval", "eval 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"unsplittable", "rg 'gh pr comment", githubPostRuleUnread, githubPostWhereCommand},
		{"substitution without a post", "eval \"$(printf 'echo hi')\"", "", ""},
		{"post inside a substitution", "X=$(gh pr comment 1 -b \"$TOKEN\")", githubPostRuleUnread, githubPostWhereCommand},
		{"post inside a quoted substitution", "echo \"posted: $(gh pr comment 1 -b plain)\"", githubPostRuleUnread, githubPostWhereCommand},
		{"post inside backticks", "echo \"`gh pr comment 1 -b plain`\"", githubPostRuleUnread, githubPostWhereCommand},
		{"plain substitution", "X=$(date)", "", ""},
		// Commands that are not targets, and the option separator.
		{"pr view", "gh pr view 1", "", ""},
		{"pr list", "gh pr list --limit 5", "", ""},
		{"pr checks", "gh pr checks 1", "", ""},
		{"pr diff", "gh pr diff 1", "", ""},
		{"issue view", "gh issue view 3", "", ""},
		{"api read", "gh api repos/o/r/pulls/1", "", ""},
		{"api other field", "gh api repos/o/r/pulls/1 -f state=open", "", ""},
		{"repo view", "gh repo view thisisjun786/codex-relay-workflow", "", ""},
		{"backticked date", "echo \"`date`\"", "", ""},
		{"mention in prose", "echo gh pr comment", "", ""},
		{"mention in a search", "rg -n 'gh pr comment' docs", "", ""},
		{"other command", "git log --oneline", "", ""},
		{"option separator", "gh pr comment 1 -- -b plain", "", ""},
		// The shapes the Codex review found: grouping, aliases, parameters, nested keys, fill, wrappers.
		{"grouping parens", "(gh pr comment 1 -b \"shows `env`\")", githubPostRuleExpand, githubPostWhereCommand},
		{"grouping braces", "{ gh pr comment 1 -b \"shows `env`\"; }", githubPostRuleExpand, githubPostWhereCommand},
		{"grouping negation", "! gh pr comment 1 -b \"shows `env`\"", githubPostRuleExpand, githubPostWhereCommand},
		{"pr new alias", "gh pr new -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"issue new alias", "gh issue new -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"special parameter", "bash -c 'gh pr create -t \"$1\" --body-file clean.md' _ 'x'", githubPostRuleExpand, githubPostWhereCommand},
		{"nested api body", "gh api graphql -F input[body]=\"see $TOKEN\"", githubPostRuleExpand, githubPostWhereCommand},
		{"fill mode", "gh pr create --fill", githubPostRuleUnread, githubPostWhereCommand},
		{"template mode", "gh pr create --title Plain --template secret.md", githubPostRuleUnread, githubPostWhereCommand},
		{"rewritten body file", "printf x > clean.md && gh pr comment 1 --body-file clean.md", githubPostRuleUnread, "clean.md"},
		{"second heredoc", "cat <<'LEFT' | gh pr comment 1 --body-file - <<'RIGHT'\nclean\nLEFT\n" + githubPostFake("xoxb-", 12) + "\nRIGHT\n", githubPostRuleSecret, "-:1"},
		{"wrapper option value", "env -u OLD gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"wrapper option value with a secret", "env -u OLD gh pr comment 1 -b \"shows `env`\"", githubPostRuleExpand, githubPostWhereCommand},
		{"sudo user option value", "sudo -u root gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		// The shapes the Devin review found: a word at a text boundary, a title that looks like a body
		// flag, an expanded body-file name, a nested key by its prefix, and an indirect runner.
		{"word at the start", "gh pr checks $(git rev-parse HEAD)", githubPostRuleUnread, githubPostWhereCommand},
		{"word at the end", "rg 'x gh pr", githubPostRuleUnread, githubPostWhereCommand},
		{"title that looks like a flag", "gh pr create --title '-b' --body-file clean.md", "", ""},
		{"expanded body file name", "gh pr comment 1 --body-file \"$(cat)\"", githubPostRuleUnread, "$(cat)"},
		{"nested body prefix key", "gh api graphql -f body[text]=plain", githubPostRuleInline, githubPostWhereCommand},
		{"nested body prefix with a variable", "gh api graphql -f body[text]=\"$TOKEN\"", githubPostRuleExpand, githubPostWhereCommand},
		{"xargs runner", "xargs gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"xargs runner with a variable", "xargs -n 1 gh pr comment 1 -b \"$TOKEN\"", githubPostRuleExpand, githubPostWhereCommand},
		{"find exec runner", "find . -exec gh pr comment 1 -b plain \\;", githubPostRuleInline, githubPostWhereCommand},
		{"siblings keep their depth", "sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'gh pr comment 1 -b \"$TOKEN\"'", githubPostRuleExpand, githubPostWhereCommand},
		// The class-7 forms the failure-class audit found: a here-string is not a heredoc, a runner the
		// guard did not read, a shell program from a heredoc, and a program it cannot read at all.
		{"herestring is not a heredoc", "tee /dev/stderr <<< 'x' | gh pr comment 1 -F -", githubPostRuleUnread, githubPostWhereCommand},
		{"herestring body file", "gh pr comment 1 --body-file - <<< 'plain'", githubPostRuleUnread, githubPostWhereCommand},
		{"su command", "su -c 'gh pr comment 1 -b plain' root", githubPostRuleInline, githubPostWhereCommand},
		{"su command long", "su --command 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"su command attached", "su -c'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"su command in a bundle", "su -lc 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"su command after a value option", "su -s /bin/sh -c 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"su without a post", "su -c 'echo hi' root", "", ""},
		{"shell heredoc program", "bash <<'EOF'\ngh pr comment 1 -b plain\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"shell heredoc without a post", "bash <<'EOF'\necho hi\nEOF\n", "", ""},
		{"split string", "env -S 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"split string attached", "env -S'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"split string without a post", "env -S 'echo hi'", "", ""},
		{"python program", "python3 -c 'import os; os.system(\"gh pr comment 1 -b plain\")'", githubPostRuleUnread, githubPostWhereCommand},
		{"perl program", "perl -e 'system \"gh pr comment 1 -b plain\"'", githubPostRuleUnread, githubPostWhereCommand},
		{"node program", "node -e 'require(\"child_process\").execSync(\"gh pr comment 1 -b plain\")'", githubPostRuleUnread, githubPostWhereCommand},
		{"awk program", "awk 'BEGIN{system(\"gh pr comment 1 -b plain\")}'", githubPostRuleUnread, githubPostWhereCommand},
		{"python without a post", "python3 -c 'print(1)'", "", ""},
		{"awk without a post", "awk '{print $1}' file.txt", "", ""},
		// The depth bound: a program the guard can no longer read is refused, never allowed because the
		// budget ran out; one that names no post is still nothing to judge.
		{"depth exhausted", githubPostNested(12, "gh pr comment 1 -b plain"), githubPostRuleUnread, githubPostWhereCommand},
		{"depth exhausted without a post", githubPostNested(12, "echo hi"), "", ""},
		{"depth exhausted with a variable", githubPostNested(12, "gh pr comment 1 -b \"$TOKEN\""), githubPostRuleUnread, githubPostWhereCommand},
		{"split string with a variable", "env -S 'gh pr comment 1 -b \"$TOKEN\"'", githubPostRuleExpand, githubPostWhereCommand},
		{"su command with a variable", "su -c 'gh pr comment 1 -b \"$TOKEN\"' root", githubPostRuleExpand, githubPostWhereCommand},
		{"heredoc program with a variable", "bash <<'EOF'\ngh pr comment 1 -b \"$TOKEN\"\nEOF\n", githubPostRuleExpand, githubPostWhereCommand},
		{"heredoc program with a secret", "bash <<'EOF'\ngh pr comment 1 -b " + githubPostFake("sk-", 16) + "\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"herestring with a secret", "gh pr comment 1 --body-file - <<< " + githubPostFake("sk-", 16), githubPostRuleUnread, githubPostWhereCommand},
	} {
		t.Run(c.name, func(t *testing.T) {
			githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, c.rule, c.place)
		})
	}
}

// TestGitHubPostGuardReadsAHomeRelativeFile: a clean home-relative body file passes.
func TestGitHubPostGuardReadsAHomeRelativeFile(t *testing.T) {
	githubPostTempHome(t)
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), "notes.md"), []byte("a clean body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := "gh pr comment 1 --body-file ~/notes.md"
	githubPostWant(t, githubPostShell(t, t.TempDir(), command), command, "", "")
}

// TestGitHubPostGuardLeavesOtherToolsAndEvents: only the shell tools' PreToolUse calls are judged.
func TestGitHubPostGuardLeavesOtherToolsAndEvents(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	command := "gh pr comment 1 -b \"shows `local` here\""
	for _, tool := range []string{"apply_patch", "Write", "Edit", "read_file", "mcp__codex_app__automation_update"} {
		githubPostWant(t, githubPostPayload(t, tool, cwd, map[string]any{"command": command}), "tool "+tool, "", "")
	}
	for _, event := range []string{"PostToolUse", "SessionStart", "UserPromptSubmit", "PermissionRequest"} {
		raw := strings.Replace(githubPostPayload(t, "Bash", cwd, map[string]any{"command": command}),
			"\"PreToolUse\"", strconv.Quote(event), 1)
		githubPostWant(t, raw, "event "+event, "", "")
	}
	for _, raw := range []string{"", "not JSON", "[]", "null", "{}", `{"hook_event_name":"PreToolUse","tool_name":"Bash"}`} {
		githubPostWant(t, raw, "payload "+raw, "", "")
	}
	for _, tool := range []string{"shell", "exec_command", "local_shell"} {
		raw := githubPostPayload(t, tool, cwd, map[string]any{"cmd": command})
		githubPostWant(t, raw, "tool "+tool, githubPostRuleExpand, githubPostWhereCommand)
	}
}

// TestGitHubPostGuardReadsAnArgvCommand: an argv array is already split, so its words carry no shell text.
func TestGitHubPostGuardReadsAnArgvCommand(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "clean.md", "a clean body\n")
	inline := githubPostPayload(t, "exec_command", cwd, map[string]any{"cmd": []any{"gh", "pr", "comment", "1", "-b", "plain"}})
	githubPostWant(t, inline, "argv with an inline body", githubPostRuleInline, githubPostWhereCommand)
	clean := githubPostPayload(t, "exec_command", cwd, map[string]any{"cmd": []any{"gh", "pr", "comment", "1", "--body-file", "clean.md"}})
	githubPostWant(t, clean, "argv with a clean file", "", "")
}

// TestGitHubPostGuardReasonCarriesNoValue is the rule the incident created: the reason names no value.
func TestGitHubPostGuardReasonCarriesNoValue(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	secret := githubPostFake("ghp_", 20)
	githubPostWrite(t, cwd, "body.md", "token "+secret+"\n")
	command := "gh pr comment 1 --body-file body.md"
	githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleSecret, "body.md:1")
	reason := githubPostReason(t, githubPostShell(t, cwd, command))
	for _, part := range []string{secret, "token "} {
		if strings.Contains(reason, part) {
			t.Errorf("the reason carries %q: %q", part, reason)
		}
	}
}

// TestGitHubPostAnswerBoundsThePayload is the row's own stdin policy: a payload within the bound is judged.
func TestGitHubPostAnswerBoundsThePayload(t *testing.T) {
	githubPostTempHome(t)
	payload := githubPostShell(t, t.TempDir(), "gh pr comment 1 -b \"shows `local` here\"")
	if answer := GitHubPostAnswer(strings.NewReader(payload)); answer == "" {
		t.Fatal("a payload under the bound was not judged")
	}
	exact := payload + strings.Repeat(" ", GitHubPostMaxStdinBytes-len(payload))
	if answer := GitHubPostAnswer(strings.NewReader(exact)); answer == "" {
		t.Fatal("a payload of exactly the bound was not judged")
	}
	over := payload + strings.Repeat(" ", GitHubPostMaxStdinBytes+1-len(payload))
	reason := githubPostAnswerReason(t, GitHubPostAnswer(strings.NewReader(over)))
	if !strings.Contains(reason, "("+githubPostRuleUnread+") at "+githubPostWhereCommand+":") {
		t.Errorf("over the bound denied as %q, want %s at %s", reason, githubPostRuleUnread, githubPostWhereCommand)
	}
	if answer := GitHubPostAnswer(githubPostFailingReader{}); answer != "" {
		t.Errorf("a failed read answered %q, want nothing", answer)
	}
}

// githubPostFailingReader fails every read.
type githubPostFailingReader struct{}

func (githubPostFailingReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }
