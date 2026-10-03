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
// agree. The recorded reasons carry the oracle's cxc names, so they go through the corpus replayer's own name table
// (cxccorpus.Substituter.Expected) before they meet the port's CRW text. A "coerce" case feeds the raw JSON through Coerce
// first (the CLI path); a "direct" case decodes it into an Attestation untouched (the library path).

type recorded struct {
	OK      bool  `json:"ok"`
	Reason  *int  `json:"reason"`
	Reasons []int `json:"reasons"`
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	must(t, err)
	must(t, json.Unmarshal(data, into))
}

// parse reads JSON text as the CLI would hand it to Coerce: numbers stay json.Number, so 1e999 survives to be dropped there.
func parse(t *testing.T, raw string) any {
	t.Helper()
	dec, v := json.NewDecoder(strings.NewReader(raw)), any(nil)
	dec.UseNumber()
	must(t, dec.Decode(&v))
	return v
}

func sameResult(t *testing.T, id string, got Result, want *recorded, text func(int) string) {
	t.Helper()
	w := Result{OK: want.OK}
	if want.Reason != nil {
		w.Reason = text(*want.Reason)
	}
	for _, i := range want.Reasons {
		w.Reasons = append(w.Reasons, text(i))
	}
	if !reflect.DeepEqual(got, w) {
		t.Errorf("%s:\n got %+v\nwant %+v", id, got, w)
	}
}

func TestAttestMatchesTheRecordedOracle(t *testing.T) {
	var fx struct {
		Texts  []string
		Attest []struct {
			ID, Mode, From, To, Input string
			Att                       *Attestation
			Validate                  *recorded
		}
		Tails []struct {
			Input string
			Fail  bool
		}
		Bindings []struct {
			Input  string
			Active *string
			Result *recorded
		}
	}
	fixture(t, "oracle-attest.json", &fx)
	sub, err := cxccorpus.LoadSubstitution("../../..")
	must(t, err)
	texts := func(i int) string { return sub.Expected(fx.Texts[i]) }
	if len(fx.Attest) < 1000 || len(fx.Tails) < 35 || len(fx.Bindings) < 24 {
		t.Fatalf("recorded cases: %d attest, %d tails, %d bindings", len(fx.Attest), len(fx.Tails), len(fx.Bindings))
	}
	for _, c := range fx.Attest {
		var att *Attestation
		if c.Mode == "direct" {
			must(t, json.Unmarshal([]byte(c.Input), &att))
		} else if att = Coerce(parse(t, c.Input)); !reflect.DeepEqual(att, c.Att) {
			t.Errorf("%s: Coerce(%s)\n got %+v\nwant %+v", c.ID, c.Input, att, c.Att)
		}
		sameResult(t, c.ID+" "+c.Input, Validate(state.Phase(c.From), state.Phase(c.To), att), c.Validate, texts)
	}
	for _, c := range fx.Tails {
		var in string
		must(t, json.Unmarshal([]byte(c.Input), &in))
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
			ID            string
			Direct        bool
			Chdir, CwdArg string
			Tree          []struct{ Path, Type, Target string }
			Input         string
			Result        struct {
				OK           bool
				Unit, Reason string
			}
		}
	}
	fixture(t, "oracle-plan-gate.json", &fx)
	sub, err := cxccorpus.LoadSubstitution("../../..")
	must(t, err)
	if len(fx.Plan) < 60 {
		t.Fatalf("%d recorded plan-gate cases", len(fx.Plan))
	}
	skipped := 0
	for _, c := range fx.Plan {
		root := t.TempDir()
		cwd := filepath.Join(root, "ws")
		expand := strings.NewReplacer("$"+"{CWD}", cwd, "$"+"{ROOT}", root).Replace
		unexpand := strings.NewReplacer(cwd, "$"+"{CWD}", root, "$"+"{ROOT}").Replace
		must(t, os.MkdirAll(cwd, 0o755))
		unreadable := ""
		for _, e := range c.Tree {
			p := filepath.Join(root, e.Path)
			must(t, os.MkdirAll(filepath.Dir(p), 0o755))
			switch e.Type {
			case "dir":
				must(t, os.MkdirAll(p, 0o755))
			case "file":
				write(t, p)
			case "symlink":
				must(t, os.Symlink(expand(e.Target), p))
			case "chmod":
				unreadable = p
			}
		}
		if unreadable != "" {
			must(t, os.Chmod(unreadable, 0))
			t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
			if _, err := os.ReadDir(unreadable); err == nil { // still readable (running as root): this case cannot happen here
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
		// A relative working directory is resolved against the process's physical directory; t.Chdir also points $PWD at the
		// directory as named, which for a symlink is the logical path os.Getwd would answer with.
		dir := cwd
		if c.Chdir != "" {
			t.Chdir(filepath.Join(root, c.Chdir))
			dir = c.CwdArg
		}
		got, want := ValidatePlanArtifacts(att, dir), PlanResult{OK: c.Result.OK, Unit: c.Result.Unit, Reason: sub.Expected(c.Result.Reason)}
		got.Unit, got.Reason = unexpand(got.Unit), unexpand(got.Reason)
		if got != want {
			t.Errorf("%s: %s\n got %+v\nwant %+v", c.ID, c.Input, got, want)
		}
	}
	if skipped > 2 {
		t.Errorf("%d cases skipped", skipped)
	}
}
