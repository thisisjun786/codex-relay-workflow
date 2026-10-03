package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// recordedArgs is JSON.stringify of the Node answer, preserving absent versus empty session.
type recordedArgs struct {
	Verb        fsm.OrchestrateVerb
	Attest      *attest.Attestation
	AttestError string
	Session     *string
	Cwd         string
	JSON        bool
	Help        bool
	Error       string
}

func observed(p OrchestrateCliParsed) recordedArgs {
	if p.Help != nil {
		return recordedArgs{Help: true, Cwd: p.Help.Cwd}
	}
	if p.Error != nil {
		return recordedArgs{Error: p.Error.Error, Cwd: p.Error.Cwd, Session: p.Error.Session}
	}
	if p.Args == nil {
		return recordedArgs{}
	}
	a := p.Args
	return recordedArgs{Verb: a.Verb, Attest: a.Attest, AttestError: a.AttestError, Session: a.Session, Cwd: a.Cwd, JSON: a.JSON}
}

type argsCase struct {
	ID    string
	Argv  []string
	Cwd   string
	Files map[string]string
	Want  recordedArgs
}

func readArgsOracle(t *testing.T) (parser, files []argsCase, hints []struct {
	Verb fsm.OrchestrateVerb
	From *state.Phase
	Want string
}, help map[string]string) {
	t.Helper()
	var fixture struct {
		Parser, Files []argsCase
		Hints         []struct {
			Verb fsm.OrchestrateVerb
			From *state.Phase
			Want string
		}
		Help map[string]string
	}
	b, err := os.ReadFile("testdata/orchestrate_args/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Parser, fixture.Files, fixture.Hints, fixture.Help
}

func crwNames(s string) string {
	return strings.NewReplacer("cxc orchestrate", "crw pabcd orchestrate", "cxc session", "crw relay session", "cxc receipt", "crw pabcd receipt", ".codexclaw", ".crw").Replace(s)
}

func assertArgs(t *testing.T, argv []string, cwd string, want recordedArgs) {
	t.Helper()
	before := append([]string(nil), argv...)
	p := ParseOrchestrateCliArgs(argv, cwd)
	variants := 0
	for _, set := range []bool{p.Args != nil, p.Help != nil, p.Error != nil} {
		if set {
			variants++
		}
	}
	if variants != 1 {
		t.Errorf("expected one variant, got %+v", p)
	}
	got := observed(p)
	want.Error = crwNames(want.Error)
	// V8/OS diagnostics and Go diagnostics differ inside the cause, not the ported prefix.
	if strings.HasPrefix(want.AttestError, "could not read the attest file at ") {
		prefix, _, _ := strings.Cut(want.AttestError, " (")
		if !strings.HasPrefix(got.AttestError, prefix+" (") || strings.HasSuffix(got.AttestError, " ()") {
			t.Errorf("file refusal: got %q, want prefix %q and non-empty cause", got.AttestError, prefix)
		}
		if strings.Contains(want.AttestError, "ENOENT") && !strings.Contains(got.AttestError, "no such file") {
			t.Errorf("missing-file cause: %q", got.AttestError)
		}
		if strings.Contains(want.AttestError, "EISDIR") && !strings.Contains(got.AttestError, "directory") {
			t.Errorf("directory cause: %q", got.AttestError)
		}
		if strings.Contains(want.AttestError, "JSON") && !strings.Contains(got.AttestError, "invalid") && !strings.Contains(got.AttestError, "unexpected") {
			t.Errorf("JSON cause: %q", got.AttestError)
		}
		got.AttestError = want.AttestError
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv %q: got %+v, want %+v", argv, got, want)
	}
	if !reflect.DeepEqual(argv, before) && !(len(argv) == 0 && len(before) == 0) {
		t.Fatal("parser changed argv")
	}
}

func TestOrchestrateArgsRecorded(t *testing.T) {
	parser, files, _, _ := readArgsOracle(t)
	for _, c := range parser {
		t.Run(c.ID, func(t *testing.T) { assertArgs(t, c.Argv, c.Cwd, c.Want) })
	}
	for _, c := range files {
		t.Run(c.ID, func(t *testing.T) {
			root := t.TempDir()
			for p, b := range c.Files {
				if err := os.WriteFile(filepath.Join(root, p), []byte(b), 0600); err != nil {
					t.Fatal(err)
				}
			}
			argv := append([]string(nil), c.Argv...)
			for i := range argv {
				argv[i] = strings.ReplaceAll(argv[i], "$R", root)
			}
			c.Want.Cwd = strings.ReplaceAll(c.Want.Cwd, "$R", root)
			c.Want.AttestError = strings.ReplaceAll(c.Want.AttestError, "$R", root)
			assertArgs(t, argv, "/unused", c.Want)
			for p, want := range c.Files {
				b, err := os.ReadFile(filepath.Join(root, p))
				if err != nil || string(b) != want {
					t.Fatalf("attest file changed: %v", err)
				}
			}
		})
	}
}

func TestOrchestrateHelpAndShapeHintsRecorded(t *testing.T) {
	_, _, hints, help := readArgsOracle(t)
	for platform, want := range help {
		t.Run(platform, func(t *testing.T) {
			got := RenderOrchestrateHelp(platform)
			if got != crwNames(want) {
				t.Errorf("help differs from Node answer: %q", got)
			}
		})
	}
	for _, c := range hints {
		from := "null"
		if c.From != nil {
			from = string(*c.From)
		}
		t.Run(string(c.Verb)+"/"+from, func(t *testing.T) {
			if got := RenderAttestShapeHint(c.Verb, c.From); got != crwNames(c.Want) {
				t.Errorf("got %q, want %q", got, crwNames(c.Want))
			}
		})
	}
}

// Direct B-class assertions from orchestrate-cli.test.ts:92-212 and attest-shape-hint.test.ts:98-145.
func TestOrchestrateArgsBClass(t *testing.T) {
	t.Run("structural quoted JSON", func(t *testing.T) {
		r := ParseOrchestrateCliArgs([]string{"a", "--attest", `{"from":"P","to":"A","did":"x y z"}`}, "/ws")
		if r.Args == nil || r.Args.Verb != fsm.VerbA || !reflect.DeepEqual(r.Args.Attest, &attest.Attestation{From: state.PhaseP, To: state.PhaseA, Did: "x y z"}) {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("unknown and malformed", func(t *testing.T) {
		if r := ParseOrchestrateCliArgs([]string{"idle"}, "/ws"); r.Error == nil {
			t.Fatal("missing unknown-verb error")
		}
		if r := ParseOrchestrateCliArgs([]string{"a", "--attest", "{nope}"}, "/ws"); r.Args == nil || r.Args.AttestError == "" {
			t.Fatal("missing malformed attest error")
		}
	})
	t.Run("help tokens", func(t *testing.T) {
		for _, a := range [][]string{{"--help"}, {"-h"}, {"help"}, {"status", "--help"}} {
			if ParseOrchestrateCliArgs(a, "/ws").Help == nil {
				t.Fatalf("%q", a)
			}
		}
	})
	t.Run("platform help", func(t *testing.T) {
		win, posix := RenderOrchestrateHelp("win32"), RenderOrchestrateHelp("linux")
		if strings.Contains(win, "--attest '{") || !strings.Contains(win, "--attest-file") || !strings.Contains(posix, "--attest '{") {
			t.Fatal("wrong platform examples")
		}
		for _, edge := range []string{`"from":"P","to":"A"`, `"from":"A","to":"B"`, `"from":"B","to":"C"`, `"from":"C","to":"D"`, `testReceiptPath`} {
			if !strings.Contains(posix, edge) {
				t.Fatalf("missing %s", edge)
			}
		}
		platform := runtime.GOOS
		if platform == "windows" {
			platform = "win32"
		}
		if RenderOrchestrateHelp("") != RenderOrchestrateHelp(platform) {
			t.Fatal("host default")
		}
	})
	t.Run("hint activation", func(t *testing.T) {
		p := state.PhaseP
		for _, v := range []fsm.OrchestrateVerb{fsm.VerbStatus, fsm.VerbReset} {
			if RenderAttestShapeHint(v, &p) != "" {
				t.Fatal("control hint")
			}
		}
		bad := RenderAttestShapeHint(fsm.VerbD, &p)
		if !strings.Contains(bad, "legal from P is I|A") || strings.Contains(bad, `"from":"P","to":"D"`) {
			t.Fatalf("%q", bad)
		}
	})
}

func TestOrchestrateAttestFileFollowsSymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "att.json")
	if err := os.WriteFile(path, []byte(`{"from":"P","to":"A","did":"file attest"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(root, "link.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	r := ParseOrchestrateCliArgs([]string{"a", "--attest-file", "link.json", "--cwd", root}, "/unused")
	if r.Args == nil || r.Args.AttestError != "" || r.Args.Attest == nil || r.Args.Attest.Did != "file attest" {
		t.Fatalf("got %+v", r)
	}
}

func TestOrchestrateInheritedVerbText(t *testing.T) {
	for v, want := range map[fsm.OrchestrateVerb]string{fsm.VerbConstructor: "function Object() { [native code] }", VerbProto: "[object Object]", fsm.VerbA: "A"} {
		if got := VerbText(v); got != want {
			t.Errorf("%q: %q", v, got)
		}
	}
}
