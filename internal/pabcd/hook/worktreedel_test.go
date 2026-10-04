package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// delRig is a managed worktree, <home>/.codex/worktrees/zk3q/repo, plus a directory outside the worktrees root. The slot
// id is chosen so that no temporary path can contain it; every path is real, because detection canonicalizes. Nothing here
// runs a command: the guard only reads text, and the tests assert on what it answers about a rig that must stay intact.
type delRig struct {
	wtRig
	other string // a directory outside the worktrees root, the allowed neighbour of every denied target
}

func newDelRig(t *testing.T) delRig {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := delRig{wtRig: wtRig{home: home, codexHome: filepath.Join(home, ".codex")}}
	r.worktrees = filepath.Join(r.codexHome, "worktrees")
	r.slotRoot = filepath.Join(r.worktrees, "zk3q")
	r.checkout = filepath.Join(r.slotRoot, "repo")
	r.other = filepath.Join(home, "elsewhere", "build")
	wtWrite(t, filepath.Join(r.checkout, ".git"), "gitdir: /fake/main/.git/worktrees/zk3q\n")
	wtWrite(t, filepath.Join(r.other, "keep"), "x")
	return r
}

func (r delRig) id() WorktreeIdentity { return detectManagedWorktree(r.checkout, r.env()) }

func (r delRig) verdict(cmd string) GuardVerdict { return evaluateCommand(cmd, r.checkout, r.id()) }

// denied asserts a deny whose reason names what, the oracle's blocked-command text.
func (r delRig) denied(t *testing.T, cmd, what string) {
	t.Helper()
	got := r.verdict(cmd)
	if !got.Deny || !strings.HasPrefix(got.Reason, "[crw: WORKTREE-GUARD-03] blocked `"+what+"`: it deletes this session's own Codex-app-managed worktree (slot: "+r.slotRoot+").") {
		t.Errorf("%q: got %+v, want a deny of %q", cmd, got, what)
	}
}

func (r delRig) allowed(t *testing.T, cmds ...string) {
	t.Helper()
	for _, cmd := range cmds {
		if got := r.verdict(cmd); got.Deny {
			t.Errorf("%q: denied (%s)", cmd, got.Reason)
		}
	}
}

func (r delRig) intact(t *testing.T) {
	t.Helper()
	for _, path := range []string{filepath.Join(r.checkout, ".git"), filepath.Join(r.other, "keep")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the rig changed: %v", err)
		}
	}
}

const unresolvable = "unresolvable target mentioning the managed worktree"

