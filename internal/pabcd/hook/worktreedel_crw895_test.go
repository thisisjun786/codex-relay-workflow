package hook

import (
	"path/filepath"
	"strings"
	"testing"
)

// CRW-895: find -delete, find -exec/-execdir/-ok/-okdir and xargs deletions. Every shape the issue and the 10-09 re-check list is
// judged through the three public entry points (HandleWorktreeGuardPreTool, HandleMemoryWriteGate, HandleGitHubPostGuard);
// testdata/shellir/rows/18-crw-895.txt holds the same shapes as reproduction rows with a verdict per gate.

// crw895Shape is one command: deny is the worktree guard's verdict; refusedEverywhere marks a command the reader cannot prove, which
// the memory gate and the GitHub guard refuse too.
type crw895Shape struct {
	cmd               string
	deny              bool
	refusedEverywhere bool
}

func crw895Shapes() []crw895Shape {
	return []crw895Shape{
		// the issue: deny
		{"find ../repo -delete", true, false},
		{"find .. -maxdepth 0 -name repo -exec rm -rf {} +", true, false},
		{"echo ../repo | xargs rm -rf", true, false},
		{`find "$PWD" -exec rm -rf {} +`, true, true},
		{"find .. -delete", true, false},
		{`printf '%s\n' ../repo | xargs rm -rf`, true, false},
		{`echo "$X" | xargs rm`, true, false},
		// the issue: allow
		{"find . -name '*.o' -delete", false, false},
		{"find build -delete", false, false},
		{"find . -type f -name '*.tmp' -exec rm {} +", false, false},
		{"find . -name '*.o' | xargs rm", false, false},
		{"git ls-files -z | xargs -0 rm -f", false, false},
		{"git ls-files -z | xargs -0 rm", false, false},
		{"ls | xargs rm", false, false},
		// the 10-09 re-check
		{"find . -type f -delete", false, false},
		{"find . -newer x -delete", false, false},
		{"find ../repo -path '*/build/*' -delete", false, false},
		{"find .. -exec rmdir {} +", true, false},
		{"find ../repo -exec shred {} +", true, false},
		{`find ../repo -exec unlink {} \;`, true, false},
		{`find ../repo -exec git worktree remove {} \;`, true, false},
		{"echo ../repo | xargs git worktree remove", true, false},
		{"echo ../repo | xargs rm", true, false},
		{"echo ../repo | xargs rmdir", true, false},
		{"echo ../repo | xargs unlink", true, false},
		{"echo ../repo | xargs shred", true, false},
		// the independent verifier
		{`find ../repo -okdir rm -rf {} \;`, true, false},
		{`echo "$(cat list.txt)" | xargs rm`, true, false},
		{"echo ~ | xargs rm", true, false},
		{"echo ../repo | parallel git worktree remove", true, true},
		{"echo ../repo | parallel rmdir", true, true},
		{"echo ../repo | parallel unlink", true, true},
		{"echo ../repo | parallel shred", true, true},
		// the pre-merge evaluation of 3d1fe314: d1 a test that matches the start, d2 start points read at run time, d3 printf
		// output, d4 -I replacement, d5 filters that rewrite, d6 an action inside a group, d7 a shell that ignores the operands
		{"find ../repo -name '*' -delete", true, false},
		{"find ../repo -name 'r*' -delete", true, false},
		{"find ../repo -name '*.o' -delete", false, false},
		{"find -files0-from roots.list -type f -delete", true, false},
		{"printf '../%s\\n' repo | xargs git worktree remove --force", true, false},
		{"printf '../re\\160o\\n' | xargs git worktree remove --force", true, false},
		{"echo repo | xargs -I{} git worktree remove --force '../{}'", true, false},
		{"echo repo | xargs -I{} sh -c 'git worktree remove ../{}'", true, false},
		{"printf '../repoX' | head -c 7 | xargs git worktree remove --force", true, false},
		{"echo ../repo | grep -o '../repo' | xargs rmdir", true, false},
		{"find . -type f \\( -exec rm {} + \\)", false, false},
		{"echo ../repo | xargs sh -c 'rm build/old.o'", false, false},
		{"echo ../repo | xargs sh -c 'rm \"$@\"' _", true, false},
		// the second verification of ad1b3d605: find substitutes {} in the text of a shell, POSIX classes, a filter that reads a file
		{`find ../repo -exec sh -c 'git worktree remove --force {}' \;`, true, false},
		{`find .. -exec bash -c 'rm -rf {}' \;`, true, false},
		{`find ../repo -exec sh -c 'rmdir {}' \;`, true, false},
		{`find build -exec sh -c 'rm -rf {}' \;`, false, false},
		{`find . -name '*.o' -exec sh -c 'rm -f {}' \;`, false, false},
		{`find ../repo -exec sh -c 'rm build/old.o' \;`, false, false},
		{"find ../repo -name '[[:alpha:]]*' -delete", true, false},
		{"find ../repo -name '[[:print:]]*' -delete", true, false},
		{"find ../repo -name '[[:digit:]]*' -delete", false, false},
		{"echo ../other | cat roots.list | xargs git worktree remove --force", true, false},
		{"echo build/old.o | cat - | xargs rm", false, false},
		// the fix round after cf7844fce: a regular expression the reader cannot evaluate never guards; an absolute start point is
		// judged as itself wherever the removal's directory is
		{`find ../repo -regex '.*\(repo\|x\)' -delete`, true, false},
		{`find ../repo -regextype posix-extended -regex '.*(repo|x)' -delete`, true, false},
		{`find ../repo -iregex '.*\(REPO\|x\)' -delete`, true, false},
		{`find ../repo -regex '\.\./re\(po\)\?' -delete`, true, false},
		{`find .. -maxdepth 0 -regex '.*\(repo\|x\)' -exec rm -rf {} +`, true, false},
		{`find ../repo -maxdepth 0 -regex '.*\(repo\|x\)' -exec git worktree remove --force {} \;`, true, false},
		{`find . -regex '.*\.o' -delete`, false, false},
		{`find . -type f -regex '.*\(o\|a\)' -delete`, false, false},
		{`find . -name '*.o' -regex '.*\(o\|a\)' -delete`, false, false},
		{`find /tmp/build -name '*.o' -exec sh -c 'cd {}; rm x' \;`, false, false},
		{`find /tmp/build -type f -exec sh -c 'D={}; cd "$D"; rm file' \;`, false, false},
		{`find /tmp/build -type f -exec sh -c 'cd {}; rm {}.o' \;`, false, false},
		{`find /tmp/build -type f -exec sh -c 'cd {}/..; rm -f ./stale' \;`, false, false},
		{`find ../repo -exec sh -c 'cd {}; rm x' \;`, true, false},
		{`find .. -maxdepth 0 -exec sh -c 'cd {}; rm x' \;`, true, false},
		// the fix round after the verification of 9f189ad1: a find -regex whose syntax find and Go do not read the same (\' is the
		// end of the path to find and a quote to Go) never guards; start points are not re-read from the unknown directory of a
		// shell that names nothing find found; a directory prefix before {} keeps the word below that directory
		{`find ../repo -regex ".*\'" -delete`, true, false},
		{`find ../repo -regex ".*\'" -exec git worktree remove --force {} \;`, true, false},
		{`find ../repo -regex '.*po\>' -delete`, true, false},
		{`find ../repo -regex '\` + "`" + `.*' -delete`, true, false},
		{`find ../repo -regex '.*[\.]po' -delete`, true, false},
		{`find .. -maxdepth 0 -regex ".*\'" -exec rm -rf {} +`, true, false},
		{`find . -regex '.*/build/.*[0-9]' -delete`, false, false},
		{`find . -regex '.*[[:digit:]]' -delete`, false, false},
		{`find . -regex '^\./src/.*$' -delete`, false, false},
		{`find build -type d -exec sh -c 'cd {}; rm -f stale' \;`, false, false},
		{`find src -exec sh -c 'D={}; cd "$D"; rm x' \;`, false, false},
		{`find . -exec sh -c 'cd {}; rm x' \;`, true, false},
		{`find build -type d -exec sh -c 'cd $X; rm -rf {}' \;`, true, false},
		{`find . -maxdepth 1 -name '*.tmp' -exec rm -f build/{} \;`, false, false},
		{`find . -maxdepth 1 -name '*.tmp' -exec rm -f /tmp/stash/{} \;`, false, false},
		{`find . -name '*.tmp' -exec rm -rf ../{} \;`, true, false},
		{`find . -name x -exec rm -rf ../repo/{} \;`, true, false},
		{`find ../build -name x -exec rm -rf build/{} \;`, true, false},
		{`find . -name x -exec rm -rf {}/../.. \;`, true, false},
		// the fix round after the verification of 632401ae: an escaped literal is evaluated as the character it names, not taken for
		// an operator of a match-all regular expression
		{`find . -regex '.*\*' -delete`, false, false},
		{`find ../repo -regex '.*\.' -delete`, false, false},
		{`find . -regex '.*\$' -delete`, false, false},
		{`find . -regex '.*\^' -delete`, false, false},
		{`find . -regex '.*\\' -delete`, false, false},
		{`find .. -maxdepth 0 -regex '.*\.' -exec rm -rf {} +`, true, false},
		{`find ../repo -regex '.*' -delete`, true, false},
		{`find ../repo -regex '\(.*\)' -delete`, true, false},
	}
}

func TestCRW895ShapesThroughTheEntryPoints(t *testing.T) {
	githubPostTempHome(t)
	r := newDelRig(t)
	cwd, root, env := gateScene(t)
	_ = root
	for _, s := range crw895Shapes() {
		payload := wtPayload(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": r.checkout,
			"tool_input": map[string]any{"command": s.cmd}})
		got := HandleWorktreeGuardPreTool(payload, r.env())
		switch {
		case s.deny && !strings.Contains(got, "WORKTREE-GUARD-03"):
			t.Errorf("%q: the worktree guard answered %q, want a WORKTREE-GUARD-03 deny", s.cmd, got)
		case !s.deny && got != "":
			t.Errorf("%q: the worktree guard denied: %s", s.cmd, got)
		}
		gate := gatePayload(t, cwd, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": s.cmd}})
		mem := HandleMemoryWriteGate(gate, env)
		post := HandleGitHubPostGuard(wtPayload(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": cwd,
			"tool_input": map[string]any{"command": s.cmd}}))
		if s.refusedEverywhere {
			if mem == "" || post == "" {
				t.Errorf("%q: memory gate %q, GitHub guard %q; the reader cannot prove it, so both refuse", s.cmd, mem, post)
			}
			continue
		}
		if mem != "" || post != "" {
			t.Errorf("%q: memory gate %q, GitHub guard %q; neither has reason to touch it", s.cmd, mem, post)
		}
	}
}

