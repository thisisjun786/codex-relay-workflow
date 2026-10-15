package supervisor

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The hold rule (holdDecision) as one table, CRW-1486 round 4: every entry path that can set, keep or
// release a superseded_by_report hold, against every kind of that hold, after every change that
// matters to it. A row stages one message, sets its hold, applies its change, runs its entry, then
// runs one daemon supervisor pass (not for a fault notice, which its own deliverer sends), and reads
// whether the message still carries the hold and whether a turn was opened for it (an attempt of it
// that sent, by the entry or by that pass).
//
// Hold kinds: none; acknowledged (supervisor-acknowledge's park); derived (the channel's own,
// journaled as the channel journals it); notice (a fault notice's park).
// Worlds: event (an event completion, fixture24), direct (a lead report to a management seat),
// notice (a fault notice, newNoticeWorld).

type holdRuleRow struct {
	name                string
	world, hold, change string
	entry               string
	held, turn          bool
}

var holdRuleRows = []holdRuleRow{
	// An acknowledged event completion.
	{"ack same, staged by hand", "event", "acknowledged", "same", "stage", true, false},
	{"ack same, daemon pass (P1: the daemon re-stage)", "event", "acknowledged", "same", "daemon_pass", true, false},
	{"ack same, pass after a restart", "event", "acknowledged", "same", "restart_pass", true, false},
	{"ack resubmitted report, daemon pass", "event", "acknowledged", "resubmitted", "daemon_pass", false, true},
	{"ack report corrected in place, daemon pass (round-4 P2)", "event", "acknowledged", "corrected_in_place", "daemon_pass", false, true},
	{"ack report corrected in place, staged by hand", "event", "acknowledged", "corrected_in_place", "stage", false, true},
	{"ack other sender session, daemon pass (P1: sender rotation)", "event", "acknowledged", "other_sender", "daemon_pass", true, false},
	{"ack other sender session, staged by hand", "event", "acknowledged", "other_sender", "stage", true, false},
	{"ack other recipient, daemon pass", "event", "acknowledged", "other_recipient", "daemon_pass", false, true},
	{"ack same, sent by id", "event", "acknowledged", "same", "send_by_id", false, true},
	{"ack same, acknowledged again", "event", "acknowledged", "same", "acknowledge", true, false},
	{"ack released then acknowledged again, daemon pass (P1: newest row)", "event", "acknowledged", "released_then_acknowledged", "daemon_pass", true, false},
	{"ack released then acknowledged again, pass after a restart", "event", "acknowledged", "released_then_acknowledged", "restart_pass", true, false},
	// A hold the channel derived, on an event completion still owed.
	{"derived same, staged by hand", "event", "derived", "same", "stage", false, true},
	{"derived same, daemon pass", "event", "derived", "same", "daemon_pass", false, true},
	{"derived same, pass after a restart", "event", "derived", "same", "restart_pass", false, true},
	{"derived other sender session, daemon pass", "event", "derived", "other_sender", "daemon_pass", false, true},
	{"derived other recipient, daemon pass", "event", "derived", "other_recipient", "daemon_pass", false, true},
	{"derived same, sent by id", "event", "derived", "same", "send_by_id", false, true},
	{"derived same, acknowledge refused, then the pass re-derives it", "event", "derived", "same", "acknowledge", false, true},
	// No hold.
	{"none, acknowledged", "event", "none", "same", "acknowledge", true, false},
	{"none, daemon pass", "event", "none", "same", "daemon_pass", false, true},
	// An acknowledged lead report (the direct path).
	{"direct ack same, sender repeats", "direct", "acknowledged", "same", "direct_stage", true, false},
	{"direct ack other text, sender restates", "direct", "acknowledged", "changed", "direct_stage", false, true},
	{"direct ack other sender session repeats", "direct", "acknowledged", "other_sender", "direct_stage", true, false},
	{"direct ack other recipient, sender repeats", "direct", "acknowledged", "other_recipient", "direct_stage", false, true},
	{"direct ack same, daemon pass", "direct", "acknowledged", "same", "daemon_pass", true, false},
	{"direct ack same, sent by id", "direct", "acknowledged", "same", "send_by_id", false, true},
	{"direct derived same, sender repeats", "direct", "derived", "same", "direct_stage", false, true},
	// A fault notice: its park is set and released by its own channel, and never acknowledged.
	{"notice none, parked", "notice", "none", "same", "notice_park", true, false},
	{"notice park, its notification staged again", "notice", "notice", "same", "notice_stage", false, false},
	{"notice park, other recipient, staged again", "notice", "notice", "other_recipient", "notice_stage", false, false},
	{"notice park, acknowledge refused", "notice", "notice", "same", "acknowledge", true, false},
	{"notice park, daemon pass", "notice", "notice", "same", "daemon_pass", true, false},
}

func TestHoldRuleTable(t *testing.T) {
	for _, row := range holdRuleRows {
		t.Run(row.name, func(t *testing.T) {
			w := newHoldWorld(t, row.world)
			w.setHold(t, row.hold)
			w.apply(t, row.change)
			w.enter(t, row.entry)
			w.pass(t)
			held, turn := w.read(t)
			if held != row.held || turn != row.turn {
				t.Fatalf("%s/%s/%s/%s: held %v turn %v, want held %v turn %v", row.world, row.hold, row.change, row.entry, held, turn, row.held, row.turn)
			}
		})
	}
}

