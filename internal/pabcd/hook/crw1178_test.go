package hook

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// crw1178Cache lays out a two-file project with a __pycache__ holding stale timestamp entries of both sources (recorded time and
// size zero), the way an earlier run leaves it once the sources were edited: the interpreter discards them and runs the sources.
func crw1178Cache(t *testing.T, cwd string) {
	t.Helper()
	header := make([]byte, 16)
	copy(header, []byte{0xcb, 0x0d, 0x0d, 0x0a})
	binary.LittleEndian.PutUint32(header[4:], 0)
	if err := os.MkdirAll(filepath.Join(cwd, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"calc", "test_calc"} {
		if err := os.WriteFile(filepath.Join(cwd, m+".py"), []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cwd, "__pycache__", m+".cpython-312.pyc"), append(header, "code"...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// CRW-1178 (S2R2-F1): a routine test run is not refused by either guard because an earlier run left a stale __pycache__. A cache
// that still agrees with its source is refused (TestCRW1178MatchingCacheRunsUnreadCode).
func TestCRW1178RepeatedTestRunIsNotRefused(t *testing.T) {
	cwd, _, env := gateScene(t)
	crw1178Cache(t, cwd)
	for _, cmd := range []string{
		"python3 -m unittest",
		"python3 -B -m unittest",
		"PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
		"env PYTHONDONTWRITEBYTECODE=1 python3 -m unittest",
		"python3 -m pytest",
	} {
		if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env); out != "" {
			t.Errorf("memory gate refused %q beside a cache of readable sources: %s", cmd, out)
		}
		if out := HandleGitHubPostGuard(gateBash(t, cwd, cmd)); out != "" {
			t.Errorf("GitHub guard refused %q beside a cache of readable sources: %s", cmd, out)
		}
	}
}

// The decision for compiled code with no source stays a deny in both guards, and the reason names the file and the cause.
func TestCRW1178SourcelessBytecodeStaysRefusedWithItsCause(t *testing.T) {
	cwd, root, env := gateScene(t)
	if err := os.WriteFile(filepath.Join(cwd, "hidden.pyc"), make([]byte, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := "python3 -m unittest"
	reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
	for _, want := range []string{"unreadable-program", "hidden.pyc", "compiled code"} {
		if !strings.Contains(reason, want) {
			t.Errorf("memory reason lacks %q: %s", want, reason)
		}
	}
	for _, bad := range []string{"MEMORY-WRITE-GATE", "protected write has not been established", "allow-write", "ask the user"} {
		if strings.Contains(reason, bad) {
			t.Errorf("an unreadable command reads as a protected write (%q): %s", bad, reason)
		}
	}
	if len(reason) > 700 {
		t.Errorf("reason over the bound: %d", len(reason))
	}
	gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd)))
	for _, want := range []string{"unreadable-github-post", "hidden.pyc", "compiled code", "names no GitHub post"} {
		if !strings.Contains(gh, want) {
			t.Errorf("GitHub reason lacks %q: %s", want, gh)
		}
	}
	for _, bad := range []string{"GitHub posting has not been established", "GitHub post blocked", "--body-file"} {
		if strings.Contains(gh, bad) {
			t.Errorf("a non-GitHub command reads as a GitHub post refusal (%q): %s", bad, gh)
		}
	}
	// A leaf is sent to its parent and keeps the cause.
	leaf := gatePayload(t, cwd, map[string]any{"agent_id": "leaf", "agent_type": "executor", "tool_name": "Bash", "tool_input": map[string]any{"command": cmd}})
	reason = gateDeny(t, HandleMemoryWriteGate(leaf, env))
	if !strings.Contains(reason, "parent") || !strings.Contains(reason, "hidden.pyc") || strings.Contains(reason, "MEMORY-WRITE-GATE") {
		t.Errorf("leaf reason: %s", reason)
	}
	// A grant still spends on the unreadable program, as before; the decision is unchanged.
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env); out != "" {
		t.Errorf("a grant must still let it pass: %s", out)
	}
	_ = root
}

// A command that does name a GitHub post keeps the GitHub wording beside the cause.
func TestCRW1178UnreadableGithubPostKeepsPostWording(t *testing.T) {
	cwd, _, _ := gateScene(t)
	gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, `"$FOO" a; gh pr comment 1 --body-file x`)))
	if !strings.Contains(gh, "unreadable-github-post") || strings.Contains(gh, "names no GitHub post") {
		t.Errorf("a command that names a post: %s", gh)
	}
}

