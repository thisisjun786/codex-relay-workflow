package hook

import (
	"strings"
	"testing"
)

// CRW-1307: the guards' refusals warn, except the three irreversible rules. These tests pin the rule each guard names for its
// refusal: the named rule is what the mode table (internal/harness/guardmode.go) keeps at block, and a refusal without it warns.

// TestCRW1307ChildForceDeleteMirrorOntoSharedBranchIsTheBlockRule: a relay child's force, delete or --mirror push onto dev or main
// is the block rule; every other refusal of the child push guard names none.
func TestCRW1307ChildForceDeleteMirrorOntoSharedBranchIsTheBlockRule(t *testing.T) {
	dir := cpScene(t)
	child := ChildPushBinding{Child: true, Own: cpOwn}
	decide := func(command string, b ChildPushBinding) (string, string) {
		return ChildPushDecision(gateBash(t, dir, command), func(string, string) ChildPushBinding { return b })
	}
	for _, c := range []string{"git push -f origin HEAD:dev", "git push --force origin HEAD:main", "git push --delete origin dev", "git push origin :dev",
		"git push origin +HEAD:dev", "git push --mirror origin", "git push -uf origin HEAD:dev", "git push --force-with-lease origin HEAD:refs/heads/main",
		"git push -d origin main", "git push origin +r2-f01:DEV", "nice git push -f origin HEAD:dev", "git push origin HEAD:r2-f01 && git push -f origin HEAD:dev",
		"bash -c 'git push --force origin HEAD:main'", "git push --force origin HEAD:heads/dev"} {
		answer, rule := decide(c, child)
		if rule != ChildPushRuleSharedHistory || !strings.Contains(gateDeny(t, answer), "통합은 부모·통합기 몫") {
			t.Errorf("%s: rule %q answer %q", c, rule, answer)
		}
	}
	// A child with no own branch is refused the same way.
	if _, rule := decide("git push -f origin HEAD:dev", ChildPushBinding{Child: true}); rule != ChildPushRuleSharedHistory {
		t.Errorf("no own branch: rule %q", rule)
	}
	// The rest of what the guard refuses warns: a plain dev push, no refspec, a pattern, --all and --tags, a force onto another
	// branch, a word the reader cannot read, an unreadable relay record.
	for _, c := range []string{"git push origin HEAD:dev", "git push origin dev", "git push", "git push origin", "git push --all origin", "git push --tags origin",
		"git push origin 'refs/heads/*:refs/heads/*'", "git push -f origin HEAD:other", "git push --delete origin r2-f01x", "git push -f origin \"$B\"",
		"eval \"$X\"", "python3 p.py", "bash dev.sh"} {
		answer, rule := decide(c, child)
		if rule != "" || answer == "" {
			t.Errorf("%s: rule %q answer %q", c, rule, answer)
		}
	}
	if answer, rule := decide("git push -f origin HEAD:dev", ChildPushBinding{Unreadable: "the relay store is locked"}); rule != "" || answer == "" {
		t.Errorf("unreadable record: rule %q answer %q", rule, answer)
	}
	// A session that is no child is not judged at all.
	if answer, rule := decide("git push -f origin HEAD:dev", ChildPushBinding{}); answer != "" || rule != "" {
		t.Errorf("parent: %q %q", answer, rule)
	}
}

