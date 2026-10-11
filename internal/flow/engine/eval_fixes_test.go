package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

func loadChild(t *testing.T, body string) *script.Program {
	t.Helper()
	p, err := script.Load(script.Source{Name: "kid.js", Text: []byte("export const meta = {}\n" + body + "\n")})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// d1: a root script that fails stops the run at once; a continuation that was queued before the failure was noticed
// starts no further request.
func TestRootRejectionStopsQueuedContinuations(t *testing.T) {
	for name, body := range map[string]string{
		"thrown after unawaited join": `parallel([async () => { await agent('a', { agentType: 'explorer' }); await agent('b', { agentType: 'explorer' }) }])
throw new Error('stop')`,
		"rejected by a later step": `let fail
const gate = new Promise((_, rej) => { fail = rej })
parallel([
  async () => { await agent('a', { agentType: 'explorer' }); await agent('b', { agentType: 'explorer' }) },
  async () => { await agent('c', { agentType: 'explorer' }); fail(new Error('stop')) },
])
await gate`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.fake.Handler = func(r FakeRequest) FakeReply {
				if r.Prompt == "a" {
					time.Sleep(150 * time.Millisecond)
				}
				return FakeReply{}
			}
			res := h.runBounded(body, 10*time.Second)
			if res.Status != RunScriptError || res.ErrorKind != ErrKindJS || !strings.Contains(res.Error, "stop") {
				t.Fatalf("%s %s: %s", res.Status, res.ErrorKind, res.Error)
			}
			for _, r := range h.fake.Requests() {
				if r.Prompt == "b" {
					t.Errorf("request after the root failed: %+v", r)
				}
			}
			assertEndsCleanly(t, h)
		})
	}
}