// CRW-1178 (S2R2-F5): with no usable cwd the grant can never be consumed, so the refusal does not offer it.
func TestCRW1178NoGrantRouteWithoutCwd(t *testing.T) {
	cwd, root, env := gateScene(t)
	edit := func(cwd, file string) string {
		return gatePayload(t, cwd, map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": file, "content": "x"}})
	}
	for name, raw := range map[string]string{
		"relative path":   edit("", "notes/x.txt"),
		"memory path":     edit("", filepath.Join(root, "n.md")),
		"memory note":     gatePayload(t, "", nil),
		"no session id":   gatePayload(t, cwd, map[string]any{"session_id": ""}),
		"relative cwd":    edit(".", "notes/x.txt"),
		"relative memory": edit("rel", filepath.Join(root, "n.md")),
	} {
		reason := gateDeny(t, HandleMemoryWriteGate(raw, env))
		if strings.Contains(reason, "allow-write") || strings.Contains(reason, "session working directory") {
			t.Errorf("%s offers a grant that cannot be spent: %s", name, reason)
		}
		if !strings.Contains(reason, "MEMORY-WRITE-GATE") {
			t.Errorf("%s: %s", name, reason)
		}
	}
	// The unknown destination of an edit still says to use an absolute path.
	if r := gateDeny(t, HandleMemoryWriteGate(edit("", "notes/x.txt"), env)); !strings.Contains(r, "unknown-destination") || !strings.Contains(r, "absolute path") {
		t.Errorf("unknown destination: %s", r)
	}
	// With an absolute cwd the grant route stays, and it is spent.
	if r := gateDeny(t, HandleMemoryWriteGate(edit(cwd, filepath.Join(root, "n.md")), env)); !strings.Contains(r, "allow-write --session "+gateSession) {
		t.Errorf("an absolute cwd must keep the grant route: %s", r)
	}
}

// A freeform apply_patch carries its patch text as a bare string; it goes through the same judgement as the patch in a record.
func TestCRW1178FreeformApplyPatchIsJudged(t *testing.T) {
	cwd, root, env := gateScene(t)
	patch := func(target string) string {
		return "*** Begin Patch\n*** Add File: " + target + "\n+x\n*** End Patch\n"
	}
	freeform := func(cwd, target string) string {
		return gatePayload(t, cwd, map[string]any{"tool_name": "apply_patch", "tool_input": patch(target)})
	}
	record := func(cwd, target string) string {
		return gatePayload(t, cwd, map[string]any{"tool_name": "apply_patch", "tool_input": map[string]any{"command": patch(target)}})
	}
	for _, c := range []struct{ cwd, target string }{{cwd, filepath.Join(root, "n.md")}, {"", filepath.Join(root, "n.md")}, {"", "notes/x.txt"}} {
		want := HandleMemoryWriteGate(record(c.cwd, c.target), env) != ""
		got := HandleMemoryWriteGate(freeform(c.cwd, c.target), env) != ""
		if !want || got != want {
			t.Errorf("freeform patch of %s from cwd %q: denied %v, the record form is denied %v", c.target, c.cwd, got, want)
		}
	}
	if out := HandleMemoryWriteGate(freeform(cwd, filepath.Join(cwd, "ok.txt")), env); out != "" {
		t.Errorf("a patch outside the memories root must pass: %s", out)
	}
	if out := HandleMemoryWriteGate(freeform(cwd, "ok.txt"), env); out != "" {
		t.Errorf("a relative patch under a usable cwd must pass: %s", out)
	}
}

// githubReaderWording is whether a GitHub guard answer is the command reader's refusal (it says what could not be read and that
// the text names no post) and not a policy refusal about GitHub posting.
func crw1178ReaderWording(t *testing.T, reason string) {
	t.Helper()
	if !strings.Contains(reason, "[crw command-reader]") || !strings.Contains(reason, "names no GitHub post") {
		t.Errorf("not the command reader's wording: %s", reason)
	}
	for _, bad := range []string{"GitHub posting has not been established", "GitHub post blocked", "--body-file"} {
		if strings.Contains(reason, bad) {
			t.Errorf("a non-GitHub command reads as a GitHub post refusal (%q): %s", bad, reason)
		}
	}
}

// A path or argument that merely contains the letters gh, pr or issue is no mention of a GitHub post.
func TestCRW1178PathSubstringsAreNoGithubPost(t *testing.T) {
	cwd, _, _ := gateScene(t)
	if err := os.WriteFile(filepath.Join(cwd, "hidden.pyc"), make([]byte, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"python3 -m unittest tests/high_priority",
		"python3 -m unittest tests.test_high_priority_review",
		"python3 -m unittest discover -s thoughts -p 'test_pr_*.py'",
	} {
		crw1178ReaderWording(t, githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd))))
	}
}

