package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRewrittenScriptIsComparedByFileIdentity: a script the text overwrites through another name for the same file (a hard link, a
// symbolic link, a link in a directory reached through a link) is the file that runs, so the body the guards read before the
// command is stale. The name strings differ; the file is the same (CRW-1028 verifier finding 2). Both consumers of the check, the
// GitHub post guard and the worktree deletion guard, refuse; a different file stays allowed.
func TestRewrittenScriptIsComparedByFileIdentity(t *testing.T) {
	r := newDelRig(t)
	cwd, _, _ := gateScene(t)
	for _, base := range []string{cwd, r.checkout} {
		write := func(name, body string) {
			t.Helper()
			p := filepath.Join(base, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write("post.sh", "echo clean\n")
		write("other.sh", "echo other\n")
		write("evil.sh", "rm -rf "+r.checkout+"\ngh pr comment 1 -b x\n")
		write("real/post2.sh", "echo clean\n")
		if err := os.Link(filepath.Join(base, "post.sh"), filepath.Join(base, "alias.sh")); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(base, "post.sh"), filepath.Join(base, "real", "alias2.sh")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", filepath.Join(base, "dirlink")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../post.sh", filepath.Join(base, "real", "sym.sh")); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		cmd    string
		denied bool
	}{
		{"cat evil.sh > alias.sh; bash post.sh", true},
		{"cp evil.sh alias.sh; bash post.sh", true},
		{"cat evil.sh > real/alias2.sh; bash post.sh", true},
		{"cat evil.sh > dirlink/alias2.sh; bash post.sh", true},
		{"cat evil.sh > dirlink/sym.sh; bash post.sh", true},
		{"cat evil.sh > alias.sh; bash real/alias2.sh", true},
		{"cat evil.sh > dirlink/post2.sh; bash real/post2.sh", true},
		{"cat evil.sh > other.sh; bash post.sh", false},
		{"cat evil.sh > real/post2.sh; bash post.sh", false},
		{"bash alias.sh", false},
	} {
		_, ghDenied := githubPostJudgeText(c.cmd, cwd)
		delDenied := r.verdict(c.cmd).Deny
		if ghDenied != c.denied {
			t.Errorf("github guard %q: denied=%v, want %v", c.cmd, ghDenied, c.denied)
		}
		if delDenied != c.denied {
			t.Errorf("worktree guard %q: denied=%v, want %v", c.cmd, delDenied, c.denied)
		}
	}
}
