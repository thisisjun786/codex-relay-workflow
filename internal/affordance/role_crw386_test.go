package affordance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pabcdhook "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-386: a session the relay registry verifies as a project parent or a supervisor is given no loop pointer, by the
// SessionStart map affordance and by the restore after a compaction (which the first prompt after a compaction would otherwise
// bring back). A dispatched task, a session with an unknown role and a session with no role reader get the oracle's output, byte
// for byte. The registry is read, never written.

// roleSpy answers one role and records the sessions it was asked about.
type roleSpy struct {
	role  pabcdhook.PromptRole
	asked []string
}

func (r *roleSpy) read(cwd, sessionID string) pabcdhook.PromptRole {
	r.asked = append(r.asked, cwd+"|"+sessionID)
	return r.role
}

var roleCases = []struct {
	name         string
	role         pabcdhook.PromptRole
	withoutLoops bool
}{
	{"parent", pabcdhook.PromptRoleParent, true},
	{"supervisor", pabcdhook.PromptRoleSupervisor, true},
	{"child", pabcdhook.PromptRoleTask, false},
	{"unknown", pabcdhook.PromptRoleUnknown, false},
}

func withoutLoop(t *testing.T, ctx string) string {
	t.Helper()
	loop := RenderLoopAffordance(testEnv)
	if !strings.Contains(ctx, loop) {
		t.Fatalf("the oracle's context has no loop pointer:\n%s", ctx)
	}
	return strings.Replace(ctx, loop+"\n\n", "", 1)
}

func TestSessionStartLoopPointerFollowsTheRegistryRole(t *testing.T) {
	ws := t.TempDir()
	seed(t, ws, 40)
	for _, sid := range []string{"SESSION", "thread-2"} {
		raw := payload(ws, "SessionStart", sid, nil)
		oracle := RunMapAffordanceSessionStart(raw, ws, testEnv)
		oracleCtx := contextOf(t, oracle, "SessionStart")
		for _, c := range roleCases {
			spy := &roleSpy{role: c.role}
			got := RunMapAffordanceSessionStartWithRole(raw, ws, testEnv, spy.read)
			if len(spy.asked) != 1 || spy.asked[0] != ws+"|"+sid {
				t.Errorf("%s: the reader is asked once for the payload's session: %v", c.name, spy.asked)
			}
			if !c.withoutLoops {
				if got != oracle {
					t.Errorf("%s: not the oracle's output:\n got %q\nwant %q", c.name, got, oracle)
				}
				continue
			}
			ctx := contextOf(t, got, "SessionStart")
			if strings.Contains(ctx, "Loop contract") || strings.Contains(ctx, RenderLoopAffordance(testEnv)) {
				t.Errorf("%s: the loop pointer is still given:\n%s", c.name, ctx)
			}
			if want := withoutLoop(t, oracleCtx); ctx != want {
				t.Errorf("%s: more than the loop pointer changed:\n got %q\nwant %q", c.name, ctx, want)
			}
			// The binding the session must pass to every mutating command is still given, as is every other pointer.
			for _, keep := range []string{RenderSessionBinding(sid, testEnv), RenderSkillSearchAffordance(testEnv), RenderStackedPrAffordance(), RenderQuestionAffordance(), RenderMapAffordance(40, testEnv)} {
				if !strings.Contains(ctx, keep) {
					t.Errorf("%s: lost %q", c.name, keep[:40])
				}
			}
		}
		// No reader is an unknown role.
		if got := RunMapAffordanceSessionStartWithRole(raw, ws, testEnv, nil); got != oracle {
			t.Errorf("nil reader differs from the oracle")
		}
	}
}

