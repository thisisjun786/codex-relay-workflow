package gui

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
)

// policyBody is the GET /api/policy answer. A value the reader could not establish is a named
// state or a null with a reason, never a zero and never an empty success.
type policyBody struct {
	State            string                      `json:"state"`
	Reason           string                      `json:"reason,omitempty"`
	Path             string                      `json:"path,omitempty"`
	Mode             string                      `json:"mode,omitempty"`
	Digest           string                      `json:"digest,omitempty"`
	RegisteredDigest string                      `json:"registeredDigest,omitempty"`
	RunningDigest    *string                     `json:"runningDigest"`
	RunningReason    string                      `json:"runningReason,omitempty"`
	Roles            []policystore.RoleView      `json:"roles"`
	Allowed          []policystore.AllowedView   `json:"allowed"`
	Exceptions       []policystore.ExceptionView `json:"exceptions"`
	Applied          string                      `json:"applied"`
	Actions          []string                    `json:"actions"`
}

// policyCheckBody is the POST /api/policy/check request. Change is one of the five kinds, and
// expectedDigest is the digest the caller read.
type policyCheckBody struct {
	ExpectedDigest string             `json:"expectedDigest"`
	Change         policystore.Change `json:"change"`
}

// checkBody is the POST /api/policy/check answer.
type checkBody struct {
	Valid         bool     `json:"valid"`
	Errors        []string `json:"errors"`
	CurrentDigest string   `json:"currentDigest"`
	Stale         bool     `json:"stale"`
	Diff          []string `json:"diff"`
}

// envLookup is the process environment as a LookupEnv.
func envLookup(key string) (string, bool) { return os.LookupEnv(key) }

// policyHandler answers GET /api/policy: the policy file the wiring record names, its declared
// values and the file, registered and running digests with the applied state.
func policyHandler(_ *Env, r *http.Request) (Response, error) {
	located := policystore.Locate(envLookup)
	reading := policystore.Read(located)
	running := policystore.RunningDigest(r.Context(), envLookup)
	applied := policystore.Applied(reading, running)
	body := policyBody{
		State:            reading.State,
		Reason:           reading.Reason,
		Path:             reading.Path,
		Digest:           reading.Digest,
		RegisteredDigest: reading.RegisteredDigest,
		Roles:            emptyIfNilRoles(reading.Roles),
		Allowed:          emptyIfNilAllowed(reading.Allowed),
		Exceptions:       emptyIfNilExceptions(reading.Exceptions),
		Applied:          applied,
		Actions:          policystore.AppliedActions(applied),
	}
	if reading.State == policystore.Registered {
		body.Mode = reading.Mode()
	}
	if running.State == policystore.RunningObserved {
		digest := running.Digest
		body.RunningDigest = &digest
	} else {
		body.RunningReason = running.Reason
	}
	return Response{Status: http.StatusOK, Body: body}, nil
}

// policyCheckHandler answers POST /api/policy/check. It applies the change to an in-memory copy of
// the policy and judges it with the bridge's own parser; it writes nothing at all.
func policyCheckHandler(_ *Env, r *http.Request) (Response, error) {
	var request policyCheckBody
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return Response{Status: http.StatusBadRequest, Body: jsonError{Error: codeBadRequest}}, nil
	}
	located := policystore.Locate(envLookup)
	if located.State != policystore.Registered {
		return Response{Status: http.StatusOK, Body: checkBody{
			Valid:  false,
			Errors: []string{"no registered execution policy: " + located.Reason},
		}}, nil
	}
	raw, err := os.ReadFile(located.Path)
	if err != nil {
		return Response{Status: http.StatusOK, Body: checkBody{
			Valid:  false,
			Errors: []string{"the execution policy could not be read: " + err.Error()},
		}}, nil
	}
	result := policystore.Check(raw, request.ExpectedDigest, request.Change)
	return Response{Status: http.StatusOK, Body: checkBody{
		Valid:         result.Valid,
		Errors:        emptyIfNil(result.Errors),
		CurrentDigest: result.CurrentDigest,
		Stale:         result.Stale,
		Diff:          emptyIfNil(result.Diff),
	}}, nil
}

func emptyIfNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func emptyIfNilRoles(values []policystore.RoleView) []policystore.RoleView {
	if values == nil {
		return []policystore.RoleView{}
	}
	return values
}

func emptyIfNilAllowed(values []policystore.AllowedView) []policystore.AllowedView {
	if values == nil {
		return []policystore.AllowedView{}
	}
	return values
}

func emptyIfNilExceptions(values []policystore.ExceptionView) []policystore.ExceptionView {
	if values == nil {
		return []policystore.ExceptionView{}
	}
	return values
}

func init() {
	Register(http.MethodGet, "/api/policy", policyHandler)
	Register(http.MethodPost, "/api/policy/check", policyCheckHandler)
}