// TestCRW895DenyTextIsTheWorktreeGuardWording: the verdict of a find or xargs removal is the WORKTREE-GUARD-03 text of a direct
// removal and names the shape.
func TestCRW895DenyTextIsTheWorktreeGuardWording(t *testing.T) {
	r := newDelRig(t)
	for cmd, what := range map[string]string{
		"find ../repo -delete":                     "find ../repo -delete",
		"find .. -exec rmdir {} +":                 "find .. -exec rmdir",
		"echo ../repo | xargs git worktree remove": "xargs git worktree remove ../repo",
		"xargs -a list.txt rm":                     "xargs rm",
	} {
		v := r.verdict(cmd)
		if !v.Deny || !strings.HasPrefix(v.Reason, "[crw: WORKTREE-GUARD-03] blocked `"+what) {
			t.Errorf("%q: %+v, want a WORKTREE-GUARD-03 deny naming %q", cmd, v, what)
		}
	}
}

// TestCRW895ShapesFromOtherDirectories: the start of a find and the names of xargs are taken from the directory the command runs
// in, so the same shape is judged from a subdirectory of the worktree, and an unknown directory refuses.
func TestCRW895ShapesFromOtherDirectories(t *testing.T) {
	r := newDelRig(t)
	id := r.id()
	sub := r.checkout + "/sub"
	wtWrite(t, sub+"/keep", "x")
	for cmd, deny := range map[string]bool{
		"find ../.. -name x -delete":          true,  // the slot root
		"find .. -delete":                     true,  // the checkout, which holds the directory the command runs in, no test
		"find .. -name x -delete":             false, // the same with a test
		"find . -delete":                      false, // CRW-1099: a safe checkout subdirectory is not a protected root
		"find . -name x -delete":              false, // a test
		"find sub -delete":                    false,
		"echo .. | xargs rm":                  true,
		"echo . | xargs rm":                   false,
		`cd "$X"; find build -name x -delete`: true,
	} {
		if got := evaluateCommand(cmd, sub, id); got.Deny != deny {
			t.Errorf("%q from %s: deny = %v, want %v (%s)", cmd, sub, got.Deny, deny, got.Reason)
		}
	}
}

