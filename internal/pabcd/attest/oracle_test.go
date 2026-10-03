package attest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// testdata/oracle-attest.json and oracle-plan-gate.json hold what the CXC v0.2.40 oracle's attest.ts and plan-gate.ts answered
// over the grids of testdata/record-oracle.mjs, recorded once under Node 24 (no Node runs here). Every case is replayed and must
// agree: the recorded reasons carry the oracle's cxc names, so they go through the corpus replayer's own name table
// (cxccorpus.Substituter.Expected) before they are compared with the port's CRW text. A "coerce" case feeds the raw JSON through
// Coerce first (the CLI path); a "direct" case decodes it into an Attestation untouched (the library path).

type recordedVerdict struct {
	OK      bool  `json:"ok"`
	Reason  *int  `json:"reason"`
	Reasons []int `json:"reasons"`
}

func substituter(t *testing.T) *cxccorpus.Substituter {
	t.Helper()
	sub, err := cxccorpus.LoadSubstitution("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func readFixture(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// parse reads JSON text as the CLI would hand it to Coerce: numbers stay json.Number, so 1e999 survives to be dropped there.
func parse(t *testing.T, raw string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

func sameResult(t *testing.T, id string, got Result, want *recordedVerdict, texts func(int) string) {
	t.Helper()
	var wantReasons []string
	for _, i := range want.Reasons {
		wantReasons = append(wantReasons, texts(i))
	}
	wantReason := ""
	if want.Reason != nil {
		wantReason = texts(*want.Reason)
	}
	if got.OK != want.OK || got.Reason != wantReason || !reflect.DeepEqual(got.Reasons, wantReasons) {
		t.Errorf("%s:\n got %+v\nwant {OK:%v Reason:%q Reasons:%q}", id, got, want.OK, wantReason, wantReasons)
	}
}

func TestAttestMatchesTheRecordedOracle(t *testing.T) {
	var fx struct {
		Texts  []string
		Attest []struct {
			ID, Mode, From, To, Input string
			Att                       *Attestation
			Validate                  *recordedVerdict
		}
		Tails []struct {
			Input string
			Fail  bool
		}
		Bindings []struct {
			Input  string
			Active *string
			Result *recordedVerdict
		}
	}
	readFixture(t, "oracle-attest.json", &fx)
	sub := substituter(t)
	texts := func(i int) string { return sub.Expected(fx.Texts[i]) }
	if len(fx.Attest) < 1000 || len(fx.Tails) < 35 || len(fx.Bindings) < 24 {
		t.Fatalf("recorded cases: %d attest, %d tails, %d bindings", len(fx.Attest), len(fx.Tails), len(fx.Bindings))
	}
	for _, c := range fx.Attest {
		var att *Attestation
		if c.Mode == "coerce" {
			if att = Coerce(parse(t, c.Input)); !reflect.DeepEqual(att, c.Att) {
				t.Errorf("%s: Coerce(%s)\n got %+v\nwant %+v", c.ID, c.Input, att, c.Att)
			}
		} else if err := json.Unmarshal([]byte(c.Input), &att); err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
		sameResult(t, c.ID+" "+c.Input, Validate(state.Phase(c.From), state.Phase(c.To), att), c.Validate, texts)
	}
	for _, c := range fx.Tails {
		var in string
		if err := json.Unmarshal([]byte(c.Input), &in); err != nil {
			t.Fatal(err)
		}
		if got := HasFailVerdictTail(in); got != c.Fail {
			t.Errorf("HasFailVerdictTail(%s) = %v, oracle %v", c.Input, got, c.Fail)
		}
	}
	for _, c := range fx.Bindings {
		sameResult(t, "binding "+c.Input, ValidateWorkPhaseBinding(Coerce(parse(t, c.Input)), c.Active), c.Result, texts)
	}
}

func TestPlanGateMatchesTheRecordedOracle(t *testing.T) {
	var fx struct {
		Plan []struct {
			ID     string
			Direct bool
			Tree   []struct{ Path, Type, Target string }
			Input  string
			Result struct {
				OK           bool
				Unit, Reason string
			}
		}
	}
	readFixture(t, "oracle-plan-gate.json", &fx)
	sub := substituter(t)
	if len(fx.Plan) < 60 {
		t.Fatalf("%d recorded plan-gate cases", len(fx.Plan))
	}
	skipped := 0
	for _, c := range fx.Plan {
		root := t.TempDir()
		cwd := filepath.Join(root, "ws")
		expand := strings.NewReplacer("$"+"{CWD}", cwd, "$"+"{ROOT}", root).Replace
		unexpand := strings.NewReplacer(cwd, "$"+"{CWD}", root, "$"+"{ROOT}").Replace
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		var unreadable []string
		for _, e := range c.Tree {
			p := filepath.Join(root, e.Path)
			switch e.Type {
			case "dir":
				err := os.MkdirAll(p, 0o755)
				must(t, err)
			case "file":
				write(t, p)
			case "symlink":
				must(t, os.MkdirAll(filepath.Dir(p), 0o755))
				must(t, os.Symlink(expand(e.Target), p))
			case "chmod":
				unreadable = append(unreadable, p)
			}
		}
		for _, p := range unreadable {
			must(t, os.Chmod(p, 0))
			t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
		}
		if len(unreadable) > 0 {
			if _, err := os.ReadDir(unreadable[0]); err == nil { // still readable (running as root): the case cannot happen here
				skipped++
				continue
			}
		}
		var att *Attestation
		if c.Direct {
			must(t, json.Unmarshal([]byte(expand(c.Input)), &att))
		} else {
			att = Coerce(parse(t, expand(c.Input)))
		}
		got := ValidatePlanArtifacts(att, cwd)
		want := PlanResult{OK: c.Result.OK, Unit: c.Result.Unit, Reason: sub.Expected(c.Result.Reason)}
		if got.OK != want.OK || unexpand(got.Unit) != want.Unit || unexpand(got.Reason) != want.Reason {
			t.Errorf("%s: %s\n got %+v\nwant %+v", c.ID, c.Input, got, want)
		}
	}
	if skipped > 2 {
		t.Errorf("%d cases skipped", skipped)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
