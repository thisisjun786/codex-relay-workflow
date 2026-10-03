package goalplan

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

type dependencyNode struct {
	id        string
	dependsOn []string
}

type GoalplanValidation struct {
	OK      bool     `json:"ok"`
	Reasons []string `json:"reasons"`
}

type GoalplanReceiptEvidence struct {
	SourceIdentity   SourceIdentity
	ArtifactManifest []gate.ArtifactDigest
}

type GoalplanValidationCtx struct {
	Cwd                   string
	CaptureSourceIdentity func(string) SourceIdentity
	CompareSource         func(SourceIdentity, SourceIdentity) source.Comparison
	ReadReceipt           func(string, gate.ReceiptKind) (GoalplanReceiptEvidence, error)
}

func duplicateIDs(ids []string) []string                                { return []string{} }
func findDependencyCycle(nodes []dependencyNode) []string               { return nil }
func GoalplanDefinitionIntegrityReasons(plan *Goalplan) []string        { return []string{} }
func GoalplanDependencyCompletionReasons(plan *Goalplan) []string       { return []string{} }
func EffectiveSchemaVersion(plan *Goalplan, markerPresent bool) float64 { return 1 }
func ComputeQaRequired(plan *Goalplan) bool                             { return false }
func supersededIntegrityReasons(plan *Goalplan) []string                { return []string{} }
func SchemaMarkerPath(cwd, slug string) (string, error)                 { return "", nil }
