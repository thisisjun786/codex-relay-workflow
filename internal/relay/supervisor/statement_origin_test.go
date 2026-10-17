package supervisor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1558: a direct statement is what its sender gave. The channel records in the packet's origin
// what it generated itself (the default supervisor-show pointer, report-up's attribution line) and the
// digest of the sender's whole text, and the statement of a row that carries that record is read
// through it. A row staged before the record existed is read the fields way on both sides, except that
// one whose stored body reached the 6000-byte cap cannot prove its tail and releases its park once
// (parent decision 10-16, Q48).

// longDirectText is a 7000-byte text, longer than the packet's body (maxDirectBody).
var longDirectText = strings.Repeat("abcdefghij", 700)

// changedAt is text with byte i replaced, past the packet's body when i >= maxDirectBody.
func changedAt(text string, i int) string {
	return text[:i] + "Z" + text[i+1:]
}

// heldNow reports whether the world's message carries superseded_by_report now.
func (w *holdWorld) heldNow(t *testing.T) bool {
	t.Helper()
	row, err := w.c.Get(w.ctx, w.id)
	if err != nil {
		t.Fatal(err)
	}
	return row.HoldReason.String == store.SupervisorHoldSuperseded
}

// storedPacket is the world's message's packet as stored.
func (w *holdWorld) storedPacket(t *testing.T) string {
	t.Helper()
	row, err := w.c.Get(w.ctx, w.id)
	if err != nil {
		t.Fatal(err)
	}
	return row.Packet
}

// storedTop is the decoded top level of the stored packet.
func (w *holdWorld) storedTop(t *testing.T) map[string]any {
	t.Helper()
	var packet map[string]any
	if err := json.Unmarshal([]byte(w.storedPacket(t)), &packet); err != nil {
		t.Fatal(err)
	}
	return packet
}

// makeOld rewrites the stored packet as a build before the origin record wrote it: the stored packet
// decoded, its origin removed and encoded again the way StageDirect encodes (directDumps), not through
// SQLite's json_remove, which does not write directDumps bytes. A packet that had no origin comes back
// byte for byte.
func (w *holdWorld) makeOld(t *testing.T) {
	t.Helper()
	stored := w.storedPacket(t)
	packet := w.storedTop(t)
	_, had := packet["origin"]
	delete(packet, "origin")
	old := directDumps(Packet(packet))
	if !had && old != stored {
		t.Fatalf("the stored packet does not encode back to its bytes:\n%s\n%s", stored, old)
	}
	w.exec(t, "UPDATE supervisor_messages SET packet=? WHERE message_id=?", old, w.id)
}

