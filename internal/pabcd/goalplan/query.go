package goalplan

type ReadyGoalplanTask struct {
	WorkPhaseID string        `json:"workPhaseId"`
	Task        *GoalplanTask `json:"task"`
}
type NextOpenGoalplanTask struct {
	WP   *GoalplanWorkPhase `json:"wp"`
	Task *GoalplanTask      `json:"task"`
}
type DependencyDeadlock struct {
	Reasons []string `json:"reasons"`
}

func RemainingWorkPhases(*Goalplan) []*GoalplanWorkPhase             { return []*GoalplanWorkPhase{} }
func WorkPhaseDependenciesMet(*Goalplan, *GoalplanWorkPhase) bool    { return false }
func TaskDependenciesMet(*GoalplanWorkPhase, *GoalplanTask) bool     { return false }
func OpenDecisionIDsForPhase(*Goalplan, *GoalplanWorkPhase) []string { return []string{} }
func WorkPhaseReadyConditionsMet(*Goalplan, *GoalplanWorkPhase) bool { return false }
func IsRunnablePhase(*Goalplan, *GoalplanWorkPhase) bool             { return false }
func ReadyWorkPhases(*Goalplan) []*GoalplanWorkPhase                 { return []*GoalplanWorkPhase{} }
func ReadyTasks(*Goalplan) []ReadyGoalplanTask                       { return []ReadyGoalplanTask{} }
func NextOpenTask(*Goalplan) *NextOpenGoalplanTask                   { return nil }
func DependencyWaitReasons(*Goalplan) []string                       { return []string{} }
func DetectDependencyDeadlock(*Goalplan) *DependencyDeadlock         { return nil }
func RemainingWorkAwaitsDecisions(*Goalplan) bool                    { return false }
