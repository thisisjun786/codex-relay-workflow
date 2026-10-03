package hook

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Compiling red checkpoint: assembly is absent until the parity tests are recorded failing.
const (
	QuestionShapeDirective  = ""
	AgbrowseSearchDirective = ""
	TriggerAuthorityNote    = ""
	PAAttestExample         = ""
	mindDispatchDirective   = ""
)

type ActiveWorkPhase struct{ ID, Title string }
type DirectiveOptions struct{ ActiveWorkPhase *ActiveWorkPhase }

func PhaseDirective(state.Phase, *DirectiveOptions) string   { return "" }
func BuildStageHeader(state.Phase) string                    { return "" }
func PhaseFooter(state.Phase) string                         { return "" }
func WithFooter(string, state.Phase) string                  { return "" }
func LoopArmDirective(string) string                         { return "" }
func InterviewDirective(host.LookupEnv) string               { return "" }
func ResolveCRWInDirective(string, host.LookupEnv) string    { return "" }
func resolveDirective(string, func() (string, error)) string { return "" }
