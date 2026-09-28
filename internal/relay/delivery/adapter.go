package delivery

import (
	"encoding/json"
	"errors"
)

// HostError is a host read or send that could not complete. Kind is the Python exception class
// name a message about it carries (ConnectionError for the fake host's failed reads).
type HostError struct {
	Kind    string
	Message string
}

func (e *HostError) Error() string               { return e.Message }
func (e *HostError) PythonExceptionKind() string { return e.Kind }

type pythonException interface {
	error
	PythonExceptionKind() string
}

func errorLabel(err error) string {
	var python pythonException
	if errors.As(err, &python) {
		return python.PythonExceptionKind() + ": " + err.Error()
	}
	return "Exception: " + err.Error()
}

// ThreadFacts is hostadapter.ThreadFacts.
type ThreadFacts struct {
	RuntimeStatus  any
	CanAcceptInput any
}

// TurnInfo is hostadapter.TurnInfo.
type TurnInfo struct {
	TurnID    string
	Status    any
	StartedAt any
}

func TurnStartedAt(turn *TurnInfo) *float64 {
	if turn == nil || turn.StartedAt == nil {
		return nil
	}
	switch value := turn.StartedAt.(type) {
	case *float64:
		return value
	case float64:
		return &value
	case json.Number:
		if number, err := value.Float64(); err == nil {
			return &number
		}
	}
	return nil
}

func TurnStatus(turn *TurnInfo) string {
	if turn == nil {
		return ""
	}
	status, _ := turn.Status.(string)
	return status
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
	ListTurnIDs(thread string, limit int) ([]any, error)
	ReadTurn(thread, turn string) (*TurnInfo, error)
	SendMessage(requestID, thread, message string, settings *TaskSettings) (Obj, error)
	GetOperation(requestID string) (Obj, error)
	FindToken(thread, token string, limit int, messageOnly bool) (TokenScan, error)
	FindDispatchedTurn(thread, turnID string, sentAt float64) (TurnPresence, error)
	FindTokenSince(thread, token string, older []string, limit int) (TokenScan, error)
	FindTokenInTurn(thread, token, turnID string, limit int) (TokenScan, error)
	// RecipientFingerprint digests the newest items' content, so an append shows up.
	RecipientFingerprint(thread string) (string, error)
}
