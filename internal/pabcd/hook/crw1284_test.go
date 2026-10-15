package hook

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
		"own.sh": "#!/bin/bash\ngit push origin HEAD:" + cpOwn + "\n", "p.py": "import subprocess\nsubprocess.run(['git', 'push', 'origin', 'HEAD:dev'])\n"} {
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
	"git push origin r2-f01 HEAD:dev", "git push origin r2-f01", "git push -u origin HEAD", "git push --delete origin r2-f01", "git push origin +r2-f01", "git push origin 'refs/heads/*:refs/heads/*'", "git push origin :", "git push origin HEAD:heads/dev",
	"git push", "git push origin", "git push -u origin", "git push --repo=origin HEAD:dev", "git push --receive-pack=x origin r2-f01",
	"git -C ../repo push origin HEAD", "cd /tmp && git push origin HEAD", "git push origin HEAD:refs/tags/v1", "git push -uf origin HEAD:dev",
	"nice git push origin HEAD:dev", "env X=1 git push origin HEAD:dev", "timeout 60 git push origin HEAD:dev", "/usr/bin/git push origin HEAD:dev",
	"ionice -c3 nice -n 5 git push origin HEAD:dev", "bash -c 'git push origin HEAD:dev'", "true && git push origin HEAD:dev", "echo dev | xargs git push origin",
	"bash w.sh git push origin r2-f01", "./w.sh git push origin r2-f01", "bash dev.sh", "./dev.sh", "sh dev.sh", "python3 p.py", "ssh h git push origin HEAD:dev",
	"python3 -c \"import subprocess; subprocess.run(['git','push','origin','HEAD:dev'])\"", "git push origin \"$BRANCH\"", "git push origin HEAD:$B", "eval \"$X\""}

var cpOwnForms = []string{"git push origin HEAD:r2-f01", "git push -u origin HEAD:r2-f01", "git push origin r2-f01:r2-f01", "git push --force-with-lease origin HEAD:r2-f01",
	"git push origin +HEAD:r2-f01", "git push origin HEAD:refs/heads/r2-f01", "git push upstream HEAD:r2-f01", "git push -o ci.skip origin HEAD:r2-f01",
	"git push -- origin HEAD:r2-f01", "nice git push origin HEAD:r2-f01", "bash own.sh", "./own.sh", "git push origin :r2-f01", "git push -uq origin HEAD:r2-f01",
	"cd /tmp && git push origin HEAD:r2-f01"}

