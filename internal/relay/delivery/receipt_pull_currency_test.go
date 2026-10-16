package delivery

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"testing"
)

func TestReceiptPullRejectsASupersededRevisionInTheSameGeneration(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "first pull", true: "replay"}[replay], func(t *testing.T) {
			f := newFixture(t, "")
			older := f.busyCompletion(t, pullTurn)
			if replay {
				_, err := f.pull(older, pullTurn, parent, parent)
				mustDo(t, err)
			}
			newer := f.readyPayload(f.rid, 1, []string{f.artifact("newer.txt", "corrected deliverable")}, 2, assigned("completed"))
			_, err := f.accept(newer, store.AcceptOptions{})
			mustDo(t, err)
			f.declare(pyjson.Text(newer.Get("eventId")), older)
			why, err := f.delivery.SupersessionReason(f.ctx, older)
			mustDo(t, err)
			if why != SupersededRevision {
				t.Fatalf("precondition: supersession=%q", why)
			}
			_, err = f.pull(older, pullTurn, parent, parent)
			requireReason(t, err, PullNotPullable)
		})
	}
}
