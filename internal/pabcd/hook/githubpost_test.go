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

// CRW-783 red-first cases. The guard is CRW's own protection, so there is no oracle case to replay: the
// incident shape, every inline-body form, the title rule, the file and heredoc reads, the programs the
// guard cannot read, the commands that are not targets and the two properties the issue fixes (the
// reason names only the rule and the place, and the payload has a bound) are asserted here.

// githubPostTempHome points HOME, CODEX_HOME and CRW_HOME at temporary directories and reports whether
// the guard left anything under the real ones.
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

// githubPostShell is one payload for a shell tool whose command is shell text.
func githubPostShell(t *testing.T, cwd, command string) string {
	t.Helper()
	return githubPostPayload(t, "Bash", cwd, map[string]any{"command": command})
}

// githubPostReason is the deny reason of the guard's answer for one payload, or "" when it answered nothing.
func githubPostReason(t *testing.T, raw string) string {
	t.Helper()
	return githubPostAnswerReason(t, HandleGitHubPostGuard(raw))
}

// githubPostAnswerReason is the deny reason inside one answer envelope, or "" when the answer is empty.
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

// githubPostWant is one command's judgement: the rule and the place it is denied as, or no denial. The
// reason must also carry the guidance the issue fixes, in the shape it fixes.
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

// githubPostFake builds a key-shaped value from pieces, so that no key-shaped literal sits in this file:
// the branch's own secret scan reads the committed bytes.
func githubPostFake(prefix string, n int) string { return prefix + strings.Repeat("a", n) }

// TestGitHubPostGuardDeniesTheIncidentShape is the 2026-10-06 incident: code mode built
// "-f body=" + JSON.stringify(body) and the shell ran the backticks inside the double quotes.
func TestGitHubPostGuardDeniesTheIncidentShape(t *testing.T) {
	githubPostTempHome(t)
	body := "the reply shows `local` here"
	command := "gh api repos/o/r/pulls/1/comments -f body=" + strconv.Quote(body)
	raw := githubPostShell(t, t.TempDir(), command)
	githubPostWant(t, raw, command, githubPostRuleExpand, githubPostWhereCommand)
	reason := githubPostReason(t, raw)
	for _, part := range []string{body, "local", "the reply"} {
		if strings.Contains(reason, part) {
			t.Errorf("the reason carries the body text %q: %q", part, reason)
		}
	}
}

func TestGitHubPostGuardDeniesInlineBodies(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	for _, c := range []struct{ name, command, rule string }{
		{"backtick", "gh pr comment 1 -b \"shows `local` here\"", githubPostRuleExpand},
		{"command substitution", "gh pr comment 1 -b \"shows $(date) here\"", githubPostRuleExpand},
		{"newline", "gh pr comment 1 -b \"line one\nline two\"", githubPostRuleExpand},
		{"single quote", "gh pr comment 1 -b \"it's fine\"", githubPostRuleInline},
		{"double quote", "gh pr comment 1 -b 'say \"hi\"'", githubPostRuleInline},
		{"plain", "gh pr comment 1 -b plain", githubPostRuleInline},
		{"body equals", "gh pr create --title Plain --body=plain text", githubPostRuleInline},
		{"issue comment", "gh issue comment 3 --body plain", githubPostRuleInline},
		{"issue create", "gh issue create --body plain", githubPostRuleInline},
		{"review", "gh pr review 7 -b plain", githubPostRuleInline},
		{"api raw field", "gh api repos/o/r/pulls/1/reviews -f body=plain", githubPostRuleInline},
		{"api field", "gh api repos/o/r/pulls/1/reviews -F body=plain", githubPostRuleInline},
		{"api attached short value", "gh api repos/o/r/pulls/1/reviews -fbody=plain", githubPostRuleInline},
		{"runner prefix timeout", "timeout 30 gh pr comment 1 -b plain", githubPostRuleInline},
		{"assignment prefix", "X=1 gh pr comment 1 -b plain", githubPostRuleInline},
		{"runner prefix nice", "nice gh pr comment 1 -b plain", githubPostRuleInline},
		{"runner prefix nohup", "nohup gh pr comment 1 -b plain", githubPostRuleInline},
		{"incident behind a runner prefix", "timeout 30 gh pr comment 1 -b \"shows `env` here\"", githubPostRuleExpand},
		{"api field with a variable", "gh api repos/o/r/pulls/1/reviews -F body=\"$X\"", githubPostRuleExpand},
		{"env prefix", "env GH_TOKEN=x gh pr comment 1 -b plain", githubPostRuleInline},
		{"sudo prefix", "sudo gh pr comment 1 -b plain", githubPostRuleInline},
		{"after a pipe", "echo x | gh pr comment 1 -b plain", githubPostRuleInline},
		{"after a semicolon", "echo x; gh pr comment 1 -b plain", githubPostRuleInline},
		{"after and", "true && gh pr comment 1 -b plain", githubPostRuleInline},
		{"after a newline", "echo x\ngh pr comment 1 -b plain", githubPostRuleInline},
		{"attached short value", "gh pr comment 1 -bplain", githubPostRuleInline},
		{"attached after a boolean bundle", "gh pr create -dbplain", githubPostRuleInline},
		{"line continuation", "gh pr comment 1 \\\n-b plain", githubPostRuleInline},
	} {
		t.Run(c.name, func(t *testing.T) {
			githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, c.rule, githubPostWhereCommand)
		})
	}
}

