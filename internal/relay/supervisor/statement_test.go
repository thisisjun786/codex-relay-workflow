package supervisor

import (
	"database/sql"
	"slices"
	"sort"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1500 U1 (plan-r3 section 1): a statement is what the work says, not the packet bytes.
// statementKey reads the obligation's columns and the allowlisted statement fields of the packet, so
// a seat move, a rotated sender, a moved project or a new clock never makes a new statement, while a
// new head, a new submission, another decision, other direct text or other direct evidence does.

// statementResolutions are two readings of the hierarchy for one message: every routing field
// differs (sender, recipient, recipient scope kind, project), as a seat move and a rotation do.
var statementResolutions = [2]Resolution{
	{Sender: "parent", Recipient: "supervisor", RecipientScopeKind: scopeKindInitiative, ProjectKey: "PRJ-1", InitiativeKey: "INI-1", Source: "store"},
	{Sender: "parent-rotated", Recipient: "management-seat", RecipientScopeKind: "management", ProjectKey: "PRJ-2", InitiativeKey: "INI-1", Source: "store"},
}

// statementClocks are the two observedAt values the packets are composed at.
var statementClocks = [2]string{"2023-11-14T22:13:20.000000+00:00", "2023-11-15T09:00:00.000000+00:00"}

// statementSide is one packet of a comparison and the columns its row carries (or would carry).
type statementSide struct {
	columns statementColumns
	packet  Packet
}

// freshKey is the statement key of a freshly composed packet.
func (s statementSide) freshKey(t *testing.T) string {
	t.Helper()
	key, err := freshStatementKey(s.columns, s.packet, statementReading{})
	if err != nil {
		t.Fatalf("fresh statement key: %v", err)
	}
	return key
}

// storedKey is the statement key of the row staging would write for this side: the packet in its
// stored encoding, read back from a supervisor_messages row.
func (s statementSide) storedKey(t *testing.T) string {
	t.Helper()
	encoded, err := canonicalPacket(s.packet)
	if err != nil {
		t.Fatal(err)
	}
	if IsDirectKind(s.columns.ObligationKind) {
		encoded = directDumps(s.packet)
	}
	row := store.SupervisorMessagesRow{MessageID: s.packet.ID(), ObligationID: s.columns.ObligationID, ObligationKind: s.columns.ObligationKind,
		EventID: s.columns.EventID, SubmissionNo: s.columns.SubmissionNo, Packet: encoded}
	key, err := rowStatementKey(row)
	if err != nil {
		t.Fatalf("row statement key: %v", err)
	}
	return key
}

// encodedPacket is the stored encoding of a side's packet, to show the routing moved the bytes.
func (s statementSide) encodedPacket(t *testing.T) string {
	t.Helper()
	if IsDirectKind(s.columns.ObligationKind) {
		return directDumps(s.packet)
	}
	encoded, err := canonicalPacket(s.packet)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// sameStatement asserts both sides are one statement although their packet bytes differ: fresh
// against fresh, stored against fresh and stored against stored.
func sameStatement(t *testing.T, a, b statementSide) {
	t.Helper()
	if a.encodedPacket(t) == b.encodedPacket(t) {
		t.Fatalf("the two packets are byte-identical, so the case shows nothing about routing:\n%s", a.encodedPacket(t))
	}
	freshA, freshB, storedA, storedB := a.freshKey(t), b.freshKey(t), a.storedKey(t), b.storedKey(t)
	if freshA != freshB || storedA != freshB || storedA != storedB || freshA != storedA {
		t.Fatalf("one statement got different keys: fresh %s / %s, stored %s / %s\nA: %s\nB: %s", freshA, freshB, storedA, storedB, a.encodedPacket(t), b.encodedPacket(t))
	}
}

// otherStatement asserts the two sides are different statements, on both the fresh and the stored side.
func otherStatement(t *testing.T, a, b statementSide) {
	t.Helper()
	if a.freshKey(t) == b.freshKey(t) || a.storedKey(t) == b.freshKey(t) || a.storedKey(t) == b.storedKey(t) {
		t.Fatalf("two statements got one key %s\nA: %s\nB: %s", a.freshKey(t), a.encodedPacket(t), b.encodedPacket(t))
	}
}

func TestStatementKeyIgnoresRouting(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	compose := func(t *testing.T, o Obligation, submission int64, side int) statementSide {
		t.Helper()
		p, err := f.c.Compose(f.ctx, o, statementResolutions[side], statementClocks[side])
		if err != nil {
			t.Fatal(err)
		}
		return statementSide{obligationStatementColumns(o, submission), p}
	}
	reading := func(t *testing.T, o Obligation, side int) statementSide {
		t.Helper()
		p, err := f.c.composeReading(f.ctx, o, statementResolutions[side], statementClocks[side], map[string]any{"schema": "reporting-observation/1"})
		if err != nil {
			t.Fatal(err)
		}
		return statementSide{obligationStatementColumns(o, 0), p}
	}
	direct := func(m DirectMessage, side int) statementSide {
		return statementSide{directStatementColumns(m), f.c.composeDirect(m, statementResolutions[side], statementClocks[side])}
	}
	event := f.obligation(t)

	t.Run("event/routing", func(t *testing.T) {
		sameStatement(t, compose(t, event, 1, 0), compose(t, event, 1, 1))
	})
	t.Run("event/artifact_head", func(t *testing.T) {
		reportNumber24(t, f, 1, 7)
		before := compose(t, event, 1, 0)
		sameStatement(t, before, compose(t, event, 1, 1))
		if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE work_reports SET head_sha='f00dfeed' WHERE event_id='event-1' AND submission_no=1"); err != nil {
			t.Fatal(err)
		}
		otherStatement(t, before, compose(t, event, 1, 0))
		otherStatement(t, before, compose(t, event, 1, 1))
	})
	t.Run("event/submission", func(t *testing.T) {
		first := compose(t, event, 1, 0)
		second := compose(t, event, 2, 0)
		if first.encodedPacket(t) != second.encodedPacket(t) {
			t.Fatal("the submission case composes one packet twice; it moved")
		}
		otherStatement(t, first, second)
		otherStatement(t, first, compose(t, event, 2, 1))
	})
	t.Run("event/other_event", func(t *testing.T) {
		// A new raising event is a new statement even when the packet would read the same.
		a, b := compose(t, event, 1, 0), compose(t, event, 1, 0)
		b.columns.EventID = sql.NullString{String: "event-2", Valid: true}
		otherStatement(t, a, b)
	})
	t.Run("decision", func(t *testing.T) {
		ask := event
		ask.Kind = "decision_request"
		ask.Detail = "merge it or hold it"
		sameStatement(t, compose(t, ask, 1, 0), compose(t, ask, 1, 1))
		other := ask
		other.Detail = "close it"
		otherStatement(t, compose(t, ask, 1, 0), compose(t, other, 1, 1))
	})
	t.Run("standing_evidence", func(t *testing.T) {
		// An obligation packet without an event points its evidence at the live project
		// (supervisor-standing --project P). The channel built that pointer, so it is routing.
		standing := event
		standing.Basis = map[string]any{}
		a, b := compose(t, standing, 0, 0), compose(t, standing, 0, 1)
		if pyEvidence(a.packet) == pyEvidence(b.packet) {
			t.Fatalf("the standing evidence did not move with the project: %s", pyEvidence(a.packet))
		}
		sameStatement(t, a, b)
	})
	omission := Obligation{ID: "omission-1", Kind: "unreported", RelationID: "rel-1", Subject: "turn-9", Generation: int64(1), Issue: func() *string { s := "REL-1"; return &s }(),
		Basis: map[string]any{"reason": "no report marker"}}
	t.Run("omission/routing", func(t *testing.T) {
		sameStatement(t, reading(t, omission, 0), reading(t, omission, 1))
	})
	t.Run("omission/reading", func(t *testing.T) {
		other := omission
		other.Basis = map[string]any{"reason": "the marker was unreadable"}
		otherStatement(t, reading(t, omission, 0), reading(t, other, 1))
	})

	report := DirectMessage{Kind: KindFlowMilestoneReport, Relationship: "project:PRJ-1", Subject: "milestone-3", Summary: "milestone 3 is done",
		Text: "the third milestone landed on dev", Evidence: []string{"linear CRW-1500", "commit 5c0e220a"}}
	lead := DirectMessage{Kind: KindLeadReport, Relationship: "initiative:INI-1", Subject: "week-42", Summary: "week 42", Text: "two projects moved"}
	ask := DirectMessage{Kind: KindLeadDecisionRequest, Relationship: "initiative:INI-1", Subject: "ask-1", Summary: "ship or hold", Text: "the release is ready"}
	t.Run("direct/routing", func(t *testing.T) {
		// A project kind resolved to a legacy initiative lead names another layer and recipient role;
		// a lead kind names the live project in its scope. Both are routing.
		for _, m := range []DirectMessage{report, lead, ask} {
			sameStatement(t, direct(m, 0), direct(m, 1))
		}
		defaulted := lead
		defaulted.Evidence = nil
		sameStatement(t, direct(defaulted, 0), direct(defaulted, 1))
	})
	t.Run("direct/text", func(t *testing.T) {
		other := report
		other.Text = "the third milestone landed on dev, and the fourth started"
		otherStatement(t, direct(report, 0), direct(other, 1))
	})
	t.Run("direct/summary", func(t *testing.T) {
		other := report
		other.Summary = "milestone 3 is done, late"
		otherStatement(t, direct(report, 0), direct(other, 0))
	})
	t.Run("direct/decision", func(t *testing.T) {
		other := ask
		other.Summary = "ship now"
		otherStatement(t, direct(ask, 0), direct(other, 1))
	})
	t.Run("direct/evidence", func(t *testing.T) {
		other := report
		other.Evidence = []string{"linear CRW-1500", "commit d5f003ee"}
		otherStatement(t, direct(report, 0), direct(other, 1))
		none := report
		none.Evidence = nil
		otherStatement(t, direct(report, 0), direct(none, 0))
	})
}

// TestStatementFieldsAllowlist pins the allowlist of plan-r3 section 1: every routing or
// reinterpretation field is dropped, every statement field kept, and the top-level evidence kept only
// for a direct message.
func TestStatementFieldsAllowlist(t *testing.T) {
	t.Parallel()
	envelope := map[string]any{}
	for _, name := range []string{"version", "direction", "kind", "purpose", "messageId", "relationId", "subject", "basis", "decision", "answerOwedBy", "correlationId", "replyTo",
		"sender", "recipient", "layer", "scope", "observedAt", "evidence", "relationRevision", "reach", "unknownEnvelopeField"} {
		envelope[name] = name
	}
	packet := map[string]any{"envelope": envelope, "unknownTopField": "x"}
	for _, name := range []string{"version", "issue", "generation", "criteriaDigest", "policy", "callback", "artifact", "body", "activation", "evidence"} {
		packet[name] = name
	}
	wantEnvelope := []string{"answerOwedBy", "basis", "correlationId", "decision", "direction", "kind", "messageId", "purpose", "relationId", "replyTo", "subject", "version"}
	wantTop := []string{"activation", "artifact", "body", "callback", "criteriaDigest", "envelope", "generation", "issue", "policy", "version"}
	for _, direct := range []bool{false, true} {
		fields := statementFields(packet, direct)
		top := wantTop
		if direct {
			top = append(append([]string{}, wantTop...), "evidence")
		}
		if got := sortedKeys(fields); !equalStrings(got, sortedStrings(top)) {
			t.Fatalf("direct=%v top-level fields %v, want %v", direct, got, sortedStrings(top))
		}
		region, ok := fields["envelope"].(map[string]any)
		if !ok {
			t.Fatalf("direct=%v envelope %#v", direct, fields["envelope"])
		}
		if got := sortedKeys(region); !equalStrings(got, wantEnvelope) {
			t.Fatalf("direct=%v envelope fields %v, want %v", direct, got, wantEnvelope)
		}
	}
	if _, ok := envelope["sender"]; !ok {
		t.Fatal("statementFields changed the packet it read")
	}
}

func pyEvidence(p Packet) string {
	evidence, _ := p["evidence"].([]string)
	if len(evidence) == 0 {
		return ""
	}
	return evidence[0]
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return sortedStrings(out)
}

func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool { return slices.Equal(a, b) }
