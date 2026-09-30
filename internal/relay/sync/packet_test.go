package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func decodeValue(d *json.Decoder) (any, error) {
	token, e := d.Token()
	if e != nil {
		return nil, e
	}
	switch token {
	case json.Delim('{'):
		out := Obj{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return nil, e
			}
			v, e := decodeValue(d)
			if e != nil {
				return nil, e
			}
			out = append(out, contract.Field{Key: k.(string), Value: v})
		}
		_, e = d.Token()
		return out, e
	case json.Delim('['):
		out := []any{}
		for d.More() {
			v, e := decodeValue(d)
			if e != nil {
				return nil, e
			}
			out = append(out, v)
		}
		_, e = d.Token()
		return out, e
	default:
		return token, nil
	}
}

// packetFixture is what a Python packet scenario asked of the packet library: the family and
// scenarios it ran and each call it made (function, args, kwargs, and packetJSON where the call
// read a packet's JSON text), in order. Its calls to entry points no Go path calls (the packet
// composer and its field constructors, report.py's restore validator, whose Go ports wave R1
// removed) are not in it.
type packetFixture struct {
	Family    string          `json:"family"`
	Scenarios []string        `json:"scenarios"`
	Calls     json.RawMessage `json:"calls"`
}

func packetReplay(t *testing.T, family string, names ...string) {
	t.Helper()
	var fixture packetFixture
	if e := json.Unmarshal(readFixture(t, "packet"), &fixture); e != nil {
		t.Fatal(e)
	}
	if fixture.Family != family {
		t.Fatalf("the fixture holds family %s, not %s", fixture.Family, family)
	}
	sameScenarios(t, fixture.Scenarios, names)
	d := json.NewDecoder(bytes.NewReader(fixture.Calls))
	d.UseNumber()
	decoded, e := decodeValue(d)
	if e != nil {
		t.Fatal(e)
	}
	calls := decoded.([]any)
	if len(calls) == 0 {
		t.Fatal("scenario captured no public calls")
	}
	for i, call := range calls {
		function := text(reception.Get(call, "function"))
		args, _ := evidence.List(reception.Get(call, "args"))
		kwargs, _ := evidence.Object(reception.Get(call, "kwargs"))
		kw := func(k string) any { return reception.Get(kwargs, k) }
		arg := func(n int) any {
			if n >= len(args) {
				return nil
			}
			return args[n]
		}
		var got any
		var err error
		switch function {
		case "packets.required_for":
			got, err = reception.RequiredFor(text(arg(0)), text(arg(1)))
		case "packets.activation_fact":
			got, err = reception.ActivationFact(arg(0), kw("source"), text(kw("detail")))
		case "packets.activation_class":
			mode := "loop"
			if kw("mode") != nil {
				mode = text(kw("mode"))
			}
			got, err = reception.ActivationClass(arg(0), mode, kw("earlier"))
		case "packets.check":
			err = reception.CheckJSONText([]byte(text(reception.Get(call, "packetJSON"))))
			if err == nil {
				err = reception.Check(arg(0))
			}
		case "packets.reception":
			err = reception.CheckJSONText([]byte(text(reception.Get(call, "packetJSON"))))
			if err == nil {
				got, err = reception.Reception(arg(0), arg(1))
			}
		case "packets.content_digest":
			got = reception.ContentDigest(arg(0))
		case "packets.repeat":
			got = reception.Repeat(arg(0), arg(1))
		case "packets.settle_repeat":
			answer, _ := evidence.Object(arg(0))
			got = reception.SettleRepeat(answer, arg(1))
		case "packets.unobserved":
			got = reception.Unobserved()
		case "packets.check_progression":
			err = reception.CheckProgression(arg(0))
		case "packets.unsupported_promotions":
			got, err = reception.UnsupportedPromotions(arg(0))
		case "packets.claims":
			got, err = reception.Claims(arg(0), arg(1))
		case "cxc.dispatch_problems":
			got = reception.SectionProblems(arg(0), reception.DispatchSections)
		default:
			t.Fatalf("no Go entry point for %s", function)
		}
		var result Obj
		if err != nil {
			var refused *store.RefusedError
			if !errors.As(err, &refused) {
				t.Fatal(err)
			}
			result = obj("error", obj("reason", refused.Reason, "detail", refused.Detail))
		} else {
			result = obj("result", got)
		}
		golden.Check(t, fmt.Sprintf("%04d %s", i, function), []byte(evidence.Dumps(result, false, false, true)))
	}
}
