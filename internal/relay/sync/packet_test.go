package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
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

// retiredPacketFunctions are the Python packet library's entry points that no Go path calls: the
// packet composer and its field constructors, and report.py's restore validator. Wave R1 removed
// their Go ports; a recorded scenario still calls them, so its other calls are compared without
// them.
var retiredPacketFunctions = map[string]bool{
	"packets.pull_request": true, "packets.locator": true, "packets.policy": true, "packets.callback": true,
	"packets.unexamined": true, "packets.compose": true, "packets.progression_lines": true,
	"report.child_purpose": true, "report._check_restore": true,
}

func packetReplay(t *testing.T, family string, names ...string) {
	t.Helper()
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(names)
	if e != nil {
		t.Fatal(e)
	}
	question, e := json.Marshal([]any{family, names})
	if e != nil {
		t.Fatal(e)
	}
	stdout := pythonAnswer(t, "packet_capture.py", question, func() ([]byte, error) {
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(root, "internal/relay/sync/testdata/packet_capture.py"), family, string(raw))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, ".venv"), "UV_CACHE_DIR="+t.TempDir()+"/uv")
		stdout, e := cmd.Output()
		if e != nil {
			var exit *exec.ExitError
			if errors.As(e, &exit) {
				return nil, fmt.Errorf("python scenario: %v\n%s", e, exit.Stderr)
			}
			return nil, e
		}
		return stdout, nil
	})
	d := json.NewDecoder(bytes.NewReader(stdout))
	d.UseNumber()
	decoded, e := decodeValue(d)
	if e != nil {
		t.Fatal(e)
	}
	calls := decoded.([]any)
	if len(calls) == 0 {
		t.Fatal("scenario captured no public calls")
	}
	compared := 0
	defer func() {
		if compared == 0 {
			t.Error("every call this scenario made is retired, so it compares nothing")
		}
	}()
	for i, call := range calls {
		function := text(reception.Get(call, "function"))
		if retiredPacketFunctions[function] {
			continue
		}
		compared++
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
		var expected Obj
		if reception.Has(call, "error") {
			expected = obj("error", reception.Get(call, "error"))
		} else {
			expected = obj("result", reception.Get(call, "result"))
		}
		want := evidence.Dumps(expected, false, false, true)
		actual := evidence.Dumps(result, false, false, true)
		if actual != want {
			t.Fatalf("call %d %s\ninput: %s\nPython: %s\nGo: %s", i, function, evidence.Dumps(call, false, false, true), want, actual)
		}
	}
}