// TestCRW1307SecretInAnInlineGitHubBodyIsTheBlockRule: an inline body warns now, so its text is scanned; a secret in it, in a
// title, or in a gh api field is the block rule, also when a refusal that warns comes first in the same text.
func TestCRW1307SecretInAnInlineGitHubBodyIsTheBlockRule(t *testing.T) {
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "clean.md", "a clean body\n")
	secret := githubPostFake("sk-", 16)
	for _, c := range []struct{ command, place string }{
		{"gh pr comment 1 --body " + secret, githubPostWhereCommand},
		{"gh pr comment 1 -b 'MY_API_KEY=sentinel'", githubPostWhereCommand},
		{"gh issue create --title x --body=" + secret, githubPostWhereCommand},
		{"gh pr create -t " + secret + " --body x", githubPostWhereTitle},
		{"gh api repos/o/r/issues/1/comments -f body=" + secret, githubPostWhereCommand},
		{"gh api repos/o/r/issues/1/comments -F body='MY_API_KEY=sentinel'", githubPostWhereCommand},
		{"gh api repos/o/r/issues/1/comments --raw-field=body=" + secret, githubPostWhereCommand},
		{"gh pr comment 1 --body hi; gh pr comment 2 -b " + secret, githubPostWhereCommand},
		{"gh pr comment 1 --body " + secret + " --unknown-flag", githubPostWhereCommand},
		{"bash -c 'gh pr comment 1 -b " + secret + "'", githubPostWhereCommand},
	} {
		raw := githubPostPayload(t, "Bash", cwd, map[string]any{"command": c.command})
		answer, rule := GitHubPostDecision(raw, false)
		if rule != GitHubPostRuleSecret {
			t.Errorf("%s: rule %q answer %q", c.command, rule, answer)
			continue
		}
		githubPostWant(t, raw, c.command, githubPostRuleSecret, c.place)
		if strings.Contains(answer, secret) || strings.Contains(answer, "sentinel") {
			t.Errorf("%s: the answer carries the value", c.command)
		}
	}
	// An inline body without a secret, and the other refusals, name a rule the table does not keep: they warn.
	for _, c := range []string{"gh pr comment 1 --body 'a b'", "gh api repos/o/r/issues/1/comments -f body=hello", "gh alias set x y",
		"gh pr comment 1 --body-file \"$F\"", "eval \"$X\"", "gh pr comment 1 --body-file -"} {
		answer, rule := GitHubPostDecision(githubPostPayload(t, "Bash", cwd, map[string]any{"command": c}), false)
		if answer == "" || rule == GitHubPostRuleSecret || rule == "" {
			t.Errorf("%s: rule %q answer %q", c, rule, answer)
		}
	}
	// An input over the bound is the unreadable rule.
	if _, rule := GitHubPostDecision("", true); rule != githubPostRuleUnread {
		t.Errorf("over the bound: %q", rule)
	}
}

// TestCRW1307ResolvedWorktreeDeletionIsTheBlockRule: a deletion whose target resolves to the managed checkout, its slot or an
// ancestor is the block rule, also after a refusal that warns; rmdir, mv, an inline program's words and what the guard cannot read
// warn.
func TestCRW1307ResolvedWorktreeDeletionIsTheBlockRule(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []string{"rm -rf .", "rm -r " + r.checkout, "rm -rf " + r.slotRoot, "git worktree remove --force .", "git worktree remove " + r.checkout,
		"find . -delete", "find " + r.slotRoot + " -delete", "find . -exec rm -rf {} +", "find " + r.checkout + " -exec rm -rf {} +", "echo " + r.checkout + " | xargs rm -rf",
		"mv . /tmp/x; rm -rf .", "rmdir " + r.checkout + "; rm -rf " + r.checkout, "python3 -c 'import shutil' ; rm -rf ."} {
		if v := r.verdict(c); !v.Deny || v.Rule != WorktreeRuleResolvedDeletion {
			t.Errorf("%q: %+v", c, v)
		}
	}
	for _, c := range []string{"rmdir " + r.checkout, "mv " + r.checkout + " /tmp/x", "rm -rf \"$UNKNOWN\"", "cd \"$UNKNOWN\"; rm -rf .",
		"echo . | xargs rm -rf", "find . -name zk3q | xargs rm -rf", "xargs rm -rf ./build",
		"python3 -c 'import shutil; shutil.rmtree(\".\")'", "find -L . -delete"} {
		if v := r.verdict(c); !v.Deny || v.Rule != "" {
			t.Errorf("%q: %+v", c, v)
		}
	}
	answer, rule := WorktreeGuardPreToolDecision(gateBash(t, r.checkout, "rm -rf ."), r.env())
	if rule != WorktreeRuleResolvedDeletion || !strings.Contains(gateDeny(t, answer), "WORKTREE-GUARD-03") {
		t.Fatalf("leg: %q %q", answer, rule)
	}
}
