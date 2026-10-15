package supervisor

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1500 fix round 1 (crosscheck of 30252780, B1 and B2). A statement is what the 부모 must judge
// anew. Two fields of a direct message's packet carry text the channel or the reporting command puts
// there without the sender saying anything new: the default evidence pointer (the supervisor-show
// line, spelled with the executable path and the store directory of the process that staged it) and
// the reporter notice report-up writes ahead of a proxy report's text (it names the initiative's lead
// of the moment). Neither makes a new statement, so an acknowledged park stays parked when only they
// move, and a real new statement still releases it.

// proxyNoticeText is the report-up reporter notice as the message text carries it: a proxy report by
// reporter while lead is the initiative's lead.
func proxyNoticeText(reporter, lead, text string) string {
	return "Reported by thread " + reporter + ", which is not the initiative's lead (" + lead + ").\n\n" + text
}

// TestAcknowledgedDirectParkKeptWhenOnlyTheProgramPathMoved (B1): the same report staged again by a
// process invoked through another executable path keeps the park; a report with other text staged
// through that path is a new statement and releases it.
func TestAcknowledgedDirectParkKeptWhenOnlyTheProgramPathMoved(t *testing.T) {
	t.Run("same report, other executable path: kept", func(t *testing.T) {
		w := newHoldWorld(t, "direct")
		w.setHold(t, "acknowledged")
		w.c.Program = "'/opt/relay-build/bin/crw' relay"
		w.enter(t, "direct_stage")
		if w.answer != acknowledgedKeptReason {
			t.Fatalf("staging the same report through another executable answered %q, want %q", w.answer, acknowledgedKeptReason)
		}
		w.expectParked(t, holdAcknowledged, "queued", 0)
		if n := w.journalRows(t, "supervisor_message_reopened"); n != 0 {
			t.Fatalf("the staging journaled %d reopened rows, want none", n)
		}
	})
	t.Run("other text, other executable path: released", func(t *testing.T) {
		w := newHoldWorld(t, "direct")
		w.setHold(t, "acknowledged")
		w.c.Program = "'/opt/relay-build/bin/crw' relay"
		w.text = "milestone one is done, and two started"
		w.enter(t, "direct_stage")
		row, err := w.c.Get(w.ctx, w.id)
		if err != nil || row.HoldReason.Valid || row.State != "queued" {
			t.Fatalf("a new statement left hold %v state %s (%v), want released and queued", row.HoldReason, row.State, err)
		}
	})
}

// TestAcknowledgedProxyReportKeptWhenOnlyTheLeadMoved (B2): a proxy report (reported by a thread that
// is not the initiative's lead, so its text starts with the reporter notice naming the lead) is parked;
// the lead is handed to another session and the same report is reported again, its notice naming the
// new lead. The message is re-addressed to the new lead and stays parked. The same handover with the
// report's own text changed releases it.
func TestAcknowledgedProxyReportKeptWhenOnlyTheLeadMoved(t *testing.T) {
	const reporter = "01helper"
	stage := func(t *testing.T, changed bool) *holdWorld {
		t.Helper()
		w := newHoldWorld(t, "direct")
		row, err := w.c.Get(w.ctx, w.id)
		if err != nil {
			t.Fatal(err)
		}
		w.text = proxyNoticeText(reporter, row.SenderTaskID, "milestone one is done")
		w.enter(t, "direct_stage")
		w.setHold(t, "acknowledged")
		w.apply(t, "other_sender")
		text := "milestone one is done"
		if changed {
			text = "milestone one is done, and two started"
		}
		w.text = proxyNoticeText(reporter, holdRuleLead, text)
		w.enter(t, "direct_stage")
		return w
	}
	t.Run("same report, new lead: kept", func(t *testing.T) {
		w := stage(t, false)
		if w.answer != acknowledgedKeptReason {
			t.Fatalf("the same proxy report under a new lead answered %q, want %q", w.answer, acknowledgedKeptReason)
		}
		w.expectParked(t, holdAcknowledged, "queued", 0)
		row, err := w.c.Get(w.ctx, w.id)
		if err != nil || row.SenderTaskID != holdRuleLead {
			t.Fatalf("the message is from %s (%v), want it re-addressed from the new lead %s", row.SenderTaskID, err, holdRuleLead)
		}
		if n := w.journalRows(t, "supervisor_message_reopened"); n != 0 {
			t.Fatalf("the staging journaled %d reopened rows, want none", n)
		}
	})
	t.Run("other text, new lead: released", func(t *testing.T) {
		w := stage(t, true)
		row, err := w.c.Get(w.ctx, w.id)
		if err != nil || row.HoldReason.Valid || row.State != "queued" {
			t.Fatalf("a new proxy statement left hold %v state %s (%v), want released and queued", row.HoldReason, row.State, err)
		}
	})
}

