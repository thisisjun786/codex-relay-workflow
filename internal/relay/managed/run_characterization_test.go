package managed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/projectlock"
)

// These tests pin what a managed start does, step by step: the order of every host effect and readiness
// ask, which context each one carries, what each error does, every refusal of the final guard and the
// answers no other test reaches. They were written and run against Start.Run before it was split into
// named steps, and they hold the split to that behavior.

type ctxTag struct{}

func tagOf(ctx context.Context) string {
	if v, ok := ctx.Value(ctxTag{}).(string); ok {
		return v
	}
	return "-"
}

// tracer is the fake host with a record of every call the engine makes to it, in order, each with the
// tag of the context it carried ("run" for the context given to Run, "guard" for the one the final guard
// is called with, "-" for any other). failAt, when set, makes that call (counted from 1, readiness asks
// included) return failErr instead of being served.
type tracer struct {
	*managedFake
	events  []string
	calls   int
	failAt  int
	failErr error
	// sendProbe replaces the send: it is given the request, whose BeforeStart is the engine's final
	// guard, and the send then fails without reaching the fake.
	sendProbe func(SendRequest)
	// rewriteSend changes the receipt the fake answers a send with.
	rewriteSend func(map[string]any) map[string]any
	// before runs ahead of each call, with its 1-based number and name.
	before func(call int, name string)
}

func (tr *tracer) note(ctx context.Context, name string) error {
	tr.calls++
	tr.events = append(tr.events, name+"@"+tagOf(ctx))
	if tr.before != nil {
		tr.before(tr.calls, name)
	}
	if tr.calls == tr.failAt {
		return tr.failErr
	}
	return nil
}

func (tr *tracer) RequireLedger(ctx context.Context, expected map[string]any) error {
	if err := tr.note(ctx, "RequireLedger"); err != nil {
		return err
	}
	return tr.managedFake.RequireLedger(ctx, expected)
}

func (tr *tracer) LedgerIdentityRecord(ctx context.Context) (map[string]any, error) {
	if err := tr.note(ctx, "LedgerIdentityRecord"); err != nil {
		return nil, err
	}
	return tr.managedFake.LedgerIdentityRecord(ctx)
}

func (tr *tracer) GetOperation(ctx context.Context, id string) (map[string]any, error) {
	name := "GetOperation(dispatch)"
	switch {
	case strings.HasPrefix(id, "managed-create-"):
		name = "GetOperation(create)"
	case strings.HasPrefix(id, "managed-standby-"):
		name = "GetOperation(recovery)"
	}
	if err := tr.note(ctx, name); err != nil {
		return nil, err
	}
	return tr.managedFake.GetOperation(ctx, id)
}

func (tr *tracer) CreateThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	if err := tr.note(ctx, "CreateThread"); err != nil {
		return nil, err
	}
	return tr.managedFake.CreateThread(ctx, in)
}

func (tr *tracer) ReadTurn(ctx context.Context, task, turn string) (*Turn, error) {
	if err := tr.note(ctx, "ReadTurn"); err != nil {
		return nil, err
	}
	return tr.managedFake.ReadTurn(ctx, task, turn)
}

func (tr *tracer) Lifecycle(ctx context.Context, task, workspace string) (bool, string, error) {
	if err := tr.note(ctx, "Lifecycle"); err != nil {
		return false, "", err
	}
	return tr.managedFake.Lifecycle(ctx, task, workspace)
}

func (tr *tracer) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if err := tr.note(ctx, "HostCall("+method+")"); err != nil {
		return nil, err
	}
	return tr.managedFake.HostCall(ctx, method, params)
}

func (tr *tracer) SendMessage(ctx context.Context, in SendRequest) (map[string]any, error) {
	name := "SendMessage(business)"
	if strings.HasPrefix(in.RequestID, "managed-standby-") {
		name = "SendMessage(recovery)"
	}
	if err := tr.note(ctx, name); err != nil {
		return nil, err
	}
	if guard := in.BeforeStart; guard != nil {
		in.BeforeStart = func(c context.Context) (map[string]any, error) {
			return guard(context.WithValue(c, ctxTag{}, "guard"))
		}
	}
	if tr.sendProbe != nil {
		tr.sendProbe(in)
		return map[string]any{"status": "failed", "reason": "probe"}, nil
	}
	receipt, err := tr.managedFake.SendMessage(ctx, in)
	if err == nil && tr.rewriteSend != nil {
		receipt = tr.rewriteSend(receipt)
	}
	return receipt, err
}

