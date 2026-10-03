package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// OrchestrateCliArgs is a structurally parsed command, before runtime/session gates.
// Session is nil when absent; a present empty string is kept. AttestError may coexist with Attest.
type OrchestrateCliArgs struct {
	Verb        fsm.OrchestrateVerb
	Attest      *attest.Attestation
	AttestError string
	Session     *string
	Cwd         string
	JSON        bool
}

// OrchestrateCliHelpArgs requests help without parsing the other flags.
type OrchestrateCliHelpArgs struct{ Cwd string }

// CliParseError is an unknown verb with its diagnostic context.
type CliParseError struct {
	Error   string
	Session *string
	Cwd     string
}

// OrchestrateCliParsed holds exactly one of the parser's three outcomes.
type OrchestrateCliParsed struct {
	Args  *OrchestrateCliArgs
	Help  *OrchestrateCliHelpArgs
	Error *CliParseError
}

// VerbProto represents the oracle's inherited Object.prototype value.
const VerbProto fsm.OrchestrateVerb = "__proto__"

// VerbText is the JavaScript interpolation of a parsed verb, including inherited values.
func VerbText(v fsm.OrchestrateVerb) string {
	switch v {
	case fsm.VerbConstructor:
		return "function Object() { [native code] }"
	case VerbProto:
		return "[object Object]"
	}
	return string(v)
}

func isHelpToken(v string) bool { return v == "help" || v == "--help" || v == "-h" }

// readFlagValue takes the first occurrence, used only on the unknown-verb path.
func readFlagValue(argv []string, name string) *string {
	for i, a := range argv {
		if a == name {
			if i+1 < len(argv) {
				return &argv[i+1]
			}
			return nil
		}
	}
	return nil
}

// Only ASCII lower-case keys can match VERBS. ASCII folding avoids Go's U+0130 -> i,
// which JavaScript lowercases to i plus a combining dot instead.
func cliVerb(token string) fsm.OrchestrateVerb {
	b := []byte(token)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	switch s := string(b); s {
	case "i", "p", "a", "b", "c", "d":
		return fsm.OrchestrateVerb(strings.ToUpper(s))
	case "status", "reset", "constructor", "__proto__":
		return fsm.OrchestrateVerb(s)
	}
	return ""
}

// Read exactly one complete JSON value, retaining number text for Coerce (1e999 is accepted
// by JSON.parse but dropped by Coerce). As in the attest/fsm ports, a lone surrogate becomes
// U+FFFD and nesting beyond 10000 containers is refused by encoding/json.
func decodeCliAttest(s string) (*attest.Attestation, error) {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return attest.Coerce(v), nil
}

// Node path.resolve(finalCwd, file), not filepath.Join on an absolute file.
func resolveCliFile(cwd, file string) string {
	if !filepath.IsAbs(file) {
		file = filepath.Join(cwd, file)
	}
	if abs, err := filepath.Abs(file); err == nil {
		return abs
	}
	return filepath.Clean(file)
}

func readCliAttest(file, cwd string) (*attest.Attestation, string) {
	path := resolveCliFile(cwd, file)
	b, err := os.ReadFile(path)
	var att *attest.Attestation
	if err == nil {
		att, err = decodeCliAttest(strings.TrimPrefix(source.DecodeUTF8(b), "\uFEFF"))
	}
	if err != nil {
		// The prefix/path are the oracle's; the OS/JSON cause is Go's native diagnostic, as in
		// gate.ParseSourceBoundReceipt. V8's version-specific SyntaxError text is not reproduced.
		return nil, fmt.Sprintf("could not read the attest file at %s (%v)", path, err)
	}
	if att == nil {
		return nil, "attest file " + path + " is missing valid from/to"
	}
	return att, ""
}