// stageAnswer stages the world's report and returns the whole answer.
func (w *holdWorld) stageAnswer(t *testing.T) map[string]any {
	t.Helper()
	answer, err := w.c.StageDirect(w.ctx, w.directMessage(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// parkDirect acknowledges the world's message and fails unless it was parked.
func (w *holdWorld) parkDirect(t *testing.T) {
	t.Helper()
	w.setHold(t, "acknowledged")
}

// expectHeld fails unless the message's hold is what want says, naming the case.
func (w *holdWorld) expectHeld(t *testing.T, want bool, what string) {
	t.Helper()
	if got := w.heldNow(t); got != want {
		t.Fatalf("%s: held %v, want %v (packet %s)", what, got, want, w.storedPacket(t))
	}
}

func TestDirectStatementKeepsAcrossProgramsWithTheDefaultPointer(t *testing.T) {
	w := newHoldWorld(t, "direct")
	w.parkDirect(t)
	w.c.Program = "/opt/other/codex-session-relay"
	w.stageDirect(t)
	w.expectHeld(t, true, "the same report staged by another executable (the channel's default pointer moved)")
}

func TestDirectStatementDefaultShapedExplicitPointerThenOmittedReleases(t *testing.T) {
	w := newHoldWorld(t, "direct_default_pointer")
	evidence, _ := w.storedTop(t)["evidence"].([]any)
	if len(evidence) != 1 || evidence[0] != w.c.command("supervisor-show", "--message", w.id) {
		t.Fatalf("the sender's explicit pointer is not the default pointer's text: %v", evidence)
	}
	w.parkDirect(t)
	w.evidence = nil
	w.stageDirect(t)
	w.expectHeld(t, false, "the sender's explicit pointer, then no evidence")
}

func TestDirectStatementLongTextTailChangeReleases(t *testing.T) {
	w := newHoldWorld(t, "direct")
	w.text = longDirectText
	w.stageDirect(t)
	w.parkDirect(t)
	w.stageDirect(t)
	w.expectHeld(t, true, "the same 7000-byte report")
	w.text = changedAt(longDirectText, 6500)
	w.stageDirect(t)
	w.expectHeld(t, false, "a 7000-byte report whose byte 6500 changed")
}

// TestDirectOldLongRowReleasesOnce: an old row (no origin) whose stored body is at the cap cannot
// prove its tail, so its next re-stage releases the park once, whether or not the tail changed, and
// records the origin; the next identical re-stage of that row keeps the park.
func TestDirectOldLongRowReleasesOnce(t *testing.T) {
	t.Run("tail_changed", func(t *testing.T) {
		w := newHoldWorld(t, "direct")
		w.text = longDirectText
		w.stageDirect(t)
		w.makeOld(t)
		w.parkDirect(t)
		w.text = changedAt(longDirectText, 6500)
		w.stageDirect(t)
		w.expectHeld(t, false, "an old 7000-byte row whose byte 6500 changed")
	})
	t.Run("identical", func(t *testing.T) {
		w := newHoldWorld(t, "direct")
		w.text = longDirectText
		w.stageDirect(t)
		w.makeOld(t)
		w.parkDirect(t)
		w.stageDirect(t)
		w.expectHeld(t, false, "an old 7000-byte row staged again identically (released once)")
		if _, ok := w.storedTop(t)["origin"].(map[string]any); !ok {
			t.Fatalf("the release did not record the origin: %s", w.storedPacket(t))
		}
		w.parkDirect(t)
		w.stageDirect(t)
		w.expectHeld(t, true, "the same row, now with its origin, staged again identically")
	})
}

// TestDirectOldShortRowSameReportKeeps: an old row whose stored body is under the cap keeps its park
// for the same report, and nothing is written.
func TestDirectOldShortRowSameReportKeeps(t *testing.T) {
	w := newHoldWorld(t, "direct")
	w.makeOld(t)
	w.parkDirect(t)
	before := w.storedPacket(t)
	restated := w.journalRows(t, "supervisor_message_restated")
	answer := w.stageAnswer(t)
	w.expectHeld(t, true, "an old short row staged again with the same report")
	if after := w.storedPacket(t); after != before {
		t.Fatalf("the replay rewrote the old row:\n%s\n%s", before, after)
	}
	if answer["restated"] == true || w.journalRows(t, "supervisor_message_restated") != restated {
		t.Fatalf("the replay of an old row was a restatement: %v", answer)
	}
}

// TestEventStatementKeyIsUnchanged pins the statement keys of an event packet (fixture24) and of an
// omission packet, computed at cc0941cd7 before the origin record: those packets are read the fields
// way, so their keys stay byte-stable.
func TestEventStatementKeyIsUnchanged(t *testing.T) {
	f := fixture24(t)
	event := f.obligation(t)
	p, err := f.c.Compose(f.ctx, event, statementResolutions[0], statementClocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if key, err := freshStatementKey(obligationStatementColumns(event, 1), p); err != nil || key != "60874e4bc89738ce18835152dbe81aaae539953390c1a997a80d617a5b264138" {
		t.Fatalf("the event statement key moved: %s %v", key, err)
	}
	encoded, err := canonicalPacket(p)
	if err != nil {
		t.Fatal(err)
	}
	columns := obligationStatementColumns(event, 1)
	row := store.SupervisorMessagesRow{ObligationID: columns.ObligationID, ObligationKind: columns.ObligationKind, EventID: columns.EventID, SubmissionNo: columns.SubmissionNo, Packet: encoded}
	if key, err := rowStatementKey(row); err != nil || key != "60874e4bc89738ce18835152dbe81aaae539953390c1a997a80d617a5b264138" {
		t.Fatalf("the stored event statement key moved: %s %v", key, err)
	}
	omission := Obligation{ID: "omission-1", Kind: "unreported", RelationID: "rel-1", Subject: "turn-9", Generation: int64(1), Issue: func() *string { s := "REL-1"; return &s }(),
		Basis: map[string]any{"reason": "no report marker"}}
	q, err := f.c.composeReading(f.ctx, omission, statementResolutions[0], statementClocks[0], map[string]any{"schema": "reporting-observation/1"})
	if err != nil {
		t.Fatal(err)
	}
	if key, err := freshStatementKey(obligationStatementColumns(omission, 0), q); err != nil || key != "2d44745d0547eb59d3f6221d5540ab1fb8397a103d82db66ece3e657242b2108" {
		t.Fatalf("the omission statement key moved: %s %v", key, err)
	}
}

// The proxy attribution report-up writes above a report made by a thread that is not the lead, and
// the base bodies of the three Step-0 proxy messages: sha256 of the packet body composeDirect writes
// for Text = proxyAttribution + "\n\n" + text (the long one's cut carries the truncation marker,
// CRW-1595).
const (
	proxyAttribution = "Reported by thread 01someone, which is not the initiative's lead (01lead)."
	otherAttribution = "Reported by thread 01other, which is not the initiative's lead (01lead)."
	baseBodyLong     = "c95c92c062c2b008f280fab41a14aefc4c414b311087e95546dd61b9885d067f" // a 7000-byte text, cut and marked (CRW-1595)
	baseBodyIndented = "d3203a7747465df0826b9104225b59e660d8c12b8bc1ade1add03ec3395f77e3" // "   indented text"
	baseBodyTitle    = "70d4312796e07dafb7cc952ed4c5599d3de29ac600a679bb8b324bebb8e892e8" // no text (title only)
)

// proxyInputs are the three Step-0 texts and their base body digests.
var proxyInputs = []struct{ name, text, body string }{
	{"long", longDirectText, baseBodyLong},
	{"indented", "   indented text", baseBodyIndented},
	{"title_only", "", baseBodyTitle},
}

// expectBody fails unless the stored body's sha256 is want.
func (w *holdWorld) expectBody(t *testing.T, want string) {
	t.Helper()
	body, _ := w.storedTop(t)["body"].(string)
	if got := pyvalue.SHA256Hex(body); got != want {
		t.Fatalf("the stored body's sha256 is %s, want %s:\n%q", got, want, body)
	}
}

// stageBaseProxy stages the world's report the way report-up did before Preface existed: the
// attribution joined into the text.
func (w *holdWorld) stageBaseProxy(t *testing.T, text string) {
	t.Helper()
	w.preface, w.text = "", proxyAttribution+"\n\n"+text
	w.stageDirect(t)
	w.text = text
}

func TestComposeDirectBodyIsTheBaseBody(t *testing.T) {
	f := fixture24(t)
	for _, in := range proxyInputs {
		t.Run(in.name, func(t *testing.T) {
			m := DirectMessage{Kind: KindLeadReport, Relationship: "initiative:INI-1", Subject: "lrp_1", Summary: "milestone one", Text: in.text, Preface: proxyAttribution}
			p := f.c.composeDirect(m, statementResolutions[0], statementClocks[0])
			body, _ := p["body"].(string)
			if got := pyvalue.SHA256Hex(body); got != in.body {
				t.Fatalf("the Preface path's body sha256 is %s, want the base %s", got, in.body)
			}
			origin, _ := p["origin"].(map[string]any)
			// Below the attribution the body keeps the text's leading indentation, so the digest does.
			var text any
			if strings.TrimSpace(in.text) != "" {
				text = "sha256:" + pyvalue.SHA256Hex(strings.TrimRight(in.text, " \t\n"))
			}
			if origin["evidence"] != "channel" || origin["text"] != text || origin["preface"] != proxyAttribution {
				t.Fatalf("origin %v, want the channel's pointer, text %v and the attribution", origin, text)
			}
		})
	}
	t.Run("no_preface", func(t *testing.T) {
		m := DirectMessage{Kind: KindLeadReport, Relationship: "initiative:INI-1", Subject: "lrp_1", Summary: "s", Text: "  said  ", Evidence: []string{"commit 1"}}
		p := f.c.composeDirect(m, statementResolutions[0], statementClocks[0])
		origin, _ := p["origin"].(map[string]any)
		if p["body"] != "said" || origin["evidence"] != "sender" || origin["text"] != "sha256:"+pyvalue.SHA256Hex("  said") || origin["preface"] != nil {
			t.Fatalf("body %v origin %v", p["body"], origin)
		}
	})
	t.Run("preface_is_not_text", func(t *testing.T) {
		m := DirectMessage{Kind: KindLeadReport, Relationship: "initiative:INI-1", Subject: "lrp_1", Preface: proxyAttribution}
		if err := checkDirect(m); err == nil {
			t.Fatal("a message with only a preface was accepted")
		}
	})
}

// TestDirectStatementPrefaceIsNotTheSenders: the attribution line is the channel's, so who reported
// a statement, or whether anyone was named, does not make a new one.
func TestDirectStatementPrefaceIsNotTheSenders(t *testing.T) {
	for _, tc := range []struct {
		name, text, before, after string
	}{
		{"other_reporter", "milestone one is done", proxyAttribution, otherAttribution},
		{"reporter_became_the_lead", "milestone one is done", proxyAttribution, ""},
		{"lead_then_another_reporter", "milestone one is done", "", proxyAttribution},
		{"title_only_other_reporter", "", proxyAttribution, otherAttribution},
		{"long_text_other_preface_length", longDirectText, proxyAttribution, "Reported by thread 01someoneElseWithALongerName, which is not the initiative's lead (01lead)."},
		{"indented_reporter_became_the_lead", "    milestone one is done", proxyAttribution, ""},
		{"indented_lead_then_another_reporter", "    milestone one is done", "", proxyAttribution},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newHoldWorld(t, "direct")
			w.text, w.preface = tc.text, tc.before
			w.stageDirect(t)
			w.parkDirect(t)
			w.preface = tc.after
			w.stageDirect(t)
			w.expectHeld(t, true, tc.name)
		})
	}
}

// TestDirectStatementPrefacedIndentationDecides: under report-up's attribution line the body keeps the
// sender's text as given, its first line's leading indentation included, so the statement compares
// that text as the body carries it. A first line whose indentation alone changed is a new statement
// and releases the park; the same text replayed keeps it.
func TestDirectStatementPrefacedIndentationDecides(t *testing.T) {
	const flat = "milestone one is done\n  - the scope is complete"
	for _, tc := range []struct{ name, before, after string }{
		{"indented", flat, "    " + flat},
		{"unindented", "    " + flat, flat},
		{"deeper", "  " + flat, "    " + flat},
		{"tab", "  " + flat, "\t" + flat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newHoldWorld(t, "direct")
			w.text, w.preface = tc.before, proxyAttribution
			w.stageDirect(t)
			w.parkDirect(t)
			w.stageDirect(t)
			w.expectHeld(t, true, "the same text replayed under the attribution")
			before, _ := w.storedTop(t)["body"].(string)
			w.text = tc.after
			w.stageDirect(t)
			w.expectHeld(t, false, "a first line whose leading indentation alone changed")
			after, _ := w.storedTop(t)["body"].(string)
			if after == before || after != proxyAttribution+"\n\n"+tc.after {
				t.Fatalf("the body is %q (was %q), want the attribution, a blank line and the changed text", after, before)
			}
			w.parkDirect(t)
			w.stageDirect(t)
			w.expectHeld(t, true, "the changed text replayed")
		})
	}
}

// TestDirectStatementSenderValuesStillDecide: what the sender gave is the statement, so any change of
// it releases the park, the sender's evidence included.
func TestDirectStatementSenderValuesStillDecide(t *testing.T) {
	remote := "codex-session-relay --state /srv/relay supervisor-show --message abc"
	for _, tc := range []struct {
		name   string
		before []string
		change func(w *holdWorld)
	}{
		{"sender_rotated_evidence_names_the_new_sender", []string{"thread " + storeRecipientSupervisor + " reported it"}, func(w *holdWorld) {
			w.evidence = []string{"thread " + holdRuleLead + " reported it"}
		}},
		{"explicit_remote_pointer_then_omitted", []string{remote}, func(w *holdWorld) { w.evidence = nil }},
		{"remote_pointer_made_local", []string{remote}, func(w *holdWorld) {
			w.evidence = []string{w.c.command("supervisor-show", "--message", w.id)}
		}},
		{"extra_pointer", []string{remote}, func(w *holdWorld) { w.evidence = []string{remote, "commit 5c0e220a"} }},
		{"one_item_changed", []string{"commit 5c0e220a"}, func(w *holdWorld) { w.evidence = []string{"commit d5f003ee"} }},
		{"no_evidence_then_explicit", nil, func(w *holdWorld) { w.evidence = []string{"commit 5c0e220a"} }},
		{"body_changed", nil, func(w *holdWorld) { w.text = "milestone one is done, and two started" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newHoldWorld(t, "direct")
			w.evidence = tc.before
			w.stageDirect(t)
			w.parkDirect(t)
			if tc.name == "sender_rotated_evidence_names_the_new_sender" {
				w.apply(t, "other_sender")
			}
			tc.change(w)
			w.stageDirect(t)
			w.expectHeld(t, false, tc.name)
		})
	}
}

// TestDirectOldProxyRowReplayKeeps: an old proxy row (base bytes, no origin) whose body is under the
// cap, replayed through the Preface path by the same reporter, keeps its park and is not rewritten.
// The long input is the cap case, which releases once (TestDirectOldLongRowReleasesOnce).
func TestDirectOldProxyRowReplayKeeps(t *testing.T) {
	for _, in := range proxyInputs {
		if in.name == "long" {
			continue
		}
		t.Run(in.name, func(t *testing.T) {
			w := newHoldWorld(t, "direct")
			w.stageBaseProxy(t, in.text)
			w.expectBody(t, in.body)
			w.makeOld(t)
			w.parkDirect(t)
			before := w.storedPacket(t)
			restated := w.journalRows(t, "supervisor_message_restated")
			w.preface = proxyAttribution
			answer := w.stageAnswer(t)
			w.expectHeld(t, true, "an old proxy row replayed by the same reporter")
			if after := w.storedPacket(t); after != before {
				t.Fatalf("the replay rewrote the old row:\n%s\n%s", before, after)
			}
			if answer["restated"] == true || w.journalRows(t, "supervisor_message_restated") != restated {
				t.Fatalf("the replay of an old proxy row was a restatement: %v", answer)
			}
		})
	}
}

// TestDirectOldProxyLongRowReleasesOnce: the long Step-0 proxy row is at the cap, so its replay by the
// same reporter releases once and records the origin; the next replay keeps.
func TestDirectOldProxyLongRowReleasesOnce(t *testing.T) {
	w := newHoldWorld(t, "direct")
	w.stageBaseProxy(t, longDirectText)
	w.expectBody(t, baseBodyLong)
	w.makeOld(t)
	w.parkDirect(t)
	w.preface = proxyAttribution
	w.stageDirect(t)
	w.expectHeld(t, false, "an old proxy row at the cap, replayed (released once)")
	w.expectBody(t, baseBodyLong)
	w.parkDirect(t)
	w.preface = otherAttribution
	w.stageDirect(t)
	w.expectHeld(t, true, "the same report, now with its origin, from another reporter")
}

// TestDirectOldRowUnchangedReplayIsNotRestated: an old row with no hold, staged again unchanged, is
// not a restatement and is not rewritten (the byte compare leaves the missing origin out).
func TestDirectOldRowUnchangedReplayIsNotRestated(t *testing.T) {
	w := newHoldWorld(t, "direct")
	w.makeOld(t)
	before := w.storedPacket(t)
	answer := w.stageAnswer(t)
	if answer["restated"] == true || w.journalRows(t, "supervisor_message_restated") != 0 || w.storedPacket(t) != before {
		t.Fatalf("an unchanged replay of an old row answered %v with %d restated rows, packet %s", answer, w.journalRows(t, "supervisor_message_restated"), w.storedPacket(t))
	}
	w.text = "milestone one is done, and two started"
	answer = w.stageAnswer(t)
	if answer["restated"] != true {
		t.Fatalf("a changed report on an old row answered %v, want a restatement", answer)
	}
	if _, ok := w.storedTop(t)["origin"].(map[string]any); !ok {
		t.Fatalf("the restatement did not record the origin: %s", w.storedPacket(t))
	}
}

// TestDirectOldRowReadsTheFieldsWay: an old row under the cap reads as the landed rule does on both
// sides. Changed text releases; another executable (the default pointer) and another reporter (the
// attribution line) release once, the accepted cost: a duplicate turn, never a hidden change.
func TestDirectOldRowReadsTheFieldsWay(t *testing.T) {
	t.Run("changed_text", func(t *testing.T) {
		w := newHoldWorld(t, "direct")
		w.makeOld(t)
		w.parkDirect(t)
		w.apply(t, "changed")
		w.stageDirect(t)
		w.expectHeld(t, false, "an old row with changed text")
	})
	t.Run("other_program", func(t *testing.T) {
		w := newHoldWorld(t, "direct")
		w.makeOld(t)
		w.parkDirect(t)
		w.apply(t, "other_program")
		w.stageDirect(t)
		w.expectHeld(t, false, "an old row staged by another executable (released once, accepted)")
		w.parkDirect(t)
		w.c.Program = "/opt/third/codex-session-relay"
		w.stageDirect(t)
		w.expectHeld(t, true, "the same row, now with its origin, staged by a third executable")
	})
	t.Run("other_reporter", func(t *testing.T) {
		w := newHoldWorld(t, "direct")
		w.stageBaseProxy(t, "milestone one is done")
		w.makeOld(t)
		w.parkDirect(t)
		w.preface = otherAttribution
		w.stageDirect(t)
		w.expectHeld(t, false, "an old proxy row from another reporter (released once, accepted)")
	})
}

// TestDirectOldRowDefaultShapedExplicitPointerThenOmittedKeeps pins the documented limit: an old row
// whose sender gave, explicitly, the exact default pointer of the same channel reads the fields way,
// so a later omission reads as the same statement and keeps the park.
func TestDirectOldRowDefaultShapedExplicitPointerThenOmittedKeeps(t *testing.T) {
	w := newHoldWorld(t, "direct_default_pointer")
	w.makeOld(t)
	w.parkDirect(t)
	w.apply(t, "evidence_omitted")
	w.stageDirect(t)
	w.expectHeld(t, true, "an old row with the default-shaped explicit pointer, then no evidence (the documented limit)")
}

// TestDirectOldRowRewrittenThenDefaultShapedOmittedReleases: once a kept old row is rewritten by this
// build (a seat move), it carries an origin, and the same omission releases.
func TestDirectOldRowRewrittenThenDefaultShapedOmittedReleases(t *testing.T) {
	w := newHoldWorld(t, "direct_default_pointer")
	w.makeOld(t)
	w.parkDirect(t)
	w.apply(t, "seat_moved")
	w.stageDirect(t)
	w.expectHeld(t, true, "an old row whose seat moved")
	origin, ok := w.storedTop(t)["origin"].(map[string]any)
	if !ok || origin["evidence"] != "sender" {
		t.Fatalf("the kept row was not rewritten with its origin: %s", w.storedPacket(t))
	}
	w.apply(t, "evidence_omitted")
	w.stageDirect(t)
	w.expectHeld(t, false, "the rewritten row, then no evidence")
}

// TestDirectOriginReadable pins which origin records are read: an object whose evidence is exactly
// "sender" or "channel" and whose text and preface are each null or a string. Any other row is read
// the fields way.
func TestDirectOriginReadable(t *testing.T) {
	for _, tc := range []struct {
		origin   any
		readable bool
	}{
		{map[string]any{"evidence": "sender", "text": nil, "preface": nil}, true},
		{map[string]any{"evidence": "channel", "text": "sha256:00", "preface": "Reported by thread x."}, true},
		{map[string]any{"text": nil, "preface": nil}, false},
		{map[string]any{"evidence": "Channel", "text": nil, "preface": nil}, false},
		{map[string]any{"evidence": "sender", "text": json.Number("1"), "preface": nil}, false},
		{map[string]any{"evidence": "sender", "text": nil, "preface": []any{}}, false},
		{"sender", false},
		{nil, false},
	} {
		if _, got := readDirectOrigin(map[string]any{"origin": tc.origin}); got != tc.readable {
			t.Fatalf("origin %#v read %v, want %v", tc.origin, got, tc.readable)
		}
	}
}