// TestChildPushToIntegrationBranchRefused: in a relay child session every push to another branch than its own is refused, through a
// wrapper, a script and any remote, and a push the reader cannot read is refused (fail-closed).
func TestChildPushToIntegrationBranchRefused(t *testing.T) {
	dir, asked := cpScene(t), 0
	child := ChildPushBinding{Child: true, Own: cpOwn}
	for _, c := range cpRefused {
		if reason := gateDeny(t, cpAnswer(t, dir, c, child, &asked)); !strings.Contains(reason, "통합은 자식·통합기 몫") {
			t.Errorf("%s: %s", c, reason)
		}
	}
	for _, b := range []ChildPushBinding{{Child: true}, {Unreadable: "the relay store is locked"}} {
		gateDeny(t, cpAnswer(t, dir, "git push origin r2-f01", b, &asked))
	}
	// git branch -M heads/dev, then git push origin HEAD:heads/dev: git resolves the destination heads/dev on the remote as
	// refs/heads/dev, so the renamed checkout has no branch of its own.
	repo := filepath.Join(cleanTempDir(t), "child")
	for _, args := range [][]string{{"init", "-q", "-b", cpOwn, repo}, {"-C", repo, "commit", "-q", "--allow-empty", "-m", "x"}, {"-C", repo, "branch", "-M", "heads/dev"}} {
		if out, err := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	renamed := ChildPushBinding{Child: true, Own: ChildPushOwnBranch(context.Background(), repo, "")}
	if reason := gateDeny(t, cpAnswer(t, repo, "git push origin HEAD:heads/dev", renamed, &asked)); !strings.Contains(reason, "no branch of its own") {
		t.Errorf("git branch -M heads/dev, git push origin HEAD:heads/dev: %s", reason)
	}
	if asked != len(cpRefused)+3 {
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
	// While the relay's record cannot be read the session may be a child: a push, or a command the reader cannot read that spells
	// push, is refused in any session; a command the reader cannot read that spells no push passes.
	for c, refused := range map[string]bool{"git push origin HEAD:dev": true, "bash dev.sh": true, "./w.sh git push origin r2-f01": true,
		"eval \"$P\" push": true, "./w.sh go version": false, "bash w.sh go test ./...": false, "eval \"$X\"": false} {
		if out := cpAnswer(t, dir, c, ChildPushBinding{Unreadable: "the relay store is locked"}, &asked); (out != "") != refused {
			t.Errorf("relay record unreadable, %s: %q", c, out)
		}
	}
}

// TestChildPushOwnBranch: the own branch is the branch the child's checkout has, unless it is an integration branch (dev, main, the
// branch origin's HEAD names) or the 자식's checkout has it, in any letter case, or its name begins with refs/, heads/, tags/ or
// remotes/ (git resolves such a destination on the remote, heads/dev as refs/heads/dev); a detached or non-repository checkout has none.
func TestChildPushOwnBranch(t *testing.T) {
	dir := cleanTempDir(t)
	git := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	repo := map[string]string{}
	for i, b := range []string{"r2-x", "R2-X", "dev", "Main", "trunk", "r2-y", "heads/dev", "heads/Main", "heads/trunk", "heads/r2-x", "refs/heads/dev",
		"tags/dev", "remotes/origin/dev", "Heads/dev", "r2-tag"} {
		repo[b] = filepath.Join(dir, fmt.Sprintf("repo%d", i))
		git("init", "-q", "-b", b, repo[b])
	}
	for _, b := range []string{"trunk", "heads/trunk"} {
		git("-C", repo[b], "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	}
	git("-C", repo["r2-y"], "commit", "-q", "--allow-empty", "-m", "x")
	git("-C", repo["r2-y"], "checkout", "-q", "--detach")
	// a tag of the branch's name makes git's short name heads/r2-tag; the branch is r2-tag
	git("-C", repo["r2-tag"], "commit", "-q", "--allow-empty", "-m", "x")
	git("-C", repo["r2-tag"], "tag", "r2-tag")
	for _, c := range []struct{ cwd, parent, want string }{{repo["r2-x"], "", "r2-x"}, {repo["r2-x"], repo["dev"], "r2-x"},
		{repo["r2-x"], filepath.Join(dir, "none"), "r2-x"}, {repo["r2-x"], repo["R2-X"], ""}, {repo["dev"], "", ""}, {repo["Main"], repo["r2-x"], ""},
		{repo["trunk"], "", ""}, {repo["r2-y"], "", ""}, {dir, "", ""}, {repo["heads/dev"], "", ""}, {repo["heads/Main"], "", ""},
		{repo["heads/trunk"], "", ""}, {repo["heads/r2-x"], repo["r2-x"], ""}, {repo["refs/heads/dev"], "", ""}, {repo["tags/dev"], "", ""},
		{repo["remotes/origin/dev"], "", ""}, {repo["Heads/dev"], "", ""}, {repo["r2-tag"], "", "r2-tag"}} {
		if got := ChildPushOwnBranch(context.Background(), c.cwd, c.parent); got != c.want {
			t.Errorf("checkout %s, parent %q: own %q, want %q", filepath.Base(c.cwd), c.parent, got, c.want)
		}
	}
}

// TestCRW1284ChildPushFuzz composes the CRW-1283 corpus (wrappers, program wrappers, PATH assignments, scripts, mutated forwarders,
// program files, depth-2 pairs) around push payloads: refspec variants, force, --all, --mirror, another remote, and the own branch.
// In a relay child session every form carrying a push of another branch is refused (a script run by its path is read, so class
// ownlimit is a target here); in any other session every form is allowed, as at the base, which has no such leg.
func TestCRW1284ChildPushFuzz(t *testing.T) {
	t.Setenv("HOME", cleanTempDir(t))
	t.Setenv("PATH", "/usr/bin:/bin")
	dir := cpScene(t)
	child := func(string, string) ChildPushBinding { return ChildPushBinding{Child: true, Own: cpOwn} }
	g := &fzGuard{name: "childpush", dir: dir, t: t, refused: func(c string) bool { return HandleChildPushGuard(gateBash(t, dir, c), child) != "" },
		payloads: []fzPayload{{"head-dev", "git push origin HEAD:dev", "git", true}, {"force", "git push -f origin +r2-f01:dev", "git", true},
			{"all", "git push --all origin", "git", true}, {"mirror", "git push --mirror origin", "git", true}, {"remote", "git push upstream HEAD:main", "git", true},
			{"own", "git push origin HEAD:r2-f01", "git", false}, {"go-version", "go version", "go", false}, {"true", "true", "true", false}}}
	g.setup()
	all, counts := g.forms()
	stats := map[string]int{}
	for _, f := range all {
		refused := g.refused(f.cmd)
		stats[f.class+"/"+map[bool]string{true: "refuse", false: "allow"}[refused]]++
		if (f.class == "target" || f.class == "ownlimit") && !refused {
			t.Errorf("a relay child is ALLOWED %s (%s): %s", f.key, f.desc, f.cmd)
		}
		if out := HandleChildPushGuard(gateBash(t, dir, f.cmd), func(string, string) ChildPushBinding { return ChildPushBinding{} }); out != "" {
			t.Errorf("a session that is no relay child is judged %s: %s", f.key, out)
		}
	}
	t.Logf("corpus %v; decisions %v", counts, stats)
	if len(all) < 7640 || stats["target/allow"]+stats["ownlimit/allow"] != 0 {
		t.Errorf("%d forms, at least 7640 pinned", len(all))
	}
	for _, c := range []string{"git push origin HEAD:r2-f01", "ionice -c3 nice -n 5 git push origin HEAD:r2-f01", "bash own.sh", "env X=1 go version"} {
		if g.refused(c) {
			t.Errorf("a relay child is refused its own branch or a command without a push: %s", c)
		}
	}
}
