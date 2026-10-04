// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/store/economics.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package adjudication

import "github.com/thisisjun786/codex-relay-workflow/internal/review"

type Ratio struct {
	Numerator   int      `json:"numerator"`
	Denominator int      `json:"denominator"`
	Value       *float64 `json:"value"`
}

type Row struct {
	Reviewer
	Runs                 int               `json:"runs"`
	Reviewers            int               `json:"reviewers"`
	KnownReviewerRuns    int               `json:"knownReviewerRuns"`
	KnownCallRuns        int               `json:"knownCallRuns"`
	Calls                int               `json:"calls"`
	KnownElapsedCalls    int               `json:"knownElapsedCalls"`
	KnownTokenCalls      int               `json:"knownTokenCalls"`
	ElapsedMillis        int64             `json:"elapsedMillis"`
	Tokens               review.TokenUsage `json:"tokens"`
	RecordedTokens       review.TokenUsage `json:"recordedTokens"`
	Findings             int               `json:"findings"`
	Pending              int               `json:"pending"`
	Correct              int               `json:"correct"`
	FalsePositive        int               `json:"falsePositive"`
	Minor                int               `json:"minor"`
	CorrectWithChange    int               `json:"correctWithChange"`
	CorrectWithoutChange int               `json:"correctWithoutChange"`
	CorrectChangeUnknown int               `json:"correctChangeUnknown"`
	UniqueCorrect        int               `json:"uniqueCorrect"`
	Precision            Ratio             `json:"precision"`
	P0                   Ratio             `json:"p0"`
	P1                   Ratio             `json:"p1"`
	Security             Ratio             `json:"security"`
	InvalidRuns          int               `json:"invalidRuns"`
	UnavailableRuns      int               `json:"unavailableRuns"`
	PartialRuns          int               `json:"partialRuns"`
	ModelMismatchRuns    int               `json:"modelMismatchRuns"`
	MissingPRs           []PR              `json:"missingPRs"`
	ServedModels         []string          `json:"servedModels"`
}

type Summary struct {
	PRs  []PR  `json:"prs"`
	Rows []Row `json:"rows"`
}

func Report(_ []Record, _ []PR, _ []Reviewer) (Summary, error) { return Summary{}, nil }
