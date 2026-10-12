package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestNightBatchStartsFromDecisionOnTempStore (CRW-1277 a, the manager's half): the document `crw manage batch decide --handoff FILE`
// records is the decision `crw flow start night-batch --decision D` starts from on a temporary store: the run's arguments are the
// decision's issues, plan and milestone, and the run pins the very bytes of the handoff the decision named by its sha256. A handoff file
// that changed after the decision starts nothing. (The run of the batch on the real world is the flow command's test of the same name.)
func TestNightBatchStartsFromDecisionOnTempStore(t *testing.T) {
	auditListHome(t)
	exe, record := batchDecideRelay(t, "raised", 0)
	old := batchDecideConfig
	batchDecideConfig = func(*Env) *Config { return &Config{Relay: coreRelay{State: "/relay/state", Socket: "/relay/sock"}} }
	defer func() { batchDecideConfig = old }()
	e, _, errOut := auditTestEnv(t)
	e.Executable = exe
	e.Now = func() time.Time { return time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC) }
	handoff := batchDecideHandoff(t, "M1", "CRW-11", "CRW-12")
	args := []string{"decide", "--plan", batchDecidePlan(t, "CRW-11", "CRW-12"), "--flow-plan", "plan-1", "--milestone", "M1", "--token-budget", "750000", "--deadline", "2026-10-13T06:00:00Z", "--by", "manager-task", "--handoff", handoff}
	if code := batchMain(context.Background(), e, args); code != 0 {
		t.Fatalf("decide: exit %d\n%s", code, errOut.String())
	}
	var document string
	raise := coreFakeCalls(t, record)[0]
	for i := 6; i+1 < len(raise); i += 2 {
		if raise[i] == "--context" {
			document = raise[i+1]
		}
	}
	if document == "" {
		t.Fatalf("the raise carries no document: %q", raise)
	}

	// the record the relay keeps, raised and answered run, in a temporary store
	state := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	rec := decisions.Record{Schema: decisions.Schema, DecisionID: "ud-night", Kind: decisions.KindPolicy, Context: document,
		Options:   []decisions.Option{{ID: flowcli.NightRun, Label: "run", Effect: "start"}, {ID: flowcli.NightSkip, Label: "skip", Effect: "nothing"}},
		Blocking:  []decisions.Blocking{{Kind: decisions.BlockingProject, Ref: "plan-1"}},
		Origin:    decisions.Origin{Project: "plan-1"},
		Source:    decisions.Source{Kind: "night_batch", Ref: "night-2026-10-12"},
		Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}, State: decisions.StateRaised, RaisedAt: "2026-10-12T20:00:00Z",
		Seen: []decisions.Seen{{At: "2026-10-12T20:00:00Z", Source: "test"}}}
	rec.Fingerprint = decisions.Fingerprint(rec.Context, rec.Blocking, rec.Options)
	if _, _, err := st.Raise(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(context.Background(), "ud-night", func(_ context.Context, r decisions.Record) (decisions.Record, error) {
		return decisions.ValidateAnswer(r, decisions.Answer{Option: flowcli.NightRun, By: "manager-task", Via: decisions.ViaManagementMessage, Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var spawned []flowcli.SpawnRequest
	deps := &flowcli.Deps{
		Now:    func() time.Time { return time.Date(2026, 10, 12, 21, 0, 0, 0, time.UTC) },
		Getenv: func(string) (string, bool) { return "", false },
		Reader: flowcli.OpenReader,
		Writer: flowcli.OpenWriter,
		Spawn: func(_ context.Context, req flowcli.SpawnRequest) (int, error) {
			spawned = append(spawned, req)
			return 4242, nil
		},
		Probe: flowcli.ProberFunc(func(context.Context, flowcli.PreflightRequest) []flowcli.Fact {
			var facts []flowcli.Fact
			for _, name := range flowcli.Facts {
				facts = append(facts, flowcli.Fact{Name: name, Pass: true, Reading: "fine"})
			}
			return facts
		}),
		Signal: func(int, syscall.Signal) error { return nil },
	}
	start := func() (int, string) {
		var out, errOut bytes.Buffer
		code := flowcli.RunWith(context.Background(), deps, []string{"start", "night-batch", "--decision", "ud-night", "--thread", "orchestrator", "--state", state},
			strings.NewReader(""), &out, &errOut)
		return code, out.String() + errOut.String()
	}
	if code, out := start(); code != 0 || len(spawned) != 1 {
		t.Fatalf("start: exit %d, %d processes\n%q", code, len(spawned), out)
	}
	spec := spawned[0].Spec
	var runArgs struct {
		Issues          []string
		Plan, Milestone string
		Handoff         flowcli.HandoffRef
	}
	if err := json.Unmarshal(spec.Args, &runArgs); err != nil || strings.Join(runArgs.Issues, ",") != "CRW-11,CRW-12" || runArgs.Plan != "plan-1" || runArgs.Milestone != "M1" {
		t.Fatalf("the run's arguments: %s %v", spec.Args, err)
	}
	raw, _ := os.ReadFile(handoff)
	pinned, _, err := flowcli.ReadHandoff(runArgs.Handoff)
	if err != nil || !bytes.Equal(pinned, raw) || runArgs.Handoff.Path != filepath.Join(flowcli.RunDir(state, spec.RunID), flowcli.HandoffFile) {
		t.Fatalf("the pinned handoff %+v: %v", runArgs.Handoff, err)
	}
	// the handoff file changes after the decision: the batch is not started from it
	if err := os.WriteFile(handoff, append(raw, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := start(); code == 0 || len(spawned) != 1 || !strings.Contains(out, "changed") {
		t.Fatalf("a handoff that changed after the decision: exit %d\n%s", code, out)
	}
}