// ParseOrchestrateCliArgs ports orchestrate-cli.ts:169-300 (CXC v0.2.40, 3c1459ac); argv
// excludes the orchestrate token. It reads only an explicitly supplied attest file and never
// writes. Help anywhere wins; unknown flags are ignored; value flags consume the next token
// even when it is another flag. These oracle quirks are deliberate parity, not gates.
func ParseOrchestrateCliArgs(argv []string, cwd string) OrchestrateCliParsed {
	if len(argv) == 0 {
		return OrchestrateCliParsed{Help: &OrchestrateCliHelpArgs{Cwd: cwd}}
	}
	for _, a := range argv {
		if isHelpToken(a) {
			return OrchestrateCliParsed{Help: &OrchestrateCliHelpArgs{Cwd: cwd}}
		}
	}
	verb := cliVerb(argv[0])
	if verb == "" {
		out := cwd
		if v := readFlagValue(argv, "--cwd"); v != nil {
			out = *v
		}
		return OrchestrateCliParsed{Error: &CliParseError{Error: fmt.Sprintf("unknown orchestrate verb '%s' (expected I|P|A|B|C|D|status|reset); run crw pabcd orchestrate --help", argv[0]), Session: readFlagValue(argv, "--session"), Cwd: out}}
	}
	a := &OrchestrateCliArgs{Verb: verb, Cwd: cwd}
	var file *string
	sawInline := false
	for i := 1; i < len(argv); i++ {
		flag := argv[i]
		next := func() *string {
			i++
			if i < len(argv) {
				return &argv[i]
			}
			return nil
		}
		switch flag {
		case "--attest":
			sawInline = true
			raw := next()
			if raw == nil {
				a.AttestError = "--attest requires a JSON argument"
				continue
			}
			att, err := decodeCliAttest(*raw)
			if err != nil {
				a.AttestError = "attest JSON is not valid JSON"
			} else if att == nil {
				a.AttestError = "attest JSON missing valid from/to"
			} else {
				a.Attest = att
			}
		case "--attest-file":
			raw := next()
			if raw == nil {
				a.AttestError = "--attest-file requires a path argument"
				continue
			}
			file = raw
		case "--session":
			a.Session = next()
		case "--cwd":
			a.Cwd = cwd
			if raw := next(); raw != nil {
				a.Cwd = *raw
			}
		case "--json":
			a.JSON = true
		}
	}
	if file != nil {
		if sawInline {
			a.AttestError = "pass --attest OR --attest-file, not both"
		} else {
			att, errText := readCliAttest(*file, a.Cwd)
			if errText != "" {
				a.AttestError = errText
			} else {
				a.Attest = att
			}
		}
	}
	return OrchestrateCliParsed{Args: a}
}

// RenderAttestShapeHint ports orchestrate-cli.ts:357-388. A nil from means unknown; an
// illegal edge names its routes instead of teaching an unusable attest. The FSM's existing
// invalid-phase refusal replaces the oracle throw for Object.prototype phase keys.
func RenderAttestShapeHint(verb fsm.OrchestrateVerb, from *state.Phase) string {
	if verb == fsm.VerbStatus || verb == fsm.VerbReset {
		return ""
	}
	to := VerbText(verb)
	if from != nil && !fsm.IsLegalEdge(*from, state.Phase(verb)) {
		routes := []string{}
		for _, p := range fsm.ValidTransitions()[*from] {
			routes = append(routes, string(p))
		}
		return fmt.Sprintf(" Note %s -> %s is not a legal edge, so no attest can advance it: legal from %s is %s.", *from, to, *from, strings.Join(routes, "|"))
	}
	fromText, statusHint := "<see status>", " Run `crw pabcd orchestrate status --session <id>` to read the current phase."
	if from != nil {
		fromText = string(*from)
		if *from != "" {
			statusHint = ""
		}
	}
	extra := ""
	switch verb {
	case fsm.VerbA:
		extra = `, plus "planUnit":"devlog/_plan/YYMMDD_slug"`
	case fsm.VerbB:
		extra = `, plus "auditOutput":"<reviewer verdict tail>" and "auditVerdict":"pass|near-pass|fail"`
	case fsm.VerbD:
		extra = `, plus "checkOutput":"<command output tail>" and "exitCode":0`
	}
	receipt := ""
	if verb == fsm.VerbD {
		receipt = ` and "testReceiptPath"`
	}
	return fmt.Sprintf(` Every attest names the edge it advances: {"from":"%s","to":"%s","did":"..."}%s. A goalplan-bound session also needs "workPhaseId"%s.%s`, fromText, to, extra, receipt, statusHint)
}

// RenderOrchestrateHelp ports orchestrate-cli.ts:178-221. Empty platform uses the host
// (Go windows corresponds to Node win32); explicit values use Node spellings. No trailing
// newline is added. Names follow contract/schema/cxc/name-substitution.json's CLI table.
func RenderOrchestrateHelp(platform string) string {
	if platform == "" {
		platform = runtime.GOOS
		if platform == "windows" {
			platform = "win32"
		}
	}
	if platform == "win32" {
		return orchestrateHelpWindows
	}
	return orchestrateHelpPosix
}