// charRun is one managed start of the fixture request over a fresh store, driven through the tracer.
type charRun struct {
	t     *testing.T
	ctx   context.Context
	dir   string
	store *store.Store
	raw   []byte
	req   map[string]any
	fake  *managedFake
	tr    *tracer
	start *Start
	// restore, set by a mutation, undoes it once the final guard has answered.
	restore func()
}

const charRelationship = "(SELECT relationship_id FROM relationships WHERE issue_key='REL-MANAGED')"

func newCharRun(t *testing.T) *charRun {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	fake := &managedFake{operations: map[string]map[string]any{}, settings: pyjson.Map(pyjson.Map(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	tr := &tracer{managedFake: fake}
	x := &charRun{t: t, ctx: ctx, dir: dir, store: s, raw: raw, req: req, fake: fake, tr: tr}
	x.start = &Start{Store: s, Adapter: tr, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(ctx context.Context, _ map[string]any) (string, error) {
		if err := tr.note(ctx, "Readiness"); err != nil {
			return "", err
		}
		return "", nil
	}}
	return x
}

func (x *charRun) exec(query string, args ...any) {
	x.t.Helper()
	if _, err := x.store.DB.ExecContext(x.ctx, query, args...); err != nil {
		x.t.Fatalf("%s: %v", query, err)
	}
}

func (x *charRun) runRaw() (contract.OrderedObject, error) {
	return x.start.Run(context.WithValue(x.ctx, ctxTag{}, "run"), x.raw)
}

func fieldsOf(out contract.OrderedObject) map[string]any {
	got := map[string]any{}
	for _, f := range out {
		got[f.Key] = f.Value
	}
	return got
}

func (x *charRun) run() map[string]any {
	x.t.Helper()
	out, err := x.runRaw()
	if err != nil {
		x.t.Fatalf("Run: %v", err)
	}
	return fieldsOf(out)
}

func (x *charRun) identity() Identity {
	x.t.Helper()
	id, err := RequestIdentity(x.ctx, x.req, x.store, x.start.Socket, x.start.MarkerRoot, x.start.StateSelector, x.fake.ledger)
	if err != nil {
		x.t.Fatal(err)
	}
	return id
}

// takeEvents returns the calls recorded since the last take and forgets them.
func (x *charRun) takeEvents() []string {
	events := x.tr.events
	x.tr.events = nil
	return events
}

func checkAnswer(t *testing.T, got map[string]any, state, stage, reason string) {
	t.Helper()
	gotReason, _ := got["reason"].(string)
	if got["state"] != state || got["stage"] != stage || gotReason != reason {
		t.Fatalf("answer %v/%v/%v, want %s/%s/%s", got["state"], got["stage"], got["reason"], state, stage, reason)
	}
}

func checkEvents(t *testing.T, name string, got, want []string) {
	t.Helper()
	if want == nil {
		t.Fatalf("%s uncaptured: %q", name, got)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s calls\n%s\nwant\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

var happyPathEvents = []string{
	"LedgerIdentityRecord@run",
	"RequireLedger@run",
	"Readiness@run",
	"RequireLedger@run",
	"GetOperation(create)@run",
	"Readiness@run",
	"RequireLedger@run",
	"CreateThread@run",
	"RequireLedger@run",
	"GetOperation(dispatch)@run",
	"ReadTurn@run",
	"Lifecycle@run",
	"Readiness@run",
	"RequireLedger@run",
	"SendMessage(business)@run",
	"RequireLedger@guard",
	"Readiness@guard",
	"HostCall(thread/list)@guard",
	"HostCall(thread/list)@guard",
	"HostCall(thread/goal/get)@guard",
	"HostCall(thread/read)@guard",
}

// The calls of a start that goes through, in order. Each ask the engine makes of the host, and each
// readiness ask, is a point where the world can have moved: the order is the contract.
func TestRunCharacterization_HappyPathCallOrder(t *testing.T) {
	x := newCharRun(t)
	got := x.run()
	checkAnswer(t, got, "admitted", "business_accepted", "")
	checkEvents(t, "happy path", x.takeEvents(), happyPathEvents)
}

var retryIncompleteEvents = []string{
	"LedgerIdentityRecord@run",
	"RequireLedger@run",
	"Readiness@run",
	"RequireLedger@run",
	"GetOperation(create)@run",
	"Readiness@run",
	"RequireLedger@run",
	"CreateThread@run",
	"RequireLedger@run",
	"GetOperation(dispatch)@run",
	"ReadTurn@run",
}
var retryCompletedEvents = []string{
	"LedgerIdentityRecord@run",
	"RequireLedger@run",
	"Readiness@run",
	"RequireLedger@run",
	"GetOperation(create)@run",
	"RequireLedger@run",
	"GetOperation(dispatch)@run",
	"ReadTurn@run",
	"Lifecycle@run",
	"Readiness@run",
	"RequireLedger@run",
	"SendMessage(business)@run",
	"RequireLedger@guard",
	"Readiness@guard",
	"HostCall(thread/list)@guard",
	"HostCall(thread/list)@guard",
	"HostCall(thread/goal/get)@guard",
	"HostCall(thread/read)@guard",
}
var replayEvents = []string{
	"LedgerIdentityRecord@run",
	"RequireLedger@run",
	"Readiness@run",
	"RequireLedger@run",
	"GetOperation(create)@run",
	"RequireLedger@run",
	"GetOperation(dispatch)@run",
}
var recoveryEvents = []string{
	"LedgerIdentityRecord@run",
	"RequireLedger@run",
	"Readiness@run",
	"RequireLedger@run",
	"GetOperation(create)@run",
	"Readiness@run",
	"RequireLedger@run",
	"CreateThread@run",
	"RequireLedger@run",
	"GetOperation(recovery)@run",
	"Lifecycle@run",
	"Readiness@run",
	"RequireLedger@run",
	"SendMessage(recovery)@run",
	"RequireLedger@guard",
	"Readiness@guard",
	"RequireLedger@run",
	"GetOperation(dispatch)@run",
	"ReadTurn@run",
	"Lifecycle@run",
	"Readiness@run",
	"RequireLedger@run",
	"SendMessage(business)@run",
	"RequireLedger@guard",
	"Readiness@guard",
	"HostCall(thread/list)@guard",
	"HostCall(thread/list)@guard",
	"HostCall(thread/goal/get)@guard",
	"HostCall(thread/read)@guard",
}

// A start whose standby turn is not finished is retried until it is, and the retry sends the business turn
// once; replaying the finished request neither creates nor sends again.
func TestRunCharacterization_RetryAndReplay(t *testing.T) {
	x := newCharRun(t)
	x.fake.standby = "inProgress"
	checkAnswer(t, x.run(), "incomplete", "standby", "standby_incomplete")
	checkEvents(t, "standby incomplete", x.takeEvents(), retryIncompleteEvents)
	if x.fake.created != 1 || x.fake.sent != 0 {
		t.Fatalf("created %d, sent %d, want 1, 0", x.fake.created, x.fake.sent)
	}
	x.fake.standby = "completed"
	checkAnswer(t, x.run(), "admitted", "business_accepted", "")
	checkEvents(t, "completed retry", x.takeEvents(), retryCompletedEvents)
	if x.fake.created != 1 || x.fake.sent != 1 {
		t.Fatalf("created %d, sent %d, want 1, 1", x.fake.created, x.fake.sent)
	}
	checkAnswer(t, x.run(), "admitted", "business_accepted", "")
	checkEvents(t, "replay", x.takeEvents(), replayEvents)
	if x.fake.created != 1 || x.fake.sent != 1 {
		t.Fatalf("replay created %d, sent %d, want 1, 1", x.fake.created, x.fake.sent)
	}
}

// A creation that failed after the thread started is recovered by sending the standby turn to the thread it
// left, before the business turn.
func TestRunCharacterization_StandbyRecoveryCallOrder(t *testing.T) {
	x := newCharRun(t)
	x.fake.partial = true
	checkAnswer(t, x.run(), "admitted", "business_accepted", "")
	checkEvents(t, "standby recovery", x.takeEvents(), recoveryEvents)
	if x.fake.created != 1 || x.fake.sent != 2 {
		t.Fatalf("created %d, sent %d, want 1, 2", x.fake.created, x.fake.sent)
	}
}

// Every call the engine makes to the host, and every readiness ask, that fails by its own error ends the
// start with that error and nothing after it; the request is then not left locked or half done, and the
// next try goes through. The one exception is inside the final guard, where a ledger that moved is not an
// error of the start but the reason the business turn is withheld.
func TestRunCharacterization_ErrorsPropagatePerCall(t *testing.T) {
	probe := newCharRun(t)
	probe.run()
	events := probe.takeEvents()
	sentinel := errors.New("injected host failure")
	for i, event := range events {
		call := i + 1
		t.Run(fmt.Sprintf("%02d %s", call, event), func(t *testing.T) {
			x := newCharRun(t)
			x.tr.failAt, x.tr.failErr = call, sentinel
			out, err := x.runRaw()
			if got := x.takeEvents(); len(got) != call {
				t.Fatalf("the start went on after the failed call: %q", got)
			}
			if event == "RequireLedger@guard" {
				if err != nil {
					t.Fatalf("the guard's ledger error ended the start with %v", err)
				}
				checkAnswer(t, fieldsOf(out), "incomplete", "business", "business_failed")
			} else if err != sentinel || out != nil {
				t.Fatalf("Run returned %v and error %v, want no receipt and the injected error", out, err)
			}
			x.tr.failAt = 0
			checkAnswer(t, x.run(), "admitted", "business_accepted", "")
		})
	}
}

// guardVerdict runs the start with the final guard probed by hand: the mutations are made just before the
// guard is called, the send fails without reaching the fake, and the verdict is the guard's answer, nil when
// it lets the send through. A restore set by a mutation runs once the guard has answered.
func (x *charRun) guardVerdict(mutations ...func(*charRun)) map[string]any {
	x.t.Helper()
	var verdict map[string]any
	probed := 0
	x.tr.sendProbe = func(in SendRequest) {
		probed++
		for _, mutate := range mutations {
			mutate(x)
		}
		var err error
		verdict, err = in.BeforeStart(context.Background())
		if err != nil {
			x.t.Errorf("the guard returned an error: %v", err)
		}
		if x.restore != nil {
			x.restore()
		}
	}
	checkAnswer(x.t, x.run(), "incomplete", "business", "business_failed")
	if probed != 1 || x.fake.sent != 0 {
		x.t.Fatalf("guard probed %d times, %d sends reached the host", probed, x.fake.sent)
	}
	return verdict
}

func sqlMutation(query string, args ...any) func(*charRun) {
	return func(x *charRun) { x.exec(query, args...) }
}

var (
	relationshipClosed  = sqlMutation("UPDATE relationships SET status='closed' WHERE issue_key='REL-MANAGED'")
	generationAdvanced  = sqlMutation("UPDATE relationships SET execution_generation=2 WHERE issue_key='REL-MANAGED'")
	rootsNotJSON        = sqlMutation("UPDATE relationships SET artifact_roots='not json' WHERE issue_key='REL-MANAGED'")
	recipientsNotJSON   = sqlMutation("UPDATE relationships SET allowed_recipients='not json' WHERE issue_key='REL-MANAGED'")
	scopeMoved          = sqlMutation("UPDATE relationships SET scope_ref='issue:other' WHERE issue_key='REL-MANAGED'")
	childMoved          = sqlMutation("UPDATE relationships SET child_task_id='other-child' WHERE issue_key='REL-MANAGED'")
	dispatchTurnChanged = sqlMutation("UPDATE generations SET dispatch_turn_id='other-turn' WHERE relationship_id=" + charRelationship)
	dispatchIDChanged   = sqlMutation("UPDATE generations SET dispatch_request_id='other-request' WHERE relationship_id=" + charRelationship)
	generationRowGone   = sqlMutation("DELETE FROM generations WHERE relationship_id=" + charRelationship)
	modeLegacy          = sqlMutation("UPDATE verification_mode SET mode='legacy' WHERE relationship_id=" + charRelationship)
	modeGone            = sqlMutation("DELETE FROM verification_mode WHERE relationship_id=" + charRelationship)
	parentSettingsGone  = sqlMutation("DELETE FROM authorized_settings WHERE task_id='parent'")
	childSettingsGone   = sqlMutation("DELETE FROM authorized_settings WHERE task_id='child-new'")
	parentSettingsJunk  = sqlMutation("UPDATE authorized_settings SET settings='not json' WHERE task_id='parent'")
	childSettingsOther  = sqlMutation("UPDATE authorized_settings SET settings='{\"cwd\":\"/elsewhere\"}' WHERE task_id='child-new'")
	criterionRetitled   = sqlMutation("UPDATE canonical_criteria SET title='other' WHERE relationship_id=" + charRelationship)
	criterionOptional   = sqlMutation("UPDATE canonical_criteria SET required=0 WHERE relationship_id=" + charRelationship)
	criterionExtra      = sqlMutation("INSERT INTO canonical_criteria(relationship_id,criterion_id,title,required,source_ref,set_digest,recorded_at) SELECT relationship_id,'c2','extra',1,source_ref,set_digest,recorded_at FROM canonical_criteria WHERE relationship_id=" + charRelationship)
	criteriaGone        = sqlMutation("DELETE FROM canonical_criteria WHERE relationship_id=" + charRelationship)
	criteriaOtherSource = sqlMutation("UPDATE canonical_criteria SET source_ref='issue:other' WHERE relationship_id=" + charRelationship)
	criteriaOtherDigest = sqlMutation("UPDATE canonical_criteria SET set_digest='other' WHERE relationship_id=" + charRelationship)
	ledgerReplaced      = func(x *charRun) { x.fake.ledger = map[string]any{"realPath": "/elsewhere", "device": 9, "inode": 9} }
)

const (
	withheldPrefix = "Managed business start withheld: "
	hostPrefix     = "Managed turn withheld: "
)

func checkVerdict(t *testing.T, verdict map[string]any, code, prefix string) {
	t.Helper()
	if verdict == nil || verdict["code"] != code || verdict["message"] != prefix+code {
		t.Fatalf("verdict %v, want %s%s", verdict, prefix, code)
	}
}

// Each way the world can have moved between the standby turn and the business send is a refusal of the
// final guard, with its own code.
func TestRunCharacterization_FinalGuardRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		code   string
		mutate func(*charRun)
	}{
		{"relationship closed", "relationship_not_active", relationshipClosed},
		{"generation advanced", "stale_generation", generationAdvanced},
		{"roots not json", "managed_scope_changed", rootsNotJSON},
		{"recipients not json", "managed_scope_changed", recipientsNotJSON},
		{"scope moved", "managed_scope_changed", scopeMoved},
		{"child moved", "managed_scope_changed", childMoved},
		{"dispatch turn changed", "managed_identity_changed", dispatchTurnChanged},
		{"dispatch request changed", "managed_identity_changed", dispatchIDChanged},
		{"generation row gone", "managed_identity_changed", generationRowGone},
		{"mode legacy", "managed_criteria_changed", modeLegacy},
		{"mode gone", "managed_criteria_changed", modeGone},
		{"parent settings gone", "managed_settings_changed", parentSettingsGone},
		{"child settings gone", "managed_settings_changed", childSettingsGone},
		{"parent settings not json", "managed_settings_changed", parentSettingsJunk},
		{"child settings changed", "managed_settings_changed", childSettingsOther},
		{"criterion retitled", "managed_criteria_changed", criterionRetitled},
		{"criterion optional", "managed_criteria_changed", criterionOptional},
		{"criterion added", "managed_criteria_changed", criterionExtra},
		{"criteria gone", "managed_criteria_changed", criteriaGone},
		{"criteria other source", "managed_criteria_changed", criteriaOtherSource},
		{"criteria other digest", "managed_criteria_changed", criteriaOtherDigest},
		{"ledger replaced", "managed_store_changed", ledgerReplaced},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newCharRun(t)
			checkVerdict(t, x.guardVerdict(c.mutate), c.code, withheldPrefix)
		})
	}
}

// With nothing moved the guard lets the send through.
func TestRunCharacterization_FinalGuardPassesAnUnchangedStart(t *testing.T) {
	x := newCharRun(t)
	if verdict := x.guardVerdict(); verdict != nil {
		t.Fatalf("guard refused an unchanged start: %v", verdict)
	}
}

// A store file replaced under its path is a different store, whatever it holds.
func TestRunCharacterization_FinalGuardRefusesAReplacedStoreFile(t *testing.T) {
	x := newCharRun(t)
	path := x.store.Path
	replace := func(x *charRun) {
		if err := os.Rename(path, path+".moved"); err != nil {
			x.t.Fatal(err)
		}
		f, err := os.Create(path)
		if err != nil {
			x.t.Fatal(err)
		}
		f.Close()
		x.restore = func() {
			if err := os.Remove(path); err != nil {
				x.t.Error(err)
			}
			if err := os.Rename(path+".moved", path); err != nil {
				x.t.Error(err)
			}
		}
	}
	checkVerdict(t, x.guardVerdict(replace), "managed_store_changed", withheldPrefix)
}

// When two things moved, the guard names the first it checks.
func TestRunCharacterization_FinalGuardRefusalOrder(t *testing.T) {
	for _, c := range []struct {
		name      string
		code      string
		mutations []func(*charRun)
	}{
		{"relationship over settings", "relationship_not_active", []func(*charRun){parentSettingsGone, relationshipClosed}},
		{"relationship over generation", "relationship_not_active", []func(*charRun){generationAdvanced, relationshipClosed}},
		{"generation over scope", "stale_generation", []func(*charRun){rootsNotJSON, generationAdvanced}},
		{"scope over identity", "managed_scope_changed", []func(*charRun){dispatchTurnChanged, rootsNotJSON}},
		{"identity over mode", "managed_identity_changed", []func(*charRun){modeLegacy, dispatchTurnChanged}},
		{"mode over settings", "managed_criteria_changed", []func(*charRun){parentSettingsGone, modeLegacy}},
		{"settings over criteria", "managed_settings_changed", []func(*charRun){criterionRetitled, childSettingsGone}},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newCharRun(t)
			checkVerdict(t, x.guardVerdict(c.mutations...), c.code, withheldPrefix)
		})
	}
}