// newDelRigNamed is newDelRig with the managed checkout under another name (Repo, 리포), for the name patterns whose case or locale
// decides whether find selects the checkout.
func newDelRigNamed(t *testing.T, name string) delRig {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := delRig{wtRig: wtRig{home: home, codexHome: filepath.Join(home, ".codex")}}
	r.worktrees = filepath.Join(r.codexHome, "worktrees")
	r.slotRoot = filepath.Join(r.worktrees, "zk3q")
	r.checkout = filepath.Join(r.slotRoot, name)
	r.other = filepath.Join(home, "elsewhere", "build")
	wtWrite(t, filepath.Join(r.checkout, ".git"), "gitdir: /fake/main/.git/worktrees/zk3q\n")
	wtWrite(t, filepath.Join(r.other, "keep"), "x")
	return r
}

// crw895GuardAnswer is the worktree guard's answer to a Bash command run in the rig's checkout, through the public entry point.
func crw895GuardAnswer(t *testing.T, r delRig, cwd, cmd string) string {
	t.Helper()
	payload := wtPayload(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": cwd,
		"tool_input": map[string]any{"command": cmd}})
	return HandleWorktreeGuardPreTool(payload, r.env())
}

// TestCRW895PlaceholderChangesTheShellContext (third verification of f0857f317): find substitutes {} anywhere in the text of a
// shell, so a {} that sets the directory (cd {}) or a variable the text uses later decides what every removal in the text names.
// The find's start points then judge each removal in the text, from the directory find runs in; a cd to a word holding {} leaves
// the directory unknown; a removal operand that puts text before {} or climbs out of it with .. names a path the start points do
// not bound. A shell that does not hold {} and ignores its parameters keeps its fixed cleanup (d7).
func TestCRW895PlaceholderChangesTheShellContext(t *testing.T) {
	r := newDelRig(t)
	wtWrite(t, filepath.Join(r.checkout, "a", "b", "keep"), "x")
	for cmd, deny := range map[string]bool{
		`find .. -maxdepth 0 -exec sh -c 'cd {}; rm -rf repo' \;`:                        true, // the verifier's trigger
		`find .. -maxdepth 0 -exec sh -c 'cd {} && git worktree remove --force repo' \;`: true,
		`find . -path ./a/b -exec sh -c 'cd {}; rm -rf ../../../repo' \;`:                true, // a guarded start, cd to a found path
		`find . -name b -exec sh -c 'D={}; cd "$D"; rm -rf ../../../repo' \;`:            true, // {} through a variable
		`find . -path ./a/b -exec sh -c 'cd {}/x; rmdir ../../../../repo' \;`:            true,
		`find . -path ./a/b -exec rm -rf {}/../../../repo \;`:                            true, // .. after {} climbs out of the found path
		`find . -name repo -exec rm -rf ../{} \;`:                                        true, // text before {} names another tree
		`find . -path ./a/b -exec git -C {}/../.. worktree remove --force ../repo \;`:    true,
		`find .. -name x -exec sh -c 'cd /tmp/a/b; rm -rf {}' \;`:                        true, // the start is judged where find runs
		// kept
		`echo ../repo | xargs sh -c 'rm build/old.o'`:            false, // d7
		`find ../repo -exec sh -c 'rm build/old.o' \;`:           false,
		`find . -name '*.o' -exec sh -c 'rm -f {}' \;`:           false,
		`find build -exec sh -c 'rm -rf {}' \;`:                  false,
		`find . -name '*.o' -exec sh -c 'cd build; rm -f {}' \;`: false,
		`find . -name '*.o' -exec rm -f {}.tmp \;`:               false,
		`find . -name '*.o' -exec rm -f ./{} \;`:                 false,
	} {
		got := crw895GuardAnswer(t, r, r.checkout, cmd)
		switch {
		case deny && !strings.Contains(got, "WORKTREE-GUARD-03"):
			t.Errorf("%q: the worktree guard answered %q, want a WORKTREE-GUARD-03 deny", cmd, got)
		case !deny && got != "":
			t.Errorf("%q: the worktree guard denied: %s", cmd, got)
		}
	}
}

