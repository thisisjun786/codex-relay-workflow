package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// loopOracleDoc is the recorded CXC v0.2.40 output for the loop parser, renderers and
// help (testdata/loop/oracle.json, recorded by testdata/loop/record-oracle.mjs).
type loopOracleDoc struct {
	Source         string          `json:"source"`
	Classification string          `json:"classification"`
	Parse          []loopParseCase `json:"parse"`
	Help           struct {
		Code   int
		Output string
	} `json:"help"`
	Plans []loopPlanCase `json:"plans"`
	Show  []loopShowCase `json:"show"`
}

type loopParseCase struct {
	Argv   []string
	Result json.RawMessage
}

type loopPlanCase struct {
	Name    string
	Lock    bool
	Present bool
	Plan    json.RawMessage
	Output  string
	Code    int
}

type loopShowCase struct {
	Name   string
	WS     string
	Argv   []string
	Output string
	Code   int
}

func loadLoopOracle(t *testing.T) loopOracleDoc {
	t.Helper()
	raw, err := os.ReadFile("testdata/loop/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc loopOracleDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// loopSubstitution is the corpus replayer's own name table: the expected text is the
// oracle's with cxc names rewritten to crw's, the same transformation the replayer
// applies, so a passing comparison is parity through the corpus, not a hand rule.
func loopSubstitution(t *testing.T) *cxccorpus.Substituter {
	t.Helper()
	sub, err := cxccorpus.LoadSubstitution("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestLoopParseOracle(t *testing.T) {
	o := loadLoopOracle(t)
	sub := loopSubstitution(t)
	if len(o.Parse) == 0 {
		t.Fatal("oracle contains no parser cases")
	}
	for _, c := range o.Parse {
		t.Run(strings.Join(c.Argv, " "), func(t *testing.T) {
			before := append([]string{}, c.Argv...)
			args, err := ParseLoopCliArgs(c.Argv, "<WS>")
			var got any
			if err != nil {
				got = map[string]any{"error": err.Error()}
			} else {
				b, merr := json.Marshal(args)
				if merr != nil {
					t.Fatal(merr)
				}
				if uerr := json.Unmarshal(b, &got); uerr != nil {
					t.Fatal(uerr)
				}
			}
			var want any
			if uerr := json.Unmarshal([]byte(sub.Expected(string(c.Result))), &want); uerr != nil {
				t.Fatal(uerr)
			}
			if !reflect.DeepEqual(got, want) {
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want)
				t.Errorf("argv %q:\n got %s\nwant %s", c.Argv, gb, wb)
			}
			if !reflect.DeepEqual(before, c.Argv) {
				t.Error("parser changed argv")
			}
		})
	}
}

func TestLoopHelpOracle(t *testing.T) {
	o := loadLoopOracle(t)
	sub := loopSubstitution(t)
	want := sub.Expected(o.Help.Output)
	if got := RenderLoopHelp(); got != want {
		t.Fatalf("help:\n got %q\nwant %q", got, want)
	}
}

// TestLoopDescribeReadFailureShapes covers the three failure shapes the recorded show
// case cannot reach (the oracle's invalid-json detail is a Node parser message). The
// sentence templates are the oracle's (:302-313); the engine detail is a stand-in.
func TestLoopDescribeReadFailureShapes(t *testing.T) {
	cases := []struct {
		name string
		read goalplan.GoalplanReadResult
		verb string
		slug string
		want string
	}{
		{"invalid-json", goalplan.GoalplanReadResult{Diagnostic: &goalplan.GoalplanReadDiagnostic{Kind: "invalid-json", Path: "/p/g.json", Detail: "Unexpected token o"}}, "show", "s",
			"loop show: the plan at /p/g.json is not valid JSON: Unexpected token o"},
		{"invalid-shape", goalplan.GoalplanReadResult{Diagnostic: &goalplan.GoalplanReadDiagnostic{Kind: "invalid-shape", Path: "/p/g.json", Field: "objective", Detail: "expected a string"}}, "validate", "s",
			"loop validate: the plan at /p/g.json is structurally invalid - field 'objective': expected a string"},
		{"unreadable", goalplan.GoalplanReadResult{Diagnostic: &goalplan.GoalplanReadDiagnostic{Kind: "unreadable", Path: "/p/g.json", Detail: "EACCES: permission denied"}}, "show", "s",
			"loop show: the plan at /p/g.json could not be read: EACCES: permission denied"},
		{"no-diagnostic", goalplan.GoalplanReadResult{}, "show", "s",
			"loop show: the plan at s could not be read: unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DescribeLoopReadFailure(c.read, c.verb, c.slug); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestLoopResolveSlugShowOracle drives the recorded show outputs through the units this
// issue ports: resolveSlug, the goalplan read and the renderer/describeReadFailure. The
// verb wiring that turns them into the CLI's exit code is CRW-646.
func TestLoopResolveSlugShowOracle(t *testing.T) {
	o := loadLoopOracle(t)
	sub := loopSubstitution(t)
	for _, c := range o.Show {
		t.Run(c.Name, func(t *testing.T) {
			cwd := t.TempDir()
			if c.WS == "bound" {
				writeLoopBoundSession(t, cwd, o)
			}
			args, err := ParseLoopCliArgs(c.Argv, cwd)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.ReplaceAll(sub.Expected(c.Output), "<WS>", cwd)
			switch {
			case strings.HasPrefix(want, "[crw loop: "):
				rest := want[len("[crw loop: "):]
				slug := rest[:strings.Index(rest, "]")]
				got := ResolveLoopSlug(args)
				if got == nil || *got != slug {
					t.Fatalf("slug = %v, want %q", got, slug)
				}
				plan := goalplan.ReadGoalplan(cwd, slug)
				if plan == nil {
					t.Fatalf("plan %q not readable", slug)
				}
				lock, err := goalplan.GoalplanWriteLockStatus(cwd, slug, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got := RenderLoopPlan(plan, &lock); got != want {
					t.Fatalf("render:\n got %q\nwant %q", got, want)
				}
			case strings.Contains(want, "no plan found at slug '"):
				at := strings.Index(want, "no plan found at slug '") + len("no plan found at slug '")
				slug := want[at : at+strings.Index(want[at:], "'")]
				got := ResolveLoopSlug(args)
				if got == nil || *got != slug {
					t.Fatalf("slug = %v, want %q", got, slug)
				}
				read := goalplan.ReadGoalplanDetailed(cwd, slug)
				if got := DescribeLoopReadFailure(read, "show", slug); got != want {
					t.Fatalf("describe:\n got %q\nwant %q", got, want)
				}
			default:
				if got := ResolveLoopSlug(args); got != nil {
					t.Fatalf("slug = %q, want nil", *got)
				}
			}
		})
	}
}

// writeLoopBoundSession makes session rec-s1 bind the recorded rich plan, so resolveSlug's
// session branch and the plan renderer have the oracle's inputs.
func writeLoopBoundSession(t *testing.T, cwd string, o loopOracleDoc) {
	t.Helper()
	var rich json.RawMessage
	for _, p := range o.Plans {
		if p.Name == "show-rich" {
			rich = p.Plan
		}
	}
	if rich == nil {
		t.Fatal("oracle has no show-rich plan")
	}
	var plan goalplan.Goalplan
	planJSON := strings.ReplaceAll(string(rich), "<TS>", "2026-01-01T00:00:00.000Z")
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		t.Fatal(err)
	}
	if err := goalplan.WriteGoalplan(cwd, &plan); err != nil {
		t.Fatal(err)
	}
	sessionRaw, err := json.Marshal(map[string]any{"phase": "IDLE", "sessionId": "rec-s1", "slug": "rich-plan"})
	if err != nil {
		t.Fatal(err)
	}
	path := state.StatePath(cwd, "rec-s1")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, sessionRaw, 0o600); err != nil {
		t.Fatal(err)
	}
}