// The host's own answer about the thread is the last thing the guard asks, and its refusal has its own wording.
func TestRunCharacterization_FinalGuardRefusesByTheHostsAnswer(t *testing.T) {
	for _, scenario := range []string{"recipient_archived", "recipient_paused", "lifecycle_unknown", "recipient_not_idle", "recipient_cannot_accept_input"} {
		t.Run(scenario, func(t *testing.T) {
			x := newCharRun(t)
			x.fake.hostScenario = scenario
			checkVerdict(t, x.guardVerdict(), scenario, hostPrefix)
		})
	}
	x := newCharRun(t)
	x.fake.hostScenario = "recipient_paused"
	checkVerdict(t, x.guardVerdict(modeLegacy), "managed_criteria_changed", withheldPrefix)
}

// Two requests of one dispatch request id that disagree on the criteria source are refused as an
// intent conflict before any host effect.
func TestRunCharacterization_IntentConflictRefusesBeforeAnyHostEffect(t *testing.T) {
	x := newCharRun(t)
	id := x.identity()
	_, err := delivery.DeclareIntent(context.Background(), id.MarkerRoot, delivery.IntentDeclaration{Workspace: id.Workspace, DispatchRequestID: id.DispatchRequestID, IssueKey: id.IssueKey, DeclaredAt: "2026-09-26T00:00:00.000000+00:00", CriteriaSource: "issue:other", BaselineRevision: x.req["baselineRevision"], AuthorizedSettings: deliveryValue(pyjson.Map(pyjson.Map(x.req["child"])["settings"])), DBPath: x.store.Path})
	if err != nil {
		t.Fatal(err)
	}
	checkAnswer(t, x.run(), "refused", "intent", "intent_conflict")
	if x.fake.created != 0 || x.fake.sent != 0 {
		t.Fatalf("host effects %d/%d after an intent conflict", x.fake.created, x.fake.sent)
	}
}

