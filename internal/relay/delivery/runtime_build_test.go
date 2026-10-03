package delivery

import (
	"os"
	"testing"
)

func TestAttemptRuntimeBuildRecordedBeforeTransport(t *testing.T) {
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	executable, err := os.Executable()
	mustDo(t, err)
	host := &hooked{Adapter: f.host, send: func(request, thread, message string, settings *TaskSettings) (Obj, error) {
		row, err := one(f.ctx, f.store, "SELECT record FROM attempts WHERE request_id=?", request)
		mustDo(t, err)
		identity, ok := loadsObj(row.S("record")).Get("runtime").(Obj)
		if !ok || identity.Get("build") != "dev" || identity.Get("executable") != executable {
			t.Fatalf("in-flight runtime %v, executable %s", identity, executable)
		}
		return f.host.SendMessage(f.ctx, request, thread, message, settings)
	}}
	out, err := f.delivery.Attempt(f.ctx, event, host, nil, "")
	mustDo(t, err)
	identity, ok := out.Get("runtime").(Obj)
	if !ok || identity.Get("build") != "dev" || identity.Get("executable") != executable {
		t.Fatalf("settled runtime %v", out.Get("runtime"))
	}
	stored, err := one(f.ctx, f.store, "SELECT record FROM attempts WHERE event_id=?", event)
	mustDo(t, err)
	requireSameJSON(t, "stored runtime", loadsObj(stored.S("record")).Get("runtime"), identity)
}