func TestGitHubPostGuardTitles(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "body.md", "a clean body\n")
	for _, c := range []struct{ name, command, rule string }{
		{"single quoted", "gh pr create -t 'Plain title' --body-file body.md", ""},
		{"single quoted with a dollar", "gh pr create -t 'v$X' --body-file body.md", ""},
		{"single quoted with a backtick", "gh pr create -t 'Fix `make test`' --body-file body.md", ""},
		{"bare", "gh pr create --title Plain --body-file body.md", ""},
		{"backtick", "gh pr create -t \"Release `date`\" --body-file body.md", githubPostRuleExpand},
		{"variable", "gh pr create -t \"Release $X\" --body-file body.md", githubPostRuleExpand},
		{"secret", "gh pr create -t \"" + githubPostFake("sk-", 16) + "\" --body-file body.md", githubPostRuleSecret},
		{"api variable", "gh api repos/o/r/pulls/1 -F title=\"$X\" -F body=@body.md", githubPostRuleExpand},
		{"title after a clean file", "gh pr create -F body.md -t \"Release $X\"", githubPostRuleExpand},
		{"secret title after a clean file", "gh pr create -F body.md -t \"" + githubPostFake("sk-", 16) + "\"", githubPostRuleSecret},
		{"api title after a clean file", "gh api repos/o/r -F body=@body.md -f title=\"see $X\"", githubPostRuleExpand},
	} {
		t.Run(c.name, func(t *testing.T) {
			place := githubPostWhereCommand
			if c.rule == "" {
				place = ""
			}
			githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, c.rule, place)
		})
	}
}

func TestGitHubPostGuardFiles(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "clean.md", "a clean body\nsecond line\n")
	githubPostWrite(t, cwd, "dump.md", "A=1\nB=2\nC=3\n")
	githubPostWrite(t, cwd, "prefix.md", "token: "+githubPostFake("ghp_", 20)+"\n")
	githubPostWrite(t, cwd, "credential.md", "MY_API_KEY="+strings.Repeat("a", 20)+"\n")
	githubPostWrite(t, cwd, "input.json", "{\"body\": \""+githubPostFake("glpat-", 20)+"\"}\n")
	githubPostWrite(t, cwd, "later.md", "clean\nclean\n"+githubPostFake("AK"+"IA", 0)+strings.Repeat("A", 16)+"\n")
	if err := os.Mkdir(filepath.Join(cwd, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, command, rule, place string }{
		{"body-file", "gh pr comment 1 --body-file clean.md", "", ""},
		{"body-file equals", "gh pr comment 1 --body-file=clean.md", "", ""},
		{"short file flag", "gh pr comment 1 -F clean.md", "", ""},
		{"dump", "gh pr comment 1 --body-file dump.md", githubPostRuleSecret, "dump.md:1"},
		{"key prefix", "gh pr comment 1 --body-file prefix.md", githubPostRuleSecret, "prefix.md:1"},
		{"credential", "gh pr comment 1 --body-file credential.md", githubPostRuleSecret, "credential.md:1"},
		{"later line", "gh pr comment 1 --body-file later.md", githubPostRuleSecret, "later.md:3"},
		{"api file field", "gh api repos/o/r/pulls/1/reviews -F body=@credential.md", githubPostRuleSecret, "credential.md:1"},
		{"api attached file field", "gh api repos/o/r/pulls/1/reviews -Fbody=@credential.md", githubPostRuleSecret, "credential.md:1"},
		{"attached body file", "gh pr comment 1 -Fcredential.md", githubPostRuleSecret, "credential.md:1"},
		{"api clean file field", "gh api repos/o/r/pulls/1/reviews -F body=@clean.md", "", ""},
		{"api input", "gh api repos/o/r/pulls/1/reviews --input input.json", githubPostRuleSecret, "input.json:1"},
		{"api input equals", "gh api repos/o/r/pulls/1/reviews --input=clean.md", "", ""},
		{"missing file", "gh pr comment 1 --body-file nowhere.md", githubPostRuleUnread, "nowhere.md"},
		{"directory", "gh pr comment 1 --body-file adir", githubPostRuleUnread, "adir"},
	} {
		t.Run(c.name, func(t *testing.T) {
			githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, c.rule, c.place)
		})
	}
}