// A script file that cannot be read is the reader's refusal when the command names no post, with the file and the cause.
func TestCRW1178UnreadableScriptFileIsReaderWording(t *testing.T) {
	cwd, _, _ := gateScene(t)
	if err := os.WriteFile(filepath.Join(cwd, "noperm.sh"), []byte("echo hi\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ cmd, file string }{
		{"bash missing.sh", "missing.sh"},
		{"sh ./missing.sh", "missing.sh"},
		{"./missing.sh", "missing.sh"},
		{"bash noperm.sh", "noperm.sh"},
	} {
		if os.Geteuid() == 0 && c.file == "noperm.sh" {
			continue
		}
		reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, c.cmd)))
		if reason == "" {
			continue // a file that is absent and run by path runs nothing: allowed
		}
		crw1178ReaderWording(t, reason)
		if !strings.Contains(reason, c.file) {
			t.Errorf("%q: the reason lacks the file: %s", c.cmd, reason)
		}
	}
	// The same refusal beside a spelled gh post keeps the GitHub wording.
	reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "bash missing.sh; gh pr comment 1 --body-file x")))
	if strings.Contains(reason, "names no GitHub post") {
		t.Errorf("a command that spells a post: %s", reason)
	}
}

// A refusal inside a script is judged by the text of that script, not by the command that runs it.
func TestCRW1178NestedScriptMentionIsOfItsOwnText(t *testing.T) {
	cwd, _, _ := gateScene(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("post.sh", "\"$FOO\" a\ngh pr comment 1 --body-file body.md\n")
	write("plain.sh", "\"$FOO\" a\necho done\n")
	post := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "bash post.sh")))
	if post == "" || strings.Contains(post, "names no GitHub post") {
		t.Errorf("a script that spells a post lost the GitHub wording: %s", post)
	}
	plain := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "bash plain.sh")))
	crw1178ReaderWording(t, plain)
	// A script that runs a script that spells a post, from a command that does not.
	write("outer.sh", "bash post.sh\n")
	outer := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "bash outer.sh")))
	if outer == "" || strings.Contains(outer, "names no GitHub post") {
		t.Errorf("a nested script that spells a post lost the GitHub wording: %s", outer)
	}
}

// An operating-system failure in the module inventory names the directory that failed, relative to the project.
func TestCRW1178WalkErrorNamesTheDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	cwd, _, env := gateScene(t)
	blocked := filepath.Join(cwd, "blocked_tests")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	cmd := "python3 -m unittest"
	mem := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
	gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd)))
	for _, r := range []string{mem, gh} {
		if !strings.Contains(r, "blocked_tests") || strings.Contains(r, cwd) {
			t.Errorf("the reason must name the directory relative to the project: %s", r)
		}
	}
}

