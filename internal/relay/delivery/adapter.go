package delivery

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
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

// TurnStartedAt is the turn's start read by HostTime, nil when the host gave none that is a time.
func TurnStartedAt(turn *TurnInfo) *float64 {
	if turn == nil {
		return nil
	}
	return HostTime(turn.StartedAt)
}

// HostTime is hostadapter.host_time: a host timestamp in seconds, or nil when it is not a finite
// number. A bool is no time; a string is read as Python's float() reads it (surrounding
// whitespace, a sign, underscores between digits, an exponent, inf and nan, Unicode decimal
// digits), and NaN or an infinity, however spelled, is no time either. Every reader of a host
// turn start takes it through here, as every fence reader takes it through host_time.
func HostTime(value any) *float64 {
	var seconds float64
	switch v := value.(type) {
	case *float64:
		if v == nil {
			return nil
		}
		seconds = *v
	case float64:
		seconds = v
	case int:
		seconds = float64(v)
	case int64:
		seconds = float64(v)
	case json.Number:
		// Python's float() of an integer too large for a double raises OverflowError, which
		// host_time reads as no time; ParseFloat answers it with an infinity.
		number, err := strconv.ParseFloat(string(v), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil
		}
		seconds = number
	case string:
		number, ok := pyvalue.ParseFloat(v)
		if !ok {
			return nil
		}
		seconds = number
	default:
		// nil, a bool, a list, an object: float() refuses each (a bool it reads, host_time does not).
		return nil
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return nil
	}
	return &seconds
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
