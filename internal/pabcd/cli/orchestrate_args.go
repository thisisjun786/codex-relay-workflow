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
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
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

// readFlagValue takes the first occurrence of name's value; the metric row reads its flags with it.
func readFlagValue(argv []string, name string) *string {
	for i, a := range argv {
		if a == name {
			if i+1 < len(argv) {
				value := argv[i+1]
				return &value
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
	case "status", "reset":
		return fsm.OrchestrateVerb(s)
	}
	// The oracle's verb table is a plain object, so constructor and __proto__ find inherited values; only the
	// documented verbs are verbs here (CRW-1109).
	return ""
}

// Read exactly one complete JSON value, retaining number text for Coerce (1e999 is accepted
// by JSON.parse but dropped by Coerce) and keeping a lone surrogate escape, which JSON.parse
// preserves and pyjson.Loads reads back as the WTF-8 bytes lowerJS keeps. The syntax check stays
// encoding/json's, so its error text is unchanged; the value reading is pyjson's, because
// encoding/json would turn the surrogate into U+FFFD before Coerce ever sees it.
func decodeCliAttest(s string) (*attest.Attestation, error) {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	v, err := pyjson.Loads(string(raw), pyjson.LoadOptions{
		Surrogates: true,
		Numbers:    pyjson.SpelledNumbers,
		Map:        true,
	})
	if err != nil {
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

// orchestrateCliFlags is one scan of the options after the verb (CRW-1109), shared by a recognized verb
// and the unknown-verb diagnostic so the two select the same session and cwd. help is a help token in an
// option position; err is the first strictness refusal; session and cwd are nil and the input cwd when a
// conflicting repeat makes them ambiguous.
type orchestrateCliFlags struct {
	help       bool
	err        string
	session    *string
	cwd        string
	json       bool
	attest     []string // each --attest value in order
	attestFile *string
}

// scanOrchestrateCliFlags reads the options strictly: --session, --cwd, --attest and --attest-file take
// a value, either the next argument or after "=" in the same one; --json takes none. An unknown option, a
// stray argument, a missing value of any of the four (refused for every verb, reset and status included,
// so no command runs with a malformed option), a value that is itself an option (a typo would otherwise
// send the work to the input cwd or to no session), and a --session or --cwd repeated with a different
// value are refusals. A help token in an option position is help; as an option's value it is
// that value. The forms the skills use - space-separated values - are unchanged.
func scanOrchestrateCliFlags(args []string, cwd string) orchestrateCliFlags {
	f := orchestrateCliFlags{cwd: cwd}
	fail := func(msg string) {
		if f.err == "" {
			f.err = msg
		}
	}
	var sessions, cwds []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		name, value, inline := tok, "", false
		if strings.HasPrefix(tok, "--") {
			if k := strings.IndexByte(tok, '='); k > 0 {
				name, value, inline = tok[:k], tok[k+1:], true
			}
		}
		switch name {
		case "--json":
			if inline {
				fail("--json takes no value, got " + tok)
				continue
			}
			f.json = true
			continue
		case "--session", "--cwd", "--attest", "--attest-file":
		default:
			switch {
			case isHelpToken(tok):
				f.help = true
			case strings.HasPrefix(tok, "-"):
				fail("unknown option " + tok + " (expected --session, --cwd, --attest, --attest-file, --json)")
			default:
				fail("unexpected argument '" + tok + "' (values follow their option: --session <id>, --cwd <path>, --attest <json>)")
			}
			continue
		}
		present := inline
		if !inline && i+1 < len(args) {
			i++
			value, present = args[i], true
			if strings.HasPrefix(value, "--") {
				fail(name + " needs a value, but the next argument is the option " + value + " (use " + name + "=<value> for a value that starts with --)")
				continue
			}
		}
		switch name {
		case "--session":
			if !present {
				fail("--session requires a value")
				continue
			}
			sessions = append(sessions, value)
		case "--cwd":
			if !present {
				fail("--cwd requires a value")
				continue
			}
			cwds = append(cwds, value)
		case "--attest":
			if !present {
				fail("--attest requires a JSON argument")
				continue
			}
			f.attest = append(f.attest, value)
		case "--attest-file":
			if !present {
				fail("--attest-file requires a path argument")
				continue
			}
			v := value
			f.attestFile = &v
		}
	}
	pick := func(flag string, values []string) (*string, bool) {
		if len(values) == 0 {
			return nil, true
		}
		for _, v := range values[1:] {
			if v != values[0] {
				fail(flag + " is given more than once with different values ('" + values[0] + "', '" + v + "'); pass it once")
				return nil, false
			}
		}
		v := values[0]
		return &v, true
	}
	f.session, _ = pick("--session", sessions)
	if c, ok := pick("--cwd", cwds); ok && c != nil {
		f.cwd = *c
	}
	return f
}

// ParseOrchestrateCliArgs ports orchestrate-cli.ts:169-300 (CXC v0.2.40, 3c1459ac); argv
// excludes the orchestrate token. It reads only an explicitly supplied attest file and never
// writes. CRW-1109 departs from the oracle's lenient scan, which ignored unknown flags and
// --flag=value forms, let a value flag swallow the next flag, took help from anywhere in argv (a
// session named help included) and picked a repeated --session or --cwd differently for the
// diagnostic and for the command: the options are scanned once, strictly
// (scanOrchestrateCliFlags), help is decided after the option values are read, and only the
// documented verbs are verbs. The attestation rules are the oracle's.
func ParseOrchestrateCliArgs(argv []string, cwd string) OrchestrateCliParsed {
	if len(argv) == 0 || isHelpToken(argv[0]) {
		return OrchestrateCliParsed{Help: &OrchestrateCliHelpArgs{Cwd: cwd}}
	}
	flags := scanOrchestrateCliFlags(argv[1:], cwd)
	if flags.help {
		return OrchestrateCliParsed{Help: &OrchestrateCliHelpArgs{Cwd: cwd}}
	}
	verb := cliVerb(argv[0])
	if verb == "" {
		return OrchestrateCliParsed{Error: &CliParseError{Error: fmt.Sprintf("unknown orchestrate verb '%s' (expected I|P|A|B|C|D|status|reset); run crw pabcd orchestrate --help", argv[0]), Session: flags.session, Cwd: flags.cwd}}
	}
	if flags.err != "" {
		return OrchestrateCliParsed{Error: &CliParseError{Error: "orchestrate " + VerbText(verb) + ": " + flags.err + "; nothing was done", Session: flags.session, Cwd: flags.cwd}}
	}
	a := &OrchestrateCliArgs{Verb: verb, Cwd: flags.cwd, Session: flags.session, JSON: flags.json}
	for _, raw := range flags.attest {
		att, err := decodeCliAttest(raw)
		if err != nil {
			a.AttestError = "attest JSON is not valid JSON"
		} else if att == nil {
			a.AttestError = "attest JSON missing valid from/to"
		} else {
			a.Attest = att
		}
	}
	if flags.attestFile != nil {
		if len(flags.attest) > 0 {
			a.AttestError = "pass --attest OR --attest-file, not both"
		} else {
			att, errText := readCliAttest(*flags.attestFile, a.Cwd)
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