// A creation answer that names no thread or no standby turn cannot be used and nothing is registered.
func TestRunCharacterization_UnobservedCreationIdentityIsIncomplete(t *testing.T) {
	for _, receipt := range []map[string]any{
		{"status": "accepted", "threadId": "", "turnId": "standby", "creation": map[string]any{}},
		{"status": "accepted", "threadId": "child-new", "turnId": "", "creation": map[string]any{}},
	} {
		x := newCharRun(t)
		x.fake.operations[x.identity().CreateRequestID] = receipt
		checkAnswer(t, x.run(), "incomplete", "creation", "creation_identity_unobserved")
		if x.fake.created != 0 || x.fake.sent != 0 {
			t.Fatalf("host effects %d/%d", x.fake.created, x.fake.sent)
		}
		var relationships int
		if err := x.store.DB.QueryRowContext(x.ctx, "SELECT COUNT(*) FROM relationships").Scan(&relationships); err != nil || relationships != 0 {
			t.Fatalf("relationships %d, %v", relationships, err)
		}
	}
}

// The assignment marker already bound to another task refuses the child the creation just made.
func TestRunCharacterization_MarkerBoundToAnotherTaskIsRefusedAtBinding(t *testing.T) {
	x := newCharRun(t)
	id := x.identity()
	x.fake.onCreate = func() {
		if _, err := delivery.BindIdentity(context.Background(), id.MarkerRoot, id.Workspace, delivery.AssignmentID(id.DispatchRequestID), "intruder", "intruder", "2026-09-26T00:00:00.000000+00:00"); err != nil {
			t.Error(err)
		}
	}
	checkAnswer(t, x.run(), "refused", "binding", "marker_identity_conflict")
	if x.fake.created != 1 || x.fake.sent != 0 {
		t.Fatalf("host effects %d/%d", x.fake.created, x.fake.sent)
	}
}

