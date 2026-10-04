package reset

type ResetScope string

const (
	State     ResetScope = "state"
	Generated ResetScope = "generated"
	Goalplans ResetScope = "goalplans"
	All       ResetScope = "all"
)

type ResetResult struct {
	Scope   ResetScope `json:"scope"`
	Removed []string   `json:"removed"`
	Absent  []string   `json:"absent"`
}

func ParseResetScope(args []string) (ResetScope, error) { return State, nil }
func RunReset(cwd string, scope ResetScope) (ResetResult, error) {
	return ResetResult{Scope: scope, Removed: []string{}, Absent: []string{}}, nil
}
func RenderReset(result ResetResult) string { return "" }