func TestGitHubPostGuardStandardInputHeredoc(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	for _, c := range []struct{ name, command, rule, place string }{
		{"clean quoted", "gh pr comment 1 --body-file - <<'EOF'\na clean body\nEOF\n", "", ""},
		{"clean double quoted", "gh pr comment 1 -F - <<\"EOF\"\na clean body\nEOF\n", "", ""},
		{"dirty quoted", "gh pr comment 1 --body-file - <<'EOF'\nclean\n" + githubPostFake("xoxb-", 12) + "\nEOF\n", githubPostRuleSecret, "-:2"},
		{"no heredoc", "echo hi | gh pr comment 1 --body-file -", githubPostRuleUnread, githubPostWhereCommand},
		{"unquoted heredoc", "gh pr comment 1 --body-file - <<EOF\na clean body\nEOF\n", githubPostRuleUnread, githubPostWhereCommand},
	} {
		t.Run(c.name, func(t *testing.T) {
			githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, c.rule, c.place)
		})
	}
}

func TestGitHubPostGuardLeavesCommandsThatAreNotTargets(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	for _, command := range []string{
		"gh pr view 1", "gh pr list --limit 5", "gh pr checks 1", "gh pr diff 1", "gh issue view 3",
		"gh api repos/o/r/pulls/1", "gh api repos/o/r/pulls/1 -f state=open",
		"gh repo view thisisjun786/codex-relay-workflow", "echo \"`date\"\"", "echo gh pr comment",
		"rg -n 'gh pr comment' docs", "git log --oneline", "ls -la",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

func TestGitHubPostGuardNestedPrograms(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "clean.md", "a clean body\n")
	for _, c := range []struct{ name, command, rule, place string }{
		{"shell -c", "bash -c 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"shell -c clean file", "sh -c 'gh pr comment 1 --body-file clean.md'", "", ""},
		{"shell -c variable", "bash -c \"gh pr comment 1 -b \\\"$X\\\"\"", githubPostRuleExpand, githubPostWhereCommand},
		{"command substitution", "eval \"$(printf 'gh pr comment 1 -b x')\"", githubPostRuleUnread, githubPostWhereCommand},
		{"readable eval", "eval 'gh pr comment 1 -b plain'", githubPostRuleInline, githubPostWhereCommand},
		{"unsplittable", "rg 'gh pr comment", githubPostRuleUnread, githubPostWhereCommand},
		{"substitution without a post", "eval \"$(printf 'echo hi')\"", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, c.rule, c.place)
		})
	}
}

// TestGitHubPostGuardDeniesACommandSubstitution is the fail-closed rule over the outer text: a gh post
// inside a command substitution is a text the guard cannot read, so it is denied rather than passed.
func TestGitHubPostGuardDeniesACommandSubstitution(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	for _, command := range []string{
		"X=$(gh pr comment 1 -b \"$TOKEN\")",
		"echo \"posted: $(gh pr comment 1 -b plain)\"",
		"echo \"`gh pr comment 1 -b plain`\"",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, githubPostWhereCommand)
	}
	for _, command := range []string{"X=$(date)", "echo \"$(uname -a)\""} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardBindsAHeredocToItsOwnLine: the quoted heredoc that feeds --body-file - must sit in
