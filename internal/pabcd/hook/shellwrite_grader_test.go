package hook

import (
	"slices"
	"testing"
)

// TestGraderShapesHeldByTheGates holds the shapes an independent review reproduced: a writer whose destination is a
// variable, a Python open bound to a name, a perl or ruby program without a reader, a sed e command, sort's output forms,
// and a cd that moves a relative write into a protected directory.
func TestGraderShapesHeldByTheGates(t *testing.T) {
	for _, cmd := range []string{
		"node -e 'require(\"fs\").writeFileSync(process.argv[1],\"x\")' /m/n.md",
		"python3 -c 'm=\"/m/n.md\"; f=open; f(m,\"w\").write(\"x\")'",
		"ruby -e 'File.write(\"/m/n.md\",\"x\")'",
		"perl -ne 'print' /m/n.md",
		"sed 'e echo hi' x.txt",
		"sed 's/a/b/e' x.txt",
	} {
		d, ok := shellIRWriteDests(cmd, "/work", nil)
		if ok && !slices.Contains(d, shellIRUnknownDest) {
			t.Errorf("%q is not an unknown destination: %q", cmd, d)
		}
	}
	for cmd, want := range map[string]string{
		"sort --output=/m/n.md /tmp/in": "/m/n.md",
		"sort -o/m/n.md /tmp/in":        "/m/n.md",
		"sort --outp /m/n.md /tmp/in":   "/m/n.md",
		"sort -ro /m/n.md /tmp/in":      "/m/n.md",
	} {
		d, ok := shellIRWriteDests(cmd, "/work", nil)
		if !ok || !slices.Contains(d, want) {
			t.Errorf("%q names %q, want %q", cmd, d, want)
		}
	}
	d, ok := shellIRWriteDestsResolved("cd /example/u/.codex/memories && echo x > n.md", "/work", nil)
	if !ok || !slices.Contains(d, "/example/u/.codex/memories/n.md") {
		t.Errorf("a write after cd names %q, want the memories file", d)
	}
	d, ok = shellIRWriteDestsResolved("echo x > n.md", "/work", nil)
	if !ok || !slices.Contains(d, "n.md") {
		t.Errorf("a write in the payload directory names %q, want n.md as written", d)
	}
}

// TestGraderCodingDeclarationLaterInTheComment holds a bytes program whose second coding name is read as a codec.
func TestGraderCodingDeclarationLaterInTheComment(t *testing.T) {
	cmd := "python3 -c 'exec(br\"\"\"# coding note; coding: unicode_escape\n\\x6fpen(file=\"/m/n.md\",mode=\"w\")\n\"\"\")'"
	if _, bad := shellIRFStringUnreadable(cmd); !bad {
		t.Error("a second coding declaration in a bytes program is read as a codec the reader does not model")
	}
}

// TestGraderWorktreeShapes holds the deletions the worktree guard reads from find and from a Python program in a managed
// checkout, and a removal after a directory change the reader cannot follow.
func TestGraderWorktreeShapes(t *testing.T) {
	r := newDelRig(t)
	r.denied(t, "find . -depth -delete", "python3 -c 'import shutil; shutil.rmtree(\"x\")'", "cd .. && rm -rf "+r.checkout)
}

// TestGraderGitConfigCodeKeys holds the git -c keys that run a program of a driver name.
func TestGraderGitConfigCodeKeys(t *testing.T) {
	for _, cmd := range []string{"git -c diff.foo.command='sh -c x' diff --ext-diff", "git -c merge.foo.driver=x merge main"} {
		if _, ok := shellIRWriteDests(cmd, "/work", nil); ok {
			t.Errorf("%q is read, want refused", cmd)
		}
	}
}

// TestGraderPlainStringWithAnF holds a plain string whose text begins with f': it is no f-string, so the program reads and
// writes nothing (CRW-1012).
func TestGraderPlainStringWithAnF(t *testing.T) {
	if _, bad := shellIRFStringUnreadable("python3 -c \"print(\\\"f'}'\\\")\""); bad {
		t.Error("print(\"f'}'\") is refused as an f-string with an unpaired brace")
	}
}
