package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRewrittenScriptIsTrackedThroughNestedScripts: what a text writes is written before every script it runs, however deep. A
// script that another script runs (runner.sh runs post.sh), a script that runs a copy in a directory, and a script file below a
// copied directory are all the files the text replaced, so the body the guards read before the command is stale. The GitHub post
// guard and the worktree deletion guard answer alike (CRW-1028 verifier round 2, findings 3, 4 and 5).
func TestRewrittenScriptIsTrackedThroughNestedScripts(t *testing.T) {
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
		write("runner.sh", "bash post.sh\n")
		write("runner2.sh", "bash runner.sh\n")
		write("copyrunner.sh", "cp evil.sh post.sh; bash post.sh\n")
		write("dirrunner.sh", "bash bin/tool\n")
		write("bin/tool", "echo clean\n")
		write("evil/tool", "rm -rf "+r.checkout+"\ngh pr comment 1 -b x\n")
	}
	for _, c := range []struct {
		cmd    string
		denied bool
	}{
		{"cat evil.sh > post.sh; bash runner.sh", true},
		{"cat evil.sh > post.sh; bash runner2.sh", true},
		{"cat evil.sh > runner.sh; bash runner2.sh", true},
		{"bash copyrunner.sh", true},
		{"cp evil/tool bin; bash dirrunner.sh", true},
		{"cp -rT evil bin; bash dirrunner.sh", true},
		{"cp -rT evil bin; source bin/tool", true},
		{"cp evil/tool bin; bash bin/tool", true},
		{"cp -r evil bin; bash bin/tool", true},
		{"cat evil.sh >&post.sh; bash runner.sh", true},
		{"cat evil.sh >&post.sh; bash post.sh", true},
		{"cat evil.sh > other.sh; bash runner.sh", false},
		{"cat evil.sh > other.sh; bash runner2.sh", false},
		{"cat evil.sh >&other.sh; bash runner2.sh", false},
		{"cp evil/tool other; bash dirrunner.sh", false},
		{"bash runner2.sh", false},
		{"bash dirrunner.sh", false},
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