// What the host answers a send with is believed only when it names this thread and a turn.
func TestRunCharacterization_BusinessReceiptIdentityAndStatus(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason string
		set    func(*charRun)
	}{
		{"wrong thread", "business_identity_unobserved", func(x *charRun) {
			x.tr.rewriteSend = func(r map[string]any) map[string]any { r["threadId"] = "other-thread"; return r }
		}},
		{"no turn", "business_identity_unobserved", func(x *charRun) { x.fake.sendStatus = "accepted" }},
		{"unknown status", "business_unknown", func(x *charRun) { x.fake.sendStatus = "unknown" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newCharRun(t)
			c.set(x)
			checkAnswer(t, x.run(), "incomplete", "business", c.reason)
		})
	}
}

// A registration that no longer matches once the business turn was accepted is reported, not admitted.
func TestRunCharacterization_RegistrationChangedAfterTheSendIsIncomplete(t *testing.T) {
	x := newCharRun(t)
	x.fake.onSend = func(SendRequest) { scopeMoved(x) }
	checkAnswer(t, x.run(), "incomplete", "business_accepted", "managed_scope_changed")
	if x.fake.sent != 1 {
		t.Fatalf("sent %d, want 1", x.fake.sent)
	}
	var admitted int
	if err := x.store.DB.QueryRowContext(x.ctx, "SELECT COUNT(*) FROM events").Scan(&admitted); err != nil {
		t.Fatal(err)
	}
	t.Logf("events after an unadmitted send: %d", admitted)
}

