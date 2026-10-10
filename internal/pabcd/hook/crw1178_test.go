package hook

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// crw1178Cache lays out a two-file project the way a first python3 -m unittest leaves it: a __pycache__ holding the Python-written
// entries of both sources.
func crw1178Cache(t *testing.T, cwd string) {
	t.Helper()
	header := make([]byte, 16)
	copy(header, []byte{0xcb, 0x0d, 0x0d, 0x0a})
	binary.LittleEndian.PutUint32(header[4:], 0)
	if err := os.MkdirAll(filepath.Join(cwd, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"calc", "test_calc"} {
		if err := os.WriteFile(filepath.Join(cwd, m+".py"), []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cwd, "__pycache__", m+".cpython-312.pyc"), append(header, "code"...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// CRW-1178 (S2R2-F1): a routine test run is not refused by either guard because an earlier run left a __pycache__.
func TestCRW1178RepeatedTestRunIsNotRefused(t *testing.T) {
	cwd, _, env := gateScene(t)
	crw1178Cache(t, cwd)
	for _, cmd := range []string{
		"python3 -m unittest",
		"python3 -B -m unittest",
		"PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
		"env PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
		"python3 -m pytest",
	} {
		if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env); out != "" {
			t.Errorf("memory gate refused %q beside a cache of readable sources: %s", cmd, out)
		}
		if out := HandleGitHubPostGuard(gateBash(t, cwd, cmd)); out != "" {
			t.Errorf("GitHub guard refused %q beside a cache of readable sources: %s", cmd, out)
		}
	}
}

// The decision for compiled code with no source stays a deny in both guards, and the reason names the file and the cause.
func TestCRW1178SourcelessBytecodeStaysRefusedWithItsCause(t *testing.T) {
	cwd, root, env := gateScene(t)
	if err := os.WriteFile(filepath.Join(cwd, "hidden.pyc"), make([]byte, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := "python3 -m unittest"
	reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
	for _, want := range []string{"unreadable-program", "hidden.pyc", "compiled code"} {
		if !strings.Contains(reason, want) {
			t.Errorf("memory reason lacks %q: %s", want, reason)
		}
	}
	for _, bad := range []string{"MEMORY-WRITE-GATE", "protected write has not been established", "allow-write", "ask the user"} {
		if strings.Contains(reason, bad) {
			t.Errorf("an unreadable command reads as a protected write (%q): %s", bad, reason)
		}
	}
	if len(reason) > 700 {
		t.Errorf("reason over the bound: %d", len(reason))
	}
	gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd)))
	for _, want := range []string{"unreadable-github-post", "hidden.pyc", "compiled code", "names no GitHub post"} {
		if !strings.Contains(gh, want) {
			t.Errorf("GitHub reason lacks %q: %s", want, gh)
		}
	}
	for _, bad := range []string{"GitHub posting has not been established", "GitHub post blocked", "--body-file"} {
		if strings.Contains(gh, bad) {
			t.Errorf("a non-GitHub command reads as a GitHub post refusal (%q): %s", bad, gh)
		}
	}
	// A leaf is sent to its parent and keeps the cause.
	leaf := gatePayload(t, cwd, map[string]any{"agent_id": "leaf", "agent_type": "executor", "tool_name": "Bash", "tool_input": map[string]any{"command": cmd}})
	reason = gateDeny(t, HandleMemoryWriteGate(leaf, env))
	if !strings.Contains(reason, "parent") || !strings.Contains(reason, "hidden.pyc") || strings.Contains(reason, "MEMORY-WRITE-GATE") {
		t.Errorf("leaf reason: %s", reason)
	}
	// A grant still spends on the unreadable program, as before; the decision is unchanged.
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env); out != "" {
		t.Errorf("a grant must still let it pass: %s", out)
	}
	_ = root
}

// A command that does name a GitHub post keeps the GitHub wording beside the cause.
func TestCRW1178UnreadableGithubPostKeepsPostWording(t *testing.T) {
	cwd, _, _ := gateScene(t)
	gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, `"$FOO" a; gh pr comment 1 --body-file x`)))
	if !strings.Contains(gh, "unreadable-github-post") || strings.Contains(gh, "names no GitHub post") {
		t.Errorf("a command that names a post: %s", gh)
	}
}

