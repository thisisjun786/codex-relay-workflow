package delivery

// CRW-1194 (R4): `crw relay receive` composes claim, ack and verdict in one process. These tests drive
// the library behind it (Receive) over the delivery fixture and a fake host, and compare what it
// writes and refuses with what the three commands write and refuse run one after another.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// delivered is an event the host has been sent, with the turn the parent acknowledges it from.
type delivered struct {
	*fixture
	event string
	turn  string
}

// newDelivered brings a fixture to the state a receive starts from: a registered relationship whose
// current revision is a final ready_for_review receipt dispatched to the parent, and a turn of the
// parent's own to acknowledge it from.
func newDelivered(t *testing.T) *delivered {
	t.Helper()
	f := newFixture(t, "")
	rid := f.register(regOpts{recipients: []string{parent, child}})
	payload := f.readyPayload(rid, 1, []string{f.artifact("out.txt", "the deliverable")}, 1, assigned("completed"))
	_, err := f.accept(payload, store.AcceptOptions{})
	mustDo(t, err)
	event := pyjson.Text(payload.Get("eventId"))
	_, err = f.delivery.Enqueue(f.ctx, event, "", "")
	mustDo(t, err)
	f.mustAttempt(event, nil)
	f.clock.Advance(5)
	turn := f.host.startTurn(parent, "ack-turn", "inProgress", "")
	return &delivered{fixture: f, event: event, turn: turn.TurnID}
}

// hostAck is the ack step with a host: the delivery is confirmed through the acknowledging turn
// first, as `ack --socket` does.
func (d *delivered) hostAck(ack *Ack) AckStep {
	rc := NewReconciler(ack.Delivery)
	return func(ctx context.Context, event, turn, proof string) (Obj, error) {
		return AckCommand(ctx, ack, rc, d.host, event, turn, proof, nil)
	}
}

// offlineAck is the ack step without a host: the acknowledgement is recorded as the parent's intent.
func offlineAck(ack *Ack) AckStep {
	return func(ctx context.Context, event, turn, proof string) (Obj, error) {
		return AckCommand(ctx, ack, nil, nil, event, turn, proof, nil)
	}
}

func (d *delivered) newAck() *Ack { return NewAck(NewService(d.store, d.clock)) }

// verdictRun is the line a `verdict` command is given, as the receive's verdict file spells it.
func verdictRun(event, turn string, fields map[string]any) VerdictRequest {
	req := VerdictRequest{Event: event, VerdictTurn: turn}
	for key, value := range fields {
		switch key {
		case "verdict":
			req.Verdict = value.(string)
		case "reason":
			req.Reason = value
		case "finding":
			req.Finding = value.([]string)
		case "criterion":
			req.Criterion = value.([]string)
		case "verified-head":
			head := value.(string)
			req.VerifiedHead = &head
		case "restoration":
			restoration := value.(string)
			req.Restoration = &restoration
		}
	}
	return req
}

// stepsRun is claim, ack and verdict as three commands one after another, each by its own handler,
// stopping at the first that refuses.
func (d *delivered) stepsRun(req VerdictRequest, doAck func(*Ack) AckStep) (Obj, error) {
	d.t.Helper()
	ack := d.newAck()
	out := Obj{}
	claim, err := ack.ClaimVerification(d.ctx, d.event, d.turn)
	if err != nil {
		return out, err
	}
	out = append(out, F{Key: "claim", Value: claim})
	record, err := doAck(ack)(d.ctx, d.event, d.turn, AckProof(d.event, d.turn))
	if err != nil {
		return out, err
	}
	out = append(out, F{Key: "ack", Value: record})
	run := &cliRun{command: "verdict", ctx: d.ctx, store: d.store, clock: d.clock, args: verdictArgs(req)}
	verdict, err := cmdVerdict(run)
	if err != nil {
		return out, err
	}
	return append(out, F{Key: "verdict", Value: verdict}), nil
}