// Verbatim platform help with the authorized name substitutions.
const orchestrateHelpPosix = "crw pabcd orchestrate — agent-gated IPABCD phase control\n" +
	"\n" +
	"Usage:\n" +
	"  crw pabcd orchestrate <I|P|A|B|C|D|status|reset> [--session <id>] [--attest <json> | --attest-file <path>] [--cwd <path>] [--json]\n" +
	"  crw pabcd orchestrate --help\n" +
	"\n" +
	"Phases:\n" +
	"  IDLE -> P -> A -> B -> C -> D -> IDLE\n" +
	"  I can be entered from IDLE/P/A/B/C/D to clarify requirements.\n" +
	"  D is a closing action; the resting state after D is IDLE.\n" +
	"\n" +
	"Agent safety:\n" +
	"  Mutating verbs (I/P/A/B/C/D/reset) require explicit --session <id>.\n" +
	"  Use your current SessionStart id, or the reserved terminal key 'cli'.\n" +
	"  status uses native CODEX_THREAD_ID when present; plain terminals may use latest-session fallback.\n" +
	"  Missing/inherited binding? Run crw relay session current, then crw relay session bind in the native cwd.\n" +
	"\n" +
	"Attestation examples:\n" +
	"  crw pabcd orchestrate A --session <id> --attest '{\"from\":\"P\",\"to\":\"A\",\"did\":\"wrote and audited the plan\",\"planUnit\":\"devlog/_plan/260714_slug\",\"workPhaseId\":\"wp1\"}'\n" +
	"  crw pabcd orchestrate B --session <id> --attest '{\"from\":\"A\",\"to\":\"B\",\"did\":\"audit passed\",\"auditOutput\":\"VERDICT: PASS\",\"auditVerdict\":\"pass\",\"workPhaseId\":\"wp1\"}'\n" +
	"  crw pabcd orchestrate C --session <id> --attest '{\"from\":\"B\",\"to\":\"C\",\"did\":\"implemented <files>\",\"workPhaseId\":\"wp1\"}'\n" +
	"  crw pabcd orchestrate D --session <id> --attest '{\"from\":\"C\",\"to\":\"D\",\"did\":\"verified\",\"checkOutput\":\"tests passed\",\"exitCode\":0,\"testReceiptPath\":\".crw/evidence/<session>/test-receipt.json\",\"workPhaseId\":\"wp1\"}'\n" +
	"  Every attest carries from/to naming the edge; they are coerced before any gate runs.\n" +
	"  (workPhaseId is required on gated edges whenever a goalplan is bound to the session,\n" +
	"   and testReceiptPath is required on C -> D for a bound session — see `crw pabcd receipt test`)\n" +
	"\n" +
	"Status:\n" +
	"  crw pabcd orchestrate status --session <id>\n" +
	"  crw pabcd orchestrate status --session <id> --json"

// Verbatim platform help with the authorized name substitutions.
const orchestrateHelpWindows = "crw pabcd orchestrate — agent-gated IPABCD phase control\n" +
	"\n" +
	"Usage:\n" +
	"  crw pabcd orchestrate <I|P|A|B|C|D|status|reset> [--session <id>] [--attest <json> | --attest-file <path>] [--cwd <path>] [--json]\n" +
	"  crw pabcd orchestrate --help\n" +
	"\n" +
	"Phases:\n" +
	"  IDLE -> P -> A -> B -> C -> D -> IDLE\n" +
	"  I can be entered from IDLE/P/A/B/C/D to clarify requirements.\n" +
	"  D is a closing action; the resting state after D is IDLE.\n" +
	"\n" +
	"Agent safety:\n" +
	"  Mutating verbs (I/P/A/B/C/D/reset) require explicit --session <id>.\n" +
	"  Use your current SessionStart id, or the reserved terminal key 'cli'.\n" +
	"  status uses native CODEX_THREAD_ID when present; plain terminals may use latest-session fallback.\n" +
	"  Missing/inherited binding? Run crw relay session current, then crw relay session bind in the native cwd.\n" +
	"\n" +
	"Attestation examples (PowerShell single quotes do NOT protect embedded double\n" +
	"quotes and cmd.exe ignores them entirely, so write the JSON to a file):\n" +
	"  '{\"from\":\"P\",\"to\":\"A\",\"did\":\"wrote and audited the plan\",\"planUnit\":\"devlog/_plan/260714_slug\",\"workPhaseId\":\"wp1\"}' | Set-Content -Encoding utf8 .crw/attest.json\n" +
	"  crw pabcd orchestrate A --session <id> --attest-file .crw/attest.json\n" +
	"  Every attest carries from/to naming the edge; they are coerced before any gate runs.\n" +
	"  (workPhaseId is required on gated edges whenever a goalplan is bound to the session,\n" +
	"   and testReceiptPath is required on C -> D for a bound session — see `crw pabcd receipt test`)\n" +
	"\n" +
	"Status:\n" +
	"  crw pabcd orchestrate status --session <id>\n" +
	"  crw pabcd orchestrate status --session <id> --json"