// A cache entry that Python itself would load (its header agrees with the source) runs code the reader never read, so both guards
// refuse the run, with the file and the route. The cache here is real: Python writes the header for the source, and the code only
// writes a file under the memories root; running the command afterwards shows the interpreter runs that code instead of the source.
// The route the refusal names (remove the cache, then run with -B) is allowed and stays allowed on repeated runs.
func TestCRW1178MatchingCacheRunsUnreadCode(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	for _, mode := range []string{"timestamp", "checked-hash"} {
		t.Run(mode, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			marker := filepath.Join(root, "n.md")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			setup := `import importlib.util,marshal,pathlib,struct,sys
cwd=pathlib.Path(sys.argv[1]);marker=sys.argv[2];mode=sys.argv[3]
p=cwd/'test_calc.py';src=b'x = 1\n';p.write_bytes(src)
cache=pathlib.Path(importlib.util.cache_from_source(str(p)));cache.parent.mkdir()
code=compile('open('+repr(marker)+', "w").write("cache executed")\nimport unittest\nclass TestCache(unittest.TestCase):\n def test_ok(self): pass\n',str(p),'exec')
if mode=='timestamp':
 s=p.stat();header=importlib.util.MAGIC_NUMBER+struct.pack('<III',0,int(s.st_mtime)&0xffffffff,s.st_size&0xffffffff)
else:
 header=importlib.util.MAGIC_NUMBER+struct.pack('<I',3)+importlib.util.source_hash(src)
cache.write_bytes(header+marshal.dumps(code))
`
			if out, err := exec.Command(python, "-I", "-c", setup, cwd, marker, mode).CombinedOutput(); err != nil {
				t.Fatalf("setup: %v %s", err, out)
			}
			for _, cmd := range []string{"python3 -B -m unittest", "python3 -m unittest", "PYTHONDONTWRITEBYTECODE=1 python3 -m unittest"} {
				mem := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
				gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd)))
				for _, r := range []string{mem, gh} {
					if !strings.Contains(r, "__pycache__/test_calc.") || !strings.Contains(r, "remove __pycache__ in a separate command first; python -B") || strings.Contains(r, "...") {
						t.Errorf("%s: the reason lacks the file or the route, or is cut: %s", cmd, r)
					}
				}
			}
			// The premise: the interpreter runs the cached code, not the source the reader read.
			run := exec.Command(python, "-B", "-m", "unittest")
			run.Dir = cwd
			run.Env = []string{"HOME=" + filepath.Dir(root), "PATH=/usr/bin:/bin", "TZ=UTC"}
			if out, err := run.CombinedOutput(); err != nil {
				t.Logf("unittest: %v %s", err, out)
			}
			if b, err := os.ReadFile(marker); err != nil || string(b) != "cache executed" {
				t.Fatalf("the cache did not run, so this test proves nothing: %v %q", err, b)
			}
			// The route: removing the cache is allowed, and -B runs leave none to load.
			rm := "rm -rf __pycache__"
			if out := HandleMemoryWriteGate(gateBash(t, cwd, rm), env); out != "" {
				t.Errorf("memory gate refused the route %q: %s", rm, out)
			}
			if out := HandleGitHubPostGuard(gateBash(t, cwd, rm)); out != "" {
				t.Errorf("GitHub guard refused the route %q: %s", rm, out)
			}
			if err := os.RemoveAll(filepath.Join(cwd, "__pycache__")); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if out := HandleMemoryWriteGate(gateBash(t, cwd, "python3 -B -m unittest"), env); out != "" {
					t.Errorf("run %d after removing the cache: %s", i, out)
				}
				run := exec.Command(python, "-B", "-m", "unittest")
				run.Dir = cwd
				run.Env = []string{"HOME=" + filepath.Dir(root), "PATH=/usr/bin:/bin", "TZ=UTC"}
				_, _ = run.CombinedOutput()
				if _, err := os.Stat(filepath.Join(cwd, "__pycache__")); !os.IsNotExist(err) {
					t.Fatalf("a -B run wrote a cache: %v", err)
				}
			}
		})
	}
}

