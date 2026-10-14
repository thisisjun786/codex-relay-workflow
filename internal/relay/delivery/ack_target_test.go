package delivery

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1302: the acknowledgement carries the notice of the target comparison as targetMoved when the comparison answers one, and is the acknowledgement
// it was otherwise: the comparison warns and the step proceeds, whatever the comparison answers, even an error.
func TestAckCarriesTheTargetNoticeAndProceeds(t *testing.T) {
	t.Parallel()
	notice := Obj{{Key: "faultClass", Value: "integration_target_moved"}, {Key: "faultId", Value: "f-1"}}
	run := func(compare TargetNoticeFunc) (Obj, string) {
		d := newDelivered(t)
		ack := d.newAck()
		var asked string
		if compare != nil {
			ack.TargetNotice = func(ctx context.Context, s *store.Store, event string) (Obj, error) {
				asked = event
				return compare(ctx, s, event)
			}
		}
		record, err := AckCommand(d.ctx, ack, nil, nil, d.event, d.turn, AckProof(d.event, d.turn), nil)
		mustDo(t, err)
		if asked != "" && asked != d.event {
			t.Fatalf("the comparison was asked about %s; want the acknowledged event %s", asked, d.event)
		}
		return record, asked
	}
	plain, _ := run(nil)
	if _, ok := plain.Lookup("targetMoved"); ok {
		t.Fatalf("an acknowledgement with no comparison carries %v", plain)
	}
	with, asked := run(func(context.Context, *store.Store, string) (Obj, error) { return notice, nil })
	if asked == "" {
		t.Fatal("the comparison was never asked")
	}
	got, ok := with.Lookup("targetMoved")
	if !ok || got.(Obj).Get("faultId") != "f-1" {
		t.Fatalf("acknowledgement %v; want targetMoved with the fault", with)
	}
	if !slices.Equal(append(keysOf(plain), "targetMoved"), keysOf(with)) {
		t.Fatalf("acknowledgement keys %v differ from the plain %v by more than targetMoved", keysOf(with), keysOf(plain))
	}
	for name, compare := range map[string]TargetNoticeFunc{
		"a comparison that finds nothing":        func(context.Context, *store.Store, string) (Obj, error) { return nil, nil },
		"a comparison that cannot read its rows": func(context.Context, *store.Store, string) (Obj, error) { return notice, errors.New("unreadable row") },
	} {
		record, _ := run(compare)
		if _, ok := record.Lookup("targetMoved"); ok || !slices.Equal(keysOf(record), keysOf(plain)) {
			t.Errorf("%s: acknowledgement %v; want the plain acknowledgement", name, record)
		}
	}
}

// keysOf is the keys of an object in order.
func keysOf(o Obj) []string {
	keys := make([]string, len(o))
	for i, f := range o {
		keys[i] = f.Key
	}
	return keys
}
