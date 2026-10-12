package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The relay child push guard (CRW-1284). r2 S8 F0-1: a relay child ran `git push origin HEAD:dev` and moved the integration branch.

const cpOwn = "r2-f01"

// cpScene is a child's workspace: a "$@" forwarder, scripts that push dev or the own branch, and a Python program that pushes dev.
func cpScene(t *testing.T) string {
	t.Helper()
	dir := cleanTempDir(t)
	for name, body := range map[string]string{"w.sh": "#!/bin/bash\nexec nice \"$@\"\n", "dev.sh": "#!/bin/bash\ngit push origin HEAD:dev\n",
		"own.sh": "#!/bin/bash\ngit push origin " + cpOwn + "\n", "p.py": "import subprocess\nsubprocess.run(['git', 'push', 'origin', 'HEAD:dev'])\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// cpAnswer is the guard's answer for a Bash command in cwd under binding b; asked counts the binding reads.
func cpAnswer(t *testing.T, cwd, command string, b ChildPushBinding, asked *int) string {
	t.Helper()
	return HandleChildPushGuard(gateBash(t, cwd, command), func(session, at string) ChildPushBinding {
		if session != gateSession || at != cwd {
			t.Errorf("binding asked for %q in %q", session, at)
		}
		*asked++
		return b
	})
}

var cpRefused = []string{"git push origin HEAD:dev", "git push origin r2-f01:dev", "git push origin r2-f01:refs/heads/dev", "git push origin +HEAD:dev",
	"git push -f origin HEAD:dev", "git push --force origin HEAD:main", "git push origin dev", "git push origin :dev", "git push --delete origin dev",
	"git push --all origin", "git push --mirror origin", "git push --tags origin", "git push upstream HEAD:dev", "git push ../origin.git HEAD:dev",
	"git push origin r2-f01 HEAD:dev", "git push origin 'refs/heads/*:refs/heads/*'", "git push origin :", "git push origin HEAD:heads/dev",
	"git push", "git push origin", "git push -u origin", "git push --repo=origin HEAD:dev", "git push --receive-pack=x origin r2-f01",
	"git -C ../repo push origin HEAD", "cd /tmp && git push origin HEAD", "git push origin HEAD:refs/tags/v1", "git push -uf origin HEAD:dev",
	"nice git push origin HEAD:dev", "env X=1 git push origin HEAD:dev", "timeout 60 git push origin HEAD:dev", "/usr/bin/git push origin HEAD:dev",
	"ionice -c3 nice -n 5 git push origin HEAD:dev", "bash -c 'git push origin HEAD:dev'", "true && git push origin HEAD:dev", "echo dev | xargs git push origin",
	"bash w.sh git push origin r2-f01", "./w.sh git push origin r2-f01", "bash dev.sh", "./dev.sh", "sh dev.sh", "python3 p.py", "ssh h git push origin HEAD:dev",
	"python3 -c \"import subprocess; subprocess.run(['git','push','origin','HEAD:dev'])\"", "git push origin \"$BRANCH\"", "git push origin HEAD:$B", "eval \"$X\""}

var cpOwnForms = []string{"git push origin r2-f01", "git push -u origin HEAD", "git push origin HEAD:r2-f01", "git push --force-with-lease origin r2-f01",
	"git push origin +r2-f01", "git push origin HEAD:refs/heads/r2-f01", "git push upstream r2-f01", "git push -o ci.skip origin r2-f01", "git push -- origin r2-f01",
	"nice git push origin r2-f01", "bash own.sh", "./own.sh", "git push --delete origin r2-f01", "git push -uq origin HEAD"}

// TestChildPushToIntegrationBranchRefused: in a relay child session every push to another branch than its own is refused, through a
// wrapper, a script and any remote, and a push the reader cannot read is refused (fail-closed).
func TestChildPushToIntegrationBranchRefused(t *testing.T) {
	dir, asked := cpScene(t), 0
	child := ChildPushBinding{Child: true, Own: cpOwn}
	for _, c := range cpRefused {
		if reason := gateDeny(t, cpAnswer(t, dir, c, child, &asked)); !strings.Contains(reason, "통합은 부모·통합기 몫") {
			t.Errorf("%s: %s", c, reason)
		}
	}
	for _, b := range []ChildPushBinding{{Child: true}, {Unreadable: "the relay store is locked"}} {
		gateDeny(t, cpAnswer(t, dir, "git push origin r2-f01", b, &asked))
	}
	if asked != len(cpRefused)+2 {
		t.Errorf("the binding was read %d times", asked)
	}
}

// TestChildPushOwnBranchAllowed: a relay child pushes its own branch, and a command that pushes nothing reads no binding.
func TestChildPushOwnBranchAllowed(t *testing.T) {
	dir, asked := cpScene(t), 0
	child := ChildPushBinding{Child: true, Own: cpOwn}
	for _, c := range cpOwnForms {
		if out := cpAnswer(t, dir, c, child, &asked); out != "" {
			t.Errorf("%s: %s", c, out)
		}
	}
	for _, c := range []string{"go test ./...", "git status", "git log --oneline -3", "ionice -c3 nice go version", "git fetch origin dev"} {
		if out := cpAnswer(t, dir, c, child, &asked); out != "" {
			t.Errorf("%s: %s", c, out)
		}
	}
	if asked != len(cpOwnForms) {
		t.Errorf("the binding was read %d times", asked)
	}
}

// TestParentAndIntegratorPushUnaffected: a session the relay does not know as a child (a parent, an integrator) pushes as before.
func TestParentAndIntegratorPushUnaffected(t *testing.T) {
	dir, asked := cpScene(t), 0
	for _, c := range append(append([]string{}, cpRefused...), cpOwnForms...) {
		if out := cpAnswer(t, dir, c, ChildPushBinding{}, &asked); out != "" {
			t.Errorf("%s: %s", c, out)
		}
	}
	for _, p := range []string{`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"git push origin HEAD:dev"}}`,
		`{"hook_event_name":"PreToolUse","tool_name":"apply_patch","tool_input":{"command":"git push origin HEAD:dev"}}`} {
		if out := HandleChildPushGuard(p, func(string, string) ChildPushBinding { return ChildPushBinding{Child: true} }); out != "" {
			t.Errorf("%s: %s", p, out)
		}
	}
}
