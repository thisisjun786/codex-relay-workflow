package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// resumeErr runs the body as a resume of the journal and returns what Run returned: a refused resume is an error, not a result.
func (h *harness) resumeErr(body string) (*Result, error) {
	h.t.Helper()
	h.cfg.Resume = &ResumeOptions{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return Run(ctx, h.load(body), h.cfg)
}

// A resume compares the child workflows the run opened with the ones this build resolves (CRW-1461): the entry script's digest is
// gated, and a child whose source changed is refused the same way, before anything is written, whether the run named it or gave its
// scriptPath. The unchanged child resumes, and a run that never opened a child is untouched.
func TestResumeRefusesARunWhoseChildWorkflowChanged(t *testing.T) {
	kid := func(t *testing.T, prompt string) *script.Program {
		t.Helper()
		p, err := script.Load(script.Source{Name: "kid.js", Text: []byte(header + "const r = await agent.call('" + prompt + "', { agentType: 'explorer', label: 'kid' }); return r.status\n")})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct{ name, body, child string }{
		{"by name", `return await workflow('kid')`, "kid"},
		{"by scriptPath", `return await workflow({ scriptPath: '/x/kid.js' })`, "/x/kid.js"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			v1, v2 := kid(t, "child work"), kid(t, "child work, changed")
			cur := v1
			h.cfg.Workflows = func(name, path string) (*script.Program, error) {
				if name == "kid" || path == "/x/kid.js" {
					return cur, nil
				}
				return nil, errors.New("unknown workflow " + name + path)
			}
			if res := h.run(tc.body); res.Status != RunCompleted {
				t.Fatalf("the first run: %s %s", res.Status, res.Error)
			}
			rows := len(h.journal.Events())

			cur = v2
			mark := len(h.fake.Requests())
			res, err := h.resumeErr(tc.body)
			if err == nil {
				t.Fatalf("a resume whose child %s changed was accepted: %s, live %v", tc.child, res.Status, h.liveSince(mark))
			}
			for _, want := range []string{tc.child, v1.Digest, v2.Digest} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
			if n := len(h.journal.Events()); n != rows {
				t.Errorf("the refused resume wrote rows: %d -> %d", rows, n)
			}
			if n := len(h.fake.Requests()) - mark; n != 0 {
				t.Errorf("the refused resume sent %d requests", n)
			}

			// a build that does not have the child refuses the run as well
			cur = nil
			h.cfg.Workflows = func(name, path string) (*script.Program, error) { return nil, errors.New("no workflows here") }
			if _, err := h.resumeErr(tc.body); err == nil || !strings.Contains(err.Error(), tc.child) || !strings.Contains(err.Error(), "does not have") {
				t.Errorf("a resume whose child %s is gone: %v", tc.child, err)
			}
			if n := len(h.journal.Events()); n != rows {
				t.Errorf("the refused resume wrote rows: %d -> %d", rows, n)
			}

			// the child the run ran resumes and replays
			cur = v1
			h.cfg.Workflows = func(name, path string) (*script.Program, error) {
				if name == "kid" || path == "/x/kid.js" {
					return cur, nil
				}
				return nil, errors.New("unknown workflow " + name + path)
			}
			mark = len(h.fake.Requests())
			res, err = h.resumeErr(tc.body)
			if err != nil || res.Status != RunCompleted {
				t.Fatalf("the unchanged child was refused: %v %+v", err, res)
			}
			if n := len(h.fake.Requests()) - mark; n != 0 {
				t.Errorf("the unchanged resume sent %d requests", n)
			}
		})
	}

	// a run that named its child is resolved by the name (the row's ref): a scriptPath of the same text that still resolves to the old
	// source does not accept the changed one
	t.Run("the name decides", func(t *testing.T) {
		h := newHarness(t)
		v1, v2 := kid(t, "child work"), kid(t, "child work, changed")
		byName := v1
		h.cfg.Workflows = func(name, path string) (*script.Program, error) {
			switch {
			case name == "kid":
				return byName, nil
			case path == "kid":
				return v1, nil
			}
			return nil, errors.New("unknown workflow " + name + path)
		}
		body := `return await workflow('kid')`
		h.run(body)
		rows := len(h.journal.Events())
		byName = v2
		if _, err := h.resumeErr(body); err == nil || !strings.Contains(err.Error(), v2.Digest) {
			t.Errorf("a changed child whose old source is still at the scriptPath of the same text: %v", err)
		}
		if n := len(h.journal.Events()); n != rows {
			t.Errorf("the refused resume wrote rows: %d -> %d", rows, n)
		}
	})

	t.Run("no child", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Workflows = func(name, path string) (*script.Program, error) { return nil, errors.New("no workflows here") }
		body := `const r = await agent.call('work', { agentType: 'explorer', label: 'w' }); return r.status`
		h.run(body)
		mark := len(h.fake.Requests())
		res, err := h.resumeErr(body)
		if err != nil || res.Status != RunCompleted {
			t.Fatalf("a run with no child was refused: %v %+v", err, res)
		}
		if n := len(h.fake.Requests()) - mark; n != 0 {
			t.Errorf("the resume sent %d requests", n)
		}
	})
}

