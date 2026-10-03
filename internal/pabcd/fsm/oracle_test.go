package fsm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// testdata/oracle-fsm.json and oracle-grammar.json hold what the CXC v0.2.40 oracle's fsm.ts and orchestrate-grammar.ts answered
// over the grids of testdata/record-oracle.mjs, recorded once under Node 24 (no Node runs here); every row is replayed and must
// agree. A state is compared as the oracle's JSON.stringify text against state.Encode of the port's result, and the input state
// must come back unchanged. A row marked 2 is a case where the oracle throws (a phase that is an Object.prototype key): the port
// answers the refusal. Recorded reasons go through the corpus replayer's name table; chat prompts are replayed in their crw spelling.

type row []any

func (r row) s(i int) string { return r[i].(string) }
func (r row) n(i int) int    { return int(r[i].(float64)) }

type fsmFixture struct {
	Seed                                              json.RawMessage
	Trackers                                          map[string]json.RawMessage
	Order                                             []state.Phase
	ValidTransitions                                  map[state.Phase][]state.Phase
	Texts, States, Inputs                             []string
	CanEnter, Legal, Next, Gates, Transitions, Derive []row
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	must(t, err)
	must(t, json.Unmarshal(data, into))
}

// seed is the recorded state with its phase and flags (bit 1 interview, 2 auditPassed, 4 checkPassed) and tracker replaced.
func (f *fsmFixture) seed(t *testing.T, phase string, flags int, tracker string) state.State {
	t.Helper()
	var s state.State
	must(t, json.Unmarshal(f.Seed, &s))
	s.Phase, s.Flags = state.Phase(phase), state.Flags{Interview: flags&1 != 0, AuditPassed: flags&2 != 0, CheckPassed: flags&4 != 0}
	if tracker != "" {
		must(t, json.Unmarshal(f.Trackers[tracker], &s.Interview))
	}
	return s
}

func compact(t *testing.T, s state.State) string {
	t.Helper()
	enc, err := state.Encode(s)
	must(t, err)
	var out bytes.Buffer
	must(t, json.Compact(&out, enc))
	return out.String()
}

func TestFsmMatchesTheRecordedOracle(t *testing.T) {
	var f fsmFixture
	load(t, "oracle-fsm.json", &f)
	sub, err := cxccorpus.LoadSubstitution("../../..")
	must(t, err)
	if len(f.CanEnter) < 700 || len(f.Legal) < 90 || len(f.Gates) < 50 || len(f.Transitions) < 2900 || len(f.Derive) < 18 {
		t.Fatalf("recorded rows: %d canEnter, %d legal, %d gates, %d transitions, %d derive", len(f.CanEnter), len(f.Legal), len(f.Gates), len(f.Transitions), len(f.Derive))
	}
	// want is the answer of a row: its verdict (0 refused, 1 allowed, 2 thrown) with its reason text.
	want := func(r row, verdict, text int) (bool, string) {
		switch r.n(verdict) {
		case 2:
			return false, fmt.Sprintf("illegal transition %s->%s", r.s(0), r.s(1))
		case 1:
			return true, ""
		}
		return false, sub.Expected(f.Texts[r.n(text)])
	}
	if !reflect.DeepEqual(Order(), f.Order) || !reflect.DeepEqual(ValidTransitions(), f.ValidTransitions) {
		t.Errorf("Order %v, ValidTransitions %v", Order(), ValidTransitions())
	}
	for _, r := range f.CanEnter {
		ok, reason := CanEnter(state.Phase(r.s(1)), f.seed(t, r.s(0), r.n(2), ""))
		if wantOK, wantReason := want(r, 3, 4); ok != wantOK || reason != wantReason {
			t.Errorf("CanEnter(%v): got %v %q, oracle %v %q", r, ok, reason, wantOK, wantReason)
		}
	}
	for _, r := range f.Legal {
		if got := IsLegalEdge(state.Phase(r.s(0)), state.Phase(r.s(1))); got != (r.n(2) == 1) {
			t.Errorf("IsLegalEdge(%v) = %v", r, got)
		}
	}
	for _, r := range f.Next {
		got, ok := NextPhase(f.seed(t, r.s(0), 0, ""))
		if string(got) != r.s(1) || ok != (r.s(1) != "") {
			t.Errorf("NextPhase(%v) = %q, %v", r, got, ok)
		}
	}
	for _, r := range f.Gates {
		s := f.seed(t, r.s(0), r.n(1), "")
		if got := []bool{IsAuditGateOpen(s), IsBuildGateOpen(s), IsDone(s), IsIdle(s)}; !reflect.DeepEqual(got, []bool{r.n(2) == 1, r.n(3) == 1, r.n(4) == 1, r.n(5) == 1}) {
			t.Errorf("gates(%v) = %v", r, got)
		}
	}
	for _, r := range f.Transitions {
		in, var1 := f.seed(t, r.s(0), r.n(2), ""), (*attest.Attestation)(nil)
		if r.n(3) >= 0 {
			var1 = attest.Coerce(decode(t, f.Inputs[r.n(3)]))
		}
		before, res := compact(t, in), Transition(in, state.Phase(r.s(1)), var1)
		wantOK, wantReason := want(r, 4, 5)
		if res.OK != wantOK || res.Reason != wantReason || (res.State != nil) != wantOK || compact(t, in) != before {
			t.Errorf("Transition(%v): got %+v, oracle %v %q", r, res, wantOK, wantReason)
		} else if wantOK && compact(t, *res.State) != f.States[r.n(6)] {
			t.Errorf("Transition(%v): state\n got %s\nwant %s", r, compact(t, *res.State), f.States[r.n(6)])
		}
	}
	for _, r := range f.Derive {
		in := f.seed(t, "I", r.n(1), r.s(0))
		if got := compact(t, DeriveInterviewFlag(in)); got != f.States[r.n(2)] {
			t.Errorf("DeriveInterviewFlag(%v):\n got %s\nwant %s", r, got, f.States[r.n(2)])
		}
	}
}