// The registration is read back before the host is asked for anything more: a relationship that changed
// between its recording and the readback is refused at the readback. The clock is called inside store
// transactions too, so the hook must not touch the store there: a first run learns which call of the clock
// is the one made just after the child's settings were recorded (the settings carry the stamp of their call),
// and the second run moves the relationship on that call.
func TestRunCharacterization_RegistrationChangedBeforeReadbackIsRefused(t *testing.T) {
	stamp := func(n int) string { return fmt.Sprintf("2026-09-26T00:%02d:%02d.000000+00:00", n/60, n%60) }
	calls := 0
	probe := newCharRun(t)
	probe.start.Now = func() string { calls++; return stamp(calls) }
	probe.run()
	var recorded string
	if err := probe.store.DB.QueryRowContext(probe.ctx, "SELECT recorded_at FROM authorized_settings WHERE task_id='child-new'").Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	moveAt := 0
	for n := 1; n <= calls; n++ {
		if stamp(n) == recorded {
			moveAt = n + 1
		}
	}
	if moveAt == 0 || moveAt > calls {
		t.Fatalf("settings stamp %q is not one of %d clock calls", recorded, calls)
	}
	x := newCharRun(t)
	calls = 0
	moved := false
	x.start.Now = func() string {
		calls++
		if calls == moveAt {
			moved = true
			scopeMoved(x)
		}
		return stamp(calls)
	}
	type outcome struct {
		out contract.OrderedObject
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := x.runRaw()
		done <- outcome{out, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Run: %v", got.err)
		}
		checkAnswer(t, fieldsOf(got.out), "refused", "readback", "managed_scope_changed")
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	if !moved || x.fake.sent != 0 {
		t.Fatalf("relationship moved %v, sent %d", moved, x.fake.sent)
	}
}

// A receipt that cannot be journaled is returned together with the error that says so.
func TestRunCharacterization_ObserveFailureReturnsReceiptAndError(t *testing.T) {
	x := newCharRun(t)
	x.start.Readiness = func(context.Context, map[string]any) (string, error) {
		x.exec("DROP TABLE journal")
		return "worker_policy_unconfigured", nil
	}
	out, err := x.runRaw()
	if err == nil || out == nil {
		t.Fatalf("Run returned a receipt: %v, error: %v; want both", out != nil, err)
	}
	checkAnswer(t, fieldsOf(out), "refused", "preflight", "worker_policy_unconfigured")
}

// lockProbe asks, ahead of the two calls that bracket the registration, what the project lock looks like.
type lockProbe struct {
	*callRecord
	at func(name string)
}

func (p *lockProbe) CreateThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	p.at("CreateThread")
	return p.callRecord.CreateThread(ctx, in)
}

