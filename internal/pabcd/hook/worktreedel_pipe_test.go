package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-726 generation 2 (c15): the pipe rule of this change, closed against the five findings of the parent's
// independent review of head 71cf063d. Each row was checked in bash 5.3.9 with a harmless stand-in for rm (touch in a
// temporary directory); no row runs a deletion, and the guard reads text and runs nothing.

// worktreeDelPipeDenied asserts a deny of the pipe position.
func worktreeDelPipeDenied(t *testing.T, r delRig, cmd string) {
	t.Helper()
	worktreeDelUnreadableDenied(t, r, cmd, "a shell program read from a pipe")
}

// TestWorktreeDelPipeStdinDescriptor is c15(a): only a word that opens a file on descriptor 0 replaces the pipe. A
// descriptor duplication or a close keeps the refusal, because bash still reads the piped program.
func TestWorktreeDelPipeStdinDescriptor(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash <&0",
		"printf 'rm -rf ../repo' | bash 0<&0",
		"printf 'rm -rf ../repo' | bash <&2",
		"printf 'rm -rf ../repo' | bash 0<&2",
		"printf 'rm -rf ../repo' | bash <&-",
		"printf 'rm -rf ../repo' | bash 0<&-",
		"printf 'rm -rf ../repo' | bash 2</dev/null",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A file on descriptor 0 replaces the pipe: bash reads the file, not the pipe.
	r.allowed(t,
		"printf 'rm -rf ../repo' | bash </dev/null",
		"printf 'rm -rf ../repo' | bash < /dev/null",
		"printf 'rm -rf ../repo' | bash 0</dev/null",
		"printf 'rm -rf ../repo' | bash 0< /dev/null",
	)
	r.intact(t)
}

// TestWorktreeDelPipeCompound is c15(b): a subshell or a brace group on the right of a pipe runs every simple command
// in it with the pipe on standard input.
func TestWorktreeDelPipeCompound(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | (true; bash)",
		"printf 'rm -rf ../repo' | (cd sub; bash)",
		"printf 'rm -rf ../repo' | ( exec 2>/dev/null; bash )",
		"printf 'rm -rf ../repo' | (cd sub && bash)",
		"printf 'rm -rf ../repo' | { bash; }",
		"printf 'rm -rf ../repo' | { cd sub; bash; }",
		"printf 'rm -rf ../repo' | (bash)",
		"printf 'rm -rf ../repo' | ( sh )",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// The pipe belongs to the shell only: a compound command that runs something else is read as today.
	r.allowed(t,
		"printf x | (cd sub; cat)",
		"printf x | { cat; }",
		"printf x | (bash -c 'cat')",
	)
	r.intact(t)
}

// TestWorktreeDelPipeWrapper is c15(c): the pipe rule skips the same wrapper set the walk's program reader skips.
func TestWorktreeDelPipeWrapper(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | nohup bash",
		"printf 'rm -rf ../repo' | timeout 5 bash",
		"printf 'rm -rf ../repo' | setsid bash",
		"printf 'rm -rf ../repo' | nice bash",
		"printf 'rm -rf ../repo' | stdbuf -o0 bash",
		"printf 'rm -rf ../repo' | exec bash",
		"printf 'rm -rf ../repo' | time bash",
		"printf 'rm -rf ../repo' | env bash",
		"printf 'rm -rf ../repo' | command bash",
		"printf 'rm -rf ../repo' | sudo bash",
		"printf 'rm -rf ../repo' | xargs bash",
		"printf 'rm -rf ../repo' | ionice bash",
		"printf 'rm -rf ../repo' | builtin bash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A wrapper around a command that is no shell reads the pipe as its own input.
	r.allowed(t,
		"printf x | nohup cat",
		"printf x | timeout 5 cat",
		"printf x | nice cat",
	)
	r.intact(t)
}

// TestWorktreeDelRemovalWrapper is c15(d): stripPrefixes skips the same wrapper set, so a removal behind a wrapper is
// named and denied.
func TestWorktreeDelRemovalWrapper(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ deny, allow, what string }{
		{"nohup rm -rf ../repo", "nohup rm -rf ../other", "rm -r ../repo"},
		{"timeout 5 rm -rf ../repo", "timeout 5 rm -rf ../other", "rm -r ../repo"},
		{"nice rm -rf ../repo", "nice rm -rf ../other", "rm -r ../repo"},
		{"nice -n 5 rm -rf ../repo", "nice -n 5 rm -rf ../other", "rm -r ../repo"},
		{"setsid rm -rf ../repo", "setsid rm -rf ../other", "rm -r ../repo"},
		{"stdbuf -o0 rm -rf ../repo", "stdbuf -o0 rm -rf ../other", "rm -r ../repo"},
		{"ionice rm -rf ../repo", "ionice rm -rf ../other", "rm -r ../repo"},
		{"xargs rm -rf ../repo", "xargs rm -rf ../other", "rm -r ../repo"},
		{"exec rm -rf ../repo", "exec rm -rf ../other", "rm -r ../repo"},
	} {
		r.denied(t, c.deny, c.what)
		r.allowed(t, c.allow)
	}
	r.intact(t)
}

// TestMemoryGatePipeWrapper is c15(e): the memory gate reads the same check, so a wrapped or compound shell behind a
// pipe is a write attempt of its own.
func TestMemoryGatePipeWrapper(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, command := range []string{
		"printf 'echo x > " + root + "/a' | nohup bash",
		"printf 'echo x > " + root + "/a' | (cd x; bash)",
		"printf 'echo x > " + root + "/a' | bash <&0",
	} {
		got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
		if got.Surface != "shell" || !strings.HasPrefix(got.Target, "(a program the gate cannot read: ") {
			t.Errorf("%q: %+v, want a shell attempt whose target names the unreadable program", command, got)
		}
		payload := gateBash(t, cwd, command)
		reason := gateDeny(t, HandleMemoryWriteGate(payload, env))
		if !strings.Contains(reason, "(a program the gate cannot read: ") {
			t.Errorf("the reason does not name the unreadable program: %s", reason)
		}
		gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
		if out := HandleMemoryWriteGate(payload, env); out != "" {
			t.Errorf("with a grant the write must pass: %s", out)
		}
	}
}
