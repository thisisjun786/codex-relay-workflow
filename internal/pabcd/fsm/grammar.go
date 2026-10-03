package fsm

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"

// OrchestrateVerb is a phase to enter or a control verb.
type OrchestrateVerb string

// The verbs of the chat grammar.
const (
	VerbI           OrchestrateVerb = "I"
	VerbP           OrchestrateVerb = "P"
	VerbA           OrchestrateVerb = "A"
	VerbB           OrchestrateVerb = "B"
	VerbC           OrchestrateVerb = "C"
	VerbD           OrchestrateVerb = "D"
	VerbStatus      OrchestrateVerb = "status"
	VerbReset       OrchestrateVerb = "reset"
	VerbConstructor OrchestrateVerb = "constructor"
)

// OrchestrateCommand is one parsed chat command.
type OrchestrateCommand struct {
	Verb        OrchestrateVerb
	RawAttest   *string
	Attest      *attest.Attestation
	AttestError string
}

// Stub: every function answers a zero value, so the tests fail by assertion until the port lands.

func ExtractBalancedJSON(s string) (string, bool)               { return "", false }
func ParseOrchestrateCommand(prompt string) *OrchestrateCommand { return nil }
func isJSSpace(r rune) bool                                     { return false }
func parseAttestTail(rest string) (*string, *attest.Attestation, string) {
	return nil, nil, ""
}
