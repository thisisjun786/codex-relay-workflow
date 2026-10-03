package cli

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// OrchestrateCliArgs is a structurally parsed command, before runtime/session gates.
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

func VerbText(v fsm.OrchestrateVerb) string { return string(v) }
func ParseOrchestrateCliArgs(argv []string, cwd string) OrchestrateCliParsed {
	return OrchestrateCliParsed{}
}
func RenderOrchestrateHelp(platform string) string                             { return "" }
func RenderAttestShapeHint(verb fsm.OrchestrateVerb, from *state.Phase) string { return "" }
