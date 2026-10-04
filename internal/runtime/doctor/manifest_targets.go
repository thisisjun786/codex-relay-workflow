package doctor

type TargetKind string

const (
	TargetHook TargetKind = "hook"
	TargetMCP  TargetKind = "mcp"
)

type TargetIssue struct {
	Kind    TargetKind `json:"kind"`
	Message string     `json:"message"`
}
type TargetParseError struct {
	Kind TargetKind
	Path string
	Err  error
}

func (e *TargetParseError) Error() string                              { return e.Err.Error() }
func (e *TargetParseError) Unwrap() error                              { return e.Err }
func ValidateManifestTargets(pluginRoot string) ([]TargetIssue, error) { return []TargetIssue{}, nil }