// CRW-1178 (S2R2-F5): with no usable cwd the grant can never be consumed, so the refusal does not offer it.
func TestCRW1178NoGrantRouteWithoutCwd(t *testing.T) {
	cwd, root, env := gateScene(t)
	edit := func(cwd, file string) string {
		return gatePayload(t, cwd, map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": file, "content": "x"}})
	}
	for name, raw := range map[string]string{
		"relative path":   edit("", "notes/x.txt"),
		"memory path":     edit("", filepath.Join(root, "n.md")),
		"memory note":     gatePayload(t, "", nil),
		"no session id":   gatePayload(t, cwd, map[string]any{"session_id": ""}),
		"relative cwd":    edit(".", "notes/x.txt"),
		"relative memory": edit("rel", filepath.Join(root, "n.md")),
	} {
		reason := gateDeny(t, HandleMemoryWriteGate(raw, env))
		if strings.Contains(reason, "allow-write") || strings.Contains(reason, "session working directory") {
			t.Errorf("%s offers a grant that cannot be spent: %s", name, reason)
		}
		if !strings.Contains(reason, "MEMORY-WRITE-GATE") {
			t.Errorf("%s: %s", name, reason)
		}
	}
	// The unknown destination of an edit still says to use an absolute path.
	if r := gateDeny(t, HandleMemoryWriteGate(edit("", "notes/x.txt"), env)); !strings.Contains(r, "unknown-destination") || !strings.Contains(r, "absolute path") {
		t.Errorf("unknown destination: %s", r)
	}
	// With an absolute cwd the grant route stays, and it is spent.
	if r := gateDeny(t, HandleMemoryWriteGate(edit(cwd, filepath.Join(root, "n.md")), env)); !strings.Contains(r, "allow-write --session "+gateSession) {
		t.Errorf("an absolute cwd must keep the grant route: %s", r)
	}
}

// A freeform apply_patch carries its patch text as a bare string; it goes through the same judgement as the patch in a record.
func TestCRW1178FreeformApplyPatchIsJudged(t *testing.T) {
	cwd, root, env := gateScene(t)
	patch := func(target string) string {
		return "*** Begin Patch\n*** Add File: " + target + "\n+x\n*** End Patch\n"
	}
	freeform := func(cwd, target string) string {
		return gatePayload(t, cwd, map[string]any{"tool_name": "apply_patch", "tool_input": patch(target)})
	}
	record := func(cwd, target string) string {
		return gatePayload(t, cwd, map[string]any{"tool_name": "apply_patch", "tool_input": map[string]any{"command": patch(target)}})
	}
	for _, c := range []struct{ cwd, target string }{{cwd, filepath.Join(root, "n.md")}, {"", filepath.Join(root, "n.md")}, {"", "notes/x.txt"}} {
		want := HandleMemoryWriteGate(record(c.cwd, c.target), env) != ""
		got := HandleMemoryWriteGate(freeform(c.cwd, c.target), env) != ""
		if !want || got != want {
			t.Errorf("freeform patch of %s from cwd %q: denied %v, the record form is denied %v", c.target, c.cwd, got, want)
		}
	}
	if out := HandleMemoryWriteGate(freeform(cwd, filepath.Join(cwd, "ok.txt")), env); out != "" {
		t.Errorf("a patch outside the memories root must pass: %s", out)
	}
	if out := HandleMemoryWriteGate(freeform(cwd, "ok.txt"), env); out != "" {
		t.Errorf("a relative patch under a usable cwd must pass: %s", out)
	}
}

// githubReaderWording is whether a GitHub guard answer is the command reader's refusal (it says what could not be read and that
// the text names no post) and not a policy refusal about GitHub posting.
func crw1178ReaderWording(t *testing.T, reason string) {
	t.Helper()
	if !strings.Contains(reason, "[crw command-reader]") || !strings.Contains(reason, "names no GitHub post") {
		t.Errorf("not the command reader's wording: %s", reason)
	}
	for _, bad := range []string{"GitHub posting has not been established", "GitHub post blocked", "--body-file"} {
		if strings.Contains(reason, bad) {
			t.Errorf("a non-GitHub command reads as a GitHub post refusal (%q): %s", bad, reason)
		}
	}
}

// A path or argument that merely contains the letters gh, pr or issue is no mention of a GitHub post.
func TestCRW1178PathSubstringsAreNoGithubPost(t *testing.T) {
	cwd, _, _ := gateScene(t)
	if err := os.WriteFile(filepath.Join(cwd, "hidden.pyc"), make([]byte, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"python3 -m unittest tests/high_priority",
		"python3 -m unittest tests.test_high_priority_review",
		"python3 -m unittest discover -s thoughts -p 'test_pr_*.py'",
	} {
		crw1178ReaderWording(t, githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd))))
	}
}

