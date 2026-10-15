package supervisor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// CRW-1476 (bundle n2-layer-words): the text the 부모 channel writes for a person names the layer
// (부모, 자식), never a relay role id written as a bare word. The relay role id `supervisor` is the
// 부모 (plugins/crw/skills/crw-canon/references/hierarchy.md "The levels"). Each text below is built
// through the channel itself, and layername.Find (the matcher the ci ratchet shares) must find no
// bare role word in it. Role ids shown as values stay ('supervisor' (부모)); wire fields, journal
// kinds and command names stay as they are.

// upwardNoMechanism is the one wording of what a report upward cannot answer (plan D6): the
// channel's reach ladder, a fault notice's ladder and reception's parent_to_supervisor entry.
const upwardNoMechanism = "a report upward is transported and read back, and nothing here records that the 부모 agreed, applied or verified anything; what became of it is read from the tracking record, confirmed"

// oldUpwardNoMechanism is that sentence as dev 7f21f0069 wrote it, which packets staged before the
// rewording still carry.
const oldUpwardNoMechanism = "a report upward is transported and read back, and nothing here records that a supervisor agreed, applied or verified anything; what became of it is read from the tracking record, confirmed"

func requireNoRoleWords(t *testing.T, what, text string) {
	t.Helper()
	if text == "" {
		t.Errorf("%s: nothing rendered", what)
		return
	}
	if hits := layername.Find(text); len(hits) > 0 {
		var words []string
		for _, h := range hits {
			start, end := max(0, h.Offset-40), min(len(text), h.Offset+len(h.Word)+40)
			words = append(words, h.Word+" in "+`"`+text[start:end]+`"`)
		}
		t.Errorf("%s names a role id where a person reads a layer (write 부모 / 자식): %s", what, strings.Join(words, "; "))
	}
}

