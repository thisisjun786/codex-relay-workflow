package supervisor

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// render's answer with each packet field set to every JSON value shape is the golden; it began
// as what Python's SupervisorChannel.render answered.
func Test24PacketAccessorPython(t *testing.T) {
	f := fixture24(t)
	_, staged := f.staged(t)
	row, err := f.c.Get(f.ctx, staged["messageId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	raw := golden.Want(t, "packet-accessors", func() []byte { return packetAccessorRows(f, row.Packet) }, golden.Substitute(f.c.StoreDirectory(), "<state>"), golden.Substitute(repoRoot(t), "<repo>"))
	for _, one := range evidence.Items(evidence.Decode(string(raw))) {
		tc := evidence.Dict(one, false)
		field := evidence.Text(tc["field"])
		t.Run(field+"/"+evidence.Repr(tc["value"]), func(t *testing.T) {
			got, failure := packetAccessor(f, row.Packet, field, tc["value"])
			if pyjson.Dumps(failure, pyjson.Options{SortKeys: true}) != pyjson.Dumps(tc["error"], pyjson.Options{SortKeys: true}) || (failure == nil && got != tc["result"]) {
				t.Fatalf("diff: Go=(%v,%v) golden=(%v,%v)", got, failure, tc["result"], tc["error"])
			}
		})
	}
}

// packetAccessorValues is every JSON value shape a packet field is set to.
const packetAccessorValues = `[null, false, true, 0, 2, 1.5, "", "x", [], [1], {}, {"a": 1}]`

// packetAccessorRows is render's answer with each field of packet set to each value shape, as
// json.dumps renders the rows.
func packetAccessorRows(f *stageFixture, packet string) []byte {
	var rows []any
	for _, field := range []string{"kind", "sender", "basis", "evidence", "observedAt", "artifact", "generation"} {
		for _, value := range evidence.Items(evidence.Decode(packetAccessorValues)) {
			result, failure := packetAccessor(f, packet, field, value)
			rows = append(rows, contract.OrderedObject{{Key: "field", Value: field}, {Key: "value", Value: value}, {Key: "result", Value: result}, {Key: "error", Value: failure}})
		}
	}
	return []byte(pyjson.Dumps(rows, pyjson.Options{}) + "\n")
}

// packetAccessor renders packet with field set to value: the rendered message, or the refusal's
// text (nil when none).
func packetAccessor(f *stageFixture, packet, field string, value any) (result, failure any) {
	p := Packet(evidence.Dict(evidence.Decode(packet), false))
	if field == "artifact" || field == "generation" {
		p[field] = value
	} else {
		envelope := evidence.Dict(p["envelope"], false)
		envelope[field] = value
		p["envelope"] = envelope
	}
	err := func() (err error) {
		defer evidence.RecoverPython(&err)
		result = f.c.render(p, "request", "token")
		return nil
	}()
	if err != nil {
		return nil, err.Error()
	}
	return result, nil
}

func Test24MalformedProposalRollsBackClaim(t *testing.T) {
	for _, packet := range []string{`{"envelope":true}`, `{"envelope":[]}`, `{}`} {
		t.Run(packet, func(t *testing.T) {
			f := fixture24(t)
			_, staged := f.staged(t)
			id := staged["messageId"].(string)
			if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET packet=? WHERE message_id=?", packet, id); err != nil {
				t.Fatal(err)
			}
			before, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			r, err := f.c.Resolve(f.ctx, before.RelationshipID)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.c.claim(f.ctx, id, r, 1700000000, f.at, "test")
			var failure *evidence.PythonError
			if !errors.As(err, &failure) {
				t.Fatalf("expected Python error, got %T %v", err, err)
			}
			after, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatalf("claim changed message: %s -> %s", a, b)
			}
			attempts, err := f.s.SupervisorAttempts(f.ctx, id)
			if err != nil || len(attempts) != 0 {
				t.Fatalf("claim wrote attempts: %v %v", attempts, err)
			}
		})
	}
}
