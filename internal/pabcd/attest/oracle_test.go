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

// testdata/oracle-*.json hold what the CXC v0.2.40 oracle's attest.ts and plan-gate.ts answered over the grids of
// testdata/record-oracle.mjs, recorded once under Node 24 (no Node runs here); every case is replayed and must agree. Recorded
// reasons carry the oracle's cxc names and go through the corpus replayer's own name table (cxccorpus.Substituter.Expected). A
// "coerce" case feeds the raw JSON through Coerce first (the CLI path); a "direct" case decodes it into an Attestation untouched.

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
		if got := HasFailVerdictTail(parse(t, c.Input).(string)); got != c.Fail {
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
			Result        PlanResult
		}
	}
	fixture(t, "oracle-plan-gate.json", &fx)
	sub, err := cxccorpus.LoadSubstitution("../../..")
	must(t, err)
	if len(fx.Plan) < 60 {
		t.Fatalf("%d recorded plan-gate cases", len(fx.Plan))
	}
	// The five recorded cases below are where this repository departs from the oracle, by decision (CRW-425: a review finding of
	// kind security): the entry resolves outside the working directory, and the gate now refuses it. They are tagged
	// intentionally-changed. The fixture stays what the oracle answered; the replay expects the new answer, written in CRW's own
	// names (it does not go through the substitution table, which rewrites the oracle's text), and a tag whose recorded answer
	// already equals the new one fails. dir_named_like_doc (p30) is not tagged: a directory named 000_a.md inside the unit still
	// counts as a numbered document, because confinement does not reach that test.
	rootRef := "$" + "{ROOT}"
	unitOutside := func(p string) PlanResult {
		return PlanResult{Reason: "planUnit " + p + " resolves outside the working directory (symlinks followed). A plan unit must live inside the workspace: create it there with `crw pabcd plan init <slug>` and write the plan docs before P -> A."}
	}
	changed := map[string]struct {
		want PlanResult
		why  string
	}{
		"p38_absolute_outside_cwd":          {unitOutside(rootRef + "/other"), "an absolute planUnit outside the working directory"},
		"p41_relative_dotdot_sibling":       {unitOutside("../other/dir"), "a planUnit that climbs out with .. to a sibling directory"},
		"p52_planpaths_outside_unit":        {PlanResult{Reason: "planPaths entry " + rootRef + " resolves outside the working directory (symlinks followed)."}, "a planPaths entry outside the working directory, after an inside entry passed"},
		"p60_unit_is_cwd_parent":            {unitOutside(".."), ".. as the planUnit: the working directory's parent"},
		"p68_cwd_symlink_physical_has_unit": {unitOutside("../unit"), "a planUnit beside the real working directory of a symlinked working directory"},
	}
	replayed := map[string]bool{}
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
		got, want := ValidatePlanArtifacts(att, dir), c.Result
		want.Reason = sub.Expected(want.Reason)
		if ch, ok := changed[c.ID]; ok {
			if want == ch.want {
				t.Errorf("%s is tagged intentionally-changed (%s) but the recorded answer is the new one", c.ID, ch.why)
			}
			want, replayed[c.ID] = ch.want, true
		}
		got.Unit, got.Reason = unexpand(got.Unit), unexpand(got.Reason)
		if got != want {
			t.Errorf("%s: %s\n got %+v\nwant %+v", c.ID, c.Input, got, want)
		}
	}
	for id := range changed {
		if !replayed[id] {
			t.Errorf("%s is tagged intentionally-changed but was not replayed", id)
		}
	}
}