// decode reads JSON text as the CLI hands it to Coerce: numbers stay json.Number.
func decode(t *testing.T, raw string) any {
	t.Helper()
	var v any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	must(t, dec.Decode(&v))
	return v
}

type recordedCommand struct {
	Verb   string
	Raw    *string
	Attest *attest.Attestation
	Err    *string
}

func sameCommand(want *recordedCommand, got *OrchestrateCommand) bool {
	if want == nil || got == nil {
		return want == nil && got == nil
	}
	verb, errText := want.Verb, ""
	if verb == "<function Object>" {
		verb = string(VerbConstructor)
	}
	if want.Err != nil {
		errText = *want.Err
	}
	return string(got.Verb) == verb && reflect.DeepEqual(got.RawAttest, want.Raw) && reflect.DeepEqual(got.Attest, want.Attest) && got.AttestError == errText
}

func TestGrammarMatchesTheRecordedOracle(t *testing.T) {
	var g struct {
		Cases []struct {
			O, C string
			Want *recordedCommand
		}
	}
	load(t, "oracle-grammar.json", &g)
	matched := 0
	if len(g.Cases) < 1400 {
		t.Fatalf("%d recorded cases", len(g.Cases))
	}
	for _, c := range g.Cases {
		got := ParseOrchestrateCommand(c.C)
		if !sameCommand(c.Want, got) {
			t.Errorf("%q: got %+v, oracle %+v", c.C, got, c.Want)
		}
		if c.Want != nil {
			matched++
			// no aliases: the oracle's own prefix spelling, alone on a line, no longer starts a command
			if c.O != c.C && !strings.ContainsAny(c.O, "\r\n") && ParseOrchestrateCommand(c.O) != nil {
				t.Errorf("%q: the oracle spelling still parses", c.O)
			}
		}
	}
	if matched < 400 {
		t.Errorf("only %d recorded cases are commands", matched)
	}
}

// Nesting deeper than 10000 levels is the one input the port answers differently from the oracle, which accepts it
// (V8's JSON.parse has no limit): encoding/json, jsontext included, stops at 10000, so the Go answer is that the JSON is not valid.
// The recorded oracle answer pins what is not carried; this test pins what the port does instead.
func TestNestingBeyondTenThousandLevelsIsRefused(t *testing.T) {
	var g struct {
		Deep struct {
			Depth int
			Want  recordedCommand
		}
	}
	load(t, "oracle-grammar.json", &g)
	if g.Deep.Want.Attest == nil || g.Deep.Want.Err != nil {
		t.Fatalf("the oracle no longer accepts the deep attest: %+v", g.Deep.Want)
	}
	nested := func(depth int) string {
		return "orchestrate A --attest {\"from\":\"P\",\"to\":\"A\",\"did\":\"x\",\"k\":" + strings.Repeat("[", depth) + strings.Repeat("]", depth) + "}"
	}
	if c := ParseOrchestrateCommand(nested(9999)); c == nil || c.Attest == nil || c.AttestError != "" {
		t.Errorf("9999 levels (10000 with the object): %+v", c)
	}
	c := ParseOrchestrateCommand(nested(g.Deep.Depth))
	if c == nil || c.Verb != VerbA || c.RawAttest == nil || c.Attest != nil || c.AttestError != "attest JSON is not valid JSON" {
		t.Errorf("%d levels: %+v", g.Deep.Depth, c)
	}
}
