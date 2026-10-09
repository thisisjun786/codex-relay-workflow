package manage

import (
	"context"
	_ "embed"
	"fmt"
)

// PremergeError is a refusal or a failure of the pre-merge evaluation or disposition. Exit is the
// status the command line ends with: 1 when no grade came out, 2 for a command line this command
// cannot use, 3 for a refusal before anything was graded or recorded. Reason is the name the
// refusal carries, Detail the sentence for the operator.
type PremergeError struct {
	Exit   int
	Reason string
	Detail string
}

func (e *PremergeError) Error() string { return e.Reason + ": " + e.Detail }

// PremergeEvalOptions is one evaluation: the pull request, and the optional head it must still be at,
// the plan node to bind when the issue has more than one live node, the inputs directory and whether an
// existing record for the head may be replaced.
type PremergeEvalOptions struct {
	PR      int
	Head    string
	Node    string
	Inputs  string
	Replace bool
}

// PremergeDefect is one defect of the evaluation as the command prints it.
type PremergeDefect struct {
	ID         string `json:"id"`
	Severity   string `json:"severity"`
	Impact     string `json:"impact"`
	Introduced bool   `json:"introduced"`
	InPromise  bool   `json:"in_promise"`
}

// PremergeEvalResult is what the evaluation prints.
type PremergeEvalResult struct {
	Record  string           `json:"record"`
	Grade   string           `json:"grade"`
	PR      int              `json:"pr"`
	Issue   string           `json:"issue"`
	Node    string           `json:"node"`
	Head    string           `json:"head"`
	Dev     string           `json:"dev"`
	Score   float64          `json:"score"`
	NotPass []string         `json:"not_pass"`
	Defects []PremergeDefect `json:"defects"`
}

// PremergeDisposeOptions is one disposition of a finding of a record.
type PremergeDisposeOptions struct {
	Record   string
	Ref      string
	Class    string
	Note     string
	FollowUp string
	By       string
}

// PremergeDisposeResult is what the disposition prints.
type PremergeDisposeResult struct {
	Record   string `json:"record"`
	Ref      string `json:"ref"`
	Class    string `json:"class"`
	By       string `json:"by"`
	At       string `json:"at"`
	Replaced string `json:"replaced"`
}

// premergePrompt is the grader prompt of the pre-merge evaluation, embedded so the record's
// prompt_digest names the bytes the grader was given.
//
//go:embed premerge_prompt.md
var premergePrompt string

// PremergeEval is not written yet.
func PremergeEval(_ context.Context, _ *Env, _ *Config, _ PremergeEvalOptions) (PremergeEvalResult, error) {
	return PremergeEvalResult{}, &PremergeError{Exit: 1, Reason: "not_implemented", Detail: "premerge eval"}
}

// PremergeDispose is not written yet.
func PremergeDispose(_ context.Context, _ *Env, _ *Config, _ PremergeDisposeOptions) (PremergeDisposeResult, error) {
	return PremergeDisposeResult{}, &PremergeError{Exit: 1, Reason: "not_implemented", Detail: "premerge dispose"}
}

var premergeCommand = Command{Name: "premerge", Summary: "evaluate a pull request before the merge and record the parent's dispositions", Run: premergeRun}

func init() { Register(premergeCommand) }

func premergeRun(ctx context.Context, e *Env, args []string) int {
	return premergeRunWith(ctx, e, coreDefaults(e), args)
}

func premergeRunWith(_ context.Context, e *Env, _ *Config, _ []string) int {
	fmt.Fprintln(e.Stderr, "not implemented")
	return 1
}