// holdWorld is one message in one world, the host its turns are opened on (a turn is an attempt of
// the message that sent), and the clock of its daemon passes.
type holdWorld struct {
	kind   string
	ctx    context.Context
	s      *store.Store
	c      *Channel
	id     string
	host   *guardHost
	now    float64
	f      *stageFixture
	notice *noticeWorld
	direct *storeRecipientWorld
	text   string
}

const holdRuleLead = "01leadNext"

func newHoldWorld(t *testing.T, kind string) *holdWorld {
	t.Helper()
	w := &holdWorld{kind: kind, host: newGuardHost(), now: guardNow}
	switch kind {
	case "event":
		w.f = fixture24(t)
		_, staged := w.f.staged(t)
		w.id, _ = staged["messageId"].(string)
		w.ctx, w.s, w.c = w.f.ctx, w.f.s, w.f.c
	case "direct":
		w.direct = newStoreRecipientWorld(t)
		w.direct.superviseInitiative()
		w.direct.manage(directManagement, directManagement, storeRecipientInitiative, storeRecipientSupervisor)
		w.ctx, w.s, w.c = w.direct.ctx, w.direct.s, w.direct.channel()
		w.text = "milestone one is done"
		w.id = w.stageDirect(t)
	case "notice":
		w.notice = newNoticeWorld(t)
		w.notice.seed(t, nsSeed{signature: nsRelationSig, scope: nsProjectScope})
		w.ctx, w.s, w.c, w.f = w.notice.ctx, w.notice.s, w.notice.c, w.notice.stageFixture
		w.id = w.stageNotice(t)
	}
	w.c.Settings = &delivery.TaskSettings{}
	return w
}