// verdictArgs is the view cmdVerdict has of a verdict line.
func verdictArgs(req VerdictRequest) map[string]any {
	args := map[string]any{"--event": req.Event, "--verdict": req.Verdict, "--verdict-turn": req.VerdictTurn,
		"--criterion": req.Criterion, "--finding": req.Finding, "--reason": req.Reason, "--expect-criteria-digest": req.ExpectCriteriaDigest}
	if req.Criteria != nil {
		args["--criteria"] = *req.Criteria
	}
	if req.Restoration != nil {
		args["--restoration"] = *req.Restoration
	}
	if req.VerifiedHead != nil {
		args["--verified-head"] = *req.VerifiedHead
	}
	return args
}

// receiveRun is the same three steps as one Receive.
func (d *delivered) receiveRun(req VerdictRequest, doAck func(*Ack) AckStep) (Obj, error) {
	d.t.Helper()
	ack := d.newAck()
	return Receive(d.ctx, ack, ReceiveRequest{Event: d.event, AckTurn: d.turn, Verdict: req}, doAck(ack))
}

// norm spells alike what differs between two fixtures: their tree, and the event ids, revision hashes
// and proofs derived from it.
func (d *delivered) norm(text string) string {
	d.t.Helper()
	var pairs []string
	events, err := all(d.ctx, d.store, "SELECT event_id, revision_hash FROM events ORDER BY rowid")
	mustDo(d.t, err)
	seen := map[string]bool{}
	for i, e := range events {
		id := e.S("event_id")
		pairs = append(pairs, AckProof(id, d.turn), fmt.Sprintf("<proof %d>", i), id, fmt.Sprintf("<event %d>", i))
		if revision := e.S("revision_hash"); !seen[revision] {
			seen[revision] = true
			pairs = append(pairs, revision, fmt.Sprintf("<revision %d>", len(seen)))
		}
	}
	text = strings.NewReplacer(append([]string{d.tree, "<tree>"}, pairs...)...).Replace(text)
	return requestIDs.ReplaceAllString(text, "del-<request>-a$1")
}

// requestIDs are the delivery requests' ids, which are derived from the event's.
var requestIDs = regexp.MustCompile(`del-[0-9a-f]+-a(\d+)`)

// dump is every non-empty table of the fixture's store, normalised.
func (d *delivered) dump() string {
	d.t.Helper()
	raw, err := json.Marshal(d.tables())
	mustDo(d.t, err)
	return d.norm(string(raw))
}

// firstDifference shows where two dumps part.
func firstDifference(got, want string) string {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	from := max(0, i-120)
	return fmt.Sprintf("at byte %d\ngot:  ...%s\nwant: ...%s", i, got[from:min(len(got), i+200)], want[from:min(len(want), i+200)])
}

func (d *delivered) answer(out Obj) string {
	d.t.Helper()
	raw, err := json.Marshal(jsonable(out))
	mustDo(d.t, err)
	return d.norm(string(raw))
}

// A receive writes the records the three commands write, and answers what they answer.
func TestReceiveComposesSameRecordsAsSteps(t *testing.T) {
	t.Parallel()
	for name, fields := range map[string]map[string]any{
		"verified":      {"verdict": "verified", "verified-head": "0123456789abcdef0123456789abcdef01234567"},
		"needs_changes": {"verdict": "needs_changes", "finding": []string{"c1=needs_changes:fix the shape"}},
		"unverified":    {"verdict": "unverified", "reason": "the host went away"},
	} {
		t.Run(name, func(t *testing.T) {
			steps, composed := newDelivered(t), newDelivered(t)
			wantOut, err := steps.stepsRun(verdictRun(steps.event, steps.turn, fields), steps.hostAck)
			mustDo(t, err)
			gotOut, err := composed.receiveRun(verdictRun(composed.event, composed.turn, fields), composed.hostAck)
			mustDo(t, err)
			if len(gotOut) != 3 || gotOut[0].Key != "claim" || gotOut[1].Key != "ack" || gotOut[2].Key != "verdict" {
				t.Fatalf("the answer names the three steps: %v", gotOut)
			}
			if got, want := composed.answer(gotOut), steps.answer(wantOut); got != want {
				t.Errorf("the answer differs from the steps' answers\ngot:  %s\nwant: %s", got, want)
			}
			if got, want := composed.dump(), steps.dump(); got != want {
				t.Errorf("the store differs from the one the steps left %s", firstDifference(got, want))
			}
			if claim := pyjson.Text(gotOut.Get("claim")); claim != "proceed" {
				t.Errorf("claim = %q", claim)
			}
			ack := gotOut.Get("ack").(Obj)
			if pyjson.Text(ack.Get("_verified")) != "verified" || pyjson.Text(ack.Get("ackTurnId")) != composed.turn {
				t.Errorf("ack = %v", ack)
			}
			if composed.count("SELECT COUNT(*) AS c FROM verification_claims WHERE event_id = ?", composed.event) != 1 ||
				composed.count("SELECT COUNT(*) AS c FROM acks WHERE event_id = ? AND verified = 'verified'", composed.event) != 1 ||
				composed.count("SELECT COUNT(*) AS c FROM verdicts WHERE event_id = ?", composed.event) != 1 {
				t.Errorf("one claim, one verified ack and one verdict are recorded")
			}
		})
	}
}

