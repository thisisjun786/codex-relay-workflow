package manage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"
)

// This file decides what happens to one inbound request from the dots side before anything is
// sent. It is pure: it reads a request, this manager's view and the ledger of earlier requests, and
// returns one action. The send itself is Deliver (deliver_send.go), reached through
// dotsContactMessage; the decision answer is the relay's own record (internal/relay/decisions).
// This file adds no transport and no command.
//
// The request carries its text because the text is the instruction being delivered. The ledger keeps
// only a digest of the command, never the text.

// Actions a verdict can name.
const (
	dotsContactDeliver  = "deliver"
	dotsContactConverge = "converge"
	dotsContactWait     = "wait"
	dotsContactRefuse   = "refuse"
)

// dotsContactRequest is one request after transport. RequestID is the dots-side identity that makes
// a repeat a repeat. Repository and Manager are the routing identifiers the request names; Manager
// is empty when the request names none. The approval fields bind an answer to one concrete action:
// Authority is the class the answer cites, AuthoritySource names the answer the relay recorded, and
// QuestionID, Revision, Action, Target and Scope must all match the open question.
type dotsContactRequest struct {
	RequestID       string
	Repository      string
	Manager         string
	Purpose         string
	Authority       string
	AuthoritySource string
	QuestionID      string
	Revision        string
	Action          string
	Target          string
	Scope           string
	Text            string
}

// dotsContactQuestion is the open question as this manager's relay record holds it. Cancelled and
// Paused are read from here, never from the incoming request.
type dotsContactQuestion struct {
	ID              string
	Revision        string
	Action          string
	Target          string
	Scope           string
	AuthoritySource string
	Cancelled       bool
	Paused          bool
}

// dotsContactLocal is this manager's view at the moment of admission. Question is nil when no
// question is open.
type dotsContactLocal struct {
	Repository string
	Manager    string
	// Liveness is busy, idle, offline or unknown. Anything else is treated as unknown.
	Liveness string
	Question *dotsContactQuestion
	Ledger   map[string]dotsContactEntry
}

// dotsContactEntry is what the ledger keeps for one earlier request: the digest of its command, the
// logical id it was sent under, and the transport outcome (accepted, refused or unknown).
type dotsContactEntry struct {
	Digest    string
	LogicalID string
	Outcome   string
}

// dotsContactVerdict is the action with the reason that settled it.
type dotsContactVerdict struct {
	Action    string
	Reason    string
	LogicalID string
}

// dotsContactAdmit returns the one action for req. Checks run in the order that never acts on a
// request whose identity or authority is unclear: encoding, identity, routing, approval, then the
// ledger, and only then the manager's liveness.
func dotsContactAdmit(req dotsContactRequest, local dotsContactLocal) dotsContactVerdict {
	if !dotsContactValidText(req) {
		return dotsContactRefusal("invalid_encoding")
	}
	if req.RequestID == "" {
		return dotsContactRefusal("no_request_id")
	}
	if req.Repository != local.Repository {
		return dotsContactRefusal("wrong_repository")
	}
	if req.Manager != "" && req.Manager != local.Manager {
		return dotsContactRefusal("previous_manager")
	}
	switch req.Purpose {
	case "instruction":
	case "approval":
		if reason := dotsContactApprovalRefusal(req, local.Question); reason != "" {
			return dotsContactRefusal(reason)
		}
	default:
		return dotsContactRefusal("unknown_purpose")
	}
	digest := dotsContactDigest(req)
	if digest == "" {
		return dotsContactRefusal("invalid_encoding")
	}
	logicalID := dotsContactLogicalID(req.RequestID)
	if entry, seen := local.Ledger[req.RequestID]; seen {
		if entry.Digest != digest {
			return dotsContactRefusal("duplicate_conflict")
		}
		switch entry.Outcome {
		case "accepted":
			return dotsContactVerdict{Action: dotsContactConverge, Reason: "accepted_not_applied", LogicalID: entry.LogicalID}
		case "unknown":
			return dotsContactVerdict{Action: dotsContactWait, Reason: "outcome_unknown_reconcile_first", LogicalID: entry.LogicalID}
		default:
			return dotsContactRefusal("previously_refused")
		}
	}
	switch local.Liveness {
	case "busy", "idle":
		return dotsContactVerdict{Action: dotsContactDeliver, Reason: "manager_" + local.Liveness, LogicalID: logicalID}
	default:
		return dotsContactVerdict{Action: dotsContactWait, Reason: "manager_unreachable", LogicalID: logicalID}
	}
}

