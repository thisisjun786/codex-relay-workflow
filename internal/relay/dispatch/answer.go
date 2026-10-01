package dispatch

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Answer is an ending a handler or a check already rendered: printed whole, with its own exit
// code.
type Answer interface {
	error
	ExitPayload() (contract.OrderedObject, int)
}

// PayloadExit is cli.PayloadExit: a completed answer that is still a refusal.
type PayloadExit struct {
	Payload contract.OrderedObject
	Code    int
}

func (e *PayloadExit) Error() string {
	for _, field := range e.Payload {
		if field.Key == "detail" {
			return fmt.Sprint(field.Value)
		}
	}
	return fmt.Sprint(nil)
}

// ExitPayload prints the payload whole.
func (e *PayloadExit) ExitPayload() (contract.OrderedObject, int) { return e.Payload, e.Code }

// UsageError is SystemExit2 raised by a handler: {"error": "usage", "detail": ...} with its own
// exit code.
type UsageError struct {
	Detail string
	Code   int
}

func (e *UsageError) Error() string { return e.Detail }

// ExitPayload is the usage envelope.
func (e *UsageError) ExitPayload() (contract.OrderedObject, int) {
	return contract.OrderedObject{{Key: "error", Value: "usage"}, {Key: "detail", Value: e.Detail}}, e.Code
}

// HostError is an unexpected failure the host envelope names with a class: "<Class>: <Detail>".
// The relay CLI's own failures answer through Host instead.
type HostError struct {
	Class  string
	Detail string
}

func (e *HostError) Error() string { return e.Class + ": " + e.Detail }

// ExitPayload is the host envelope.
func (e *HostError) ExitPayload() (contract.OrderedObject, int) {
	return hostEnvelope(e.Error())
}

// Host is a failure answered with the host envelope and this detail (exit 3), whatever the
// command's family makes of an unclassified failure.
func Host(detail string) error {
	payload, code := hostEnvelope(detail)
	return &PayloadExit{Payload: payload, Code: code}
}

// Detail is err's text for an answer's detail. The store's ErrNoHome keeps the words it is
// stored with, which an answer leaves out of the error that wraps it.
func Detail(err error) string {
	return strings.Replace(err.Error(), store.ErrNoHome.Error()+": ", "", 1)
}

func hostEnvelope(detail string) (contract.OrderedObject, int) {
	return contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: detail}}, contract.ExitHost
}

// emit is cli.main's reply: the result on success, else the ending as its envelope (a rendered
// answer whole; a string sqlite3 or an identity hash could not encode, raised through whatever
// wrapped it; a refusal; any other failure as a host failure, in the family's words).
func emit(stdout, stderr io.Writer, result any, err error, family *Family) int {
	code := contract.ExitOk
	var answer Answer
	var refused *store.RefusedError
	switch {
	case err == nil:
	case errors.As(err, &answer):
		result, code = answer.ExitPayload()
	case store.EncodeError(err) != nil:
		result, code = hostEnvelope(store.EncodeError(err).HostDetail())
	case errors.As(err, &refused):
		var reason any
		if refused.Reason != "" {
			reason = refused.Reason
		}
		result, code = contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: reason}, {Key: "detail", Value: refused.Detail}}, contract.ExitRefused
	case family != nil && family.HostDetail != nil:
		result, code = hostEnvelope(family.HostDetail(err))
	default:
		result, code = hostEnvelope(err.Error())
	}
	if err := contract.Emit(stdout, result); err != nil {
		fmt.Fprintln(stderr, err)
		return contract.ExitHost
	}
	return code
}
