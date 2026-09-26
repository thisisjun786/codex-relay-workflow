package delivery

// HostError is a host read or send that could not complete. Kind is the Python exception class
// name a message about it carries (ConnectionError for the fake host's failed reads).
type HostError struct {
	Kind    string
	Message string
}

func (e *HostError) Error() string { return e.Message }

func errorLabel(err error) string {
	if h, ok := err.(*HostError); ok {
		return h.Kind + ": " + h.Message
	}
	return "Exception: " + err.Error()
}

// ThreadFacts is hostadapter.ThreadFacts.
type ThreadFacts struct {
	RuntimeStatus  string
	CanAcceptInput *bool
}

// TurnInfo is hostadapter.TurnInfo.
type TurnInfo struct {
	TurnID    string
	Status    string
	StartedAt *float64
}

// TokenScan is hostadapter.TokenScan.
type TokenScan struct {
	Found                bool
	TurnID               any
	Exhausted            bool
	Scanned              int
	OtherTurn, OtherKind any
}

// Adapter is hostadapter.HostAdapter: reads, and the one supported send. Nothing here can change
// a task's model, effort, sandbox, approval policy, goal or archive state.
type Adapter interface {
	ReadThread(thread string) (ThreadFacts, error)
	IsArchived(thread string, cwd any) (*bool, error)
	ReadGoalStatus(thread string) (any, error)
	ListTurnIDs(thread string, limit int) ([]string, error)
	ReadTurn(thread, turn string) (*TurnInfo, error)
	SendMessage(requestID, thread, message string, settings *TaskSettings) (Obj, error)
	GetOperation(requestID string) (Obj, error)
	FindToken(thread, token string, limit int, messageOnly bool) (TokenScan, error)
	FindDispatchedTurn(thread, turnID string, sentAt float64) (TurnPresence, error)
	FindTokenSince(thread, token string, older []string, limit int) (TokenScan, error)
	FindTokenInTurn(thread, token, turnID string, limit int) (TokenScan, error)
}