// Malformed input and a missing or empty session id name no session, so there is nothing to look up: the oracle's pointers stay and
// the registry is not read. (An id the pointer's print rule rejects is still a session: the registry decides its role.)
func TestSessionStartReadsNoRoleWithoutASession(t *testing.T) {
	ws := t.TempDir()
	for _, raw := range []string{"", "{", `{"cwd":"` + ws + `"}`, `{"session_id":""}`} {
		spy := &roleSpy{role: pabcdhook.PromptRoleParent}
		got := RunMapAffordanceSessionStartWithRole(raw, ws, testEnv, spy.read)
		if len(spy.asked) != 0 {
			t.Errorf("%q: the registry was read for %v", raw, spy.asked)
		}
		if got != RunMapAffordanceSessionStart(raw, ws, testEnv) {
			t.Errorf("%q: differs from the oracle's output", raw)
		}
	}
}

// The compaction restore: a prompt after a compaction of a verified coordinating seat gets the restore without the loop pointer,
// and the registry is read only once a marker is consumed.
func TestCompactRestoreLoopPointerFollowsTheRegistryRole(t *testing.T) {
	for _, c := range roleCases {
		ws := t.TempDir()
		prompt := payload(ws, "UserPromptSubmit", "SESSION", nil)
		spy := &roleSpy{role: c.role}
		if got := RunUserPromptAffordanceWithRole(prompt, testEnv, spy.read); got != "" || len(spy.asked) != 0 {
			t.Fatalf("%s: no marker, no output and no registry read: %q %v", c.name, got, spy.asked)
		}
		RunPostCompactAffordance(payload(ws, "PostCompact", "SESSION", nil))
		got := RunUserPromptAffordanceWithRole(prompt, testEnv, spy.read)
		if len(spy.asked) != 1 || spy.asked[0] != ws+"|SESSION" {
			t.Errorf("%s: the reader is asked once for the payload's session: %v", c.name, spy.asked)
		}
		if !c.withoutLoops {
			if got != golden(t, "compact") {
				t.Errorf("%s: not the oracle's restore: %q", c.name, got)
			}
			continue
		}
		ctx := contextOf(t, got, "UserPromptSubmit")
		if strings.Contains(ctx, "Loop contract") {
			t.Errorf("%s: the restore brings the loop pointer back:\n%s", c.name, ctx)
		}
		if want := withoutLoop(t, contextOf(t, golden(t, "compact"), "UserPromptSubmit")); ctx != want {
			t.Errorf("%s: more than the loop pointer changed:\n got %q\nwant %q", c.name, ctx, want)
		}
		// The marker is consumed whatever the role: the restore is not repeated.
		if RunUserPromptAffordanceWithRole(prompt, testEnv, spy.read) != "" {
			t.Errorf("%s: the restore repeated", c.name)
		}
	}
}

// longParentID is a canonical session id (state.IsCanonicalSessionID) one character past the pointer's 128-character print limit.
var longParentID = strings.Repeat("a", 129)