// The word gh is a mention of a post only as a command word or a path whose last component is gh; a file named gh.sh, gh.py or a
// module tests.gh is not.
func TestCRW1178GhInAFileNameIsNoGithubPost(t *testing.T) {
	cwd, _, _ := gateScene(t)
	for _, cmd := range []string{"bash missing/gh.sh", "\"$FOO\" tests/gh.py", "\"$FOO\" gh.sh", "\"$FOO\" --file=gh.txt"} {
		crw1178ReaderWording(t, githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd))))
	}
	for _, c := range []struct {
		text string
		want bool
	}{
		{"gh pr comment 1", true},
		{"/usr/bin/gh pr comment 1", true},
		{"./gh pr comment 1", true},
		{`g""h pr comment 1`, true},
		{"x=$(gh api user)", true},
		{"a; gh issue comment 2", true},
		{"a|gh pr comment 1", true},
		{"bash missing/gh.sh", false},
		{"python3 tests/gh.py", false},
		{"python3 -m unittest tests.gh", false},
		{"high_priority ghost", false},
		{"cat gh-notes.md", false},
	} {
		if got := githubPostSpellsPost(c.text); got != c.want {
			t.Errorf("githubPostSpellsPost(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// CRW-1178 evaluation d1 and d2 through both guards: an untagged __pycache__/calc.pyc (importable as the sourceless module
// __pycache__.calc) and a touch through a hard link beside a stale entry are refused by the memory gate and the GitHub guard.
func TestCRW1178CacheBypassShapesStayRefused(t *testing.T) {
	cwd, _, env := gateScene(t)
	crw1178Cache(t, cwd)
	untagged := filepath.Join(cwd, "__pycache__", "calc.pyc")
	if err := os.WriteFile(untagged, append(make([]byte, 16), "code"...), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"python3 -m unittest", "python3 -B -m unittest"} {
		reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
		if !strings.Contains(reason, "__pycache__/calc.pyc") || strings.Contains(reason, "MEMORY-WRITE-GATE") {
			t.Errorf("untagged cache, memory gate, %q: %s", cmd, reason)
		}
		gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd)))
		if !strings.Contains(gh, "__pycache__/calc.pyc") {
			t.Errorf("untagged cache, GitHub guard, %q: %s", cmd, gh)
		}
	}
	if err := os.Remove(untagged); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(cwd, "calc.py"), filepath.Join(cwd, "stamp")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	for _, cmd := range []string{"touch -d @1 stamp && python3 -B -m unittest", "truncate -s 6 stamp; python3 -m unittest"} {
		reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
		if !strings.Contains(reason, "stale only until") {
			t.Errorf("touch through a link, memory gate, %q: %s", cmd, reason)
		}
		gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd)))
		if !strings.Contains(gh, "stale only until") {
			t.Errorf("touch through a link, GitHub guard, %q: %s", cmd, gh)
		}
	}
}

// CRW-1178 verification round 3: cp -p (and any command not known to change no file) through a hard link of the source can make a
// stale entry current before the run. Both guards refuse it with the cause; the same run alone stays allowed.
func TestCRW1178PreservedTimeCopyBesideStaleCacheIsRefused(t *testing.T) {
	cwd, _, env := gateScene(t)
	crw1178Cache(t, cwd)
	if err := os.Link(filepath.Join(cwd, "calc.py"), filepath.Join(cwd, "stamp")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "donor"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"cp -p donor stamp && python3 -B -m unittest",
		"cat donor > stamp; python3 -m unittest",
		"python3 -B -m unittest | cp -p donor stamp",
	} {
		reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
		if !strings.Contains(reason, "stale only until") {
			t.Errorf("memory gate, %q: %s", cmd, reason)
		}
		gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd)))
		if !strings.Contains(gh, "stale only until") {
			t.Errorf("GitHub guard, %q: %s", cmd, gh)
		}
	}
	if out := HandleMemoryWriteGate(gateBash(t, cwd, "cd . && python3 -B -m unittest 2>&1 | tail -n 5"), env); out != "" {
		t.Errorf("a run beside a stale cache with read-only commands was refused: %s", out)
	}
}

