package role

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
)

const flowCardThread = "0190a000-0000-7000-8000-00000000000a"

// bindFlowCardThread writes the binding line of a thread by hand into the home of the test.
func bindFlowCardThread(t *testing.T, kind string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir, err := bound.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pair := `{"model":"explorer-model","effort":"high"}`
	line := `{"run_id":"run-1","thread_id":"` + flowCardThread + `","kind":"engine","call_id":"c_000000000001","role":"executor","nativeAllowance":1,"helperPairs":{"explorer":` + pair + `},"policyDigest":"d"}`
	if kind == bound.KindOrchestrator {
		line = `{"run_id":"run-1","thread_id":"` + flowCardThread + `","kind":"orchestrator","nativeAllowance":null,"helperPairs":{"explorer":` + pair + `,"reviewer":` + pair + `},"policyDigest":"d"}`
	}
	path := filepath.Join(dir, flowCardThread+bound.FileSuffix)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A card for a thread a flow run bound shows the role key and no model or low effort; the orchestrator's review form carries the
// producer header line that picks the reviewer pair. The ordinary card keeps its text.
func TestDispatchCardUsesRoleKeyWhenFlowBound(t *testing.T) {
	_, env := fallbackTestEnv(t)
	engine, err := RenderFlowBoundDispatchCard(bound.KindEngine)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator, err := RenderFlowBoundDispatchCard(bound.KindOrchestrator)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := RenderDispatchCard(env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ordinary, `reasoning_effort:"low"`) {
		t.Fatal("the ordinary card changed")
	}
	for name, card := range map[string]string{"engine": engine, "orchestrator": orchestrator} {
		if strings.Contains(card, `reasoning_effort:"low"`) || strings.Contains(card, "model:") || strings.Contains(card, "Aliases") {
			t.Errorf("%s card still shows a model or a low effort:\n%s", name, card)
		}
		if !strings.Contains(card, `agent_type:"explorer"`) {
			t.Errorf("%s card has no role key:\n%s", name, card)
		}
		if dispatchUTF16Len(card) > 1200 {
			t.Errorf("%s card is %d units", name, dispatchUTF16Len(card))
		}
	}
	if !strings.Contains(orchestrator, `message:"CRW-ROLE: reviewer\nTASK:`) || !strings.Contains(orchestrator, "CRW-ROLE: explorer") {
		t.Errorf("the orchestrator card does not show the header forms:\n%s", orchestrator)
	}
	if strings.Contains(engine, "CRW-ROLE") {
		t.Errorf("an engine call's card names a header:\n%s", engine)
	}
	if _, err := RenderFlowBoundDispatchCard("closed"); err == nil {
		t.Error("a card for a kind that is not bound")
	}

	// the SessionStart hook gives the card of the session's binding: flow-bound, closed or unregistered
	for _, c := range []struct {
		name, kind, session string
		want                string
	}{
		{"engine thread", bound.KindEngine, flowCardThread, engine},
		{"orchestrator thread", bound.KindOrchestrator, flowCardThread, orchestrator},
		{"unregistered thread", bound.KindEngine, "0190a000-0000-7000-8000-00000000000b", ordinary},
		{"session that is not a thread id", bound.KindEngine, "s", ordinary},
	} {
		t.Run(c.name, func(t *testing.T) {
			bindFlowCardThread(t, c.kind)
			var out strings.Builder
			raw := `{"session_id":"` + c.session + `","source":"startup"}`
			if code := RunFallbackNoticeHook(context.Background(), strings.NewReader(raw), &out, env, func(b []byte) string { return string(b) }); code != 0 {
				t.Fatalf("exit %d", code)
			}
			// the answer is a JSON string: compare through the notice it is built from
			var want strings.Builder
			notice, err := fallbackSessionNotice(env, func() (string, error) { return c.want, nil })
			if err != nil {
				t.Fatal(err)
			}
			want.WriteString(notice)
			if out.String() != want.String() {
				t.Fatalf("notice\n got %q\nwant %q", out.String(), want.String())
			}
		})
	}
}
