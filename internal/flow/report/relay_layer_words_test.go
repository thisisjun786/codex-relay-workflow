package report

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// CRW-1476 (bundle n2-layer-words): what the flow stages upward through the 부모 channel, and what
// RelaySender answers when it cannot stage, names the layer (부모), never a relay role id written as
// a bare word. The relay role id `supervisor` is the 부모
// (plugins/crw/skills/crw-canon/references/hierarchy.md "The levels"). layername.Find is the matcher
// the ci ratchet shares. The subject a message is staged under (and so its message id) is unchanged.

// requireNoRoleWords fails when text is empty or holds a bare role word.
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

func TestStagedReportTextNamesTheParent(t *testing.T) {
	t.Run("the stored packet of a staged report", func(t *testing.T) {
		w := newWorld(t)
		w.seat("manager-seat")
		w.manage(testManagement)
		res, err := w.relayWriter().Up(ctx, UpRequest{Kind: KindMilestoneReport, Run: testRun, Milestone: "M1", Body: []byte("milestone M1 is closed")})
		if err != nil {
			t.Fatal(err)
		}
		msgs := w.messages()
		if len(msgs) != 1 {
			t.Fatalf("%d messages, want 1", len(msgs))
		}
		m := msgs[0]
		if want := supervisor.FlowSubject(testRun, res.ReportID); m.Subject != want {
			t.Fatalf("subject %q, want %q (the staged subject, and so the message id, ignores the wording)", m.Subject, want)
		}
		requireNoRoleWords(t, "the stored packet", m.Packet)
		if !strings.Contains(m.Packet, "부모") {
			t.Errorf("the stored packet does not name the 부모: %s", m.Packet)
		}
	})

	t.Run("a report the channel has no kind, run or project for", func(t *testing.T) {
		_, err := RelaySender{}.SendSupervisor(ctx, SupervisorSend{Kind: KindMilestoneReport, ReportID: "r1"})
		if err == nil {
			t.Fatal("staged a report with no run and no project")
		}
		requireNoRoleWords(t, "the refusal", err.Error())
		if !strings.Contains(err.Error(), "the 부모 channel") {
			t.Errorf("the refusal %q does not name the 부모 channel", err)
		}
	})

	t.Run("a notice with no channel kind and no fallback", func(t *testing.T) {
		_, err := RelaySender{}.SendParent(ctx, ParentSend{LogicalID: "summary-s1", Text: "owed"})
		if err == nil {
			t.Fatal("staged a notice with no kind")
		}
		requireNoRoleWords(t, "the refusal", err.Error())
		if !strings.Contains(err.Error(), "the 부모 channel") {
			t.Errorf("the refusal %q does not name the 부모 channel", err)
		}
	})
}
