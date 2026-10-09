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

// githubPostAbs is a path under the test's own temporary directory, for a case that names an absolute one.
func githubPostAbs(dir, name string) string { return filepath.Join(dir, name) }

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
	// The closed rule reads a body file only under a temporary root, so the payload's working directory is
	// the test's own TMPDIR: a body file the cases name relative to cwd lies under a root the guard trusts.
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "clean.md", "a clean body\nsecond line\n")
	githubPostWrite(t, cwd, "dump.md", "A=1\nB=2\nC=3\n")
	githubPostWrite(t, cwd, "prefix.md", "token: "+githubPostFake("ghp_", 20)+"\n")
	githubPostWrite(t, cwd, "credential.md", "MY_API_KEY="+strings.Repeat("a", 20)+"\n")
	githubPostWrite(t, cwd, "input.json", "{\"body\": \""+githubPostFake("glpat-", 20)+"\"}\n")
	githubPostWrite(t, cwd, "later.md", "clean\nclean\n"+githubPostFake("AK"+"IA", 0)+strings.Repeat("A", 16)+"\n")
	githubPostWrite(t, cwd, "secret.md", "GH_TOKEN="+strings.Repeat("a", 20)+"\n")
	githubPostWrite(t, cwd, "body.md", "a clean body\n")
	if err := os.Mkdir(filepath.Join(cwd, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	clean := "gh pr create -F clean.md -t "
	for _, c := range []struct{ name, command, rule, place string }{
		// The incident, and every inline body.
		{"incident", "gh api repos/o/r/pulls/1/comments -f body=" + strconv.Quote("the reply shows `local` here"), githubPostRuleExpand, githubPostWhereCommand},
		// The generation-5 closed rule: a command that is not one simple command of literal words in the
		// one allowed form is refused when it names a post; an allow-listed program's output is a shell
		// program, a gh alias may expand to a post, and a body file must lie under a temporary root.
		{"allow-listed program piped to a shell", "printf 'gh pr comment 1 -b plain' | bash", githubPostRuleUnread, githubPostWhereCommand},
		{"allow-listed program piped to sh", "echo gh pr comment 1 -b plain | sh", githubPostRuleUnread, githubPostWhereCommand},
		{"gh alias", "gh c 1 --body plain", githubPostRuleUnread, githubPostWhereCommand},
		{"gh alias set", "gh alias set c 'pr comment'", githubPostRuleUnread, githubPostWhereCommand},
		{"api mutation query", `gh api graphql -f query='mutation{addComment(input:{subjectId:"x",body:"plain"}){clientMutationId}}'`, githubPostRuleUnread, githubPostWhereCommand},
		{"api query field", "gh api graphql -f query='query{viewer{login}}'", githubPostRuleUnread, githubPostWhereCommand},
		{"sed e command", "sed -n '1e gh pr comment 1 -b plain' f", githubPostRuleUnread, githubPostWhereCommand},
		{"backtick", "gh pr comment 1 -b \"shows `local` here\"", githubPostRuleInline, githubPostWhereCommand},
		{"command substitution", "gh pr comment 1 -b \"shows $(date) here\"", githubPostRuleInline, githubPostWhereCommand},
		{"newline", "gh pr comment 1 -b \"line one\nline two\"", githubPostRuleInline, githubPostWhereCommand},
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
		{"api attached short value", "gh api repos/o/r/pulls/1/reviews -fbody=plain", githubPostRuleUnread, githubPostWhereCommand},
		{"attached short value", "gh pr comment 1 -bplain", githubPostRuleUnread, githubPostWhereCommand},
		{"attached after a boolean bundle", "gh pr create -dbplain", githubPostRuleUnread, githubPostWhereCommand},
		{"line continuation", "gh pr comment 1 \\\n-b plain", githubPostRuleInline, githubPostWhereCommand},
		{"env prefix", "env GH_TOKEN=x gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"sudo prefix", "sudo gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"runner prefix timeout", "timeout 30 gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"runner prefix nice", "nice gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"runner prefix nohup", "nohup gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"assignment prefix", "X=1 gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"incident behind a runner prefix", "timeout 30 gh pr comment 1 -b \"shows `env` here\"", githubPostRuleInline, githubPostWhereCommand},
		{"after a pipe", "echo x | gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"after a semicolon", "echo x; gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"after and", "true && gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"after a newline", "echo x\ngh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"single quoted title", "gh pr create -t 'Plain title' --body-file clean.md", "", ""},
		// A bang is a literal character in the operator's word list, so an unquoted one is allowed.
		{"unquoted bang in a title", "gh pr create --title Fix! --body-file body.md", "", ""},
		// The generation-5 ruling's own controls, each a case.
		{"a full form A create", "gh pr create --base dev --title 'CRW-1: x' --body-file " + githubPostAbs(cwd, "body.md"), "", ""},
		{"an api file field on a nested path", "gh api repos/o/r/pulls/1/comments/2/replies -F body=@" + githubPostAbs(cwd, "body.md"), "", ""},
		{"an api input that is not JSON", "gh api graphql --input " + githubPostAbs(cwd, "clean.md"), githubPostRuleUnread, githubPostAbs(cwd, "clean.md")},
		{"an issue list with a search", "gh issue list --search review", "", ""},
		{"a review approval", "gh pr review 1 --approve", "", ""},
		{"a git log with a grep", "git log --grep 'gh api'", "", ""},
		{"a pipe naming no post", "echo hi | cat", "", ""},
		// The generation-6 rule: the program word is the word the shell builds (quote removal and
		// backslash removal), compared by its last path element, so a path or quote pieces are the same
		// word; and a pr or issue subcommand outside the read list may carry text, so it is refused.
		{"a path-spelled post", "/usr/bin/gh pr comment 1 -b \"$(env)\"", githubPostRuleInline, githubPostWhereCommand},
		{"a relative path post", "./gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"a single quote piece in the program", "g''h pr comment 1 -b \"$(env)\"", githubPostRuleInline, githubPostWhereCommand},
		{"a double quote piece in the program", "g\"\"h pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"a quoted piece in the program", "'g'h pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"a pr close with a comment", "gh pr close 1 --comment plain", githubPostRuleUnread, githubPostWhereCommand},
		{"a pr merge with a body", "gh pr merge 1 --body plain", githubPostRuleUnread, githubPostWhereCommand},
		{"a release with notes", "gh release create v1 --notes plain", githubPostRuleUnread, githubPostWhereCommand},
		// The generation-7 correction closes the release read list the way the pr and issue one is closed:
		// only list, view and download read, and every other release subcommand may carry text whatever
		// flags follow, so it is refused. The short flags matter too: -n is --notes and -t is --title.
		{"a release with a short notes flag and a substitution", "gh release create v1 -n \"$(env)\"", githubPostRuleUnread, githubPostWhereCommand},
		{"a release with a short notes flag", "gh release create v1 -n plain", githubPostRuleUnread, githubPostWhereCommand},
		{"a release with a notes file", "gh release create v1 -F notes.md", githubPostRuleUnread, githubPostWhereCommand},
		{"a release edit with a title", "gh release edit v1 -t \"Release $(date)\"", githubPostRuleUnread, githubPostWhereCommand},
		{"a release create on a later line", "cd /tmp && gh release create v1 -n plain", githubPostRuleUnread, githubPostWhereCommand},
		// The controls the release read list keeps allowed.
		{"a release view", "gh release view v1", "", ""},
		{"a release list", "gh release list", "", ""},
		{"a release download", "gh release download v1", "", ""},
		{"a release list piped", "gh release list | jq .", "", ""},
		// The controls the rule keeps allowed.
		{"a path-spelled clean body-file post", "/usr/bin/gh pr comment 1 --body-file " + githubPostAbs(cwd, "body.md"), "", ""},
		{"pr status", "gh pr status", "", ""},
		{"single quoted dollar", "gh pr create -t 'v$X' --body-file clean.md", "", ""},
		{"single quoted backtick", "gh pr create -t 'Fix `make test`' --body-file clean.md", "", ""},
		{"bare title", "gh pr create --title Plain --body-file clean.md", "", ""},
		{"title backtick", "gh pr create -t \"Release `date`\" --body-file clean.md", githubPostRuleExpand, githubPostWhereCommand},
		{"title variable", "gh pr create -t \"Release $X\" --body-file clean.md", githubPostRuleExpand, githubPostWhereCommand},
		{"title secret", "gh pr create -t \"" + githubPostFake("sk-", 16) + "\" --body-file clean.md", githubPostRuleSecret, githubPostWhereTitle},
		{"title after a clean file", clean + "\"Release $X\"", githubPostRuleExpand, githubPostWhereCommand},
		{"secret title after a clean file", clean + "\"" + githubPostFake("sk-", 16) + "\"", githubPostRuleSecret, githubPostWhereTitle},
		{"api variable title", "gh api repos/o/r/pulls/1 -F title=\"$X\" -F body=@clean.md", githubPostRuleExpand, githubPostWhereCommand},
		{"api title after a clean file", "gh api repos/o/r -F body=@clean.md -f title=\"see $X\"", githubPostRuleExpand, githubPostWhereCommand},
		{"body-file", "gh pr comment 1 --body-file clean.md", "", ""},
		{"body-file equals", "gh pr comment 1 --body-file=clean.md", "", ""},
		{"short file flag", "gh pr comment 1 -F clean.md", "", ""},
		{"attached body file", "gh pr comment 1 -Fcredential.md", githubPostRuleUnread, githubPostWhereCommand},
		{"dump", "gh pr comment 1 --body-file dump.md", githubPostRuleSecret, "dump.md:1"},
		{"key prefix", "gh pr comment 1 --body-file prefix.md", githubPostRuleSecret, "prefix.md:1"},
		{"credential", "gh pr comment 1 --body-file credential.md", githubPostRuleSecret, "credential.md:1"},
		{"later line", "gh pr comment 1 --body-file later.md", githubPostRuleSecret, "later.md:3"},
		{"api file field", "gh api repos/o/r/pulls/1/reviews -F body=@credential.md", githubPostRuleSecret, "credential.md:1"},
		{"api attached file field", "gh api repos/o/r/pulls/1/reviews -Fbody=@credential.md", githubPostRuleUnread, githubPostWhereCommand},
		{"api clean file field", "gh api repos/o/r/pulls/1/reviews -F body=@clean.md", "", ""},
		{"api input", "gh api repos/o/r/pulls/1/reviews --input input.json", githubPostRuleSecret, "input.json:2"}, // the place numbers the JSON strings the scan reads: the key is line 1, its value line 2
		{"api input equals, not JSON", "gh api repos/o/r/pulls/1/reviews --input=clean.md", githubPostRuleUnread, "clean.md"},
		{"missing file", "gh pr comment 1 --body-file nowhere.md", githubPostRuleUnread, "nowhere.md"},
		{"directory", "gh pr comment 1 --body-file adir", githubPostRuleUnread, "adir"},
		{"clean quoted heredoc", "gh pr comment 1 --body-file - <<'EOF'\na clean body\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"clean double quoted heredoc", "gh pr comment 1 -F - <<\"EOF\"\na clean body\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"dirty quoted heredoc", "gh pr comment 1 --body-file - <<'EOF'\nclean\n" + githubPostFake("xoxb-", 12) + "\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"no heredoc", "echo hi | gh pr comment 1 --body-file -", githubPostRuleInline, githubPostWhereCommand},
		{"unquoted heredoc", "gh pr comment 1 --body-file - <<EOF\na clean body\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"heredoc bound to another line", "echo x <<'EOF'\nclean\nEOF\ncat secret.md | gh pr comment 1 --body-file -", githubPostRuleInline, githubPostWhereCommand},
		{"shell -c", "bash -c 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"shell -c clean file", "sh -c 'gh pr comment 1 --body-file clean.md'", githubPostRuleUnread, githubPostWhereCommand},
		{"shell -c variable", "bash -c \"gh pr comment 1 -b \\\"$X\\\"\"", githubPostRuleUnread, githubPostWhereCommand},
		{"command substitution", "eval \"$(printf 'gh pr comment 1 -b x')\"", githubPostRuleUnread, githubPostWhereCommand},
		{"readable eval", "eval 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"unsplittable", "rg 'gh pr comment", githubPostRuleUnread, githubPostWhereCommand},
		{"substitution without a post", "eval \"$(printf 'echo hi')\"", githubPostRuleUnread, githubPostWhereCommand},
		{"post inside a substitution", "X=$(gh pr comment 1 -b \"$TOKEN\")", githubPostRuleInline, githubPostWhereCommand},
		{"post inside a quoted substitution", "echo \"posted: $(gh pr comment 1 -b plain)\"", githubPostRuleInline, githubPostWhereCommand},
		{"post inside backticks", "echo \"`gh pr comment 1 -b plain`\"", githubPostRuleInline, githubPostWhereCommand},
		{"plain substitution", "X=$(date)", "", ""},
		{"pr view", "gh pr view 1", "", ""},
		{"pr list", "gh pr list --limit 5", "", ""},
		{"pr checks", "gh pr checks 1", "", ""},
		{"pr diff", "gh pr diff 1", "", ""},
		{"issue view", "gh issue view 3", "", ""},
		{"api read", "gh api repos/o/r/pulls/1", "", ""},
		{"read only with a body word", "gh pr view 1 --json body", "", ""},
		{"api other field", "gh api repos/o/r/pulls/1 -f state=open", githubPostRuleUnread, githubPostWhereCommand},
		{"repo view", "gh repo view thisisjun786/codex-relay-workflow", "", ""},
		{"backticked date", "echo \"`date`\"", "", ""},
		{"mention in prose", "echo gh pr comment", "", ""},
		{"mention in a search", "rg -n 'gh pr comment' docs", "", ""},
		{"other command", "git log --oneline", "", ""},
		{"option separator", "gh pr comment 1 -- -b plain", githubPostRuleUnread, githubPostWhereCommand},
		{"grouping parens", "(gh pr comment 1 -b \"shows `env`\")", githubPostRuleInline, githubPostWhereCommand},
		{"grouping braces", "{ gh pr comment 1 -b \"shows `env`\"; }", githubPostRuleInline, githubPostWhereCommand},
		{"grouping negation", "! gh pr comment 1 -b \"shows `env`\"", githubPostRuleInline, githubPostWhereCommand},
		{"pr new alias", "gh pr new -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"issue new alias", "gh issue new -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"special parameter", "bash -c 'gh pr create -t \"$1\" --body-file clean.md' _ 'x'", githubPostRuleExpand, githubPostWhereCommand},
		{"nested api body", "gh api graphql -F input[body]=\"see $TOKEN\"", githubPostRuleExpand, githubPostWhereCommand},
		{"fill mode", "gh pr create --fill", githubPostRuleUnread, githubPostWhereCommand},
		{"template mode", "gh pr create --title Plain --template secret.md", githubPostRuleUnread, githubPostWhereCommand},
		{"rewritten body file", "printf x > clean.md && gh pr comment 1 --body-file clean.md", githubPostRuleUnread, githubPostWhereCommand},
		{"second heredoc", "cat <<'LEFT' | gh pr comment 1 --body-file - <<'RIGHT'\nclean\nLEFT\n" + githubPostFake("xoxb-", 12) + "\nRIGHT\n", githubPostRuleInline, githubPostWhereCommand},
		{"wrapper option value", "env -u OLD gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"wrapper option value with a secret", "env -u OLD gh pr comment 1 -b \"shows `env`\"", githubPostRuleInline, githubPostWhereCommand},
		{"sudo user option value", "sudo -u root gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"word at the start", "gh pr checks $(git rev-parse HEAD)", githubPostRuleExpand, githubPostWhereCommand},
		{"word at the end", "rg 'x gh pr", githubPostRuleUnread, githubPostWhereCommand},
		{"title that looks like a flag", "gh pr create --title '-b' --body-file clean.md", "", ""},
		{"expanded body file name", "gh pr comment 1 --body-file \"$(cat)\"", githubPostRuleUnread, githubPostWhereCommand},
		{"nested body prefix key", "gh api graphql -f body[text]=plain", githubPostRuleExpand, githubPostWhereCommand},
		{"nested body prefix with a variable", "gh api graphql -f body[text]=\"$TOKEN\"", githubPostRuleExpand, githubPostWhereCommand},
		{"xargs runner", "xargs gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"xargs runner with a variable", "xargs -n 1 gh pr comment 1 -b \"$TOKEN\"", githubPostRuleInline, githubPostWhereCommand},
		{"find exec runner", "find . -exec gh pr comment 1 -b plain \\;", githubPostRuleInline, githubPostWhereCommand},
		{"siblings keep their depth", "sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'true'; sh -c 'gh pr comment 1 -b \"$TOKEN\"'", githubPostRuleInline, githubPostWhereCommand},
		{"herestring is not a heredoc", "tee /dev/stderr <<< 'x' | gh pr comment 1 -F -", githubPostRuleInline, githubPostWhereCommand},
		{"herestring body file", "gh pr comment 1 --body-file - <<< 'plain'", githubPostRuleInline, githubPostWhereCommand},
		{"su command", "su -c 'gh pr comment 1 -b plain' root", githubPostRuleUnread, githubPostWhereCommand},
		{"su command long", "su --command 'gh pr comment 1 -b plain'", githubPostRuleUnread, githubPostWhereCommand},
		{"su command attached", "su -c'gh pr comment 1 -b plain'", githubPostRuleUnread, githubPostWhereCommand},
		{"su command in a bundle", "su -lc 'gh pr comment 1 -b plain'", githubPostRuleUnread, githubPostWhereCommand},
		{"su command after a value option", "su -s /bin/sh -c 'gh pr comment 1 -b plain'", githubPostRuleUnread, githubPostWhereCommand},
		{"su without a post", "su -c 'echo hi' root", githubPostRuleUnread, githubPostWhereCommand},
		{"shell heredoc program", "bash <<'EOF'\ngh pr comment 1 -b plain\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"shell heredoc without a post", "bash <<'EOF'\necho hi\nEOF\n", "", ""},
		{"split string", "env -S 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"split string attached", "env -S'gh pr comment 1 -b plain'", githubPostRuleUnread, githubPostWhereCommand},
		{"split string without a post", "env -S 'echo hi'", "", ""},
		{"python program", "python3 -c 'import os; os.system(\"gh pr comment 1 -b plain\")'", githubPostRuleUnread, githubPostWhereCommand},
		{"perl program", "perl -e 'system \"gh pr comment 1 -b plain\"'", githubPostRuleUnread, githubPostWhereCommand},
		{"node program", "node -e 'require(\"child_process\").execSync(\"gh pr comment 1 -b plain\")'", githubPostRuleUnread, githubPostWhereCommand},
		{"awk program", "awk 'BEGIN{system(\"gh pr comment 1 -b plain\")}'", githubPostRuleUnread, githubPostWhereCommand},
		{"python without a post", "python3 -c 'print(1)'", "", ""},
		{"awk without a post", "awk '{print $1}' file.txt", "", ""},
		{"depth exhausted", githubPostNested(12, "gh pr comment 1 -b plain"), githubPostRuleInline, githubPostWhereCommand},
		{"depth exhausted without a post", githubPostNested(12, "echo hi"), "", ""},
		{"depth exhausted with a variable", githubPostNested(12, "gh pr comment 1 -b \"$TOKEN\""), githubPostRuleUnread, githubPostWhereCommand},
		{"split string with a variable", "env -S 'gh pr comment 1 -b \"$TOKEN\"'", githubPostRuleUnread, githubPostWhereCommand},
		{"su command with a variable", "su -c 'gh pr comment 1 -b \"$TOKEN\"' root", githubPostRuleUnread, githubPostWhereCommand},
		{"heredoc program with a variable", "bash <<'EOF'\ngh pr comment 1 -b \"$TOKEN\"\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"heredoc program with a secret", "bash <<'EOF'\ngh pr comment 1 -b " + githubPostFake("sk-", 16) + "\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"herestring with a secret", "gh pr comment 1 --body-file - <<< " + githubPostFake("sk-", 16), githubPostRuleInline, githubPostWhereCommand},
		{"expanded command word", "G=gh; $G pr comment 1 --body x", githubPostRuleInline, githubPostWhereCommand},
		// The generation-7 rules read this text through rule 2's canonical words, so the body it spells is
		// the inline-body rule now, where generation 6 named the expanded program word instead.
		{"expanded command word with no assignment", "$G pr comment 1 -b plain", githubPostRuleUnread, githubPostWhereCommand},
		{"expanded quoted command word", "GH=gh; \"$GH\" api repos/o/r/issues/1/comments -F body=@f", githubPostRuleUnread, "f"},
		{"substituted command word", "$(echo gh) issue comment 1 --body x", githubPostRuleUnread, githubPostWhereCommand},
		{"braced command word", "GH=gh; ${GH} pr comment 1 --body x", githubPostRuleInline, githubPostWhereCommand},
		{"backticked command word", "G=gh; `echo $G` pr comment 1 --body x", githubPostRuleUnread, githubPostWhereCommand},
		{"expanded command word behind a wrapper", "G=gh; sudo $G pr comment 1 --body x", githubPostRuleInline, githubPostWhereCommand},
		{"expanded command word with a variable only", "G=gh; $G pr comment 1 --body \"$TOKEN\"", githubPostRuleInline, githubPostWhereCommand},
		{"expanded command word without a post", "$EDITOR notes.md", githubPostRuleUnread, githubPostWhereCommand},
		{"expanded quoted command word without a post", "\"$PAGER\" README.md", githubPostRuleUnread, githubPostWhereCommand},
		{"single quoted command word", "'$G' pr comment 1 --body x", "", ""},
		{"expanded command word alone", "$G", githubPostRuleUnread, githubPostWhereCommand},
		{"for loop", "for i in 1 2; do gh pr comment 1 -b \"$BODY\"; done", githubPostRuleInline, githubPostWhereCommand},
		{"while loop", "while true; do gh pr comment 1 -b plain; done", githubPostRuleInline, githubPostWhereCommand},
		{"until loop", "until false; do gh pr comment 1 -b plain; done", githubPostRuleInline, githubPostWhereCommand},
		{"select loop", "select x in a; do gh pr comment 1 -b plain; done", githubPostRuleInline, githubPostWhereCommand},
		{"if conditional", "if gh pr comment 1 -b plain; then :; fi", githubPostRuleInline, githubPostWhereCommand},
		{"then branch", "if true; then gh pr comment 1 -b plain; fi", githubPostRuleInline, githubPostWhereCommand},
		{"case clause", "case x in a) gh pr comment 1 -b plain;; esac", githubPostRuleInline, githubPostWhereCommand},
		{"case label then a post", "case x in a) ;; esac; gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"function name() { }", "post() { gh pr comment 1 -b plain; }", githubPostRuleInline, githubPostWhereCommand},
		{"function keyword", "function post { gh pr comment 1 -b plain; }", githubPostRuleInline, githubPostWhereCommand},
		{"background job", "sleep 5 & gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"post before a background job", "gh pr comment 1 -b plain & echo ok", githubPostRuleInline, githubPostWhereCommand},
		{"subshell group", "( gh pr comment 1 -b plain )", githubPostRuleInline, githubPostWhereCommand},
		{"brace group", "{ gh pr comment 1 -b plain; }", githubPostRuleInline, githubPostWhereCommand},
		{"and list", "true && gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"or list", "gh pr comment 1 -b plain || true", githubPostRuleInline, githubPostWhereCommand},
		{"source then a post", "source setup.sh; gh pr comment 1 -b plain", githubPostRuleUnread, githubPostWhereCommand},
		{"dot then a post", ". ./setup.sh; gh pr comment 1 -b plain", githubPostRuleUnread, githubPostWhereCommand},
		{"expanded word through a shell", "G=gh bash -c '$G pr comment 1 --body x'", githubPostRuleUnread, githubPostWhereCommand},
		{"exported expanded word through a shell", "export G=gh; bash -c '$G pr comment 1 --body x'", githubPostRuleInline, githubPostWhereCommand},
		{"expanded word through eval", "G=gh; eval \"$G pr comment 1 --body x\"", githubPostRuleInline, githubPostWhereCommand},
		{"expanded word through su", "G=gh; su -c \"$G pr comment 1 --body x\" root", githubPostRuleUnread, githubPostWhereCommand},
		{"expanded word through split string", "G=gh; env -S \"$G pr comment 1 --body x\"", githubPostRuleInline, githubPostWhereCommand},
		{"expanded word through a heredoc program", "bash <<'EOF'\nG=gh; $G pr comment 1 --body x\nEOF\n", githubPostRuleInline, githubPostWhereCommand},
		{"body file rewritten on an earlier line", "env | sort > body.md\ngh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"body file written by a redirect on an earlier line", "printf x > body.md\ngh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"body file written by a heredoc on an earlier line", "cat <<'EOF' > body.md\nplain\nEOF\ngh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"clean body file post", "gh pr comment 1 --body-file body.md", "", ""},
		{"loop with nothing to do with gh", "for i in 1 2; do echo hi; done", "", ""},
		{"background job with nothing to do with gh", "sleep 5 & echo ok", "", ""},
		{"expanded word with nothing to do with gh", "G=ls bash -c '$G -l'", githubPostRuleUnread, githubPostWhereCommand},
		{"another file written before a clean post", "echo hi > other.md\ngh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"another file written by tee before a clean post", "tee other.md <<'EOF'\nnotes\nEOF\ngh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"if with nothing to do with gh", "if true; then echo hi; fi", "", ""},
		{"function with nothing to do with gh", "post() { echo hi; }", "", ""},
		{"nested control structures", "if true; then for i in 1 2; do gh pr comment 1 -b plain; done; fi", githubPostRuleInline, githubPostWhereCommand},
		{"post after a loop closes", "for i in 1; do :; done; gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"nested control structures with nothing to do with gh", "if true; then for i in 1 2; do echo hi; done; fi", "", ""},
		{"clean post after a loop closes", "for i in 1; do :; done; gh pr comment 1 --body-file body.md", githubPostRuleUnread, "body.md"}, // the loop leaves the directory unknown, so the body file is unreadable where it is named
		{"timeout with a duration suffix", "timeout 30s gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"timeout with a duration and a variable", "timeout 30s gh pr comment 1 -b \"$BODY\"", githubPostRuleInline, githubPostWhereCommand},
		{"exec", "exec gh pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"exec a shell", "exec bash -c 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"ssh with a post", "ssh host gh pr comment 1 -b plain", githubPostRuleUnread, githubPostWhereCommand},
		{"ssh with a quoted post", "ssh host 'gh pr comment 1 -b plain'", githubPostRuleUnread, githubPostWhereCommand},
		{"watch", "watch gh pr comment 1 -b plain", githubPostRuleUnread, githubPostWhereCommand},
		{"process substitution", "cat <(gh pr comment 1 -b plain)", githubPostRuleInline, githubPostWhereCommand},
		{"source a process substitution", "source <(echo 'gh pr comment 1 -b plain')", githubPostRuleUnread, githubPostWhereCommand},
		{"a shell fed a process substitution", "bash < <(gh pr comment 1 -b plain)", githubPostRuleUnread, githubPostWhereCommand},
		{"body file written by an expanded destination", "env | sort > \"$PWD/body.md\"\ngh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"body file written by an absolute destination", "printf x > " + githubPostAbs(cwd, "body.md") + "\ngh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"body file written to the same last element", "env > sub/body.md\ngh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"body file after a directory change", "mkdir -p sub && cd sub && gh pr comment 1 --body-file body.md", githubPostRuleUnread, "body.md"},
		{"timeout reading", "timeout 30s gh pr view 1", "", ""},
		{"timeout with a clean file post", "timeout 30s gh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"exec with nothing to do with gh", "exec ls", "", ""},
		{"ssh with nothing to do with gh", "ssh host ls", "", ""},
		{"process substitution with nothing to do with gh", "cat <(ls)", "", ""},
		{"a message naming a post", "git commit -m \"deny gh pr comment inline bodies\"", "", ""},
		{"a search naming a post", "rg 'gh pr comment' internal", "", ""},
		{"a print naming a post", "echo 'gh pr comment'", "", ""},
		{"another file written", "env > other.txt\ngh pr comment 1 --body-file body.md", githubPostRuleUnread, githubPostWhereCommand},
		{"a directory change with nothing to do with gh", "cd sub && ls", "", ""},
		{"git with a config word", "git -c alias.p='!gh pr comment 1 -b plain' p", githubPostRuleUnread, githubPostWhereCommand},
		{"git with an alias subcommand", "git -c core.pager=cat show gh pr comment", githubPostRuleUnread, githubPostWhereCommand},
		{"body file after a pushd", "pushd sub && gh pr comment 1 --body-file body.md", githubPostRuleUnread, "body.md"},
		// The generation-7 rules. R1 reads the program word the way the shell builds it and compares it in
		// ASCII lower case, because a case-insensitive file system runs gh for GH. R2 reads a text that is
		// not one simple command through its canonical words (every quote and backslash removed, the ASCII
		// letters lowered), so a program word built in quote pieces, holding a backslash, or holding an
		// expansion in a later command is read as the word it is and refused when it names a post.
		{"an expansion in a later command word", "x=g; ${x}h pr comment 1 -b \"$(env)\"", githubPostRuleInline, githubPostWhereCommand},
		{"an expansion in a later command word after an and", "true && ${X:-g}h pr comment 1 -b \"$(env)\"", githubPostRuleUnread, githubPostWhereCommand},
		{"quote pieces one level down", "bash -c \"g''h pr comment 1 -b \\\"$(env)\\\"\"", githubPostRuleUnread, githubPostWhereCommand},
		{"a quote piece inside a single quoted program", "sh -c 'g\"h\" pr comment 1 -b \"$(env)\"'", githubPostRuleInline, githubPostWhereCommand},
		{"a backslash inside a program word", "bash -c 'g\\h pr comment 1 -b \"$(env)\"'", githubPostRuleInline, githubPostWhereCommand},
		{"quote pieces in a here string", "bash <<< \"g''h pr comment 1 -b x\"", githubPostRuleInline, githubPostWhereCommand},
		{"an upper case program word", "GH pr comment 1 -b \"$(env)\"", githubPostRuleInline, githubPostWhereCommand},
		{"a mixed case program word", "Gh pr comment 1 --body \"$(env)\"", githubPostRuleInline, githubPostWhereCommand},
		{"an upper case program word with a plain body", "GH pr comment 1 -b plain", githubPostRuleInline, githubPostWhereCommand},
		{"an upper case path program word", "/usr/local/bin/GH pr comment 1 --body-file /proc/self/environ", githubPostRuleUnread, "/proc/self/environ"},
		{"an upper case program word on a later line", "cd /tmp\nGH pr comment 1 -b \"$(env)\"", githubPostRuleInline, githubPostWhereCommand},
		// The controls rule R2 keeps allowed.
		{"an upper case read", "GH pr view 1", "", ""},
		{"a read piped to jq", "gh pr list | jq .", "", ""},
		{"a read after a directory change", "cd /tmp && gh pr view 1", "", ""},
		{"a loop over a variable with nothing to do with gh", "for f in a b; do echo $f; done", "", ""},
		{"a pipeline that names gh", "cat /tmp/x | grep gh", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, c.rule, c.place)
		})
	}
}

// TestGitHubPostGuardReadsAHomeRelativeFile: a body file outside the temporary roots is refused, because
// the closed rule reads only a literal path under TMPDIR, /tmp or /var/tmp.
func TestGitHubPostGuardReadsAHomeRelativeFile(t *testing.T) {
	githubPostTempHome(t)
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), "notes.md"), []byte("a clean body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := "gh pr comment 1 --body-file ~/notes.md"
	githubPostWant(t, githubPostShell(t, t.TempDir(), command), command, githubPostRuleUnread, githubPostWhereCommand)
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
		githubPostWant(t, raw, "tool "+tool, githubPostRuleInline, githubPostWhereCommand)
	}
}

// TestGitHubPostGuardReadsAnArgvCommand: an argv array is already split, so its words carry no shell text.
// TestGitHubPostGuardRefusesABodyFileOutsideTheRoots: the closed rule reads only a literal path under
// TMPDIR, /tmp or /var/tmp, so a body file in the payload's own working directory is refused when that
// directory is not a temporary root.
func TestGitHubPostGuardRefusesABodyFileOutsideTheRoots(t *testing.T) {
	githubPostTempHome(t)
	// The checkout's own go.mod is a readable regular file outside TMPDIR, /tmp and /var/tmp, so a post
	// that names it is refused: the closed rule reads only a path under a temporary root.
	t.Setenv("TMPDIR", t.TempDir())
	repo, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Skipf("the checkout's go.mod is not at %s: %v", repo, err)
	}
	command := "gh pr comment 1 --body-file go.mod"
	githubPostWant(t, githubPostShell(t, repo, command), command, githubPostRuleUnread, "go.mod")
}

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
