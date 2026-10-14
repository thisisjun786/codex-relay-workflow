package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rolloutTree writes a Codex home's sessions tree: one rollout per thread, whose first line is the thread's session_meta record. parents maps a
// thread to the parent_thread_id of its source.subagent.thread_spawn; a thread with no entry is a root session (source "vscode").
func rolloutTree(t *testing.T, parents map[string]string, threads ...string) (home string, path func(string) string) {
	t.Helper()
	home = t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "14")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range threads {
		source := `"vscode"`
		if parent, ok := parents[id]; ok {
			source = fmt.Sprintf(`{"subagent":{"thread_spawn":{"parent_thread_id":%q,"depth":2}}}`, parent)
		}
		line := fmt.Sprintf(`{"timestamp":"2026-10-14T00:00:00Z","type":"session_meta","payload":{"id":%q,"session_id":"01a13570-0000-7000-8000-00000000root","source":%s,"base_instructions":"%s"}}`+"\n", id, source, strings.Repeat("x", 20000))
		if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-14T00-00-00-"+id+".jsonl"), []byte(line+`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home, func(id string) string { return filepath.Join(dir, "rollout-2026-10-14T00-00-00-"+id+".jsonl") }
}

func threadID(n int) string { return fmt.Sprintf("01a13570-0000-7000-8000-%012d", n) }

// CRW-1370: the lineage fallback follows parent_thread_id through the rollouts, at most LineageDepthLimit parents, to a thread the relay
// knows as a child; a lineage that ends at a root with no child above it is no child and silent; anything unreadable is never a child and says why.
func TestChildBindingByLineage(t *testing.T) {
	ctx := context.Background()
	kid, rel := threadID(1), "/work/parent"
	known := func(_ context.Context, session string) (bool, string, error) {
		if session == kid {
			return true, rel, nil
		}
		return false, "", nil
	}
	// root (0) -> kid (1) -> s2 -> s3 -> s4 -> s5 -> s6: the chain under the relay child, one thread per level.
	chain := map[string]string{kid: threadID(0), threadID(2): kid, threadID(3): threadID(2), threadID(4): threadID(3), threadID(5): threadID(4), threadID(6): threadID(5)}
	_, path := rolloutTree(t, chain, threadID(0), kid, threadID(2), threadID(3), threadID(4), threadID(5), threadID(6))

	for _, c := range []struct {
		name       string
		from       int
		child      bool
		unreadable string
	}{
		{"a thread directly under the child", 2, true, ""},
		{"two levels below the child", 3, true, ""},
		{"four parents up is the child (the depth limit)", 5, true, ""},
		{"five parents up is beyond the depth limit", 6, false, "deeper than 4 parents"},
	} {
		child, cwd, unreadable := ChildBindingByLineage(ctx, path(threadID(c.from)), known)
		if child != c.child || (child && cwd != rel) || (c.unreadable == "") != (unreadable == "") || !strings.Contains(unreadable, c.unreadable) {
			t.Errorf("%s: child %v, cwd %q, unreadable %q; want child %v, unreadable %q", c.name, child, cwd, unreadable, c.child, c.unreadable)
		}
	}

	// A lineage that reaches a root with no relay child above it: not a child, and nothing to warn about.
	stranger := func(context.Context, string) (bool, string, error) { return false, "", nil }
	if child, _, unreadable := ChildBindingByLineage(ctx, path(threadID(3)), stranger); child || unreadable != "" {
		t.Errorf("a lineage with no child: child %v, unreadable %q; want neither", child, unreadable)
	}
	// The root of the chain itself, and a depth-4 thread whose fourth parent is the root, end the lineage cleanly.
	if child, _, unreadable := ChildBindingByLineage(ctx, path(threadID(0)), known); child || unreadable != "" {
		t.Errorf("a root session's rollout: child %v, unreadable %q", child, unreadable)
	}
	if child, _, unreadable := ChildBindingByLineage(ctx, path(threadID(4)), stranger); child || unreadable != "" {
		t.Errorf("a lineage that ends exactly at the depth limit: child %v, unreadable %q", child, unreadable)
	}
}