// TestCRW895NamePatternCaseAndLocale (third verification of f0857f317): find -iname folds the case of letters but tests a POSIX
// class against the name as it is ([[:upper:]] selects Repo), and in a UTF-8 locale a class or ? takes a whole character ([[:alpha:]]
// selects 리포). A pattern the reader cannot evaluate in every locale is not a test that leaves the checkout out.
func TestCRW895NamePatternCaseAndLocale(t *testing.T) {
	for _, c := range []struct {
		name, cmd string
		deny      bool
	}{
		{"Repo", "find ../Repo -iname '[[:upper:]]*' -delete", true},
		{"Repo", "find ../Repo -iname 'repo' -delete", true},
		{"Repo", "find ../Repo -iname '[^[:lower:]]*' -delete", true},
		{"Repo", "find ../Repo -ipath '../[[:upper:]]*' -delete", true},
		{"Repo", "find ../Repo -iname '[[:digit:]]*' -delete", false},
		{"Repo", "find ../Repo -iname '*.o' -delete", false},
		{"리포", "find ../리포 -iname '[[:alpha:]]*' -delete", true},
		{"리포", "find ../리포 -name '[[:print:]]*' -delete", true},
		{"리포", "find ../리포 -name '??' -delete", true},
		{"리포", "find ../리포 -name '*.o' -delete", false},
		{"리포", "find ../리포 -name '리*' -delete", true},
		{"리포", "find ../리포 -name '*포' -delete", true},
		{"리포", "find ../리포 -name '나*' -delete", false},
		{"repo", "find ../repo -name '[[.r.]]*' -delete", true},
		{"repo", "find ../repo -name '[[=r=]]*' -delete", true},
	} {
		r := newDelRigNamed(t, c.name)
		got := crw895GuardAnswer(t, r, r.checkout, c.cmd)
		switch {
		case c.deny && !strings.Contains(got, "WORKTREE-GUARD-03"):
			t.Errorf("%q in %s: the worktree guard answered %q, want a WORKTREE-GUARD-03 deny", c.cmd, c.name, got)
		case !c.deny && got != "":
			t.Errorf("%q in %s: the worktree guard denied: %s", c.cmd, c.name, got)
		}
	}
}
