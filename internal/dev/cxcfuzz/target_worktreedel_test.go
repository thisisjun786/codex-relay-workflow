//go:build dev

package cxcfuzz

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The target is in the registry under its own name, so `crw-dev fuzz worktreedel` finds it, and its
// oracle is the shim beside its cases.
func TestWorktreeDelTargetIsRegistered(t *testing.T) {
	target, ok := Lookup("worktreedel")
	if !ok {
		t.Fatalf("worktreedel is not registered (registered: %v)", Names())
	}
	if target.Generate == nil || target.Go == nil || target.Compare == nil {
		t.Fatalf("the target is incomplete: %+v", target)
	}
	if target.Oracle.Command != "node" || !strings.HasSuffix(target.Oracle.Shim, filepath.Join("testdata", "worktreedel", "shim.mjs")) {
		t.Fatalf("the oracle is %+v", target.Oracle)
	}
}

// The comparison is the issue's: an oracle deny the Go side allows is a miss, a Go-only deny is an
// extra, and two denies whose reasons differ are a differ.
func TestWorktreeDelCompareClassifies(t *testing.T) {
	deny := func(reason string) pyjson.Object {
		return pyjson.Object{{Key: "decision", Value: "deny"}, {Key: "reason", Value: reason}}
	}
	allow := pyjson.Object{{Key: "decision", Value: "allow"}, {Key: "reason", Value: ""}}
	for _, c := range []struct {
		name      string
		goOut     pyjson.Object
		oracleOut pyjson.Object
		want      Kind
	}{
		{"both allow", allow, allow, Same},
		{"both deny alike", deny("x"), deny("x"), Same},
		{"oracle denies and go allows", allow, deny("x"), Miss},
		{"go denies and oracle allows", deny("x"), allow, Extra},
		{"both deny with different reasons", deny("x"), deny("y"), Differ},
	} {
		if got := worktreeDelCompare(c.goOut, c.oracleOut); got.Kind != c.want {
			t.Errorf("%s: %v, want %v", c.name, got.Kind, c.want)
		}
	}
}

// The Go side builds the payload from the case, calls the guard in a managed layout, and answers the
// decision and the reason: the checkout's removal is a deny, an unrelated one is an allow.
func TestWorktreeDelGoAnswersTheDecision(t *testing.T) {
	for _, c := range []struct {
		command string
		deny    bool
	}{
		{"rm -rf .", true},
		{"rm -rf /tmp/unrelated", false},
		{"git status", false},
	} {
		root := t.TempDir()
		if err := PrepareRoot(root); err != nil {
			t.Fatal(err)
		}
		input := worktreeDelCaseInput(c.command)
		if _, err := Scenarios(root, input); err != nil {
			t.Fatal(err)
		}
		value, err := worktreeDelGo(input, RootEnv(root))
		if err != nil {
			t.Fatalf("%q: %v", c.command, err)
		}
		decision, reason := worktreeDelDecision(value)
		if c.deny {
			if decision != "deny" || !strings.Contains(reason, "WORKTREE-GUARD-03") {
				t.Errorf("%q: %q %q, want a deny naming the guard", c.command, decision, reason)
			}
			continue
		}
		if decision != "allow" || reason != "" {
			t.Errorf("%q: %q %q, want an allow with no reason", c.command, decision, reason)
		}
	}
}

// worktreeDelCaseInput is one case in the grammar the target reads: the managed layout and a
// PreToolUse payload inside the checkout.
func worktreeDelCaseInput(command string) pyjson.Object {
	return pyjson.Object{
		{Key: "fs", Value: worktreeDelLayout()},
		{Key: "cwd", Value: "home/.codex/worktrees/" + worktreeDelSlot + "/" + worktreeDelRepo},
		{Key: "command", Value: command},
		{Key: "tool", Value: "Bash"},
		{Key: "event", Value: "PreToolUse"},
	}
}