func TestSupervisorTextNamesTheLayers(t *testing.T) {
	t.Parallel()
	t.Run("the upward reach ladder", func(t *testing.T) {
		if noMechanism != upwardNoMechanism {
			t.Errorf("noMechanism = %q, want %q", noMechanism, upwardNoMechanism)
		}
		for name, raw := range reachUnmeasured() {
			requireNoRoleWords(t, "the reach detail of "+name, raw.(map[string]any)["detail"].(string))
		}
		packet, err := composeNoticeForTest(nsNotice(), nsLive(), nsAt, "evidence line")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"agreed", "applied", "verified"} {
			detail := packet["envelope"].(map[string]any)["reach"].(map[string]any)[name].(map[string]any)["detail"]
			if detail != upwardNoMechanism {
				t.Errorf("the fault notice's %s detail = %q, want the shared sentence %q", name, detail, upwardNoMechanism)
			}
		}
	})

	t.Run("a staged report, its journal, show and send", func(t *testing.T) {
		f := fixture24(t)
		f.c.Socket = "/tmp/host.sock"
		f.c.Settings = &delivery.TaskSettings{}
		_, staged := f.staged(t)
		id := staged["messageId"].(string)
		var note string
		if err := f.s.DB.QueryRowContext(f.ctx, "SELECT detail FROM journal WHERE kind='supervisor_report' AND subject=?", f.obligation(t).ID).Scan(&note); err != nil {
			t.Fatal(err)
		}
		requireNoRoleWords(t, "the staging journal note", jsonField(t, note, "note"))
		shown, err := f.c.Show(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		requireNoRoleWords(t, "show's limits", shown["limits"].(string))
		host := &sendHost{status: "idle"}
		if _, err := f.c.Attempt(f.ctx, id, host, 1_700_000_000); err != nil {
			t.Fatal(err)
		}
		if len(host.sends) != 1 {
			t.Fatalf("sends %v", host.sends)
		}
		header, _, _ := strings.Cut(host.sends[0], "\n")
		if want := "[codex-session-relay] 부모 report"; header != want {
			t.Errorf("the sent report's header = %q, want %q", header, want)
		}
		_, err = f.c.Get(f.ctx, "missing-message")
		requireNoRoleWords(t, "the refusal for a message nobody staged", refusalDetail(t, err))
	})

	t.Run("standing and staging the standing", func(t *testing.T) {
		f := fixture24(t)
		standing, err := f.c.Standing(f.ctx, "PRJ-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		requireNoRoleWords(t, "standing's limits", standing["limits"].(string))
		staged, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
		if err != nil {
			t.Fatal(err)
		}
		requireNoRoleWords(t, "stage-standing's limits", staged["limits"].(string))
	})

	t.Run("two live seats above a project", func(t *testing.T) {
		w := newStoreRecipientWorld(t)
		if _, err := w.s.DB.ExecContext(w.ctx, "DROP INDEX scope_bindings_one_live_owner"); err != nil {
			t.Fatal(err)
		}
		w.seat("bnd-store-one", storeRecipientSeatTask)
		w.seat("bnd-store-two", storeRecipientOtherSeat)
		_, err := w.resolve()
		reason, detail := w.refusal(err)
		if reason != "duplicate_scope_owner" {
			t.Fatalf("refusal %q, want duplicate_scope_owner", reason)
		}
		requireNoRoleWords(t, "the duplicate seat refusal", detail)
		if !strings.Contains(detail, "more than one live 부모 above ") {
			t.Errorf("the duplicate seat refusal %q does not name the 부모", detail)
		}
	})
}

// TestOldWordingUnsentRowRestatesOnce (plan D8): a report staged under the old wording and never sent
// carries the old sentence in its stored packet. The next claim restates it once, with one
// supervisor_message_restated row, so the message that goes carries the new wording; after that the
// packet is stable and nothing more is journalled. Its ids do not move.
func TestOldWordingUnsentRowRestatesOnce(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	current, old := pythonJSONSorted(noMechanism), pythonJSONSorted(oldUpwardNoMechanism)
	if !strings.Contains(row.Packet, current) {
		t.Fatalf("the staged packet does not carry the reach sentence %s: %s", current, row.Packet)
	}
	if _, err = f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET packet=? WHERE message_id=?", strings.ReplaceAll(row.Packet, current, old), id); err != nil {
		t.Fatal(err)
	}
	refresh := func() bool {
		t.Helper()
		row, err := f.c.Get(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r, err := f.c.Resolve(f.ctx, row.RelationshipID)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := f.c.refreshProposal(f.ctx, row, r, f.at, 0)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if !refresh() {
		t.Fatal("an unsent report in the old wording was not restated")
	}
	after, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after.Packet, old) || !strings.Contains(after.Packet, pythonJSONSorted(upwardNoMechanism)) {
		t.Fatalf("the restated packet does not carry the new wording: %s", after.Packet)
	}
	if after.MessageID != id || after.ObligationID != row.ObligationID {
		t.Fatalf("the restatement moved an id: %s/%s -> %s/%s", id, row.ObligationID, after.MessageID, after.ObligationID)
	}
	if n := restatedRows(t, f.s); n != 1 {
		t.Fatalf("restatement rows after the first claim: %d, want 1", n)
	}
	if refresh() {
		t.Fatal("a report already in the new wording was restated again")
	}
	if n := restatedRows(t, f.s); n != 1 {
		t.Fatalf("restatement rows after the second claim: %d, want 1", n)
	}
}

// TestOldWordingSentRowStaysAsItIs (plan D8, the other half): a report that went under the old wording
// keeps the packet it went with. The restatement guard (refreshProposal's UPDATE: the row is in a
// restatable state and no other attempt of it may have gone) refuses the refresh, so no byte of the
// packet changes and no restatement is journalled. Each clause must hold alone: a dispatched row
// refreshed as the transport start of its own attempt (which the attempt clause leaves out) meets only
// the state clause, and a row that reads unsent again while an attempt that may have gone stands (no
// path writes that state today) meets only the attempt clause.
func TestOldWordingSentRowStaysAsItIs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// state is what the row reads when it is refreshed ("" the state the send left); attempt is the
		// attempt the refresh is made for (0 none).
		state   string
		attempt int64
	}{
		{"dispatched", "", 0},
		{"dispatched, refreshed for its own attempt", "", 1},
		{"unsent again behind an attempt that may have gone", "queued", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := fixture24(t)
			f.c.Socket = "/tmp/host.sock"
			f.c.Settings = &delivery.TaskSettings{}
			_, staged := f.staged(t)
			id := staged["messageId"].(string)
			host := &sendHost{status: "idle"}
			if _, err := f.c.Attempt(f.ctx, id, host, 1_700_000_000); err != nil {
				t.Fatal(err)
			}
			sent, err := f.c.Get(f.ctx, id)
			if err != nil || sent.State != "dispatched" || len(host.sends) != 1 {
				t.Fatalf("the send: state %q, %d sends, %v", sent.State, len(host.sends), err)
			}
			// the row as an earlier build sent it: the old sentence in its packet
			packet := strings.ReplaceAll(sent.Packet, pythonJSONSorted(noMechanism), pythonJSONSorted(oldUpwardNoMechanism))
			if packet == sent.Packet {
				t.Fatalf("the sent packet does not carry the reach sentence: %s", sent.Packet)
			}
			state := sent.State
			if c.state != "" {
				state = c.state
			}
			if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET packet=?, state=? WHERE message_id=?", packet, state, id); err != nil {
				t.Fatal(err)
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			r, err := f.c.Resolve(f.ctx, row.RelationshipID)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := f.c.refreshProposal(f.ctx, row, r, f.at, c.attempt)
			if changed {
				t.Fatal("the packet of a report that went was restated")
			}
			if detail := refusalDetail(t, err); detail != "the report was sent; the staged packet cannot be changed" {
				t.Fatalf("the refresh refused with %q", detail)
			}
			after, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.Packet != packet || after.MessageID != id || after.ObligationID != row.ObligationID {
				t.Fatalf("the sent row moved: %s", after.Packet)
			}
			if n := restatedRows(t, f.s); n != 0 {
				t.Fatalf("restatement rows: %d, want none", n)
			}
		})
	}
}

// jsonField is one text field of a JSON object text.
func jsonField(t *testing.T, text, key string) string {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		t.Fatalf("not a JSON object: %s: %v", text, err)
	}
	value, _ := object[key].(string)
	return value
}

// refusalDetail is the detail of the refusal err is.
func refusalDetail(t *testing.T, err error) string {
	t.Helper()
	var refused Refusal
	if !errors.As(err, &refused) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	return refused.Detail
}
