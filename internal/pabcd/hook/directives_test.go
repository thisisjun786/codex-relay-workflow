package hook

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

type directiveGolden struct {
	Constants   map[string]string
	Phases      []struct{ Phase, Directive, Active, Header, Footer, WithFooter, Empty string }
	Platforms   []struct{ Platform, Directive string }
	Resolutions []struct{ Input, Output string }
	Interview   string
	Options     []struct {
		Opts   *DirectiveOptions
		Output string
	}
	Footers  []struct{ Input, Output string }
	Literal  struct{ Input, Output string }
	FailOpen struct {
		Input, Output string
		Calls         int
	}
}

func readDirectiveJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func loadDirectiveGolden(t *testing.T) directiveGolden {
	t.Helper()
	var golden directiveGolden
	readDirectiveJSON(t, "testdata/oracle-directives.json", &golden)
	return golden
}

func directiveEqual(t *testing.T, label, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %q, want %q", label, got, want)
	}
}

func directiveEnv(bin, home string) host.LookupEnv {
	return func(key string) (string, bool) {
		switch key {
		case "CRW_BIN":
			return bin, true
		case "HOME":
			return home, true
		}
		return "", false
	}
}

func TestK5DirectiveConstants(t *testing.T) {
	var extraction struct {
		Modules map[string]map[string]json.RawMessage
	}
	readDirectiveJSON(t, "../../../contract/schema/cxc/injected-text.json", &extraction)
	sub, err := cxccorpus.LoadSubstitution("../../..")
	if err != nil {
		t.Fatal(err)
	}
	golden := loadDirectiveGolden(t)
	for _, c := range []struct{ Module, Name, Value string }{
		{"hook", "QUESTION_SHAPE_DIRECTIVE", QuestionShapeDirective},
		{"hook", "AGBROWSE_SEARCH_DIRECTIVE", AgbrowseSearchDirective},
		{"hook", "TRIGGER_AUTHORITY_NOTE", TriggerAuthorityNote},
		{"minds", "MIND_DISPATCH_DIRECTIVE", mindDispatchDirective},
	} {
		t.Run(c.Name, func(t *testing.T) {
			var raw string
			value, exists := extraction.Modules["components/pabcd-state/dist/"+c.Module+".js"][c.Name]
			if !exists {
				t.Fatal("K5 constant missing")
			}
			if err := json.Unmarshal(value, &raw); err != nil {
				t.Fatal(err)
			}
			want := sub.Expected(raw)
			directiveEqual(t, "K5", c.Value, want)
			directiveEqual(t, "recorded table substitution", golden.Constants[c.Name], want)
		})
	}
	directiveEqual(t, "PA_ATTEST_EXAMPLE", PAAttestExample, golden.Constants["PA_ATTEST_EXAMPLE"])
}

func TestRecordedPhaseAssembly(t *testing.T) {
	golden := loadDirectiveGolden(t)
	if len(golden.Phases) != 8 {
		t.Fatal("phase recording incomplete")
	}
	options := &DirectiveOptions{ActiveWorkPhase: &ActiveWorkPhase{ID: "wp2", Title: "한국어 🧪 slice"}}
	for _, c := range golden.Phases {
		t.Run(c.Phase, func(t *testing.T) {
			phase := state.Phase(c.Phase)
			directiveEqual(t, "base", PhaseDirective(phase, nil), c.Directive)
			directiveEqual(t, "active", PhaseDirective(phase, options), c.Active)
			directiveEqual(t, "header", BuildStageHeader(phase), c.Header)
			directiveEqual(t, "footer", PhaseFooter(phase), c.Footer)
			directiveEqual(t, "with footer", WithFooter("sample\r\n", phase), c.WithFooter)
			directiveEqual(t, "empty", WithFooter("", phase), c.Empty)
		})
	}
	for i, c := range golden.Options {
		directiveEqual(t, "work-phase option "+string(rune('0'+i)), PhaseDirective(state.PhaseB, c.Opts), c.Output)
	}
	for _, c := range golden.Footers {
		directiveEqual(t, "footer content", WithFooter(c.Input, state.PhaseP), c.Output)
	}
}

func TestRecordedLoopArmPlatforms(t *testing.T) {
	golden := loadDirectiveGolden(t)
	if len(golden.Platforms) != 6 {
		t.Fatal("platform recording incomplete")
	}
	for _, c := range golden.Platforms {
		t.Run(c.Platform, func(t *testing.T) { directiveEqual(t, "loop arm", LoopArmDirective(c.Platform), c.Directive) })
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	directiveEqual(t, "host default", LoopArmDirective(""), LoopArmDirective(platform))
}

func TestRecordedDirectiveResolution(t *testing.T) {
	golden := loadDirectiveGolden(t)
	for _, c := range golden.Resolutions {
		directiveEqual(t, "resolution", ResolveCRWInDirective(c.Input, directiveEnv("{CRW}", "/synthetic-home")), c.Output)
	}
	directiveEqual(t, "interview", InterviewDirective(directiveEnv("{CRW}", "/synthetic-home")), golden.Interview)
	directiveEqual(t, "literal replacement", ResolveCRWInDirective(golden.Literal.Input, directiveEnv(" literal $& $$ $1 ", "/synthetic-home")), golden.Literal.Output)
	input := "`crw pabcd scan record`"
	prefix, err := host.Invocation(directiveEnv("\uFEFF  ", "/synthetic-home"))
	if err != nil {
		t.Fatal(err)
	}
	directiveEqual(t, "runtime pointer", ResolveCRWInDirective(input, directiveEnv("\uFEFF  ", "/synthetic-home")), "`"+prefix+" pabcd scan record`")
	t.Setenv("CRW_BIN", "{CRW}")
	directiveEqual(t, "nil environment", ResolveCRWInDirective(input, nil), "`{CRW} pabcd scan record`")
	directiveEqual(t, "nil interview environment", InterviewDirective(nil), golden.Interview)
}

func TestResolutionFailureReturnsWholeInput(t *testing.T) {
	golden := loadDirectiveGolden(t)
	calls := 0
	got := resolveDirective(golden.FailOpen.Input, func() (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("recorded invocation failure")
		}
		return "{CRW}", nil
	})
	directiveEqual(t, "whole-string fail-open", got, golden.FailOpen.Output)
	if calls != golden.FailOpen.Calls {
		t.Errorf("calls = %d, want %d", calls, golden.FailOpen.Calls)
	}
	got = resolveDirective("`crw scan`", func() (string, error) { return "", errors.New("first failure") })
	directiveEqual(t, "first failure", got, "`crw scan`")
	got = resolveDirective("owns crw orchestration; `crw `", func() (string, error) { t.Fatal("invocation without command"); return "", nil })
	directiveEqual(t, "no command", got, "owns crw orchestration; `crw `")
}

func TestConstantsRemainUnresolved(t *testing.T) {
	before := PhaseDirective(state.PhaseI, nil)
	ResolveCRWInDirective(before, directiveEnv("other invocation", "/synthetic-home"))
	directiveEqual(t, "immutable phase base", PhaseDirective(state.PhaseI, nil), before)
	if !strings.Contains(before, "`crw pabcd scan") {
		t.Fatal("constant was resolved or command suffix missing")
	}
}
