package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1476 (bundle n2-layer-words): the text the daemon writes for a person names the layer (부모,
// 자식), never a relay role id written as a bare word. The relay role id `supervisor` is the 부모 and
// `parent` is the 자식 (plugins/crw/skills/crw-canon/references/hierarchy.md "The levels"). Each text
// below is built by the daemon itself, and layername.Find (the matcher the ci ratchet shares) must
// find no bare role word in it. Thread ids, table names and journal kinds stay as they are.

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

// TestDaemonWakeTextNamesTheLayers: when the 부모 channel's part of the idle pass fails for a reason
// that is not damage (a refused write, a missing table), the tick's error names the 부모 channel, both
// for the wake of an idle report and for the read of the recipients that wait on a busy 부모 message.
func TestDaemonWakeTextNamesTheLayers(t *testing.T) {
	t.Parallel()
	t.Run("an idle report the 부모 channel did not take", func(t *testing.T) {
		t.Parallel()
		host := &idleHost{}
		d, s, _ := supervisorWakeDaemon(t, host)
		tickAt(t, d, idleNow)
		host.reports = []delivery.IdleReport{{ThreadID: "01recipient-thread", Status: "idle"}}
		// The staged message waits on that recipient's edge, and the store refuses the wake's write of it,
		// a failure of the 부모 channel's own wake only: the delivery wake before it goes through.
		d.idle.beforeIdle = func() {
			exec(t, s, "UPDATE supervisor_messages SET recipient_task_id='01recipient-thread', state='deferred_busy', next_eligible_at=?", idleNow+300)
			exec(t, s, "CREATE TRIGGER refuse_the_wake BEFORE UPDATE ON supervisor_messages BEGIN SELECT RAISE(ABORT, 'the test refuses this write'); END")
		}
		d.Clock.(*delivery.FakeClock).T = idleNow + 1
		_, err := d.Tick(context.Background())
		if err == nil {
			t.Fatal("the fixture did not fail the 부모 channel's wake")
		}
		text := err.Error()
		if !strings.Contains(text, "idle report for 01recipient-thread not applied to the 부모 channel: ") {
			t.Errorf("the failed wake %q does not name the 부모 channel", text)
		}
		requireNoRoleWords(t, "the failed wake", text)
	})
	t.Run("the busy recipients the 부모 channel could not read", func(t *testing.T) {
		t.Parallel()
		host := &idleHost{}
		d, s, _ := supervisorWakeDaemon(t, host)
		d.idle.beforeHold = func() { exec(t, s, "DROP TABLE supervisor_messages") }
		_, err := d.Tick(context.Background())
		if err == nil {
			t.Fatal("the fixture did not fail the read of the busy recipients")
		}
		text := err.Error()
		if !strings.HasPrefix(text, "busy 부모 recipients not read: ") {
			t.Errorf("the failed read %q does not name the 부모", text)
		}
		requireNoRoleWords(t, "the failed read", text)
	})
}

// TestDaemonObserveNotesNameTheChild: a turn that is not observed again because a final child receipt
// already stands for it (its own, or a later admitted turn's) is noted with the 자식 the receipt is
// owed to, the relay parent's layer (the c1/c2 fixture of own_receipt_test.go and its later-receipt
// counterpart of late_observation_test.go).
func TestDaemonObserveNotesNameTheChild(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, observed, status string
		seed                   func(t *testing.T, s *store.Store)
		want                   string
	}{
		{"the turn's own final child receipt", "business", "interrupted", func(t *testing.T, s *store.Store) {
			ownReceipt(t, s, "business", "ready_for_review", "final", "", owedQueued)
		}, "observation of business (interrupted) not asserted: the turn holds its own final child receipt own-business, which is owed to the 자식"},
		{"a later admitted turn's final child receipt", "business", "interrupted", func(t *testing.T, s *store.Store) {
			lateClaim{"continuation", "child", "final", owedQueued}.insert(t, s)
		}, ", which is owed to the 자식"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx, s := lateStore(t)
			statuses := map[string]string{"anchor": "completed", "business": "completed", "continuation": "completed"}
			statuses[c.observed] = c.status
			d := New(s, &observationHost{status: "completed", statuses: statuses}, &delivery.FakeClock{T: 1700000000}, nil)
			d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
			c.seed(t, s)
			report, err := d.Tick(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var notes []string
			for _, note := range report.Notes {
				if strings.Contains(note, "not asserted") {
					notes = append(notes, note)
				}
			}
			if len(notes) != 1 {
				t.Fatalf("notes %q: want one saying the observation was not asserted", report.Notes)
			}
			if !strings.HasPrefix(notes[0], c.want) && !strings.HasSuffix(notes[0], c.want) {
				t.Errorf("the note %q does not end with the 자식 the receipt is owed to (%q)", notes[0], c.want)
			}
			requireNoRoleWords(t, "the not-asserted note", notes[0])
		})
	}
}