// A script file that cannot be read is the reader's refusal when the command names no post, with the file and the cause.
func TestCRW1178UnreadableScriptFileIsReaderWording(t *testing.T) {
	cwd, _, _ := gateScene(t)
	if err := os.WriteFile(filepath.Join(cwd, "noperm.sh"), []byte("echo hi\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ cmd, file string }{
		{"bash missing.sh", "missing.sh"},
		{"sh ./missing.sh", "missing.sh"},
		{"./missing.sh", "missing.sh"},
		{"bash noperm.sh", "noperm.sh"},
	} {
		if os.Geteuid() == 0 && c.file == "noperm.sh" {
			continue
		}
		reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, c.cmd)))
		if reason == "" {
			continue // a file that is absent and run by path runs nothing: allowed
		}
		crw1178ReaderWording(t, reason)
		if !strings.Contains(reason, c.file) {
			t.Errorf("%q: the reason lacks the file: %s", c.cmd, reason)
		}
	}
	// The same refusal beside a spelled gh post keeps the GitHub wording.
	reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "bash missing.sh; gh pr comment 1 --body-file x")))
	if strings.Contains(reason, "names no GitHub post") {
		t.Errorf("a command that spells a post: %s", reason)
	}
}

// A refusal inside a script is judged by the text of that script, not by the command that runs it.
func TestCRW1178NestedScriptMentionIsOfItsOwnText(t *testing.T) {
	cwd, _, _ := gateScene(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("post.sh", "\"$FOO\" a\ngh pr comment 1 --body-file body.md\n")
	write("plain.sh", "\"$FOO\" a\necho done\n")
	post := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "bash post.sh")))
	if post == "" || strings.Contains(post, "names no GitHub post") {
		t.Errorf("a script that spells a post lost the GitHub wording: %s", post)
	}
	plain := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "bash plain.sh")))
	crw1178ReaderWording(t, plain)
	// A script that runs a script that spells a post, from a command that does not.
	write("outer.sh", "bash post.sh\n")
	outer := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "bash outer.sh")))
	if outer == "" || strings.Contains(outer, "names no GitHub post") {
		t.Errorf("a nested script that spells a post lost the GitHub wording: %s", outer)
	}
}

// An operating-system failure in the module inventory names the directory that failed, relative to the project.
func TestCRW1178WalkErrorNamesTheDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	cwd, _, env := gateScene(t)
	blocked := filepath.Join(cwd, "blocked_tests")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	cmd := "python3 -m unittest"
	mem := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
	gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd)))
	for _, r := range []string{mem, gh} {
		if !strings.Contains(r, "blocked_tests") || strings.Contains(r, cwd) {
			t.Errorf("the reason must name the directory relative to the project: %s", r)
		}
	}
}