func (p *lockProbe) RequireLedger(ctx context.Context, expected map[string]any) error {
	p.at("RequireLedger")
	return p.callRecord.RequireLedger(ctx, expected)
}

// The project lock is held from before the creation until the child is registered and no longer; the
// request's own lock is held to the end.
func TestRunCharacterization_ProjectLockIsLetGoAtRegistrationAndRequestLockIsKept(t *testing.T) {
	x := newScopeRun(t)
	x.bind("parent")
	state := func() string {
		saved := ownership.LockWait
		ownership.LockWait = 150 * time.Millisecond
		defer func() { ownership.LockWait = saved }()
		release, err := projectlock.Exclusive(x.ctx, x.store.Path, scopeProject)
		var expired *ownership.LockWaitExpired
		switch {
		case err == nil:
			_ = release()
			return "free"
		case errors.As(err, &expired):
			return "held"
		}
		t.Errorf("project lock: %v", err)
		return "error"
	}
	var atCreation, afterRegistration string
	created := false
	probe := &lockProbe{callRecord: x.host}
	probe.at = func(name string) {
		switch {
		case name == "CreateThread":
			created = true
			atCreation = state()
		case created && afterRegistration == "":
			afterRegistration = state()
			release, err := Lock(x.store.Path, "managed-1")
			if err == nil {
				_ = release()
			}
			if !errors.Is(err, ErrBusy) {
				t.Errorf("the request lock was free after the registration: %v", err)
			}
		}
	}
	x.start.Adapter = probe
	if got := x.run(); got["state"] != "admitted" {
		t.Fatalf("answer %v", got)
	}
	if atCreation != "held" || afterRegistration != "free" {
		t.Fatalf("project lock at the creation: %s, after the registration: %s", atCreation, afterRegistration)
	}
}

// The final guard compares the relationship with the parent the start registered, the one the request named
// when the child was registered. A request that a readiness callback changes afterwards does not move it: the
// start goes on to send, and the registration that is read back after the send is the one that disagrees.
func TestRunCharacterization_GuardKeepsTheParentRegisteredWithTheChild(t *testing.T) {
	x := newCharRun(t)
	x.start.Readiness = func(ctx context.Context, req map[string]any) (string, error) {
		if tagOf(ctx) == "guard" {
			changed := map[string]any{}
			for key, value := range pyjson.Map(req["parent"]) {
				changed[key] = value
			}
			changed["taskId"] = "other-parent"
			req["parent"] = changed
		}
		return "", nil
	}
	checkAnswer(t, x.run(), "incomplete", "business_accepted", "managed_scope_changed")
	if x.fake.sent != 1 {
		t.Fatalf("sent %d, want 1", x.fake.sent)
	}
}