func (w *holdWorld) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (w *holdWorld) stageDirect(t *testing.T) string {
	t.Helper()
	answer, err := w.c.StageDirect(w.ctx, DirectMessage{Kind: KindLeadReport, Relationship: "initiative:" + storeRecipientInitiative, Subject: "lrp_1",
		Summary: "milestone one", Text: w.text, At: "2026-10-14T00:00:00.000000+00:00"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := answer["messageId"].(string)
	return id
}

func (w *holdWorld) stageNotice(t *testing.T) string {
	t.Helper()
	answer, err := w.notice.channel.StageNotice(w.ctx, w.notice.facts(t, nsID))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := answer["messageId"].(string)
	return id
}

func (w *holdWorld) recipient() string {
	if w.kind == "direct" {
		return directManagement
	}
	return "supervisor"
}

func (w *holdWorld) acknowledge(t *testing.T) {
	t.Helper()
	if _, err := w.c.Acknowledge(w.ctx, AcknowledgeRequest{Recipient: w.recipient(), Actor: w.recipient(), Messages: []string{w.id}, At: "2023-11-14T22:13:30.000000+00:00"}); err != nil {
		t.Fatal(err)
	}
}

// derive holds the message as the channel holds one whose obligation moved between the claim and the
// transport: the hold word and the channel's own supervisor_message_superseded row.
func (w *holdWorld) derive(t *testing.T) {
	t.Helper()
	w.exec(t, "UPDATE supervisor_messages SET hold_reason=? WHERE message_id=?", store.SupervisorHoldSuperseded, w.id)
	w.exec(t, `INSERT INTO journal(at,kind,subject,detail) VALUES('2023-11-14T22:13:40.000000+00:00','supervisor_message_superseded',?,'{"requestId": "r-derived", "detail": "what is owed moved"}')`, w.id)
}

func (w *holdWorld) setHold(t *testing.T, hold string) {
	t.Helper()
	switch hold {
	case "acknowledged":
		w.acknowledge(t)
	case "derived":
		w.derive(t)
	case "notice":
		if err := w.notice.channel.Park(w.ctx, w.id, "superseded elsewhere"); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *holdWorld) resubmit(t *testing.T) {
	t.Helper()
	w.exec(t, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done, corrected','merge','2023-11-14T22:14:00.000000+00:00')`)
}

func (w *holdWorld) stagePass(t *testing.T, at string) {
	t.Helper()
	if _, err := w.c.StageUnsent(w.ctx, "PRJ-1", at, 300); err != nil {
		t.Fatal(err)
	}
}

func (w *holdWorld) apply(t *testing.T, change string) {
	t.Helper()
	switch change {
	case "resubmitted":
		w.resubmit(t)
	case "corrected_in_place":
		w.exec(t, "UPDATE work_reports SET pr_number=11,head_sha='a1b2c3d4e5f60718293a4b5c6d7e8f9012345611' WHERE event_id='event-1' AND submission_no=1")
	case "changed":
		w.text = "milestone one is done, and two started"
	case "other_sender":
		if w.kind == "direct" {
			w.exec(t, "UPDATE scope_bindings SET task_id=? WHERE binding_id='bnd-supervisor'", holdRuleLead)
			w.exec(t, "UPDATE scope_links SET upper_task_id=? WHERE link_id='lnk-project'", holdRuleLead)
			w.exec(t, "UPDATE scope_links SET lower_task_id=? WHERE link_id=?", holdRuleLead, "lnk-m-"+directManagement+"-"+storeRecipientInitiative)
		} else {
			w.exec(t, "UPDATE scope_bindings SET task_id='parent2' WHERE binding_id='b-parent'")
			w.exec(t, "UPDATE scope_links SET upper_task_id='parent2' WHERE link_id='lnk-issue'")
			w.exec(t, "UPDATE scope_links SET lower_task_id='parent2' WHERE link_id='lnk-project'")
		}
		w.expectResolution(t, true)
	case "other_recipient":
		if w.kind == "direct" {
			w.exec(t, "UPDATE scope_bindings SET task_id='01mgmtNext' WHERE binding_id=?", "bnd-m-"+directManagement)
			w.exec(t, "UPDATE scope_links SET upper_task_id='01mgmtNext' WHERE link_id=?", "lnk-m-"+directManagement+"-"+storeRecipientInitiative)
		} else {
			w.exec(t, "UPDATE scope_bindings SET task_id='supervisor2' WHERE binding_id='b-supervisor'")
			w.exec(t, "UPDATE scope_links SET upper_task_id='supervisor2' WHERE link_id='lnk-project'")
		}
		w.expectResolution(t, false)
	case "released_then_acknowledged":
		// The acknowledgement's park is released by a resubmission, the channel derives a hold of its
		// own that a pass releases, and the message is acknowledged again: the hold it carries is the
		// second acknowledgement, read from the newest superseded row.
		w.resubmit(t)
		w.stagePass(t, "2023-11-14T22:14:20.000000+00:00")
		w.derive(t)
		w.stagePass(t, "2023-11-14T22:15:20.000000+00:00")
		if held, _ := w.read(t); held {
			t.Fatal("the derived hold between the two acknowledgements was not released")
		}
		w.acknowledge(t)
	}
}

// expectResolution checks that a change moved only the sender (sameRecipient) or the recipient.
func (w *holdWorld) expectResolution(t *testing.T, sameRecipient bool) {
	t.Helper()
	relation := "rel-1"
	if w.kind == "direct" {
		relation = "initiative:" + storeRecipientInitiative
	}
	r, err := w.c.Resolve(w.ctx, relation)
	row, rowErr := w.c.Get(w.ctx, w.id)
	if err != nil || rowErr != nil || (r.Recipient == row.RecipientTaskID) != sameRecipient || (sameRecipient && r.Sender == row.SenderTaskID) {
		t.Fatalf("the change did not move what it names: %+v %v (row %s -> %s)", r, err, row.SenderTaskID, row.RecipientTaskID)
	}
}

func (w *holdWorld) autoSend(t *testing.T, c *Channel) {
	t.Helper()
	w.now += 60
	if _, err := c.AutoSend(w.ctx, w.host, w.now, 10, 10, "", "", ""); err != nil {
		t.Fatal(err)
	}
}

func (w *holdWorld) enter(t *testing.T, entry string) {
	t.Helper()
	switch entry {
	case "stage":
		o := w.f.obligation(t)
		if _, err := w.c.Stage(w.ctx, o, "", "2023-11-14T22:16:20.000000+00:00"); err != nil {
			t.Fatal(err)
		}
	case "direct_stage":
		w.stageDirect(t)
	case "notice_stage":
		w.stageNotice(t)
	case "daemon_pass":
		w.autoSend(t, w.c)
	case "restart_pass":
		// A daemon started again holds nothing in memory: a fresh channel over the same store.
		fresh := &Channel{Store: w.s, Linkage: StoreLinkage{w.s}, Program: w.c.Program, Settings: &delivery.TaskSettings{}}
		w.autoSend(t, fresh)
	case "send_by_id":
		w.now += 60
		if _, err := w.c.Attempt(w.ctx, w.id, w.host, w.now); err != nil {
			t.Fatal(err)
		}
	case "acknowledge":
		w.acknowledge(t)
	case "notice_park":
		if err := w.notice.channel.Park(w.ctx, w.id, "superseded elsewhere"); err != nil {
			t.Fatal(err)
		}
	}
}

// pass is the daemon supervisor pass that follows every entry. A fault notice is sent by its own
// deliverer once its notification is reserved, not by this pass, so a notice row reads the hold its
// entry left and no pass follows.
func (w *holdWorld) pass(t *testing.T) {
	t.Helper()
	if w.kind == "notice" {
		return
	}
	w.autoSend(t, w.c)
}

func (w *holdWorld) read(t *testing.T) (held, turn bool) {
	t.Helper()
	row, err := w.c.Get(w.ctx, w.id)
	if err != nil {
		t.Fatal(err)
	}
	var sent int
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT count(*) FROM supervisor_attempts WHERE message_id=? AND send_attempted<>'no'", w.id).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	return row.HoldReason.String == store.SupervisorHoldSuperseded, sent > 0
}