// the same command line, or the guard would scan a body nobody posts.
func TestGitHubPostGuardBindsAHeredocToItsOwnLine(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "secret.md", "GH_TOKEN="+strings.Repeat("a", 20)+"\n")
	command := "echo x <<'EOF'\nclean\nEOF\ncat secret.md | gh pr comment 1 --body-file -"
	githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, githubPostWhereCommand)
}

// TestGitHubPostGuardHonoursTheOptionSeparator: everything after -- is an operand, so a word that looks
// like a body flag there is not one.
func TestGitHubPostGuardHonoursTheOptionSeparator(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	command := "gh pr comment 1 -- -b plain"
	githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
}

// TestGitHubPostGuardReadsAHomeRelativeFile: a name the shell expands from the home directory is a file
// the guard can read, so a clean one passes.
func TestGitHubPostGuardReadsAHomeRelativeFile(t *testing.T) {
	githubPostTempHome(t)
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, "notes.md"), []byte("a clean body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := "gh pr comment 1 --body-file ~/notes.md"
	githubPostWant(t, githubPostShell(t, t.TempDir(), command), command, "", "")
}

func TestGitHubPostGuardLeavesOtherToolsAndEvents(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	command := "gh pr comment 1 -b \"shows `local` here\""
	for _, tool := range []string{"apply_patch", "Write", "Edit", "read_file", "mcp__codex_app__automation_update"} {
		raw := githubPostPayload(t, tool, cwd, map[string]any{"command": command})
		githubPostWant(t, raw, "tool "+tool, "", "")
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

func TestGitHubPostGuardReadsAnArgvCommand(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "clean.md", "a clean body\n")
	inline := githubPostPayload(t, "exec_command", cwd, map[string]any{"cmd": []any{"gh", "pr", "comment", "1", "-b", "plain"}})
	githubPostWant(t, inline, "argv with an inline body", githubPostRuleInline, githubPostWhereCommand)
	clean := githubPostPayload(t, "exec_command", cwd, map[string]any{"cmd": []any{"gh", "pr", "comment", "1", "--body-file", "clean.md"}})
	githubPostWant(t, clean, "argv with a clean file", "", "")
}

// TestGitHubPostGuardReasonCarriesNoValue is the property the incident turned into a rule: the reason a
// public thread sees names the rule and the place and nothing of the text.
func TestGitHubPostGuardReasonCarriesNoValue(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	secret := githubPostFake("ghp_", 20)
	githubPostWrite(t, cwd, "body.md", "token "+secret+"\n")
	reason := githubPostReason(t, githubPostShell(t, cwd, "gh pr comment 1 --body-file body.md"))
	githubPostWant(t, githubPostShell(t, cwd, "gh pr comment 1 --body-file body.md"), "secret file", githubPostRuleSecret, "body.md:1")
	for _, part := range []string{secret, "token "} {
		if strings.Contains(reason, part) {
			t.Errorf("the reason carries %q: %q", part, reason)
		}
	}
}

// TestGitHubPostAnswerBoundsThePayload is the row's own stdin policy: a payload within the bound is
// judged, one over it is refused rather than passed unjudged, and a failed read answers nothing.
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
	if reason == "" {
		t.Fatal("a payload over the bound was not refused")
	}
	if !strings.Contains(reason, "("+githubPostRuleUnread+") at "+githubPostWhereCommand+":") {
		t.Errorf("over the bound denied as %q, want %s at %s", reason, githubPostRuleUnread, githubPostWhereCommand)
	}
	if answer := GitHubPostAnswer(githubPostFailingReader{}); answer != "" {
		t.Errorf("a failed read answered %q, want nothing", answer)
	}
}

// githubPostWrite writes one file under the case's temporary directory.
func githubPostWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// githubPostFailingReader fails every read.
type githubPostFailingReader struct{}

func (githubPostFailingReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }
