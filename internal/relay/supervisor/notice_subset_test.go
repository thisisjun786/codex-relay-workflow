package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// These tests pin what the supervisor channel does with a fault notice: the packet it composes, how it
// stages, re-addresses and parks the message, how it restates the packet at the claim, and the contact
// reading that decides whether the level above can be told. Each step records the raw stored packet text,
// every supervisor_messages row and every journal row (raw detail text included), so the stored bytes are
// what the golden holds. The goldens were recorded on the code as it stood when these tests were written,
// before the notice subset moved out of the faults package, and the move did not change them.

const (
	nsNow            = 1_700_000_000.0
	nsAt             = "2023-11-14T22:13:20.000000+00:00"
	nsFault          = "abc123"
	nsID             = "n1"
	nsProjectSig     = "{\"turn\": \"turn-1\"}"
	nsRelationSig    = "{\"relationship\": \"rel-1\", \"turn\": \"turn-1\"}"
	nsProjectScope   = "{\"projectKey\": \"PRJ-1\"}"
	nsMessageColumns = "SELECT * FROM supervisor_messages ORDER BY staged_at, message_id"
)

type noticeWorld struct {
	*stageFixture
	clock   *delivery.FakeClock
	ledger  *faults.Ledger
	channel NoticeChannel
}

func newNoticeWorld(t *testing.T) *noticeWorld {
	t.Helper()
	f := fixture24(t)
	clock := &delivery.FakeClock{T: nsNow}
	ledger := &faults.Ledger{Store: f.s, Clock: clock}
	return &noticeWorld{stageFixture: f, clock: clock, ledger: ledger, channel: NoticeChannel{Channel: f.c, Ledger: ledger}}
}

func (w *noticeWorld) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

type nsSeed struct {
	fault, notification, signature, scope, kind, state string
	lease                                              any
}

// seed writes one open fault of class report_omitted and one notification about it, the rows a raised
// notification has when the deliverer reads its facts.
func (w *noticeWorld) seed(t *testing.T, s nsSeed) {
	t.Helper()
	for field, def := range map[*string]string{&s.fault: nsFault, &s.notification: nsID, &s.kind: "blocking", &s.state: "pending"} {
		if *field == "" {
			*field = def
		}
	}
	w.exec(t, "INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)", s.fault, "crw", "report_omitted", "c", "broken", s.signature, s.scope, "P", "open", "t", "t", "t")
	w.exec(t, "INSERT INTO fault_notifications (notification_id, fault_id, product, kind, cycle, state, lease_until, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)", s.notification, s.fault, "crw", s.kind, 1, s.state, s.lease, "t", "t")
}

func (w *noticeWorld) facts(t *testing.T, notification string) map[string]any {
	t.Helper()
	notice, err := w.ledger.NoticeFacts(w.ctx, notification)
	if err != nil || notice == nil {
		t.Fatalf("notice facts %q: %v, %v", notification, notice, err)
	}
	return notice
}

func nsRowMap(r store.Row) map[string]any {
	m := map[string]any{}
	for _, column := range r {
		if raw, ok := column.Value.([]byte); ok {
			m[column.Name] = string(raw)
		} else {
			m[column.Name] = column.Value
		}
	}
	return m
}

// nsPlain turns what the code answers into values the golden encodes: rows become maps.
func nsPlain(v any) any {
	switch x := v.(type) {
	case store.Row:
		return nsPlain(nsRowMap(x))
	case map[string]any:
		if x == nil {
			return nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = nsPlain(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = nsPlain(e)
		}
		return out
	}
	return v
}

func nsError(err error) any {
	if err == nil {
		return nil
	}
	var notice *faults.NoticeError
	if errors.As(err, &notice) {
		return map[string]any{"type": "NoticeError", "kind": notice.Kind, "detail": notice.Detail}
	}
	var refusal Refusal
	if errors.As(err, &refusal) {
		return map[string]any{"type": "Refusal", "reason": refusal.Reason, "detail": refusal.Detail}
	}
	return map[string]any{"type": fmt.Sprintf("%T", err), "text": err.Error()}
}

// nsSnapshot is every supervisor_messages row (the packet as stored, and decoded beside it) and every
// journal row, in order.
func nsSnapshot(t *testing.T, ctx context.Context, s *store.Store) map[string]any {
	t.Helper()
	rows, err := s.All(ctx, nsMessageColumns)
	if err != nil {
		t.Fatal(err)
	}
	messages := []any{}
	for _, r := range rows {
		m := nsRowMap(r)
		var decoded any
		if err := json.Unmarshal([]byte(m["packet"].(string)), &decoded); err == nil {
			m["packetDecoded"] = decoded
		}
		messages = append(messages, m)
	}
	entries, err := s.All(ctx, "SELECT seq, at, kind, subject, detail FROM journal ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	journal := []any{}
	for _, r := range entries {
		journal = append(journal, nsRowMap(r))
	}
	return map[string]any{"messages": messages, "journal": journal}
}

type nsSteps struct {
	t *testing.T
	w *noticeWorld
	n int
}

func (s *nsSteps) record(name string, answer any, err error) {
	s.t.Helper()
	s.n++
	golden.CheckJSON(s.t, fmt.Sprintf("%02d %s", s.n, name), map[string]any{"answer": nsPlain(answer), "error": nsError(err), "state": nsSnapshot(s.t, s.w.ctx, s.w.s)}, golden.Substitute(s.w.root, "<root>"))
}

func (w *noticeWorld) stage(t *testing.T, steps *nsSteps, notification, name string) string {
	t.Helper()
	answer, err := w.channel.StageNotice(w.ctx, w.facts(t, notification))
	steps.record(name, answer, err)
	id, _ := answer["messageId"].(string)
	return id
}

func nsNotice() map[string]any {
	return map[string]any{"notificationId": "n1", "deliveryKey": "relay-notification:n1", "faultId": "abc123", "kind": "blocking", "reason": nil, "cycle": int64(1), "state": "pending", "leaseUntil": nil, "attempts": int64(0), "product": "crw", "faultClass": "report_omitted", "severity": "broken", "faultState": "open", "externalRef": nil, "issuePublished": false, "anchor": "rel-1", "issueKey": nil}
}

func nsLive() map[string]any {
	return map[string]any{"sender": "parent", "recipient": "supervisor", "projectKey": "PRJ-1"}
}

func TestNoticeSubset_Compose(t *testing.T) {
	cases := []struct {
		name         string
		notice, live func(map[string]any)
	}{
		{"blocking, no issue", nil, nil},
		{"blocking, with an issue", func(n map[string]any) { n["issueKey"] = "REL-1" }, nil},
		{"blocking, with a published reference", func(n map[string]any) {
			n["issueKey"] = "REL-1"
			n["externalRef"] = "CRW-9"
			n["issuePublished"] = true
		}, nil},
		{"blocking, published without a reference", func(n map[string]any) { n["issuePublished"] = true }, nil},
		{"blocking, with a reason", func(n map[string]any) { n["reason"] = "write 7 is failed" }, nil},
		{"blocking, cycle two", func(n map[string]any) { n["cycle"] = int64(2) }, nil},
		{"blocking, withdrawn fault", func(n map[string]any) { n["faultState"] = "withdrawn" }, nil},
		{"resolved", func(n map[string]any) { n["kind"] = "resolved" }, nil},
		{"decision, with a reason", func(n map[string]any) {
			n["kind"] = "decision"
			n["reason"] = "raised by a caller (its words are on the notification: fault-notifications)"
		}, nil},
		{"decision, without a reason", func(n map[string]any) { n["kind"] = "decision" }, nil},
		{"unfit hierarchy: project", nil, func(l map[string]any) { l["projectKey"] = "" }},
		{"unfit hierarchy: sender", nil, func(l map[string]any) { l["sender"] = "bad sender!" }},
		{"unfit hierarchy: recipient", nil, func(l map[string]any) { delete(l, "recipient") }},
		{"unfit notice: fault class", func(n map[string]any) { n["faultClass"] = "bad class" }, nil},
		{"unfit notice: product", func(n map[string]any) { n["product"] = "bad product" }, nil},
		{"unfit notice: severity", func(n map[string]any) { n["severity"] = "loud" }, nil},
		{"unfit notice: fault state", func(n map[string]any) { n["faultState"] = "weird" }, nil},
		{"unfit notice: kind", func(n map[string]any) { n["kind"] = "weird" }, nil},
		{"unfit notice: fault id", func(n map[string]any) { n["faultId"] = "XYZ" }, nil},
		{"issue is not an issue key", func(n map[string]any) { n["issueKey"] = "not an issue" }, nil},
	}
	for _, c := range cases {
		notice, live := nsNotice(), nsLive()
		if c.notice != nil {
			c.notice(notice)
		}
		if c.live != nil {
			c.live(live)
		}
		packet, err := composeNoticeForTest(notice, live, nsAt, "evidence line")
		golden.CheckJSON(t, c.name, map[string]any{"packet": nsPlain(packet), "error": nsError(err)})
	}
}

func TestNoticeSubset_StageByProject(t *testing.T) {
	w := newNoticeWorld(t)
	steps := &nsSteps{t: t, w: w}
	w.seed(t, nsSeed{signature: nsProjectSig, scope: nsProjectScope})
	message := w.stage(t, steps, nsID, "first stage")
	w.stage(t, steps, nsID, "staged again, nothing changed")
	w.exec(t, "UPDATE fault_ledger SET severity='degraded' WHERE fault_id=?", nsFault)
	w.stage(t, steps, nsID, "the fault changed")
	w.exec(t, "UPDATE scope_bindings SET task_id='supervisor-2' WHERE role='supervisor'")
	w.exec(t, "UPDATE scope_links SET upper_task_id='supervisor-2' WHERE upper_kind='initiative'")
	w.stage(t, steps, nsID, "the recipient moved")
	steps.record("park", nil, parkNoticeForTest(w.ctx, w.channel, message, "superseded elsewhere"))
	w.stage(t, steps, nsID, "staged after the park")
	w.exec(t, "UPDATE supervisor_messages SET hold_reason='hierarchy_unresolved' WHERE message_id=?", message)
	w.stage(t, steps, nsID, "staged after an unaddressed hold")
	w.exec(t, "UPDATE supervisor_messages SET hold_reason='held by an operator' WHERE message_id=?", message)
	w.stage(t, steps, nsID, "staged under a hold that is not the notice's")
	w.exec(t, "UPDATE supervisor_messages SET hold_reason=NULL WHERE message_id=?", message)
	w.exec(t, "UPDATE scope_bindings SET status='released' WHERE scope_kind='project'")
	w.stage(t, steps, nsID, "the project has no owner")
	w.exec(t, "UPDATE scope_bindings SET status='active' WHERE scope_kind='project'")
	notice := w.facts(t, nsID)
	notice["anchor"] = ""
	answer, err := w.channel.StageNotice(w.ctx, notice)
	steps.record("no anchor", answer, err)
}

// scriptedLinkage answers like the store's linkage walk, and lets a test change the answer of one call.
type scriptedLinkage struct {
	inner  Linkage
	script func(call int, reading map[string]any) (map[string]any, error)
	calls  int
}

func (l *scriptedLinkage) Up(ctx context.Context, relationshipID string) (map[string]any, error) {
	l.calls++
	reading, err := l.inner.Up(ctx, relationshipID)
	if err != nil {
		return nil, err
	}
	return l.script(l.calls, reading)
}

func TestNoticeSubset_StageByRelationship(t *testing.T) {
	w := newNoticeWorld(t)
	steps := &nsSteps{t: t, w: w}
	w.seed(t, nsSeed{signature: nsRelationSig, scope: nsProjectScope})
	message := w.stage(t, steps, nsID, "first stage")
	w.exec(t, "UPDATE supervisor_messages SET relationship_id='rel-old' WHERE message_id=?", message)
	w.stage(t, steps, nsID, "the relationship moved, the ends did not")
	w.exec(t, "UPDATE supervisor_messages SET state='dispatched' WHERE message_id=?", message)
	w.stage(t, steps, nsID, "dispatched, staged again, nothing changed")
	w.exec(t, "UPDATE fault_ledger SET severity='degraded' WHERE fault_id=?", nsFault)
	w.stage(t, steps, nsID, "dispatched, the fault changed")

	w.seed(t, nsSeed{fault: "def456", notification: "n2", signature: nsRelationSig, scope: nsProjectScope})
	drift := &scriptedLinkage{inner: StoreLinkage{w.s}, script: func(call int, reading map[string]any) (map[string]any, error) {
		if call == 2 {
			for _, level := range reading["levels"].([]any) {
				if l := level.(map[string]any); l["scopeKind"] == "initiative" {
					l["owner"] = map[string]any{"taskId": "supervisor-2"}
				}
			}
		}
		return reading, nil
	}}
	w.c.Linkage = drift
	answer, err := w.channel.StageNotice(w.ctx, w.facts(t, "n2"))
	steps.record("the owner changed under the lock", map[string]any{"linkageCalls": drift.calls, "answer": answer}, err)

	refused := &scriptedLinkage{inner: StoreLinkage{w.s}, script: func(call int, reading map[string]any) (map[string]any, error) {
		if call == 2 {
			return nil, errors.New("the linkage went away")
		}
		return reading, nil
	}}
	w.c.Linkage = refused
	answer, err = w.channel.StageNotice(w.ctx, w.facts(t, "n2"))
	steps.record("the linkage is refused under the lock", map[string]any{"linkageCalls": refused.calls, "answer": answer}, err)
}

// An advancing clock makes the two reads of StageNotice visible: the packet's observedAt is read first,
// the message's staged_at second.
func TestNoticeSubset_StageClockReadOrder(t *testing.T) {
	w := newNoticeWorld(t)
	steps := &nsSteps{t: t, w: w}
	w.seed(t, nsSeed{signature: nsRelationSig, scope: nsProjectScope})
	w.clock.OnISO = func() { w.clock.T += 0.001 }
	w.stage(t, steps, nsID, "first stage on an advancing clock")
}

// The evidence line of a packet names the program and the state directory, which can hold any character.
// The packet is stored with non-ASCII kept as it is and a byte that is not UTF-8 read as U+FFFD.
func TestNoticeSubset_StageProgramPathEncoding(t *testing.T) {
	for _, c := range []struct{ name, program string }{
		{"a program path that is not ASCII", "/opt/r\u00e9lay/codex-session-relay"},
		{"a program path with a byte that is not UTF-8", "/opt/\xff/codex-session-relay"},
	} {
		w := newNoticeWorld(t)
		steps := &nsSteps{t: t, w: w}
		w.c.Program = c.program
		w.seed(t, nsSeed{signature: nsProjectSig, scope: nsProjectScope})
		w.stage(t, steps, nsID, c.name)
	}
}

func TestNoticeSubset_Refresh(t *testing.T) {
	sending := "UPDATE supervisor_messages SET state='sending', lease_owner='daemon', lease_until=1700000300, attempt_count=1 WHERE message_id=?"
	changed := func(t *testing.T, w *noticeWorld, _ string) {
		w.exec(t, "UPDATE fault_ledger SET severity='degraded' WHERE fault_id=?", nsFault)
	}
	cases := []struct {
		name    string
		attempt int64
		change  func(t *testing.T, w *noticeWorld, message string)
	}{
		{"unchanged", 0, func(*testing.T, *noticeWorld, string) {}},
		{"the fault changed, message queued", 0, changed},
		{"the fault changed, message sending", 1, func(t *testing.T, w *noticeWorld, message string) {
			changed(t, w, message)
			w.exec(t, sending, message)
		}},
		{"the fault changed, an earlier attempt may have gone", 2, func(t *testing.T, w *noticeWorld, message string) {
			changed(t, w, message)
			w.exec(t, "INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, observed_at) VALUES ('req-1', ?, 1, 'm', 'withheld_pre_send', 'yes', 1, '{}', 't', 't')", message)
		}},
		{"the reservation lapsed", 0, func(t *testing.T, w *noticeWorld, _ string) {
			w.exec(t, "UPDATE fault_notifications SET lease_until=1699999000")
		}},
		{"the notification was not reserved", 0, func(t *testing.T, w *noticeWorld, _ string) {
			w.exec(t, "UPDATE fault_notifications SET state='pending'")
		}},
		{"the notification is gone", 0, func(t *testing.T, w *noticeWorld, _ string) {
			w.exec(t, "DELETE FROM fault_notifications")
		}},
		{"a blocking notice whose fault withdrew", 0, func(t *testing.T, w *noticeWorld, _ string) {
			w.exec(t, "UPDATE fault_ledger SET state='withdrawn'")
		}},
		{"the relationship is paused", 0, func(t *testing.T, w *noticeWorld, _ string) {
			w.exec(t, "UPDATE relationships SET status='paused'")
		}},
		{"the notice is about another relationship now", 0, func(t *testing.T, w *noticeWorld, message string) {
			w.exec(t, "UPDATE supervisor_messages SET relationship_id='rel-old' WHERE message_id=?", message)
		}},
	}
	for _, c := range cases {
		w := newNoticeWorld(t)
		w.seed(t, nsSeed{signature: nsRelationSig, scope: nsProjectScope, state: "reserved", lease: nsNow + 300})
		w.exec(t, "INSERT INTO recipient_lifecycle (task_id, deliverable, observed_at) VALUES ('parent','yes','2023-11-14T22:13:10.000000+00:00')")
		answer, err := w.channel.StageNotice(w.ctx, w.facts(t, nsID))
		if err != nil {
			t.Fatalf("%s: stage: %v", c.name, err)
		}
		message := answer["messageId"].(string)
		row, err := w.c.Get(w.ctx, message)
		if err != nil {
			t.Fatal(err)
		}
		live, err := w.c.Resolve(w.ctx, row.RelationshipID)
		if err != nil {
			t.Fatal(err)
		}
		c.change(t, w, message)
		row, err = w.c.Get(w.ctx, message)
		if err != nil {
			t.Fatal(err)
		}
		restated, err := w.c.refreshNotice(w.ctx, row, live, nsAt, c.attempt)
		golden.CheckJSON(t, c.name, map[string]any{"restated": restated, "error": nsError(err), "state": nsSnapshot(t, w.ctx, w.s)}, golden.Substitute(w.root, "<root>"))
	}
}

func seedNoticeMessage(t *testing.T, ctx context.Context, s *store.Store, id, recipient, state, staged string, assignments ...string) {
	t.Helper()
	statements := []string{fmt.Sprintf("INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES ('%s','obl-%s','report','rel','p','k','parent','%s','s','{}','%s','%s','%s')", id, id, recipient, state, staged, staged)}
	for _, assignment := range assignments {
		statements = append(statements, fmt.Sprintf("UPDATE supervisor_messages SET %s WHERE message_id='%s'", assignment, id))
	}
	for _, statement := range statements {
		if _, err := s.Q(ctx).ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestNoticeSubset_Park(t *testing.T) {
	cases := []struct {
		name, state string
		assignments []string
		attempt     string
	}{
		{"queued", "queued", []string{"obligation_kind='fault_notification'"}, ""},
		{"deferred busy", "deferred_busy", []string{"obligation_kind='fault_notification'"}, ""},
		{"withheld before the send", "withheld_pre_send", []string{"obligation_kind='fault_notification'"}, ""},
		{"queued after an attempt that sent nothing and was retry safe", "queued", []string{"obligation_kind='fault_notification'"}, "'no',1"},
		{"queued after an attempt that sent", "queued", []string{"obligation_kind='fault_notification'"}, "'yes',1"},
		{"queued after an attempt whose retry was not shown safe", "queued", []string{"obligation_kind='fault_notification'"}, "'no',0"},
		{"already held", "queued", []string{"obligation_kind='fault_notification'", "hold_reason='held'"}, ""},
		{"sending", "sending", []string{"obligation_kind='fault_notification'"}, ""},
		{"dispatched", "dispatched", []string{"obligation_kind='fault_notification'"}, ""},
		{"read", "read", []string{"obligation_kind='fault_notification'"}, ""},
		{"held uncertain", "held_uncertain", []string{"obligation_kind='fault_notification'"}, ""},
		{"a message that is not a fault notice", "queued", nil, ""},
	}
	for _, c := range cases {
		ctx := context.Background()
		s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		ledger := &faults.Ledger{Store: s, Clock: &delivery.FakeClock{T: 100000}}
		seedNoticeMessage(t, ctx, s, "n1", "supervisor", c.state, "2023-11-14T22:13:20.000000+00:00", c.assignments...)
		if c.attempt != "" {
			if _, err := s.Q(ctx).ExecContext(ctx, "INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, observed_at) VALUES ('req-1','n1',1,'m','withheld_pre_send',"+c.attempt+",'{}','t','t')"); err != nil {
				t.Fatal(err)
			}
		}
		err = parkNoticeForTest(ctx, NoticeChannel{Ledger: ledger}, "n1", "why")
		golden.CheckJSON(t, c.name, map[string]any{"error": nsError(err), "state": nsSnapshot(t, ctx, s)})
	}
}

func TestNoticeSubset_ContactReading(t *testing.T) {
	w := newNoticeWorld(t)
	w.seed(t, nsSeed{signature: nsRelationSig, scope: nsProjectScope})
	stamp := func(offset float64) string { return delivery.ISOOf(nsNow + offset) }
	type observation struct{ deliverable, withhold, observed any }
	cases := []struct {
		name string
		now  float64
		seed *observation
		mut  []string
	}{
		{"no parent is named", nsNow, nil, []string{"UPDATE relationships SET parent_task_id=''"}},
		{"the host was never observed", nsNow, nil, nil},
		{"not deliverable, with a withhold reason", nsNow, &observation{"no", "archived", stamp(-10)}, nil},
		{"not deliverable, no withhold reason", nsNow, &observation{"unknown", nil, stamp(-10)}, nil},
		{"unreadable time", nsNow, &observation{"yes", nil, "unreadable"}, nil},
		{"dated 61 seconds ahead", nsNow, &observation{"yes", nil, stamp(61)}, nil},
		{"dated 60 seconds ahead", nsNow, &observation{"yes", nil, stamp(60)}, nil},
		{"901 seconds old", nsNow, &observation{"yes", nil, stamp(-901)}, nil},
		{"900 seconds old", nsNow, &observation{"yes", nil, stamp(-900)}, nil},
		{"fresh", nsNow, &observation{"yes", nil, stamp(-10)}, nil},
		{"fresh, relationship paused", nsNow, &observation{"yes", nil, stamp(-10)}, []string{"UPDATE relationships SET status='paused'"}},
		{"fresh, relationship cancelled", nsNow, &observation{"yes", nil, stamp(-10)}, []string{"UPDATE relationships SET status='cancelled'"}},
		{"fresh, relationship archived", nsNow, &observation{"yes", nil, stamp(-10)}, []string{"UPDATE relationships SET status='archived'"}},
	}
	// Microsecond stamps with a fractional now: the age is rounded as the bytes of the reading print it, which
	// differs between float64(UnixNano())/1e9 and float64(UnixMicro())/1e6 for some of these, so the golden pins
	// which one the reading uses.
	discriminating := 0
	for i := 0; i < 8; i++ {
		at := time.Unix(1699999990+int64(i), int64((i*152461+9)%1000000)*1000).UTC()
		observed := at.Format("2006-01-02T15:04:05.000000+00:00")
		now := nsNow + 0.123456 + float64(i)
		parsed, err := time.Parse(time.RFC3339Nano, observed)
		if err != nil {
			t.Fatal(err)
		}
		if now-float64(parsed.UnixNano())/1e9 != now-float64(parsed.UnixMicro())/1e6 {
			discriminating++
		}
		cases = append(cases, struct {
			name string
			now  float64
			seed *observation
			mut  []string
		}{fmt.Sprintf("fractional stamp %d", i), now, &observation{"yes", nil, observed}, nil})
	}
	if discriminating < 2 {
		t.Fatalf("only %d of the fractional stamps tell the two roundings apart", discriminating)
	}
	for _, c := range cases {
		w.exec(t, "DELETE FROM recipient_lifecycle")
		w.exec(t, "UPDATE relationships SET status='active', parent_task_id='parent'")
		if c.seed != nil {
			w.exec(t, "INSERT INTO recipient_lifecycle (task_id, deliverable, withhold_reason, observed_at) VALUES ('parent', ?, ?, ?)", c.seed.deliverable, c.seed.withhold, c.seed.observed)
		}
		for _, statement := range c.mut {
			w.exec(t, statement)
		}
		eligibility, err := w.ledger.NotificationEligibility(w.ctx, nsFault, c.now)
		golden.CheckJSON(t, c.name, map[string]any{"eligibility": nsPlain(eligibility), "error": nsError(err)})
	}
}