// CRW-1178 verification round 4 through both guards with a real interpreter: pytest's assertion-rewrite cache
// (__pycache__/test_calc.<tag>-pytest-<version>.pyc) that is stale is discarded by pytest and allowed again, while the same entry
// agreeing with its source runs unread code and is refused; and an entry recording the second Python reads for a source time just
// below a whole second (1700000000.999999999 reads as 1700000001.0) runs, so it is refused.
func TestCRW1178RealCacheShapesInBothGuards(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	setup := `import importlib.util,marshal,os,pathlib,struct,sys
cwd=pathlib.Path(sys.argv[1]);marker=sys.argv[2];mode=sys.argv[3]
p=cwd/'test_calc.py';p.write_bytes(b'def test_ok():\n    pass\n')
if mode=='rounded':
 os.utime(p,ns=(1700000000999999999,1700000000999999999))
 if p.stat().st_mtime_ns!=1700000000999999999: print('no-ns');sys.exit(0)
 cache=pathlib.Path(importlib.util.cache_from_source(str(p)))
else:
 from _pytest.assertion.rewrite import PYTEST_TAG
 cache=p.parent/'__pycache__'/('test_calc.'+PYTEST_TAG+'.pyc')
cache.parent.mkdir(exist_ok=True)
code=compile('open('+repr(marker)+', "w").write("cache executed")\nimport unittest\nclass TestCache(unittest.TestCase):\n def test_ok(self): pass\ndef test_ok(): pass\n',str(p),'exec')
s=p.stat();t=int(s.st_mtime)
if mode=='stale': t+=7
cache.write_bytes(importlib.util.MAGIC_NUMBER+struct.pack('<III',0,t&0xffffffff,s.st_size&0xffffffff)+marshal.dumps(code))
print(cache.name)
`
	for _, c := range []struct {
		mode, run, real string
		refused         bool
	}{
		{"current", "python3 -B -m pytest", "-B -m pytest -q -p no:cacheprovider", true},
		{"stale", "python3 -B -m pytest", "-B -m pytest -q -p no:cacheprovider", false},
		{"rounded", "python3 -B -m unittest", "-B -m unittest", true},
	} {
		t.Run(c.mode, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			marker := filepath.Join(root, "n.md")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(python, "-I", "-c", setup, cwd, marker, c.mode).CombinedOutput()
			if err != nil {
				t.Skipf("setup (pytest missing?): %v %s", err, out)
			}
			name := strings.TrimSpace(string(out))
			if name == "no-ns" {
				t.Skip("no nanosecond times here")
			}
			mem := HandleMemoryWriteGate(gateBash(t, cwd, c.run), env)
			gh := HandleGitHubPostGuard(gateBash(t, cwd, c.run))
			if c.refused {
				for _, r := range []string{gateDeny(t, mem), githubPostAnswerReason(t, gh)} {
					if !strings.Contains(r, "__pycache__/"+name) || !strings.Contains(r, "remove __pycache__ in a separate command first; python -B") {
						t.Errorf("%s: the reason lacks the file or the route: %s", c.run, r)
					}
				}
			} else if mem != "" || gh != "" {
				t.Errorf("%s: a stale pytest cache was refused: %s %s", c.run, mem, gh)
			}
			// The premise: the interpreter runs the cache exactly when it is refused.
			run := exec.Command(python, strings.Fields(c.real)...)
			run.Dir = cwd
			run.Env = []string{"HOME=" + filepath.Dir(root), "PATH=/usr/bin:/bin", "TZ=UTC"}
			if out, err := run.CombinedOutput(); err != nil {
				t.Logf("%s: %v %s", c.run, err, out)
			}
			_, err = os.Stat(marker)
			if ran := err == nil; ran != c.refused {
				t.Fatalf("%s: the cache ran %v, expected %v, so this test proves nothing", c.run, ran, c.refused)
			}
		})
	}
}

// crw1178OutsideLinks lays out, beside the project directory and outside its walk, lnk -> ext/deep, ext/sl -> calc.py and ext/hard (a
// hard link of calc.py): from the project, ../lnk/../sl and ../lnk/../hard reach the source for the kernel, which resolves '..'
// after the link, while a text clean of the path names <parent>/sl, which does not exist.
func crw1178OutsideLinks(t *testing.T, cwd string) {
	t.Helper()
	parent := filepath.Dir(cwd)
	if err := os.MkdirAll(filepath.Join(parent, "ext", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "ext", "deep"), filepath.Join(parent, "lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cwd, "calc.py"), filepath.Join(parent, "ext", "sl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(cwd, "calc.py"), filepath.Join(parent, "ext", "hard")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
}

// CRW-1178 verification rounds 5 and 6 (P0) through both guards: a write redirection of the module run itself to a name of a source
// (a hard link, or a symlink or hard link reached through a directory link and '..') can make a stale entry current. Beside a stale
// entry the run may write only to /dev/null and descriptor copies or closes; both guards refuse any file target, a new log file
// included, with the cause and the route. With no stale entry the same log redirections stay allowed.
func TestCRW1178RunRedirectThroughLinkIsRefusedByBothGuards(t *testing.T) {
	cwd, _, env := gateScene(t)
	crw1178Cache(t, cwd)
	if err := os.Link(filepath.Join(cwd, "calc.py"), filepath.Join(cwd, "stamp")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	crw1178OutsideLinks(t, cwd)
	for _, c := range []struct{ cmd, target string }{
		{"python3 -B -m unittest > stamp", "stamp"},
		{"python3 -B -m unittest 2> stamp", "stamp"},
		{"sleep 3; python3 -B -m unittest > stamp", "stamp"},
		{"sleep 3; python3 -B -m unittest 2> stamp", "stamp"},
		{"python3 -B -m unittest > ../lnk/../sl", "../lnk/../sl"},
		{"python3 -B -m unittest 2> ../lnk/../sl", "../lnk/../sl"},
		{"python3 -B -m unittest >> ../lnk/../sl", "../lnk/../sl"},
		{"sleep 3; python3 -B -m unittest > ../lnk/../sl", "../lnk/../sl"},
		{"python3 -B -m unittest > ../lnk/../hard", "../lnk/../hard"},
		{"python3 -B -m unittest 2> ../lnk/../hard", "../lnk/../hard"},
		{"python3 -B -m unittest >> ../lnk/../hard", "../lnk/../hard"},
		{"sleep 3; python3 -B -m unittest 2> ../lnk/../hard", "../lnk/../hard"},
		{"python3 -B -m unittest > out.log 2>&1", "out.log"},
		{"sleep 1; python3 -B -m unittest 2> out.log", "out.log"},
		{"python3 -B -m unittest >> notes.log", "notes.log"},
	} {
		mem := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, c.cmd), env))
		gh := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, c.cmd)))
		for _, r := range []string{mem, gh} {
			if !strings.Contains(r, "stale only until") || !strings.Contains(r, c.target) || !strings.Contains(r, "separate command") || strings.Contains(r, "MEMORY-WRITE-GATE") {
				t.Errorf("%q: the reason lacks the cause or the route: %s", c.cmd, r)
			}
		}
	}
	for _, cmd := range []string{"python3 -B -m unittest > /dev/null 2>&1", "sleep 1; python3 -B -m unittest 2>&1 | tail -n 5"} {
		if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env); out != "" {
			t.Errorf("memory gate refused %q: %s", cmd, out)
		}
		if out := HandleGitHubPostGuard(gateBash(t, cwd, cmd)); out != "" {
			t.Errorf("GitHub guard refused %q: %s", cmd, out)
		}
	}
	// No stale entry skipped: a run that writes its log is allowed as before.
	fresh, _, freshEnv := gateScene(t)
	for _, m := range []string{"calc", "test_calc"} {
		if err := os.WriteFile(filepath.Join(fresh, m+".py"), []byte("x = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range []string{"python3 -B -m unittest > out.log 2>&1", "sleep 1; python3 -B -m unittest 2> out.log"} {
		if out := HandleMemoryWriteGate(gateBash(t, fresh, cmd), freshEnv); out != "" {
			t.Errorf("memory gate refused %q with no cache: %s", cmd, out)
		}
		if out := HandleGitHubPostGuard(gateBash(t, fresh, cmd)); out != "" {
			t.Errorf("GitHub guard refused %q with no cache: %s", cmd, out)
		}
	}
}

// CRW-1178 verification rounds 5 and 6 (P0) end to end with a real interpreter: a stale entry recording size 0 and a second just
// ahead, and a name of its source the run writes to: a hard link stamp in the project, or a plain symlink reached through a
// directory link and '..' (../lnk/../sl). The command run in that second truncates the source, the entry agrees, and its code (which
// writes a memory note) runs instead of the source the guards read. Both guards refuse the command beforehand.
func TestCRW1178RunRedirectThroughLinkRunsUnreadCacheEndToEnd(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	for _, c := range []struct {
		cmd  string
		link func(t *testing.T, cwd string)
	}{
		{"python3 -B -m unittest > stamp", func(t *testing.T, cwd string) {
			if err := os.Link(filepath.Join(cwd, "calc.py"), filepath.Join(cwd, "stamp")); err != nil {
				t.Skipf("no hard links here: %v", err)
			}
		}},
		{"python3 -B -m unittest > ../lnk/../sl", crw1178OutsideLinks},
	} {
		t.Run(c.cmd, func(t *testing.T) {
			// The premise needs the run to start in the second the entry records; a loaded host may miss it, so it is retried.
			for attempt := 1; ; attempt++ {
				cwd, root, env := gateScene(t)
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(root, "injected.md")
				for name, body := range map[string]string{
					"calc.py":      "def add(a, b):\n    return a + b\n",
					"test_calc.py": "import unittest\nimport calc\nclass T(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(calc.add(1, 2), 3)\n",
				} {
					if err := os.WriteFile(filepath.Join(cwd, name), []byte(body), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				c.link(t, cwd)
				second := time.Now().Unix() + 2
				setup := `import importlib.util,marshal,struct,sys
src=sys.argv[1]+'/calc.py';marker=sys.argv[2];t=int(sys.argv[3])
code=compile('open('+repr(marker)+', "w").write("unread bytecode ran")\ndef add(a, b):\n    return a + b\n',src,'exec')
cache=importlib.util.cache_from_source(src)
import os;os.makedirs(os.path.dirname(cache))
open(cache,'wb').write(importlib.util.MAGIC_NUMBER+struct.pack('<III',0,t&0xffffffff,0)+marshal.dumps(code))
`
				if out, err := exec.Command(python, "-I", "-c", setup, cwd, marker, strconv.FormatInt(second, 10)).CombinedOutput(); err != nil {
					t.Fatalf("setup: %v %s", err, out)
				}
				mem := HandleMemoryWriteGate(gateBash(t, cwd, c.cmd), env)
				gh := HandleGitHubPostGuard(gateBash(t, cwd, c.cmd))
				if mem == "" || gh == "" {
					t.Errorf("a run that truncates its source through a link beside a stale entry was allowed: memory gate %q, GitHub guard %q", mem, gh)
				} else {
					for _, r := range []string{gateDeny(t, mem), githubPostAnswerReason(t, gh)} {
						if !strings.Contains(r, "stale only until") || !strings.Contains(r, "separate command") {
							t.Errorf("the reason lacks the cause or the route: %s", r)
						}
					}
				}
				// The premise: run in the recorded second, the entry's code runs and writes the memory note.
				for time.Now().Unix() < second {
					time.Sleep(10 * time.Millisecond)
				}
				run := exec.Command(bash, "-c", c.cmd)
				run.Dir = cwd
				run.Env = []string{"HOME=" + filepath.Dir(root), "PATH=" + filepath.Dir(python) + ":/usr/bin:/bin", "TZ=UTC"}
				out, _ := run.CombinedOutput()
				if b, err := os.ReadFile(marker); err == nil && string(b) == "unread bytecode ran" {
					return
				}
				if attempt == 3 {
					t.Fatalf("the cache never ran, so this test proves nothing: %s", out)
				}
				t.Logf("attempt %d missed the second: %s", attempt, out)
			}
		})
	}
}
