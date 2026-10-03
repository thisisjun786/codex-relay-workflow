// Package gate is the source-bound receipt parser and the C>D check gate of CXC v0.2.40 (commit 3c1459ac).
package gate

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// ReceiptKind is what a receipt records: a test run or a QA verdict set.
type ReceiptKind string

// The two kinds of receipt.
const (
	ReceiptTest ReceiptKind = "test"
	ReceiptQA   ReceiptKind = "qa"
)

// ArtifactKind names a manifest entry: a verdict file or an artifact identity file.
type ArtifactKind string

// The two kinds of manifest entry.
const (
	ArtifactVerdict  ArtifactKind = "verdict"
	ArtifactIdentity ArtifactKind = "artifact-identity"
)

// ArtifactDigest is one manifest entry.
type ArtifactDigest struct {
	Path         string       `json:"path"`
	SHA256       string       `json:"sha256"`
	Kind         ArtifactKind `json:"kind"`
	CriterionIDs []string     `json:"criterionIds,omitempty"`
}

// SourceBoundReceipt is a receipt that passed the parser.
type SourceBoundReceipt struct {
	Kind              ReceiptKind          `json:"kind"`
	SourceIdentity    state.SourceIdentity `json:"sourceIdentity"`
	Command           *string              `json:"command,omitempty"`
	ExitCode          *float64             `json:"exitCode,omitempty"`
	CreatedAt         string               `json:"createdAt"`
	OwnerSessionID    *string              `json:"ownerSessionId,omitempty"`
	CheckEpoch        *string              `json:"checkEpoch,omitempty"`
	GeneratedPaths    []string             `json:"generatedPaths,omitempty"`
	ArtifactManifest  []ArtifactDigest     `json:"artifactManifest,omitempty"`
	CreatedAtProvided bool                 `json:"createdAtProvided"`
}

// ReceiptError is why a receipt was refused.
type ReceiptError struct{ Message string }

func (e ReceiptError) Error() string { return e.Message }

// ParseSourceBoundReceipt is not ported yet.
func ParseSourceBoundReceipt(path, cwd string, expected ReceiptKind) (*SourceBoundReceipt, error) {
	return nil, ReceiptError{Message: "not implemented"}
}