// d2: a child workflow nothing can settle does not keep the run waiting, and cancelling the run's context ends it.
func TestUnsettledChildWorkflowEndsRun(t *testing.T) {
	h := newHarness(t)
	h.cfg.Workflows = func(name, path string) (*script.Program, error) {
		return loadChild(t, "await new Promise(() => {})"), nil
	}
	res := h.runBounded(`return await workflow('kid')`, 5*time.Second)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindUnsettled {
		t.Errorf("%s %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	assertEndsCleanly(t, h)
}

func TestCancelledContextEndsRun(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		time.Sleep(10 * time.Millisecond)
		return FakeReply{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	done := make(chan *Result, 1)
	prog := h.load(`for (;;) { await agent('loop', { agentType: 'explorer' }) }`)
	go func() {
		res, err := Run(ctx, prog, h.cfg)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.Status != RunScriptError || res.ErrorKind != ErrKindCancelled {
			t.Errorf("%s %s: %s", res.Status, res.ErrorKind, res.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled run did not end")
	}
	assertEndsCleanly(t, h)
}

// d3: the value or error of a child workflow that settled at once is rendered under the child's step limit.
func TestChildSettledAtOnceIsSerialisedUnderLimits(t *testing.T) {
	for name, child := range map[string]string{
		"toJSON loops":      "return { toJSON() { while (true) {} } }",
		"toString loops":    "throw { toString() { while (true) {} } }",
		"then getter loops": "return { get x() { while (true) {} } }",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.Limits = script.Limits{Step: time.Second}
			h.cfg.Workflows = func(name, path string) (*script.Program, error) { return loadChild(t, child), nil }
			res := h.runBounded(`return await workflow('kid')`, 20*time.Second)
			if res.Status != RunScriptError || res.ErrorKind != ErrKindStepLimit {
				t.Errorf("%s %s: %s", res.Status, res.ErrorKind, res.Error)
			}
		})
	}
	// a patched Promise species getter of the child runs under the limit as well
	h := newHarness(t)
	h.cfg.Limits = script.Limits{Step: time.Second}
	h.cfg.Workflows = func(name, path string) (*script.Program, error) {
		return loadChild(t, "Object.defineProperty(Promise, Symbol.species, { get() { while (true) {} } })\nreturn 1"), nil
	}
	res := h.runBounded(`return await workflow('kid')`, 20*time.Second)
	if res.Status != RunScriptError || res.ErrorKind != ErrKindStepLimit {
		t.Errorf("species: %s %s: %s", res.Status, res.ErrorKind, res.Error)
	}
}

// d4: crw.classify reads an executor RECEIPT only.
func TestClassifyRequiresExecutorReceipt(t *testing.T) {
	h := newHarness(t)
	res := h.run(`
const errs = []
const attempt = (name, f) => { try { errs.push(name + ': ' + f()) } catch (e) { errs.push(name + ': ' + e.name) } }
const reduced = { type: 'object', properties: { outcome: { type: 'string', enum: ['ready_for_review'] } }, required: ['outcome'], additionalProperties: false }
const thin = await agent.call('x', { agentType: 'executor', schema: reduced })
const other = await agent.call('x', { agentType: 'explorer', schema: reduced })
const full = await agent.call('x', { agentType: 'executor', schema: RECEIPT })
const fullOther = await agent.call('x', { agentType: 'reviewer', schema: RECEIPT })
attempt('thin executor', () => crw.classify(thin))
attempt('thin handle', () => crw.classify(thin.handle))
attempt('explorer', () => crw.classify(other))
attempt('reviewer receipt', () => crw.classify(fullOther))
attempt('full', () => crw.classify(full))
return errs`)
	v := value[[]string](t, res)
	want := []string{"thin executor: TypeError", "thin handle: TypeError", "explorer: TypeError", "reviewer receipt: TypeError", "full: ready"}
	if strings.Join(v, "|") != strings.Join(want, "|") {
		t.Errorf("%v", v)
	}
}

// d5: the fake answers a strict schema with a value that satisfies its numeric and pattern constraints.
func TestFakeSynthesisSatisfiesConstraints(t *testing.T) {
	props := map[string]string{
		"max":      `{"type":"integer","maximum":-1}`,
		"exmax":    `{"type":"integer","exclusiveMaximum":0}`,
		"exmin":    `{"type":"integer","exclusiveMinimum":5}`,
		"range":    `{"type":"number","exclusiveMinimum":1,"exclusiveMaximum":2}`,
		"multiple": `{"type":"integer","multipleOf":7,"minimum":10}`,
		"multneg":  `{"type":"integer","multipleOf":7,"maximum":-10}`,
		"fraction": `{"type":"number","multipleOf":0.25,"minimum":0.3}`,
		"pattern":  `{"type":"string","pattern":"^[A-Z]{3}-[0-9]+$"}`,
		"alt":      `{"type":"string","pattern":"^(?:alpha|beta)$"}`,
		"unanchor": `{"type":"string","pattern":"abc"}`,
		"digits":   `{"type":"string","pattern":"^\\d{4}-\\d{2}$"}`,
		"arr":      `{"type":"array","minItems":2,"items":{"type":"integer","minimum":3}}`,
		"nullable": `{"type":["integer","null"],"minimum":4}`,
		"minplain": `{"type":"integer","minimum":-5}`,
	}
	for name, p := range props {
		t.Run(name, func(t *testing.T) {
			schema := `{"type":"object","properties":{"n":` + p + `},"required":["n"],"additionalProperties":false}`
			h := newHarness(t)
			res := h.run(`const r = await agent.call('x', { agentType: 'explorer', schema: ` + schema + ` })
return [r.status, r.reason, r.output, r.meta.repairUsed]`)
			v := value[[]any](t, res)
			if v[0] != "completed" || v[3] != false {
				b, _ := json.Marshal(v)
				t.Errorf("%s", b)
			}
		})
	}
}

// A number without multipleOf is continuous: a range that holds no whole number still has an answer, and a bound that
// the schema does not name plays no part (positive, negative and mixed ranges, inclusive and exclusive bounds).
func TestFakeSynthesisContinuousBounds(t *testing.T) {
	props := map[string]string{
		"pos":        `{"type":"number","minimum":0.3,"maximum":0.7}`,
		"posex":      `{"type":"number","exclusiveMinimum":0.3,"exclusiveMaximum":0.7}`,
		"posminonly": `{"type":"number","exclusiveMinimum":0.3}`,
		"postight":   `{"type":"number","minimum":0.3,"exclusiveMinimum":0.4,"maximum":0.5}`,
		"neg":        `{"type":"number","minimum":-0.7,"maximum":-0.3}`,
		"negex":      `{"type":"number","exclusiveMinimum":-0.7,"exclusiveMaximum":-0.3}`,
		"negmaxonly": `{"type":"number","exclusiveMaximum":-0.3}`,
		"negtight":   `{"type":"number","minimum":-0.5,"maximum":-0.3,"exclusiveMaximum":-0.4}`,
		"mixbelow0":  `{"type":"number","minimum":-0.7,"exclusiveMaximum":0}`,
		"mixabove0":  `{"type":"number","exclusiveMinimum":0,"maximum":0.5}`,
		"mixexex":    `{"type":"number","exclusiveMinimum":-0.25,"exclusiveMaximum":0}`,
		"huge":       `{"type":"number","exclusiveMinimum":1e300}`,
		"hugeneg":    `{"type":"number","exclusiveMaximum":-1e300}`,
		"intpos":     `{"type":"integer","minimum":0.3,"maximum":1.7}`,
		"intneg":     `{"type":"integer","minimum":-1.7,"maximum":-0.3}`,
		"fracneg":    `{"type":"number","multipleOf":0.25,"maximum":-0.3}`,
	}
	for name, p := range props {
		t.Run(name, func(t *testing.T) {
			schema := `{"type":"object","properties":{"n":` + p + `},"required":["n"],"additionalProperties":false}`
			h := newHarness(t)
			res := h.run(`const r = await agent.call('x', { agentType: 'explorer', schema: ` + schema + ` })
return [r.status, r.reason, r.output, r.meta.repairUsed]`)
			v := value[[]any](t, res)
			if v[0] != "completed" || v[3] != false {
				b, _ := json.Marshal(v)
				t.Errorf("%s", b)
			}
		})
	}
}

// d6: the printed value of a run holds no journal sequence numbers either.
func TestCLIValueHoldsNoSequenceNumbers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "seq.js")
	body := "export const meta = { name: 'seq' }\nconst r = await parallel([async () => agent.call('a', { agentType: 'explorer' }), async () => agent.call('b', { agentType: 'explorer' })])\nlog(JSON.stringify(r[0].meta))\nreturn r\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runFlow(t, "run", "--backend", "fake", p)
	if code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if strings.Contains(out, "Seq") {
		t.Errorf("output holds sequence numbers: %s", out)
	}
	code, out, _ = runFlow(t, "run", "--backend", "fake", "--journal", p)
	if code != 0 || !strings.Contains(out, `"seq"`) {
		t.Errorf("the journal rows keep their sequence numbers: %d %s", code, out)
	}
}

// The Go text of the RECEIPT schema is the one crw:lib/schemas exports.
func TestReceiptSchemaMatchesLibrary(t *testing.T) {
	h := newHarness(t)
	res := h.run(`return RECEIPT`)
	var fromJS, fromGo any
	if err := json.Unmarshal(res.Value, &fromJS); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(receiptSchema), &fromGo); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromJS, fromGo) {
		t.Errorf("RECEIPT in schemas.js differs from receiptSchema:\njs: %s\ngo: %s", res.Value, receiptSchema)
	}
}