func TestChildBindingByLineageIsNeverABindingOrARefusalWhenSomethingIsUnreadable(t *testing.T) {
	ctx := context.Background()
	kid := threadID(1)
	known := func(_ context.Context, session string) (bool, string, error) {
		return session == kid, "/work/parent", nil
	}
	home, path := rolloutTree(t, map[string]string{threadID(3): threadID(2), threadID(2): kid}, kid, threadID(2), threadID(3))
	dir := filepath.Join(home, "sessions", "2026", "10", "14")
	broken := filepath.Join(dir, "rollout-2026-10-14T00-00-00-"+threadID(9)+".jsonl")
	if err := os.WriteFile(broken, []byte("this is not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A transcript outside any sessions tree: its own parent is read from it, a grandparent's rollout cannot be found from it.
	stray := filepath.Join(t.TempDir(), "rollout-x-"+threadID(3)+".jsonl")
	if err := os.WriteFile(stray, []byte(`{"type":"session_meta","payload":{"source":{"subagent":{"thread_spawn":{"parent_thread_id":"`+threadID(2)+`"}}}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, orphan := rolloutTree(t, map[string]string{threadID(7): threadID(8)}, threadID(7)) // the parent thread 8 has no rollout
	for name, c := range map[string]struct {
		transcript string
		bind       func(context.Context, string) (bool, string, error)
		wantIn     string
	}{
		"no such transcript":                   {filepath.Join(home, "nope.jsonl"), known, "cannot be read"},
		"a transcript that is not a rollout":   {broken, known, "not a session_meta record"},
		"a parent thread with no rollout":      {orphan(threadID(7)), known, "cannot be found"},
		"a transcript outside a sessions tree": {stray, known, "sessions directory"},
		"the relay's record cannot be read": {path(threadID(3)), func(context.Context, string) (bool, string, error) {
			return false, "", errors.New("database is locked")
		}, "database is locked"},
	} {
		child, cwd, unreadable := ChildBindingByLineage(ctx, c.transcript, c.bind)
		if child || cwd != "" || !strings.Contains(unreadable, c.wantIn) {
			t.Errorf("%s: child %v, cwd %q, unreadable %q; want no binding and a reason with %q", name, child, cwd, unreadable, c.wantIn)
		}
	}
}

// countingRollout is a rollout of size bytes with no newline, which counts how much of it was read.
type countingRollout struct {
	left, read int
}

func (r *countingRollout) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.left)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.left -= n
	r.read += n
	return n, nil
}

func (r *countingRollout) Close() error { return nil }

// CRW-1387 d2: the first line of a rollout is read through a reader bounded at lineageMetaLimit+1 bytes, so a first line over the limit with no
// newline is never read whole: the lineage is unreadable (the caller warns, fail-open) after at most lineageMetaLimit+1 bytes.
func TestRolloutParentReadsAtMostTheLimitOfAFirstLineWithoutANewline(t *testing.T) {
	// A file on disk: a first line over the limit without a newline is an unreadable lineage that says why.
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "14")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(dir, "rollout-2026-10-14T00-00-00-"+threadID(3)+".jsonl")
	head := `{"type":"session_meta","payload":{"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + threadID(2) + `"}}},"base_instructions":"`
	if err := os.WriteFile(long, []byte(head+strings.Repeat("x", lineageMetaLimit)), 0o600); err != nil {
		t.Fatal(err)
	}
	known := func(context.Context, string) (bool, string, error) { return true, "/work/parent", nil }
	if child, cwd, unreadable := ChildBindingByLineage(context.Background(), long, known); child || cwd != "" || !strings.Contains(unreadable, "too long") {
		t.Errorf("a first line over the limit: child %v, cwd %q, unreadable %q; want an unreadable lineage (too long)", child, cwd, unreadable)
	}

	// The read is bounded: of a first line three times the limit, at most lineageMetaLimit+1 bytes are read.
	r := &countingRollout{left: 3 * lineageMetaLimit}
	defer func(open func(string) (io.ReadCloser, error)) { openRollout = open }(openRollout)
	openRollout = func(string) (io.ReadCloser, error) { return r, nil }
	if _, err := rolloutParent("rollout"); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("a first line of %d bytes: %v; want too long", 3*lineageMetaLimit, err)
	}
	if r.read > lineageMetaLimit+1 {
		t.Errorf("%d bytes of a first line without a newline were read; want at most lineageMetaLimit+1 = %d", r.read, lineageMetaLimit+1)
	}
}