// dotsContactApprovalRefusal returns the reason an approval may not proceed, or the empty string
// when it may. Every value the local question must hold is checked first, so an empty local value
// fails closed instead of matching an empty request value.
func dotsContactApprovalRefusal(req dotsContactRequest, question *dotsContactQuestion) string {
	if question == nil {
		return "no_open_question"
	}
	if question.ID == "" || question.Revision == "" || question.Action == "" || question.Target == "" || question.Scope == "" || question.AuthoritySource == "" {
		return "incomplete_decision"
	}
	if req.Authority != "user" {
		return "no_permission"
	}
	if req.QuestionID == "" || req.Revision == "" || req.Action == "" || req.Target == "" || req.Scope == "" || req.AuthoritySource == "" {
		return "incomplete_decision"
	}
	if question.Cancelled {
		return "cancelled"
	}
	if question.Paused {
		return "paused"
	}
	if req.QuestionID != question.ID || req.Revision != question.Revision {
		return "stale_decision"
	}
	if req.Action != question.Action || req.Target != question.Target || req.Scope != question.Scope {
		return "unbound_decision"
	}
	if req.AuthoritySource != question.AuthoritySource {
		return "no_permission"
	}
	return ""
}

// dotsContactRefusal is a refusal with no logical id: nothing is sent under it.
func dotsContactRefusal(reason string) dotsContactVerdict {
	return dotsContactVerdict{Action: dotsContactRefuse, Reason: reason}
}

// dotsContactValidText refuses any field that is not valid UTF-8. Such a byte sequence would be
// replaced on encoding, so two different requests could hash to one digest.
func dotsContactValidText(req dotsContactRequest) bool {
	fields := []string{req.RequestID, req.Repository, req.Manager, req.Purpose, req.Authority, req.AuthoritySource,
		req.QuestionID, req.Revision, req.Action, req.Target, req.Scope, req.Text}
	for _, field := range fields {
		if !utf8.ValidString(field) {
			return false
		}
	}
	return true
}

// dotsContactDigest hashes every field that defines the command, as one JSON array, so a change to
// any field, including the purpose, the authority or the question binding, is a different command,
// and a delimiter inside a field cannot move a boundary. It returns the empty string only if the
// encoding fails, which admission reads as a refusal.
func dotsContactDigest(req dotsContactRequest) string {
	raw, err := json.Marshal([]string{"dots-contact-digest/1", req.Repository, req.Manager, req.Purpose,
		req.Authority, req.AuthoritySource, req.QuestionID, req.Revision, req.Action, req.Target, req.Scope, req.Text})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// dotsContactLogicalID derives the logical id from the request id. It is one path component of
// bounded length, and the same request id always yields the same id.
func dotsContactLogicalID(requestID string) string {
	raw, err := json.Marshal([]string{"dots-contact/1", requestID})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "dots-" + hex.EncodeToString(sum[:])[:32]
}

// dotsContactMessage turns an admitted deliver verdict into the input Deliver takes: the logical id
// the verdict chose, the thread and the parent's role and settings from the caller, and a text that
// names the request and, for an approval, the answer it binds to. Any other verdict yields no
// message, so nothing reaches the bridge without an admission.
func dotsContactMessage(req dotsContactRequest, verdict dotsContactVerdict, thread, role string, settings coreSettings) (Message, bool) {
	if verdict.Action != dotsContactDeliver || verdict.LogicalID == "" || thread == "" {
		return Message{}, false
	}
	header := "dots request " + req.RequestID + "\npurpose: " + req.Purpose
	if req.Purpose == "approval" {
		header += "\nanswer to question " + req.QuestionID + " revision " + req.Revision +
			"\naction: " + req.Action + "\ntarget: " + req.Target + "\nscope: " + req.Scope
	}
	return Message{LogicalID: verdict.LogicalID, Thread: thread, Text: header + "\n\n" + req.Text, Role: role, Settings: settings}, true
}