// A repeated receive is a replay of each step: the claim is already held, the acknowledgement is
// settled and the verdict is the one on record.
func TestReceiveRepeatedIsAReplayOfEachStep(t *testing.T) {
	t.Parallel()
	d := newDelivered(t)
	req := verdictRun(d.event, d.turn, map[string]any{"verdict": "verified"})
	_, err := d.receiveRun(req, d.hostAck)
	mustDo(t, err)
	before := d.dump()
	again, err := d.receiveRun(req, d.hostAck)
	mustDo(t, err)
	if claim := pyjson.Text(again.Get("claim")); claim != "already_claimed" {
		t.Errorf("claim = %q", claim)
	}
	if !truthy(again.Get("verdict").(Obj).Get("_replay")) {
		t.Errorf("the second verdict is not a replay: %v", again)
	}
	if d.dump() != before {
		t.Errorf("a repeated receive changed the store")
	}
}

// Every refusal is the refusal of the step that makes it, with that step's name and detail, and what
// the steps before it recorded stays recorded.
func TestReceiveRefusalsMatchSteps(t *testing.T) {
	t.Parallel()
	verified := map[string]any{"verdict": "verified"}
	cases := []struct {
		name   string
		setup  func(*delivered)
		fields map[string]any
		doAck  func(*delivered) func(*Ack) AckStep
		reason string
	}{
		{"an event nothing was delivered for", func(d *delivered) { d.event = strings.Repeat("ab", 16) }, verified, hostAcker, NotClaimable},
		{"an event not sent yet", func(d *delivered) {
			rid := d.rid
			payload := d.readyPayload(rid, 1, []string{d.artifact("second.txt", "a second deliverable")}, 2, assigned("completed"))
			_, err := d.accept(payload, store.AcceptOptions{})
			mustDo(d.t, err)
			d.event = pyjson.Text(payload.Get("eventId"))
		}, verified, hostAcker, NotClaimable},
		{"a turn the host does not show on the recipient's thread", func(d *delivered) {
			d.turn = d.host.startTurn(child, "child-turn", "inProgress", "").TurnID
		}, verified, hostAcker, NotAcknowledged},
		{"no host to verify the acknowledgement", func(*delivered) {}, verified, offlineAcker, NotAcknowledged},
		{"a verified ruling carrying a head on a needs_changes verdict", func(*delivered) {}, map[string]any{"verdict": "needs_changes", "verified-head": strings.Repeat("a", 40)}, hostAcker, DispositionConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			steps, composed := newDelivered(t), newDelivered(t)
			c.setup(steps)
			c.setup(composed)
			_, wantErr := steps.stepsRun(verdictRun(steps.event, steps.turn, c.fields), c.doAck(steps))
			_, gotErr := composed.receiveRun(verdictRun(composed.event, composed.turn, c.fields), c.doAck(composed))
			if wantErr == nil || Reason(wantErr) != c.reason {
				t.Fatalf("the steps refuse %v, wanted %s", wantErr, c.reason)
			}
			if gotErr == nil || Reason(gotErr) != Reason(wantErr) {
				t.Fatalf("receive refused %v, the step refused %v", gotErr, wantErr)
			}
			if got, want := composed.norm(gotErr.Error()), steps.norm(wantErr.Error()); got != want {
				t.Errorf("the refusal reads differently\ngot:  %s\nwant: %s", got, want)
			}
			if got, want := composed.dump(), steps.dump(); got != want {
				t.Errorf("the store differs from the one the steps left %s", firstDifference(got, want))
			}
		})
	}
}

