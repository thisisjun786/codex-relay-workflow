package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestProjectDecisionRequestAnswerApplies (CRW-1574): the project lane asks a design question of the milestone; the child lifts it to
// the parent with `crw flow report-up --kind project_decision_request --question`; the parent records the answer with that report as
// its source (`--source report=<report id>`, no blocking entry, the manager's class); `crw flow answer` over the lab's store applies it:
// one decision_applied row for the question, and the question is answered.
func TestProjectDecisionRequestAnswerApplies(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	d.OpenQuestions = []ops.OpenQuestion{policyQuestion}
	l.Design = &d
	sender := l.wireUpward(t, true)
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	asks := designAsks(l)
	if len(asks) != 1 || !asks[0].Waits() {
		t.Fatalf("the design asks: %+v", asks)
	}
	q := asks[0]

	// the child lifts the question to the initiative
	evidence := filepath.Join(t.TempDir(), "request.md")
	if err := os.WriteFile(evidence, []byte("escalation "+q.ID+" r1 (design_question, ask design): "+policyQuestion.Question), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	env := flowcli.Environment{Stdout: &out, Stderr: &errOut, Sender: sender,
		Open: func(context.Context, string, string) (*store.Store, string, error) {
			st, err := store.Open(context.Background(), l.StorePath, "")
			return st, l.Root, err
		}}
	if code := flowcli.RunEnv(context.Background(), []string{"report-up", "--kind", report.KindProjectDecisionRequest, "--run", l.RunID, "--evidence", evidence, "--subject", "the flag in 1.0", "--question", q.ID}, env); code != flowcli.ExitOK {
		t.Fatalf("report-up: exit %d %s", code, errOut.String())
	}
	var up report.UpResult
	if err := json.Unmarshal(out.Bytes(), &up); err != nil || up.ReportID == "" {
		t.Fatalf("report-up printed %q: %v", out.String(), err)
	}

	// the parent records the answer with the report as its source
	rec := decisions.Record{
		Schema: decisions.Schema, DecisionID: "ud-pdr", Kind: decisions.KindPolicy, Context: "does the project ship the flag in 1.0?",
		Options:   []decisions.Option{{ID: "ship", Label: "ship it", Effect: "the flag is in 1.0"}, {ID: "hold", Label: "hold it", Effect: "the flag waits"}},
		Origin:    decisions.Origin{Project: upProject},
		Source:    decisions.Source{Kind: "report", Ref: up.ReportID},
		Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}, State: decisions.StateRaised, RaisedAt: "2026-10-16T04:05:00Z", Seen: []decisions.Seen{{At: "2026-10-16T04:05:00Z", Source: "test"}},
	}
	rec.Fingerprint = decisions.Fingerprint(rec.Context, rec.Blocking, rec.Options)
	ctx := context.Background()
	if _, _, err := l.Store.Raise(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Store.Update(ctx, rec.DecisionID, func(_ context.Context, r decisions.Record) (decisions.Record, error) {
		return decisions.ValidateAnswer(r, decisions.Answer{Option: "hold", By: "parent", Via: decisions.ViaManagementMessage, Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}})
	}); err != nil {
		t.Fatal(err)
	}

	// the parent applies it to the run, as a process of its own over the lab's store
	_ = l.Store.Close()
	out.Reset()
	errOut.Reset()
	code := flowcli.RunWith(ctx, flowcli.Default(), []string{"answer", "--state", l.Root, "--socket", l.Host.SocketPath, "--decision", rec.DecisionID, "--run", l.RunID,
		"--question", q.ID, "--revision", "1", "--thread", "parent", "--no-resume", "--json"}, strings.NewReader(""), &out, &errOut)
	l.Store = l.openStore()
	var reply flowcli.AnswerReply
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		t.Fatalf("answer printed %q: %v (%s)", out.String(), err, errOut.String())
	}
	if code != 0 || !reply.Applied || reply.Refusal != nil {
		t.Fatalf("the answer of the report-sourced record: exit %d %+v %s", code, reply, errOut.String())
	}
	events, _, err := journal.Load(ctx, l.Store, l.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var applied int
	for _, e := range events {
		if e.Event == ops.EvDecisionApplied && str(e.Data, "ask") == q.ID && str(e.Data, "node") == q.Node {
			applied++
		}
	}
	answered := flowcli.Questions(events)
	if applied != 1 || len(answered) != 1 || answered[0].Applied != rec.DecisionID {
		t.Errorf("after the answer: %d decision_applied rows for %s, questions %+v", applied, q.ID, answered)
	}
}