// TestStatementKeyIgnoresIncidentalDirectText: the pure key over composeDirect packets. The default
// evidence pointer spelled by two programs and two stores, and a body whose reporter notice names
// two leads (or none), give one key; a sender-given evidence pointer, other report text and another
// summary give another. ProxyReporterNotice is the notice report-up writes.
func TestStatementKeyIgnoresIncidentalDirectText(t *testing.T) {
	if got := ProxyReporterNotice("01helper", "01lead"); got != proxyNoticeText("01helper", "01lead", "") {
		t.Fatalf("ProxyReporterNotice = %q, want the notice report-up writes", got)
	}
	r := statementResolutions[0]
	key := func(t *testing.T, program, state string, m DirectMessage) string {
		t.Helper()
		c := &Channel{Program: program, Store: &store.Store{Path: state + "/relay.sqlite3"}}
		k, err := freshStatementKey(directStatementColumns(m), c.composeDirect(m, r, statementClocks[0]), statementReading{defaultEvidence: len(m.Evidence) == 0})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	base := DirectMessage{Kind: KindLeadReport, Relationship: "initiative:INI-1", Subject: "lrp_1", Summary: "milestone one", Text: "milestone one is done"}
	with := func(edit func(*DirectMessage)) DirectMessage {
		m := base
		edit(&m)
		return m
	}
	want := key(t, "codex-session-relay", "/state/a", base)
	for name, got := range map[string]string{
		"another program":              key(t, "'/opt/relay-build/bin/crw' relay", "/state/a", base),
		"another store directory":      key(t, "codex-session-relay", "/state/b", base),
		"a reporter notice":            key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) { m.Text = proxyNoticeText("01helper", "01lead", m.Text) })),
		"a notice naming another lead": key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) { m.Text = proxyNoticeText("01helper", "01lead2", m.Text) })),
		"a notice by another reporter": key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) { m.Text = proxyNoticeText("01helper2", "01lead", m.Text) })),
	} {
		if got != want {
			t.Errorf("%s made a new statement", name)
		}
	}
	for name, got := range map[string]string{
		"a sender-given evidence pointer": key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) { m.Evidence = []string{"CRW-1500"} })),
		"other report text":               key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) { m.Text = "milestone one is done, and two started" })),
		"other text after a notice":       key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) { m.Text = proxyNoticeText("01helper", "01lead", "milestone two") })),
		"another summary":                 key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) { m.Summary = "milestone two" })),
		"a sender-given pointer of the same tail": key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) {
			m.Evidence = []string{"ssh host-b codex-session-relay --state /state/a supervisor-show --message " + DirectMessageID(base)}
		})),
	} {
		if got == want {
			t.Errorf("%s is read as the same statement", name)
		}
	}
	// A sender's own pointer is compared as written, even when it ends in the message's show line.
	viaHost := func(host string) string {
		return key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) {
			m.Evidence = []string{"ssh " + host + " codex-session-relay --state /state/a supervisor-show --message " + DirectMessageID(base)}
		}))
	}
	if viaHost("host-a") == viaHost("host-b") {
		t.Error("a sender-given pointer that moved to another host is read as the same statement")
	}
	// Bugbot x1500 r2: a sender-given pointer of the default shape is the sender's too.
	viaStore := func(state string) string {
		return key(t, "codex-session-relay", "/state/a", with(func(m *DirectMessage) {
			m.Evidence = []string{"crw relay --state " + state + " supervisor-show --message " + DirectMessageID(base)}
		}))
	}
	if viaStore("/state/a") == viaStore("/state/b") {
		t.Error("a sender-given pointer of the default shape that moved to another store is read as the same statement")
	}
	// The masked key: two statements that differ only in naming one sender session or the other.
	m1 := with(func(m *DirectMessage) { m.Text = "milestone one is done; ask 01lead for the logs" })
	m2 := with(func(m *DirectMessage) { m.Text = "milestone one is done; ask 01lead2 for the logs" })
	c := &Channel{Program: "codex-session-relay", Store: &store.Store{Path: "/state/a/relay.sqlite3"}}
	k1, err1 := freshStatementKey(directStatementColumns(m1), c.composeDirect(m1, r, statementClocks[0]), statementReading{masked: []string{"01lead", "01lead2"}})
	k2, err2 := freshStatementKey(directStatementColumns(m2), c.composeDirect(m2, r, statementClocks[0]), statementReading{masked: []string{"01lead", "01lead2"}})
	if err1 != nil || err2 != nil || k1 != k2 {
		t.Errorf("with both senders masked the two statements differ (%v %v)", err1, err2)
	}
}

// TestAcknowledgedDirectParkReleasedWhenTheSenderMovedItsPointer (bugbot x1500 r2): a direct report
// whose sender gives a show-line pointer is parked; the sender reports it again with the pointer
// naming another store. That is the sender's evidence, compared as written: a new statement, released.
func TestAcknowledgedDirectParkReleasedWhenTheSenderMovedItsPointer(t *testing.T) {
	w := newHoldWorld(t, "direct")
	pointer := func(state string) []string {
		return []string{"crw relay --state " + state + " supervisor-show --message " + w.id}
	}
	m := DirectMessage{Kind: KindLeadReport, Relationship: "initiative:" + storeRecipientInitiative, Subject: "lrp_1",
		Summary: "milestone one", Text: w.text, Evidence: pointer("/state/a"), At: "2026-10-14T00:00:00.000000+00:00"}
	if _, err := w.c.StageDirect(w.ctx, m, nil); err != nil {
		t.Fatal(err)
	}
	w.setHold(t, "acknowledged")
	m.Evidence = pointer("/state/b")
	if _, err := w.c.StageDirect(w.ctx, m, nil); err != nil {
		t.Fatal(err)
	}
	row, err := w.c.Get(w.ctx, w.id)
	if err != nil || row.HoldReason.Valid || row.State != "queued" {
		t.Fatalf("a moved sender pointer left hold %v state %s (%v), want released and queued", row.HoldReason, row.State, err)
	}
}
