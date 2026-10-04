package cli

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// ReviewRoundVerb is a review-round verb.
type ReviewRoundVerb string

// The verbs of review-round.
const (
	ReviewRoundVerbOpen  ReviewRoundVerb = "open"
	ReviewRoundVerbShow  ReviewRoundVerb = "show"
	ReviewRoundVerbAbort ReviewRoundVerb = "abort"
	ReviewRoundVerbHelp  ReviewRoundVerb = "help"
)

// ReviewRoundCliArgs are the parsed arguments of a review-round verb.
type ReviewRoundCliArgs struct {
	Verb      ReviewRoundVerb `json:"verb"`
	Cwd       string          `json:"cwd"`
	Session   *string         `json:"session,omitempty"`
	PlanPaths []string        `json:"planPaths"`
	Reason    *string         `json:"reason,omitempty"`
	JSON      bool            `json:"json,omitempty"`
}

// ReviewRoundCliParsed holds exactly one of Args and a non-empty Error.
type ReviewRoundCliParsed struct {
	Args  *ReviewRoundCliArgs
	Error string
}

// ReviewRoundCliResult is the terminal answer of a run.
type ReviewRoundCliResult = CliResult

// ParseReviewRoundCliArgs is a stub: it answers nothing until the port lands.
func ParseReviewRoundCliArgs(argv []string, cwd string) ReviewRoundCliParsed {
	return ReviewRoundCliParsed{}
}

// RenderReviewRoundHelp is a stub.
func RenderReviewRoundHelp() string { return "" }

// PlanFilesHash is a stub.
func PlanFilesHash(files []goalplan.PlanFileHash) string { return "" }

// Recomputed is a stub.
func Recomputed(cwd string, files []goalplan.PlanFileHash) []goalplan.PlanFileHash { return nil }

func reviewRoundArgsCollectPlanFiles(cwd, planUnit string, paths []string) ([]goalplan.PlanFileHash, string, error) {
	return nil, "", nil
}

func reviewRoundArgsV2SpawnSurface(env host.LookupEnv) (bool, error) { return false, nil }

func reviewRoundArgsRenderOpenPacket(round goalplan.ReviewRoundState, fileCount int, env host.LookupEnv) (string, error) {
	return "", nil
}
