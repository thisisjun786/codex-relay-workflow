package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/job"
)

func TestBgComponentHookModeRoutesTheThreeLegs(t *testing.T) {
	old := os.Stdin
	t.Cleanup(func() { os.Stdin = old })
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CRW_BGWAKE", "1")
	for _, c := range []struct{ event, id, want string }{
		{"stop", "stop-waking-on-background-completion", `"decision":"block"`},
		{"user-prompt-submit", "user-prompt-submit-delivering-background-completions", `"hookEventName":"UserPromptSubmit"`},
		{"session-start", "session-start-adopting-background-completions", `"hookEventName":"SessionStart"`},
		{"unclaimed-component-event", "no-component", ""},
	} {
		ws := t.TempDir()
		if _, err := job.EnsureDir(ws); err != nil {
			t.Fatal(err)
		}
		sid, ended, zero := "S1", "2026-09-09T00:04:12.000Z", float64(0)
		r := job.BgRecord{ID: "build1", Cwd: ws, SessionID: &sid, Command: []string{"build"}, Status: job.StatusComplete, ExitCode: &zero, StartedAt: "2026-09-09T00:00:00.000Z", EndedAt: &ended}
		if err := job.WriteRecord(ws, r); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(ws, "input")
		if err := os.WriteFile(p, []byte(`{"session_id":"S1","cwd":"`+ws+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin = f
		var out, errOut strings.Builder
		code := run(context.Background(), "crw", []string{"hook", c.event, "--leg", c.id}, &out, &errOut)
		f.Close()
		if code != 0 || errOut.Len() != 0 || c.want == "" && out.Len() != 0 || c.want != "" && !strings.Contains(out.String(), c.want) {
			t.Fatalf("%s: %d %q %q", c.id, code, out.String(), errOut.String())
		}
		back, _ := job.ReadRecord(ws, "build1")
		if (back.DeliveredAt != nil) != (c.event == "stop" || c.event == "user-prompt-submit") {
			t.Fatal("unexpected delivery")
		}
	}
}

type unreadComponentInput struct{}

func (unreadComponentInput) Read([]byte) (int, error) { panic("unclaimed input read") }

func TestComponentHookTableExtendsWithoutChangingDispatch(t *testing.T) {
	ran := false
	rows := append(componentHooks(), componentHook{"future-component", "future-event", func(c invocation, in io.Reader) int { ran = true; return 7 }})
	for _, args := range [][]string{{"future-event", "--leg", "future-component"}, {"future-event", "--leg=future-component"}} {
		ran = false
		claimed, code := runComponentHook(invocation{ctx: context.Background(), args: args}, unreadComponentInput{}, rows)
		if !claimed || code != 7 || !ran {
			t.Fatal("row extension not routed")
		}
	}
	for _, args := range [][]string{nil, {"future-event"}, {"wrong", "--leg", "future-component"}, {"future-event", "--leg", "wrong"}, {"future-event", "--leg=future-component", "extra"}} {
		ran = false
		claimed, _ := runComponentHook(invocation{args: args}, unreadComponentInput{}, rows)
		if claimed || ran {
			t.Fatalf("claimed %v", args)
		}
	}
}