// The tokenizer test of the oracle (worktree-guard.test.ts, section 5), with the cases its grammar leaves open.
func TestSplitSegmentsAndTokenize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"a && b || c; d | e", []string{"a", "b", "c", "d", "e"}},
		{"echo 'a;b' && ls", []string{"echo 'a;b'", "ls"}},
		{`echo "a|b" | cat`, []string{`echo "a|b"`, "cat"}},
		{"a;;|| b ;", []string{"a", "b"}},
		{"a & b", []string{"a & b"}}, // a single ampersand is not a cut
		{"a\nb", []string{"a\nb"}},   // neither is a newline
		{"echo 'open; b", []string{"echo 'open; b"}},
		{"  ;  ", nil},
	} {
		if got := splitSegments(c.in, false); !slices.Equal(got, c.want) {
			t.Errorf("splitSegments(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, c := range []struct {
		in   string
		want []string
	}{
		{`rm -rf "some dir/"`, []string{"rm", "-rf", "some dir/"}},
		{`a 'b c'd "e"`, []string{"a", "b cd", "e"}},
		{`x ""`, []string{"x", ""}},                              // an empty quoted token is a token
		{"a\u00a0b\ufeffc\u3000d", []string{"a", "b", "c", "d"}}, // JavaScript's \s
		{"a\u0085b", []string{"a\u0085b"}},                       // U+0085 is not
		{`a "unterminated b`, []string{"a", "unterminated b"}},
		{"", nil},
	} {
		if got := tokenize(c.in); !slices.Equal(got, c.want) {
			t.Errorf("tokenize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The extended grammar of the second walk: more cuts, parentheses kept as scope markers, a continued line joined.
func TestSplitSegmentsExtended(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"cd /tmp\nrm x", []string{"cd /tmp", "rm x"}},
		{"sleep 1 & rm x && ls", []string{"sleep 1", "rm x", "ls"}},
		{"(cd /tmp); rm x", []string{"(", "cd /tmp", ")", "rm x"}},
		{"echo $(rm x)", []string{"echo $", "(", "rm x", ")"}},
		{"{ rm x; }", []string{"rm x"}},
		{"a;{ rm x; }", []string{"a", "rm x"}},
		{"rm -rf .{cache,local} ${HOME}/x {a,b}", []string{"rm -rf .{cache,local} ${HOME}/x {a,b}"}}, // a brace inside a word is not a group
		{"echo 'a\nb (c)' && ls", []string{"echo 'a\nb (c)'", "ls"}},                                 // quotes protect every cut
		{"a || b | c", []string{"a", "b", "c"}},
	} {
		if got := splitSegments(c.in, true); !slices.Equal(got, c.want) {
			t.Errorf("splitSegments(%q, extended) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGitWorktreeRemoveOfTheOwnCheckout(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"git worktree remove " + r.checkout,
		"git worktree remove --force " + r.checkout,
		"git -C /tmp worktree remove " + r.checkout,
		"git -c core.x=y worktree remove " + r.checkout,
		"git worktree remove " + r.slotRoot,
		"sudo git worktree remove -f " + r.checkout,
	} {
		r.denied(t, cmd, "git worktree remove "+cmd[strings.LastIndex(cmd, " ")+1:])
	}
	r.denied(t, "git worktree remove .", "git worktree remove .")
	r.denied(t, "git -C "+r.slotRoot+" worktree remove ./repo", "git worktree remove ./repo")
	r.allowed(t,
		"git status", "git worktree list", "git worktree prune",
		"git worktree remove /some/other/place", "git worktree remove "+r.other, "git -C "+r.other+" status",
		"git worktree remove ''", // an empty target is no target
		"git worktree add "+r.other,
	)
	r.intact(t)
}

func TestRmAndRmdirOfTheProtectedTree(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ cmd, what string }{
		{"rm -rf " + r.slotRoot, "rm -r " + r.slotRoot},
		{"sudo rm -rf " + r.slotRoot, "rm -r " + r.slotRoot},
		{"/usr/bin/sudo /bin/rm -rf " + r.slotRoot, "rm -r " + r.slotRoot},
		{"env FOO=1 BAR_2=x rm -rf " + r.slotRoot, "rm -r " + r.slotRoot},
		{"command rm -rf " + r.slotRoot, "rm -r " + r.slotRoot},
		{"builtin rm -rf " + r.slotRoot, "rm -r " + r.slotRoot},
		{"/bin/rm -rf .", "rm -r ."},
		{"rm --recursive --force " + r.checkout, "rm -r " + r.checkout},
		{"rm -rf -- " + r.checkout, "rm -r " + r.checkout},
		{"rm -fR " + r.checkout + "/", "rm -r " + r.checkout + "/"},
		{"rmdir " + r.checkout, "rmdir " + r.checkout},
		{"rmdir -p " + r.checkout, "rmdir " + r.checkout},
		{"cd /tmp && rm -rf " + r.slotRoot, "rm -r " + r.slotRoot},
		{"rm -rf ..", "rm -r .."},
		{"rm -rf ../..", "rm -r ../.."}, // the worktrees root is an ancestor of the cwd
		{"rm -rf ''", "rm -r "},         // an empty token resolves to the cwd
		{"rm -rf ./src/..", "rm -r ./src/.."},
	} {
		r.denied(t, c.cmd, c.what)
	}
	r.allowed(t,
		"rm -rf ./build", "rm -rf "+r.other, "rm -rf "+r.other+" && echo ok",
		"rm -f ./dist/bundle.js", // no recursive flag: a file removal
		"rm "+r.other,            // rm without -r cannot remove a directory
		"rm --force "+r.other,    // a long flag other than --recursive carries no recursion
		"rmdir "+r.other, "unlink "+r.checkout+"/somefile", "ls -la",
		"rm -rf -- -rf", // after -- every token is a target
		"echo rm -rf "+r.other,
	)
	r.intact(t)
}

func TestFallbackForUnresolvableTargets(t *testing.T) {
	r := newDelRig(t)
	r.denied(t, `rm -rf "$X" && echo cleaning zk3q`, unresolvable)
	r.denied(t, "rm -rf ./build-zk3q", unresolvable) // oracle defect, kept: the slot id is a raw substring
	r.denied(t, "rm "+r.checkout, unresolvable)      // no -r, but any removal verb that names the slot reaches the fallback
	r.denied(t, `rm -rf "$X" && echo `+r.worktrees, unresolvable)
	r.denied(t, "find . -name zk3q | xargs rm -rf", unresolvable)
	r.allowed(t, `rm -rf "$X" && echo cleaning`, "echo zk3q", "ls "+r.slotRoot, "rm -rf ./build")
	// Windows removal verbs are not ported (rework decision): only the fallback sees them, with the fallback's reason.
	r.denied(t, "rd "+r.checkout, unresolvable)
	r.denied(t, "Remove-Item -Recurse -LiteralPath "+r.slotRoot, unresolvable)
	r.denied(t, "REMOVE-ITEM "+r.checkout, unresolvable)
	r.allowed(t, "Remove-Item .", "rd .", "Remove-Item -Recurse .") // the oracle denies the last two; the port cannot see them
	// The oracle's /i folds ASCII only: a long s is not an s, a Kelvin sign is not a k.
	r.allowed(t, "era\u017fe "+r.slotRoot, "git wor\u212atree remove "+r.checkout)
}

func TestNonManagedCwdNeverDenies(t *testing.T) {
	outside := t.TempDir()
	id := detectManagedWorktree(outside, wtEnv(map[string]string{"HOME": outside, "CODEX_HOME": filepath.Join(outside, "nope")}))
	for _, cmd := range []string{"rm -rf " + outside, "rm -rf .", "git worktree remove .", ""} {
		if got := evaluateCommand(cmd, outside, id); got.Deny {
			t.Errorf("%q: denied outside a managed worktree", cmd)
		}
	}
	r := newDelRig(t)
	r.allowed(t, "", "   ", "\n\t")
}

// The second walk adds denies for shapes the oracle's segments never see, each paired with a neighbour that stays allowed.
func TestExtendedWalkAddsDenies(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ deny, allow, what string }{
		{"echo hi\nrm -rf .", "echo hi\nrm -rf ./build", "rm -r ."},
		{"cd ..\nrm -rf repo", "cd ..\nrm -rf other", "rm -r repo"},
		{"cd /tmp\nrm -rf " + r.slotRoot, "cd /tmp\nrm -rf " + r.other, "rm -r " + r.slotRoot},
		{"sleep 1 & rm -rf .", "sleep 1 & rm -rf ./build", "rm -r ."},
		{"(rm -rf .)", "(rm -rf ./build)", "rm -r ."},
		{"echo $(rm -rf .)", "echo $(rm -rf ./build)", "rm -r ."},
		{"{ rm -rf .; }", "{ rm -rf ./build; }", "rm -r ."},
		{"{\\\n rm -rf ../repo; }", "{\\\n rm -rf ../other; }", "rm -r ../repo"},      // a continued line after the brace
		{"cleanup() { rm -rf .; }", "cleanup() { rm -rf ./build; }", "rm -r ."},       // accepted false positive: the guard cannot tell whether the function is called
		{"(cd /tmp)\nrm -rf ../repo", "(cd /tmp && rm -rf ./build)", "rm -r ../repo"}, // a subshell's cd does not leak
		{"git worktree remove \\\n.", "git worktree list \\\n.", "git worktree remove ."},
		{"rm -rf --no-preserve-root /", "rm -rf --no-preserve-root " + r.other, "rm -r /"}, // the root is an ancestor
		{"rmdir /", "rmdir " + r.other, "rmdir /"},
		{"cat > clean.sh <<'EOF'\nrm -rf .\nEOF", "cat > clean.sh <<'EOF'\nrm -rf ./build\nEOF", "rm -r ."}, // accepted false positive: the body is not run
	} {
		r.denied(t, c.deny, c.what)
		r.allowed(t, c.allow)
	}
	// A quoted or grouped verb that names the slot reaches the fallback; the oracle's lead set misses the quote.
	r.denied(t, `bash -c "rm -rf `+r.slotRoot+`"`, unresolvable)
	r.denied(t, "sh -c 'rm -rf "+r.checkout+"'", unresolvable)
	r.denied(t, `echo "rm -rf `+r.slotRoot+`"`, unresolvable) // accepted false positive: only the text is quoted
	r.allowed(t, `bash -c "rm -rf `+r.other+`"`, `echo "rm -rf ./build"`, "rm -rf .{cache,local}", "rm -rf ${TMPDIR}/x", "find . -exec ls {} +", "rm -rf .\\\n{\\\ncache,local}")
	r.intact(t)
}

// Everything the oracle denies keeps the oracle's own reason: the first walk is the oracle's walk, the extended one only
// runs after it allows. These commands deny in the first walk and would deny differently in the extended one.
func TestFirstWalkKeepsTheOracleReason(t *testing.T) {
	r := newDelRig(t)
	r.denied(t, "rm -rf / ..", "rm -r ..")
	r.denied(t, "rm -rf /; rmdir .", "rmdir .")
	r.denied(t, "echo x\nrm -rf ..; rmdir .", "rmdir .")
	r.denied(t, "rm -rf \\\n.", "rm -r .")
	r.denied(t, "echo x\nrm -rf "+r.checkout, unresolvable) // the oracle's fallback, not the extended verb deny
	r.denied(t, "rm -rf . 2>&1", "rm -r .")
	r.denied(t, "rm -rf 2>&1 .", "rm -r .")
}

// The effective directory of a segment is its cwd (after cd or git -C): a target equal to it, or above it, is protected,
// whichever directory that is. Oracle behaviour, kept: it also denies the same words after a cd to an unrelated directory.
func TestEffectiveDirectoryIsWhatIsCompared(t *testing.T) {
	r := newDelRig(t)
	r.denied(t, "cd /tmp && rm -rf .", "rm -r .")
	r.denied(t, "git -C "+r.other+" worktree remove .", "git worktree remove .")
	r.allowed(t, "cd /tmp && rm -rf ./build")
}

func TestIsProtectedTarget(t *testing.T) {
	r := newDelRig(t)
	id := r.id()
	outside, err := os.MkdirTemp(r.home, "outside-")
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r.slotRoot, "escape")
	wtLink(t, outside, link)
	for _, c := range []struct {
		target   string
		extended bool
		want     bool
	}{
		{r.slotRoot, false, true},
		{r.checkout, false, true},
		{r.checkout + "/", false, true},
		{r.checkout + "/src/..", false, true},
		{"", false, true}, // an empty token is the cwd
		{".", false, true},
		{"..", false, true},
		{"../..", false, true},
		{link, false, false},        // a link out of the slot names the outside directory
		{link + "/x", false, false}, // and so does a path through it
		{filepath.Join(r.slotRoot, "no-such"), false, false},
		{r.other, false, false},
		{"build", false, false},
		{"/", false, false}, // oracle defect, kept in the first walk: cwd.startsWith("//") never holds
		{"/", true, true},   // port: fixed in the extended walk
		{"//", true, true},
		{r.home, true, true},
	} {
		if got := isProtectedTarget(c.target, r.checkout, id, c.extended); got != c.want {
			t.Errorf("isProtectedTarget(%q, extended %v) = %v, want %v", c.target, c.extended, got, c.want)
		}
	}
}

func preToolPayload(t *testing.T, r delRig, command any, extra map[string]any) string {
	t.Helper()
	fields := map[string]any{
		"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": r.checkout,
		"tool_name": "Bash", "tool_input": map[string]any{"command": command},
	}
	for k, v := range extra {
		if v == nil {
			delete(fields, k)
		} else {
			fields[k] = v
		}
	}
	return wtPayload(t, fields)
}

func TestHandleWorktreeGuardPreToolDeniesThroughTheEnvelope(t *testing.T) {
	r := newDelRig(t)
	cmd := "git worktree remove " + r.checkout
	out := HandleWorktreeGuardPreTool(preToolPayload(t, r, cmd, nil), r.env())
	var got struct {
		Specific struct {
			Event    string `json:"hookEventName"`
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
			Context  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if !strings.HasSuffix(out, "}}\n") || strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("not one JSON line: %q", out)
	}
	s := got.Specific
	if s.Event != "PreToolUse" || s.Decision != "deny" || s.Reason != s.Context || !strings.Contains(s.Reason, "WORKTREE-GUARD-03") ||
		!strings.Contains(s.Reason, "blocked `"+cmd+"`") || !strings.Contains(s.Reason, "(slot: "+r.slotRoot+")") || !strings.HasSuffix(s.Reason, "See $crw:crw-worktree-guardian.") {
		t.Errorf("envelope: %+v", s)
	}
	const keys = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"`
	if !strings.HasPrefix(out, keys) || !strings.Contains(out, `","additionalContext":"`) {
		t.Errorf("key order: %q", out)
	}
	if HandleWorktreeGuardPreTool(preToolPayload(t, r, "git status", nil), r.env()) != "" {
		t.Error("a benign command is answered")
	}
}

func TestHandleWorktreeGuardPreToolSubagentAndToolName(t *testing.T) {
	r := newDelRig(t)
	deny := "rm -rf " + r.slotRoot
	for name, extra := range map[string]map[string]any{
		"subagent-stamped":      {"agent_id": "child-agent-1", "agent_type": "worker"},
		"no tool_name":          {"tool_name": nil},
		"empty tool_name":       {"tool_name": ""},
		"non-string tool_name":  {"tool_name": 7},
		"unrelated extra field": {"turn_id": "t1", "deep": []any{[]any{[]any{1}}}},
	} {
		if out := HandleWorktreeGuardPreTool(preToolPayload(t, r, deny, extra), r.env()); !strings.Contains(out, `"permissionDecision":"deny"`) {
			t.Errorf("%s: not denied: %q", name, out)
		}
	}
}

func TestHandleWorktreeGuardPreToolFailsOpen(t *testing.T) {
	r := newDelRig(t)
	deny := "rm -rf " + r.slotRoot
	silent := map[string]string{
		"not json":         "not json",
		"empty":            "",
		"array":            "[]",
		"leading BOM":      "\ufeff" + preToolPayload(t, r, deny, nil),
		"other event":      preToolPayload(t, r, deny, map[string]any{"hook_event_name": "PostToolUse"}),
		"no event":         preToolPayload(t, r, deny, map[string]any{"hook_event_name": nil}),
		"other tool":       preToolPayload(t, r, deny, map[string]any{"tool_name": "apply_patch"}),
		"no cwd":           preToolPayload(t, r, deny, map[string]any{"cwd": nil}),
		"unmanaged cwd":    preToolPayload(t, r, deny, map[string]any{"cwd": r.home}),
		"empty command":    preToolPayload(t, r, "", nil),
		"blank command":    preToolPayload(t, r, " \t ", nil),
		"number command":   preToolPayload(t, r, 5, nil),
		"no command":       preToolPayload(t, r, deny, map[string]any{"tool_input": map[string]any{}}),
		"array tool_input": preToolPayload(t, r, deny, map[string]any{"tool_input": []any{deny}}),
	}
	for name, raw := range silent {
		if out := HandleWorktreeGuardPreTool(raw, r.env()); out != "" {
			t.Errorf("%s: answered %q", name, out)
		}
	}
}
