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

	// the label does not say whether the run named the child or gave its scriptPath: a name that resolves is the child compared, and
	// a scriptPath of the same text that still resolves to the old source does not accept the changed one
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