func hostAcker(d *delivered) func(*Ack) AckStep  { return d.hostAck }
func offlineAcker(*delivered) func(*Ack) AckStep { return offlineAck }

// A verdict line the verdict command would refuse before it reads the store is refused by receive
// before it writes anything: no claim and no acknowledgement are left behind by it.
func TestReceiveRefusesAVerdictItCannotReadBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	for name, req := range map[string]VerdictRequest{
		"a restoration naming no finding": verdictRun("", "", map[string]any{"verdict": "needs_changes", "finding": []string{"c1=needs_changes:fix"}, "restoration": "c9"}),
		"a malformed verified head":       verdictRun("", "", map[string]any{"verdict": "verified", "verified-head": "not-a-head"}),
	} {
		t.Run(name, func(t *testing.T) {
			d := newDelivered(t)
			before := d.dump()
			req.Event, req.VerdictTurn = d.event, d.turn
			_, err := d.receiveRun(req, d.hostAck)
			if err == nil {
				t.Fatal("receive accepted a verdict the verdict command refuses")
			}
			if d.dump() != before {
				t.Errorf("a refused verdict line left records behind")
			}
		})
	}
}

// The proof is computed from the turn id the caller gives and nothing else: the library takes no
// proof, the ack step is handed AckProof of the caller's own turn, and the record names that turn.
func TestReceiveProofFromCallerTurnOnly(t *testing.T) {
	t.Parallel()
	for _, field := range reflect.VisibleFields(reflect.TypeOf(ReceiveRequest{})) {
		if strings.Contains(strings.ToLower(field.Name), "proof") {
			t.Errorf("ReceiveRequest has a %s field: a caller must not be able to bring a proof", field.Name)
		}
	}
	d := newDelivered(t)
	ack := d.newAck()
	var seen []string
	spy := func(ctx context.Context, event, turn, proof string) (Obj, error) {
		seen = append(seen, event, turn, proof)
		return d.hostAck(ack)(ctx, event, turn, proof)
	}
	out, err := Receive(d.ctx, ack, ReceiveRequest{Event: d.event, AckTurn: d.turn, Verdict: verdictRun(d.event, d.turn, map[string]any{"verdict": "verified"})}, spy)
	mustDo(t, err)
	if !reflect.DeepEqual(seen, []string{d.event, d.turn, AckProof(d.event, d.turn)}) {
		t.Fatalf("the ack step was given %v", seen)
	}
	record := out.Get("ack").(Obj)
	if pyjson.Text(record.Get("ackProof")) != AckProof(d.event, d.turn) || pyjson.Text(record.Get("ackTurnId")) != d.turn {
		t.Errorf("the acknowledgement is not the caller's own turn's: %v", record)
	}
	if row := d.one("SELECT ack_turn_id FROM acks WHERE event_id = ?", d.event); row.S("ack_turn_id") != d.turn {
		t.Errorf("acks.ack_turn_id = %q", row.S("ack_turn_id"))
	}
	if row := d.one("SELECT verdict_turn_id FROM verdicts WHERE event_id = ?", d.event); row.S("verdict_turn_id") != d.turn {
		t.Errorf("the verdict is ruled from %q, not the caller's turn", row.S("verdict_turn_id"))
	}
	// a malformed event id or an empty turn is refused as ack-proof refuses it, before any step writes
	for _, c := range []struct{ event, turn, detail string }{
		{"nope", d.turn, "event id must be 32 lowercase hex characters"},
		{d.event, "  ", "ack_turn_id must be a non-empty string"},
	} {
		fresh := newDelivered(t)
		before := fresh.dump()
		_, err := Receive(fresh.ctx, fresh.newAck(), ReceiveRequest{Event: c.event, AckTurn: c.turn, Verdict: verdictRun(c.event, c.turn, map[string]any{"verdict": "verified"})}, func(context.Context, string, string, string) (Obj, error) {
			t.Fatal("the ack step ran")
			return nil, nil
		})
		if err == nil || !strings.Contains(err.Error(), c.detail) {
			t.Errorf("event %q turn %q: %v, wanted %q", c.event, c.turn, err, c.detail)
		}
		if fresh.dump() != before {
			t.Errorf("a refused proof left records behind")
		}
	}
}

