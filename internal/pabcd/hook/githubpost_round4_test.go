package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScriptBodyWritesByHowTheFileRuns: one file has two bodies to the reader: run by path, a #! line that names another program
// (#!/bin/true) makes it that program's input, whose writes are not read; run by a shell (bash writer.sh, source writer.sh), the
// shell reads every line and the #! line is a comment. The writes of the shell reading must reach the scripts after it however the
// same file ran before in the text (CRW-1028 verifier round 4, finding 1).
func TestScriptBodyWritesByHowTheFileRuns(t *testing.T) {
	r, cwd := round3Scene(t, map[string]string{
		"writer.sh":      "#!/bin/true\ncat evil.sh > post.sh\n",
		"otherwriter.sh": "#!/bin/true\ncat evil.sh > other.sh\n",
	})
	for _, c := range []struct {
		cmd       string
		ghDenied  bool
		delDenied bool
	}{
		{"./writer.sh; bash writer.sh; bash post.sh", true, true},
		{"./writer.sh; source writer.sh; bash post.sh", true, true},
		{"./writer.sh && sh writer.sh && bash post.sh", true, true},
		{"bash writer.sh; ./writer.sh; bash post.sh", true, true},
		{"bash writer.sh; bash post.sh", true, true},
		// controls: run by path, /bin/true reads the file and writes nothing the reader follows; the shell reading writes other.sh
		{"./writer.sh; bash post.sh", false, false},
		{"./otherwriter.sh; bash otherwriter.sh; bash post.sh", false, false},
	} {
		_, gh := githubPostJudgeText(c.cmd, cwd)
		del := r.verdict(c.cmd).Deny
		if gh != c.ghDenied {
			t.Errorf("github guard %q: denied=%v, want %v", c.cmd, gh, c.ghDenied)
		}
		if del != c.delDenied {
			t.Errorf("worktree guard %q: denied=%v, want %v", c.cmd, del, c.delDenied)
		}
	}
}

// TestGitHubPostDirectGhRewrittenBySibling: ./gh is a file of the directory that the guard reads; a sibling script, or the text
// itself, that writes ./gh before it runs makes the file read before the command stale, whatever read-only gh words follow (CRW-1028
// verifier round 4, finding 2). A write to another file leaves ./gh readable.
func TestGitHubPostDirectGhRewrittenBySibling(t *testing.T) {
	_, cwd := round3Scene(t, map[string]string{
		"gh":             "#!/bin/sh\necho clean\n",
		"ghwriter.sh":    "cat evil.sh > gh\n",
		"otherwriter.sh": "cat evil.sh > other.sh\n",
	})
	for _, c := range []struct {
		cmd    string
		denied bool
	}{
		{"bash ghwriter.sh; ./gh pr view 1", true},
		{"bash ghwriter.sh; ./gh issue view 1", true},
		{"bash ghwriter.sh; ./gh repo view", true},
		{"cp evil.sh gh; ./gh repo view", true},
		{"cat evil.sh > gh && ./gh pr view 1", true},
		// controls
		{"./gh pr view 1", false},
		{"bash otherwriter.sh; ./gh pr view 1", false},
		{"bash ghwriter.sh; gh pr view 1", false},
	} {
		if _, got := githubPostJudgeText(c.cmd, cwd); got != c.denied {
			t.Errorf("%q: denied=%v, want %v", c.cmd, got, c.denied)
		}
	}
}

// TestGitHubPostDirectBinaryRunnerArguments: a binary run by path is not read, but its name and arguments are: ./tmux, ./ssh or
// ./script with an argument that names a post (or an unknown word) runs that post, so the runner rule applies before the binary is
// let through, whatever the binary's size (CRW-1028 verifier round 4, finding 3). A large build output such as ./bin/crw and a
// runner whose arguments name no post stay allowed.
func TestGitHubPostDirectBinaryRunnerArguments(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if len(exe) <= githubPostMaxFileBytes {
		t.Fatalf("the test binary is %d bytes, not over the script limit", len(exe))
	}
	small := []byte("\x7fELF\x02\x01\x01\x00\x00\x00")
	for name, body := range map[string][]byte{"tmux": exe, "bin/crw": exe, "bin/ssh": small, "script": small} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		cmd    string
		denied bool
	}{
		{"./tmux new-session 'gh pr comment 1 -b x'", true},
		{"./tmux new-session \"$CMD\"", true},
		{"./bin/ssh host gh issue create", true},
		{"./script -c 'gh pr comment 1 -b x'", true},
		// controls
		{"./tmux ls", false},
		{"./bin/crw version", false},
		{"./bin/crw gh pr comment 1 -b x", false},
	} {
		if _, got := githubPostJudgeText(c.cmd, dir); got != c.denied {
			t.Errorf("%q: denied=%v, want %v", c.cmd, got, c.denied)
		}
	}
}
