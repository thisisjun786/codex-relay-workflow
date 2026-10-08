package manage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"unicode/utf8"
)

// This file decides what happens to one inbound request from the dots side before anything is
// sent. It is pure: it reads a request, this manager's view and the ledger of earlier requests, and
// returns one action. The send itself is Deliver (deliver_send.go) and the decision answer is the
// relay's own record (internal/relay/decisions); this file adds no transport and no command.
//
// The request carries a digest of its payload, never the payload, so the ledger does not copy the
// user's words beside the request id.

// Actions a verdict can name.
const (
	dotsContactDeliver  = "deliver"
	dotsContactConverge = "converge"
	dotsContactWait     = "wait"
	dotsContactRefuse   = "refuse"
)

// dotsContactRequest is one request after transport. RequestID is the dots-side identity that makes
// a repeat a repeat. Repository and Manager are the routing identifiers the request names; Manager
// is empty when the request names none. Authority and the question fields apply to an approval.
type dotsContactRequest struct {
	RequestID  string
	Repository string
	Manager    string
	Purpose    string
	Authority  string
	QuestionID string
	Revision   string
	Cancelled  bool
	Payload    []string
}

// dotsContactLocal is this manager's view at the moment of admission.
type dotsContactLocal struct {
	Repository string
	Manager    string
	// Liveness is busy, idle, offline or unknown. Anything else is treated as unknown.
	Liveness string
	Question string
	Revision string
	Ledger   map[string]dotsContactEntry
}

// dotsContactEntry is what the ledger keeps for one earlier request: its payload digest, the logical
// id it was sent under, and the transport outcome (accepted, refused or unknown).
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
		if req.Authority != "user" {
			return dotsContactRefusal("no_permission")
		}
		if req.Cancelled {
			return dotsContactRefusal("cancelled")
		}
		if req.QuestionID != local.Question || req.Revision != local.Revision {
			return dotsContactRefusal("stale_decision")
		}
	default:
		return dotsContactRefusal("unknown_purpose")
	}
	logicalID := dotsContactLogicalID(req.RequestID)
	digest := dotsContactDigest(req.Payload)
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

// dotsContactRefusal is a refusal with no logical id: nothing is sent under it.
func dotsContactRefusal(reason string) dotsContactVerdict {
	return dotsContactVerdict{Action: dotsContactRefuse, Reason: reason}
}

// dotsContactValidText refuses any field that is not valid UTF-8. Such a byte sequence would be
// replaced on encoding, so two different requests could hash to one digest.
func dotsContactValidText(req dotsContactRequest) bool {
	fields := append([]string{req.RequestID, req.Repository, req.Manager, req.Purpose, req.Authority, req.QuestionID, req.Revision}, req.Payload...)
	for _, field := range fields {
		if !utf8.ValidString(field) {
			return false
		}
	}
	return true
}

// dotsContactDigest hashes the payload fields as one JSON array, so a field boundary cannot be
// moved by a delimiter inside a field.
func dotsContactDigest(parts []string) string {
	raw, err := json.Marshal(parts)
	if err != nil {
		// A []string of valid UTF-8 always marshals; a failure here must not yield a shared digest.
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