// With a socket the command acks through the host, and without one it records the parent's intent
// and the verdict that follows is refused, as the three commands do.
func TestReceiveCommandAcksThroughTheHostWhenThereIsASocket(t *testing.T) {
	d := newDelivered(t)
	var hostCalls []string
	old := HostCommand
	HostCommand = func(ctx context.Context, command, state, socket string, args map[string]any, clock Clock) (any, error) {
		hostCalls = append(hostCalls, command+" "+socket)
		ack := d.newAck()
		return AckCommand(ctx, ack, NewReconciler(ack.Delivery), d.host, pyjson.Text(args["--event"]), pyjson.Text(args["--ack-turn"]), pyjson.Text(args["--ack-proof"]), args["--reject"])
	}
	t.Cleanup(func() { HostCommand = old })
	file := filepath.Join(t.TempDir(), "verdict.json")
	mustDo(t, os.WriteFile(file, []byte(`{"verdict":"verified"}`), 0o600))
	run := &cliRun{command: "receive", ctx: d.ctx, store: d.store, clock: d.clock, socket: "/sock/app.sock",
		args: map[string]any{"--event": d.event, "--ack-turn": d.turn, "--verdict-file": file}}
	out, err := cmdReceive(run)
	mustDo(t, err)
	if !reflect.DeepEqual(hostCalls, []string{"ack /sock/app.sock"}) {
		t.Fatalf("host calls: %v", hostCalls)
	}
	if verdict := out.(Obj).Get("verdict").(Obj); pyjson.Text(verdict.Get("verdict")) != "verified" {
		t.Fatalf("verdict = %v", verdict)
	}

	offline := newDelivered(t)
	run = &cliRun{command: "receive", ctx: offline.ctx, store: offline.store, clock: offline.clock,
		args: map[string]any{"--event": offline.event, "--ack-turn": offline.turn, "--verdict-file": file}}
	_, err = cmdReceive(run)
	requireReason(t, err, NotAcknowledged)
	if len(hostCalls) != 1 {
		t.Errorf("the host was called without a socket: %v", hostCalls)
	}
}