// A case that points CODEX_HOME elsewhere is not inside a managed worktree, so the same command is
// allowed: the case's own environment is what the guard reads.
func TestWorktreeDelGoFollowsTheCaseEnvironment(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	input := worktreeDelCaseInput("rm -rf .").Set("env", pyjson.Object{{Key: "CODEX_HOME", Value: "home/elsewhere/codex"}})
	if _, err := Scenarios(root, input); err != nil {
		t.Fatal(err)
	}
	value, err := worktreeDelGo(input, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if decision, reason := worktreeDelDecision(value); decision != "allow" || reason != "" {
		t.Fatalf("a case outside the managed root answered %q %q, want an allow", decision, reason)
	}
}

// A case with no cwd is allowed: the oracle answers an empty cwd before it reads anything else, and
// the Go side must hand it an empty cwd rather than its own root.
func TestWorktreeDelGoKeepsAnAbsentCwd(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	input := worktreeDelCaseInput("rm -rf .").Set("cwd", "")
	if _, err := Scenarios(root, input); err != nil {
		t.Fatal(err)
	}
	value, err := worktreeDelGo(input, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if decision, reason := worktreeDelDecision(value); decision != "allow" || reason != "" {
		t.Fatalf("an absent cwd answered %q %q, want an allow", decision, reason)
	}
}

// A case that declares an extra worktree root is managed there too: the second root's checkout is a
// protected target, so a removal inside it is denied.
func TestWorktreeDelGoFollowsAnExtraWorktreeRoot(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	input := worktreeDelCaseInput("rm -rf .").
		Set("cwd", worktreeDelOtherRoot+"/"+worktreeDelOtherSlot+"/"+worktreeDelRepo).
		Set("worktree_roots", []any{worktreeDelOtherRoot})
	if _, err := Scenarios(root, input); err != nil {
		t.Fatal(err)
	}
	value, err := worktreeDelGo(input, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if decision, reason := worktreeDelDecision(value); decision != "deny" || !strings.Contains(reason, worktreeDelOtherSlot) {
		t.Fatalf("an extra root's checkout answered %q %q, want a deny naming its slot", decision, reason)
	}
}

// The generator is deterministic for one rng and builds the fields the target reads.
func TestWorktreeDelGenerateIsDeterministic(t *testing.T) {
	first := canonical(worktreeDelGenerate(rand.New(rand.NewSource(7)), 1))
	second := canonical(worktreeDelGenerate(rand.New(rand.NewSource(7)), 1))
	if first != second {
		t.Fatalf("the generator is not deterministic:\n%s\n%s", first, second)
	}
	value, err := decode(first)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := value.(pyjson.Object)
	if !ok {
		t.Fatalf("the generator answered %T", value)
	}
	for _, key := range []string{"fs", "cwd", "command", "tool", "event"} {
		if _, found := object.Lookup(key); !found {
			t.Errorf("the case has no %q: %s", key, first)
		}
	}
}

// A generated command is written as content and never executed, so the tree the case declares is
// still there after the guard has read a whole batch of commands.
func TestWorktreeDelGenerateNeverRunsItsCommand(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 100; i++ {
		root := t.TempDir()
		if err := PrepareRoot(root); err != nil {
			t.Fatal(err)
		}
		input := worktreeDelGenerate(rng, i)
		if _, err := Scenarios(root, input); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if _, err := worktreeDelGo(input, RootEnv(root)); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		marker := filepath.Join(root, "home", "elsewhere", "build", "keep")
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("case %d: the tree changed: %v", i, err)
		}
	}
}

// c2 (CRW-938): the continued-line command carries a real backslash before its line feed, so the
// shell joins r and m into rm and the guard judges the removal. Red before the fix: the command held
// a bare line feed, which cuts the command instead of joining it, so the removal was never seen.
func TestWorktreeDelContinuedLineJoinsTheRemoval(t *testing.T) {
	const command = "sh -c 'r\\\nm -rf .'"
	if !strings.Contains(command, "\\\n") {
		t.Fatal("the command does not carry a backslash before its line feed")
	}
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	input := worktreeDelCaseInput(command)
	if _, err := Scenarios(root, input); err != nil {
		t.Fatal(err)
	}
	value, err := worktreeDelGo(input, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if decision, reason := worktreeDelDecision(value); decision != "deny" || !strings.Contains(reason, "WORKTREE-GUARD-03") {
		t.Fatalf("the continued line answered %q %q, want a deny naming the guard", decision, reason)
	}
}

// c2 (CRW-938): the generator's command list and the pinned crw-585 case carry the same backslash,
// so the case that is replayed is the case the generator builds.
func TestWorktreeDelGeneratorAndPinnedCaseCarryTheBackslash(t *testing.T) {
	found := false
	for _, command := range worktreeDelCommands() {
		if strings.Contains(command, "r\\\nm -rf .") {
			found = true
		}
	}
	if !found {
		t.Fatal("the generator's commands hold no continued line with a backslash")
	}
	cases, err := LoadCases(filepath.Join("testdata", "worktreedel"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name != "crw-585-continued-line-inside-sh-c" {
			continue
		}
		if !strings.Contains(c.Input, "r\\\\\\nm -rf .") {
			t.Fatalf("the pinned case's input holds no backslash before its line feed: %s", c.Input)
		}
		return
	}
	t.Fatal("the pinned case crw-585-continued-line-inside-sh-c is missing")
}

// c4 (CRW-938): home/link-into-slot targets the managed checkout, so the link reaches the slot and
// a removal through it is judged as a removal inside the slot. Red before the fix: the target was
// written relative to the link's own directory, so it resolved to <root>/home/home/.codex/... and
// never reached the checkout.
func TestWorktreeDelLinkTargetsTheManagedCheckout(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	if _, err := Scenarios(root, worktreeDelCaseInput("rm -rf .")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "home", "link-into-slot")
	checkout := filepath.Join(root, "home", ".codex", "worktrees", worktreeDelSlot, worktreeDelRepo)
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("the link does not resolve: %v", err)
	}
	want, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != want {
		t.Fatalf("the link resolves to %q, want the checkout %q", resolved, want)
	}
}

// c4 (CRW-938): a removal whose cwd is the link into the slot is denied, because the link reaches
// the managed checkout.
func TestWorktreeDelDeniesThroughTheLink(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	input := worktreeDelCaseInput("rm -rf .").Set("cwd", "home/link-into-slot")
	if _, err := Scenarios(root, input); err != nil {
		t.Fatal(err)
	}
	value, err := worktreeDelGo(input, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if decision, reason := worktreeDelDecision(value); decision != "deny" || !strings.Contains(reason, worktreeDelSlot) {
		t.Fatalf("a removal through the link answered %q %q, want a deny naming the slot", decision, reason)
	}
}