// withoutRef is the journal as a build that did not record the reference kind of a child wrote it: every workflow_opened row without
// its ref.
func (h *harness) withoutRef() {
	h.t.Helper()
	old := NewMemoryJournal()
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindFlow && ev.Event == "workflow_opened" {
			if _, ok := ev.Data["ref"]; !ok {
				h.t.Fatalf("the workflow_opened row %+v records no ref", ev)
			}
			data := map[string]any{}
			for k, v := range ev.Data {
				if k != "ref" {
					data[k] = v
				}
			}
			ev.Data = data
		}
		old.Append(ev)
	}
	h.journal = old
	h.cfg.Journal = old
}

// openedRef is the ref of the run's workflow_opened row.
func (h *harness) openedRef() string {
	h.t.Helper()
	for _, ev := range h.journal.Events() {
		if ev.Kind == KindFlow && ev.Event == "workflow_opened" {
			return rowStr(ev.Data, "ref")
		}
	}
	h.t.Fatal("no workflow_opened row")
	return ""
}

// A resume resolves a child the way the run referred to it, as its workflow_opened row records (ref): a name and a scriptPath of the same
// text may read different files ('kid' is kid.js by its name and the file kid by its path), and the unchanged child of either kind
// resumes. A row written before the kind was recorded is resolved both ways: when only one resolves, or both read the same source, the
// source is compared as before; when both resolve to different sources the row cannot say which one ran, and the resume is refused,
// naming both and how to go on, before anything is written.
func TestResumeResolvesTheChildAsTheRunReferredToIt(t *testing.T) {
	prog := func(t *testing.T, name, prompt string) *script.Program {
		t.Helper()
		p, err := script.Load(script.Source{Name: name, Text: []byte(header + "const r = await agent.call('" + prompt + "', { agentType: 'explorer', label: 'kid' }); return r.status\n")})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// two children: kid.js, which workflow('kid') reads, and the file kid, which workflow({scriptPath: 'kid'}) reads
	type world struct{ named, pathed *script.Program }
	resolver := func(w *world) func(name, path string) (*script.Program, error) {
		return func(name, path string) (*script.Program, error) {
			switch {
			case name == "kid" && w.named != nil:
				return w.named, nil
			case path == "kid" && w.pathed != nil:
				return w.pathed, nil
			}
			return nil, errors.New("unknown workflow " + name + path)
		}
	}
	accepted := func(t *testing.T, h *harness, body string) {
		t.Helper()
		mark := len(h.fake.Requests())
		res, err := h.resumeErr(body)
		if err != nil || res.Status != RunCompleted {
			t.Fatalf("the unchanged child was refused: %v %+v", err, res)
		}
		if n := len(h.fake.Requests()) - mark; n != 0 {
			t.Errorf("the unchanged resume sent %d requests", n)
		}
	}
	refused := func(t *testing.T, h *harness, body string, want ...string) {
		t.Helper()
		rows := len(h.journal.Events())
		mark := len(h.fake.Requests())
		_, err := h.resumeErr(body)
		if err == nil {
			t.Fatalf("the resume was accepted, want a refusal naming %q", want)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("the refusal %q does not name %q", err, w)
			}
		}
		if n := len(h.journal.Events()); n != rows {
			t.Errorf("the refused resume wrote rows: %d -> %d", rows, n)
		}
		if n := len(h.fake.Requests()) - mark; n != 0 {
			t.Errorf("the refused resume sent %d requests", n)
		}
	}
	byPath, byName := `return await workflow({ scriptPath: 'kid' })`, `return await workflow('kid')`

	t.Run("a scriptPath beside a name that reads another file", func(t *testing.T) {
		h := newHarness(t)
		w := &world{named: prog(t, "kid.js", "the named child"), pathed: prog(t, "kid", "the child at the path")}
		h.cfg.Workflows = resolver(w)
		if res := h.run(byPath); res.Status != RunCompleted {
			t.Fatalf("the first run: %s %s", res.Status, res.Error)
		}
		accepted(t, h, byPath)
		if ref := h.openedRef(); ref != "script" {
			t.Fatalf("the workflow_opened row records ref %q, want script", ref)
		}
		old := w.pathed
		w.pathed = prog(t, "kid", "the child at the path, changed")
		refused(t, h, byPath, "kid", old.Digest, w.pathed.Digest)
	})

	t.Run("a name beside a scriptPath that reads another file", func(t *testing.T) {
		h := newHarness(t)
		w := &world{named: prog(t, "kid.js", "the named child"), pathed: prog(t, "kid", "the child at the path")}
		h.cfg.Workflows = resolver(w)
		if res := h.run(byName); res.Status != RunCompleted {
			t.Fatalf("the first run: %s %s", res.Status, res.Error)
		}
		if ref := h.openedRef(); ref != "name" {
			t.Fatalf("the workflow_opened row records ref %q, want name", ref)
		}
		accepted(t, h, byName)
		// the file at the path is no child of this run: it may go
		w.pathed = nil
		accepted(t, h, byName)
	})

	t.Run("a row without ref, one reference resolves", func(t *testing.T) {
		h := newHarness(t)
		w := &world{pathed: prog(t, "kid", "the child at the path")}
		h.cfg.Workflows = resolver(w)
		h.run(byPath)
		h.withoutRef()
		accepted(t, h, byPath)
		old := w.pathed
		w.pathed = prog(t, "kid", "the child at the path, changed")
		refused(t, h, byPath, "kid", old.Digest, w.pathed.Digest)
	})

	t.Run("a row without ref, both references read the same source", func(t *testing.T) {
		h := newHarness(t)
		same := prog(t, "kid.js", "the child")
		w := &world{named: same, pathed: same}
		h.cfg.Workflows = resolver(w)
		h.run(byPath)
		h.withoutRef()
		accepted(t, h, byPath)
	})

	t.Run("a row without ref, both references read different sources", func(t *testing.T) {
		h := newHarness(t)
		w := &world{named: prog(t, "kid.js", "the named child"), pathed: prog(t, "kid", "the child at the path")}
		h.cfg.Workflows = resolver(w)
		h.run(byPath)
		h.withoutRef()
		// the source at the path is the one the run ran, but the row does not say so: the resume does not guess
		refused(t, h, byPath, "kid.js", w.named.Digest, w.pathed.Digest, "does not say whether", "move or rename one of the two", "start a new run")
		// with only the source the run ran left, the row is unambiguous again
		w.named = nil
		accepted(t, h, byPath)
	})
}
