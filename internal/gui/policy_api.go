package gui

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
)

// policyWriteSeams are the seams the write route runs with. The zero value is the production one:
// the clock, install.Main called in this process, and the running relay's worker policy digest. A
// test replaces them, so the route can be driven end to end without starting an installer or
// reading a real relay.
var policyWriteSeams policystore.WriteOptions

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

// policyWriteRequest is the POST /api/policy request: the digest the caller read and the change it
// proposes. It is the same shape the check route takes, because a caller checks and then writes the
// same proposal.
type policyWriteRequest struct {
	ExpectedDigest string             `json:"expectedDigest"`
	Change         policystore.Change `json:"change"`
}

// policyWriteDigest is one digest of the write result.
type policyWriteDigest struct {
	Digest string `json:"digest"`
	// RecordBackup is the wiring record's own backup, taken by the installer. It is a different
	// artifact from the top-level backup, which holds the policy file's previous bytes.
	RecordBackup string `json:"recordBackup,omitempty"`
	// RestartRequired is the installer's advice about a bridge or relay service already running.
	RestartRequired string `json:"restartRequired,omitempty"`
}

// policyWriteBody is the 200 answer: what was stored, what the wiring record now names, whether the
// running relay has it, and the work a caller must still do. The three are separate fields because
// they are three separate facts.
type policyWriteBody struct {
	Stored policyWriteDigest `json:"stored"`
	// Registered is omitted when the wiring record could not be read back after the registration
	// reported success: the digest it names was not established, and reporting the file's digest here
	// would equate two facts this answer exists to keep apart.
	Registered *policyWriteDigest `json:"registered,omitempty"`
	Applied    string             `json:"applied"`
	Actions    []string           `json:"actions"`
	Backup     pathText           `json:"backup,omitempty"`
	Warnings   []string           `json:"warnings,omitempty"`
}

// policyWriteErrorBody is every refusal of the write route. Only the fields the outcome has are
// set, so a stale digest carries the digest on disk and a recovery carries both digests and the
// command that settles them. It never carries an environment variable, a secret or the token.
type policyWriteErrorBody struct {
	Error            string   `json:"error"`
	Reason           string   `json:"reason,omitempty"`
	CurrentDigest    string   `json:"currentDigest,omitempty"`
	Errors           []string `json:"errors,omitempty"`
	Restored         bool     `json:"restored,omitempty"`
	FileDigest       string   `json:"fileDigest,omitempty"`
	RegisteredDigest string   `json:"registeredDigest,omitempty"`
	Backup           pathText `json:"backup,omitempty"`
	Recovery         string   `json:"recovery,omitempty"`
	Step             string   `json:"step,omitempty"`
	// Warnings carry what the outcome could not establish: a restore whose directory was not synced,
	// for example, is still a restore but may not survive a power loss.
	Warnings []string `json:"warnings,omitempty"`
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
		Actions:          policystore.AppliedActions(reading, applied),
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
	raw, err := policystore.ReadRaw(located.Path)
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

// policyWriteHandler answers POST /api/policy. It is a thin mapper over policystore.Write: the
// write path owns the order, the lock, the backup, the atomic replacement and the judgement of the
// registration's answer, and this maps its named outcome to a status and a body.
func policyWriteHandler(_ *Env, r *http.Request) (Response, error) {
	var request policyWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return Response{Status: http.StatusBadRequest, Body: jsonError{Error: codeBadRequest}}, nil
	}
	result := policystore.Write(r.Context(), envLookup, policyWriteSeams,
		policystore.WriteRequest{ExpectedDigest: request.ExpectedDigest, Change: request.Change})
	switch result.Kind {
	case policystore.WriteStored:
		var registered *policyWriteDigest
		if result.RegisteredDigest != "" {
			registered = &policyWriteDigest{Digest: result.RegisteredDigest,
				RecordBackup: result.RecordBackup, RestartRequired: result.RestartRequired}
		}
		return Response{Status: http.StatusOK, Body: policyWriteBody{
			Stored:     policyWriteDigest{Digest: result.StoredDigest},
			Registered: registered,
			Applied:    result.Applied,
			Actions:    emptyIfNil(result.Actions),
			Backup:     pathText(result.Backup),
			Warnings:   result.Warnings,
		}}, nil
	case policystore.WriteStaleDigest:
		return Response{Status: http.StatusConflict, Body: policyWriteErrorBody{
			Error: "stale_digest", CurrentDigest: result.CurrentDigest}}, nil
	case policystore.WriteInvalidPolicy:
		return Response{Status: http.StatusUnprocessableEntity, Body: policyWriteErrorBody{
			Error: "invalid_policy", Errors: emptyIfNil(result.Errors)}}, nil
	case policystore.WriteRegisterFailed:
		// The file was put back, so restored is the headline; the warnings and errors carry what the
		// restore could not establish (a directory that was not synced, say) and the backup names the
		// bytes that were put back.
		return Response{Status: http.StatusBadGateway, Body: policyWriteErrorBody{
			Error: "register_failed", Restored: result.Restored, Backup: pathText(result.Backup),
			Warnings: result.Warnings, Errors: emptyIfNil(result.Errors)}}, nil
	case policystore.WriteRecoveryNeeded:
		return Response{Status: http.StatusInternalServerError, Body: policyWriteErrorBody{
			Error: "recovery_needed", FileDigest: result.FileDigest, RegisteredDigest: result.RegisteredDigest,
			Backup: pathText(result.Backup), Recovery: result.Recovery, Errors: emptyIfNil(result.Errors)}}, nil
	case policystore.WriteCancelled:
		return Response{Status: http.StatusInternalServerError, Body: policyWriteErrorBody{
			Error: "cancelled", Step: result.Step, Backup: pathText(result.Backup), FileDigest: result.FileDigest}}, nil
	case policystore.WriteFailed:
		return Response{Status: http.StatusInternalServerError, Body: policyWriteErrorBody{
			Error: "failed", Reason: firstReason(result.Errors)}}, nil
	default:
		// not_registered, unreadable, symlinked and busy are all "the write could not be started",
		// each with its own reason.
		return Response{Status: http.StatusConflict, Body: policyWriteErrorBody{
			Error: result.Kind, Reason: firstReason(result.Errors)}}, nil
	}
}

// firstReason is the first reason a result carries, or "".
func firstReason(errors []string) string {
	if len(errors) == 0 {
		return ""
	}
	return errors[0]
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
	Register(http.MethodPost, "/api/policy", policyWriteHandler)
}
