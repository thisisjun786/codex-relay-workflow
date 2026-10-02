package source

import (
	"errors"
	"time"
)

// Stub for the red run: the types and signatures of the port, with behaviour that does nothing.

type Kind string

const KindResolved, KindUnavailable Kind = "resolved", "unavailable"

type Identity struct {
	Kind       Kind    `json:"kind"`
	CommitSha  string  `json:"commitSha"`
	Dirty      bool    `json:"dirty"`
	TreeHash   string  `json:"treeHash,omitempty"`
	CapturedAt string  `json:"capturedAt"`
	SourceRoot *string `json:"sourceRoot,omitempty"`
}

type ComparisonKind string

const ComparisonSame, ComparisonDifferent, ComparisonUnavailable ComparisonKind = "same", "different", "unavailable"

type Comparison struct {
	Kind   ComparisonKind `json:"kind"`
	Detail string         `json:"detail,omitempty"`
	Reason string         `json:"reason,omitempty"`
}

type Options struct {
	ExcludeStateArtifacts bool
	GeneratedPaths        []string
	Now                   func() time.Time
}

type statusRecord struct {
	xy, path []uint16
	origPath []uint16
	hasOrig  bool
}

func Capture(cwd string, o Options) Identity                     { return Identity{} }
func captureWithLimit(cwd string, o Options, limit int) Identity { return Identity{} }
func resolve(cwd, commitSha, capturedAt string, status []byte, o Options) Identity {
	return Identity{}
}
func Compare(a, b Identity) Comparison       { return Comparison{} }
func Describe(id Identity) string            { return "" }
func LooksLikeRepo(cwd string) bool          { return false }
func AssertNever(value any) error            { return nil }
func GitEnv(base []string) []string          { return nil }
func isRoutingVar(name string) bool          { return false }
func parseStatusZ(out []byte) []statusRecord { return nil }
func run(cwd string, limit int, name string, args ...string) ([]byte, error) {
	return nil, errors.New("not implemented")
}