// The verdict file is read strictly: one object, only the verdict's own options, and no turn of its
// own (the verdict is ruled from the acknowledging turn).
func TestReceiveVerdictFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(text string) string {
		path := filepath.Join(dir, "v.json")
		mustDo(t, os.WriteFile(path, []byte(text), 0o600))
		return path
	}
	event, turn := strings.Repeat("ab", 16), "t1"
	req, err := ReadVerdictFile(write(`{"verdict":"needs_changes","finding":["c1=needs_changes:fix"],"criterion":["c2=verified"],"reason":"r","restoration":"c1","expect-criteria-digest":"d","criteria":[{"id":"c3","verdict":"verified"}]}`), event, turn)
	mustDo(t, err)
	if req.Event != event || req.VerdictTurn != turn || req.Verdict != "needs_changes" || !reflect.DeepEqual(req.Finding, []string{"c1=needs_changes:fix"}) ||
		!reflect.DeepEqual(req.Criterion, []string{"c2=verified"}) || req.Reason != "r" || req.ExpectCriteriaDigest != "d" || req.Restoration == nil || *req.Restoration != "c1" ||
		req.Criteria == nil || *req.Criteria != `[{"id": "c3", "verdict": "verified"}]` {
		t.Fatalf("read as %+v", req)
	}
	for name, text := range map[string]string{
		"not JSON":           `{`,
		"not an object":      `["verified"]`,
		"no verdict":         `{}`,
		"an unknown verdict": `{"verdict":"maybe"}`,
		"a verdict turn":     `{"verdict":"verified","verdict-turn":"other"}`,
		"an unknown key":     `{"verdict":"verified","ack-proof":"x"}`,
		"a mistyped list":    `{"verdict":"verified","finding":"c1=verified"}`,
		"a mistyped reason":  `{"verdict":"verified","reason":3}`,
		"a repeated key":     `{"verdict":"needs_changes","verdict":"verified"}`,
		"a repeated nested":  `{"verdict":"verified","criteria":[{"id":"c1","id":"c2"}]}`,
		"trailing data":      `{"verdict":"verified"}}not-json`,
		"a second document":  `{"verdict":"verified"} {"verdict":"verified"}`,
	} {
		if _, err := ReadVerdictFile(write(text), event, turn); err == nil {
			t.Errorf("%s was read", name)
		}
	}
	if _, err := ReadVerdictFile(filepath.Join(dir, "absent.json"), event, turn); err == nil {
		t.Error("an absent file was read")
	}
}

// claim, ack and verdict answer what they answered before receive existed: their handlers, driven
// over the fixture, give the recorded golden (taken from the handlers before verdict was split into
// the reading and the recording that receive shares).
func TestExistingCommandsGoldenUnchanged(t *testing.T) {
	t.Parallel()
	d := newDelivered(t)
	run := func(command string, args map[string]any, handler func(*cliRun) (any, error)) any {
		out, err := handler(&cliRun{command: command, ctx: d.ctx, store: d.store, clock: d.clock, args: args})
		if err != nil {
			return map[string]any{"reason": Reason(err), "error": err.Error()}
		}
		return out
	}
	hostAck := func(c *cliRun) (any, error) {
		d.t.Helper()
		ack := NewAck(NewService(d.store, d.clock))
		return AckCommand(d.ctx, ack, NewReconciler(ack.Delivery), d.host, d.event, d.turn, AckProof(d.event, d.turn), nil)
	}
	verified := map[string]any{"--event": d.event, "--verdict": "verified", "--verdict-turn": "v"}
	answers := []any{
		run("claim", map[string]any{"--event": d.event, "--turn": d.turn}, cmdClaim),
		run("claim", map[string]any{"--event": d.event, "--turn": d.turn}, cmdClaim),
		run("verdict", verified, cmdVerdict),
		run("ack", map[string]any{"--event": d.event, "--ack-turn": d.turn, "--ack-proof": "wrong"}, cmdAck),
		run("ack", nil, hostAck),
		run("verdict", verified, cmdVerdict),
		run("verdict", verified, cmdVerdict),
		run("verdict", map[string]any{"--event": d.event, "--verdict": "verified", "--verdict-turn": "v", "--restoration": ""}, cmdVerdict),
		run("verdict", map[string]any{"--event": d.event, "--verdict": "needs_changes", "--verdict-turn": "v", "--finding": []string{"c1=needs_changes:fix"}, "--restoration": "c9"}, cmdVerdict),
		run("verdict", map[string]any{"--event": d.event, "--verdict": "verified", "--verdict-turn": "v", "--verified-head": "nope"}, cmdVerdict),
		run("verdict", map[string]any{"--event": d.event, "--verdict": "unverified", "--verdict-turn": "v", "--criteria": "@/nonexistent/criteria.json"}, cmdVerdict),
	}
	raw, err := json.Marshal(jsonable(answers))
	mustDo(t, err)
	text := d.norm(string(raw))
	var value any
	mustDo(t, json.Unmarshal([]byte(text), &value))
	golden.CheckJSON(t, "existing claim ack verdict handlers", value)
}