// seededRegistry points the production reader at a registry holding one binding per role and returns the session ids.
func seededRegistry(t *testing.T) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "relay-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", stateDir)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(stateDir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	bind := func(id, role, kind, key, task string) {
		t.Helper()
		row := store.ScopeBindingsRow{BindingID: id, Role: role, ScopeKind: kind, ScopeKey: key, TaskID: task, HostID: "h", Status: "active", Revision: 1,
			CreatedAt: "2026-10-11T00:00:00.000000+00:00", UpdatedAt: "2026-10-11T00:00:00.000000+00:00"}
		if err := storeseed.InsertScopeBinding(ctx, st, row); err != nil {
			t.Fatal(err)
		}
	}
	bind("b1", "parent", "project", "P-1", "thread-parent")
	bind("b2", "child", "issue", "CRW-1", "thread-child")
	bind("b3", "supervisor", "initiative", "I-1", "thread-supervisor")
	bind("b4", "parent", "project", "P-2", "thread-both")
	bind("b5", "child", "issue", "CRW-2", "thread-both")
	// Session ids the registry holds as parents that the pointer's own id rule would not print (CRW-386 verification round 1).
	bind("b6", "parent", "project", "P-3", "thread.parent")
	bind("b7", "parent", "project", "P-4", "_parent")
	bind("b8", "parent", "project", "P-5", longParentID)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// The wired hooks (RunHook, as `crw hook` runs them) against a seeded registry: the SessionStart answer and the first prompt
// after a compaction, for a parent, a supervisor, a dispatched task, a mixed role, an unbound session and an absent registry.
func TestWiredHooksReadTheRegistryRole(t *testing.T) {
	seededRegistry(t)
	run := func(event string, raw, ws string) string {
		t.Helper()
		var out strings.Builder
		if code := RunHook(context.Background(), event, strings.NewReader(raw), &out, testEnv, ws); code != 0 {
			t.Fatalf("%s exit %d", event, code)
		}
		return out.String()
	}
	cases := []struct {
		session      string
		withoutLoops bool
	}{
		{"thread-parent", true}, {"thread-supervisor", true},
		{"thread-child", false}, {"thread-both", false}, {"thread-unbound", false},
		// The role lookup does not use the pointer's id-print rule: Stop and UserPromptSubmit accept these ids as sessions.
		{"thread.parent", true}, {"_parent", true}, {longParentID, true},
	}
	for _, c := range cases {
		t.Run(c.session[:min(len(c.session), 20)], func(t *testing.T) {
			ws := t.TempDir()
			start := contextOf(t, run("session-start", payload(ws, "SessionStart", c.session, nil), ws), "SessionStart")
			if has := strings.Contains(start, RenderLoopAffordance(testEnv)); has == c.withoutLoops {
				t.Errorf("SessionStart loop pointer present=%v:\n%s", has, start)
			}
			run("post-compact", payload(ws, "PostCompact", c.session, nil), ws)
			restore := contextOf(t, run("user-prompt-submit", payload(ws, "UserPromptSubmit", c.session, nil), ws), "UserPromptSubmit")
			if has := strings.Contains(restore, RenderLoopAffordance(testEnv)); has == c.withoutLoops {
				t.Errorf("compaction restore loop pointer present=%v:\n%s", has, restore)
			}
		})
	}
	t.Run("absent registry", func(t *testing.T) {
		t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(t.TempDir(), "absent"))
		ws := t.TempDir()
		start := contextOf(t, run("session-start", payload(ws, "SessionStart", "thread-parent", nil), ws), "SessionStart")
		if !strings.Contains(start, RenderLoopAffordance(testEnv)) {
			t.Errorf("an unreadable registry is no role evidence, so the oracle's pointer stays:\n%s", start)
		}
	})
	// The registry is only read: no store file appears in an absent state directory.
	t.Run("the read creates nothing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "never-created")
		t.Setenv("CODEX_SESSION_RELAY_STATE", dir)
		ws := t.TempDir()
		run("session-start", payload(ws, "SessionStart", "thread-parent", nil), ws)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("the role read created %s (%v)", dir, err)
		}
	})
}

// A coordinating seat's resume is judged against what the seat itself was given: a resume right after the seat's own start
// re-issues the binding only, as it does for every session.
func TestCoordinatingSeatResumeKeepsTheRecord(t *testing.T) {
	seededRegistry(t)
	ws := t.TempDir()
	run := func(raw string) string {
		var out strings.Builder
		if code := RunHook(context.Background(), "session-start", strings.NewReader(raw), &out, testEnv, ws); code != 0 {
			t.Fatal(code)
		}
		return out.String()
	}
	startup := contextOf(t, run(payload(ws, "SessionStart", "thread-parent", map[string]any{"source": "startup"})), "SessionStart")
	if strings.Contains(startup, "Loop contract") || !strings.Contains(startup, RenderQuestionAffordance()) {
		t.Fatalf("startup:\n%s", startup)
	}
	resume := contextOf(t, run(payload(ws, "SessionStart", "thread-parent", map[string]any{"source": "resume"})), "SessionStart")
	if strings.Contains(resume, RenderQuestionAffordance()) || !strings.Contains(resume, RenderSessionBinding("thread-parent", testEnv)) {
		t.Fatalf("a resume of what the seat was given re-issues the binding only:\n%s", resume)
	}
}
