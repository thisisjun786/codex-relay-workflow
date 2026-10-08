package manage

import (
	"strings"
	"testing"
)

// dotsContactTestLocal is this manager's view: one repository, one manager, one open question.
func dotsContactTestLocal(liveness string) dotsContactLocal {
	return dotsContactLocal{
		Repository: "repo-a",
		Manager:    "mgr-1",
		Liveness:   liveness,
		Question:   "q-1",
		Revision:   "rev-1",
		Ledger:     map[string]dotsContactEntry{},
	}
}

func dotsContactTestInstruction() dotsContactRequest {
	return dotsContactRequest{
		RequestID:  "req-1",
		Repository: "repo-a",
		Manager:    "mgr-1",
		Purpose:    "instruction",
		Payload:    []string{"scope-a", "finish the midpoint check"},
	}
}

func dotsContactTestApproval() dotsContactRequest {
	return dotsContactRequest{
		RequestID:  "req-2",
		Repository: "repo-a",
		Manager:    "mgr-1",
		Purpose:    "approval",
		Authority:  "user",
		QuestionID: "q-1",
		Revision:   "rev-1",
		Payload:    []string{"approve the merge of the held branch"},
	}
}

func TestDotsContactAdmitDeliversNewInstructionToLiveManager(t *testing.T) {
	for _, liveness := range []string{"busy", "idle"} {
		got := dotsContactAdmit(dotsContactTestInstruction(), dotsContactTestLocal(liveness))
		if got.Action != dotsContactDeliver {
			t.Fatalf("liveness %s: action = %q, want deliver (reason %q)", liveness, got.Action, got.Reason)
		}
		if got.LogicalID != dotsContactLogicalID("req-1") {
			t.Fatalf("logical id = %q, want the id derived from the request id", got.LogicalID)
		}
	}
}

func TestDotsContactAdmitWaitsWhenManagerIsOfflineOrUnknown(t *testing.T) {
	for _, liveness := range []string{"offline", "unknown"} {
		got := dotsContactAdmit(dotsContactTestInstruction(), dotsContactTestLocal(liveness))
		if got.Action != dotsContactWait {
			t.Fatalf("liveness %s: action = %q, want wait", liveness, got.Action)
		}
	}
}

func TestDotsContactAdmitConvergesDuplicateWithSameDigest(t *testing.T) {
	req := dotsContactTestInstruction()
	local := dotsContactTestLocal("idle")
	local.Ledger[req.RequestID] = dotsContactEntry{
		Digest:    dotsContactDigest(req.Payload),
		LogicalID: dotsContactLogicalID(req.RequestID),
		Outcome:   "accepted",
	}
	got := dotsContactAdmit(req, local)
	if got.Action != dotsContactConverge {
		t.Fatalf("action = %q, want converge: a duplicate must not start new work", got.Action)
	}
	if got.Reason != "accepted_not_applied" {
		t.Fatalf("reason = %q, want accepted_not_applied: transport acceptance is not application", got.Reason)
	}
}

func TestDotsContactAdmitRefusesSameRequestIDWithDifferentPayload(t *testing.T) {
	req := dotsContactTestInstruction()
	local := dotsContactTestLocal("idle")
	local.Ledger[req.RequestID] = dotsContactEntry{Digest: dotsContactDigest([]string{"other"}), Outcome: "accepted"}
	got := dotsContactAdmit(req, local)
	if got.Action != dotsContactRefuse || got.Reason != "duplicate_conflict" {
		t.Fatalf("got %q/%q, want refuse/duplicate_conflict", got.Action, got.Reason)
	}
}

func TestDotsContactAdmitWaitsBeforeResendingUnknownOutcome(t *testing.T) {
	req := dotsContactTestInstruction()
	local := dotsContactTestLocal("idle")
	local.Ledger[req.RequestID] = dotsContactEntry{Digest: dotsContactDigest(req.Payload), Outcome: "unknown"}
	got := dotsContactAdmit(req, local)
	if got.Action != dotsContactWait || got.Reason != "outcome_unknown_reconcile_first" {
		t.Fatalf("got %q/%q, want wait/outcome_unknown_reconcile_first: the same effect must not repeat", got.Action, got.Reason)
	}
}

func TestDotsContactAdmitRefusesWrongRepositoryAndPreviousManager(t *testing.T) {
	req := dotsContactTestInstruction()
	req.Repository = "repo-b"
	if got := dotsContactAdmit(req, dotsContactTestLocal("idle")); got.Action != dotsContactRefuse || got.Reason != "wrong_repository" {
		t.Fatalf("wrong repository: got %q/%q", got.Action, got.Reason)
	}
	req = dotsContactTestInstruction()
	req.Manager = "mgr-0"
	if got := dotsContactAdmit(req, dotsContactTestLocal("idle")); got.Action != dotsContactRefuse || got.Reason != "previous_manager" {
		t.Fatalf("previous manager: got %q/%q", got.Action, got.Reason)
	}
}

func TestDotsContactAdmitRefusesStaleOrCancelledDecision(t *testing.T) {
	req := dotsContactTestApproval()
	req.Revision = "rev-0"
	if got := dotsContactAdmit(req, dotsContactTestLocal("idle")); got.Action != dotsContactRefuse || got.Reason != "stale_decision" {
		t.Fatalf("stale revision: got %q/%q", got.Action, got.Reason)
	}
	req = dotsContactTestApproval()
	req.QuestionID = "q-0"
	if got := dotsContactAdmit(req, dotsContactTestLocal("idle")); got.Action != dotsContactRefuse || got.Reason != "stale_decision" {
		t.Fatalf("answer to another question: got %q/%q", got.Action, got.Reason)
	}
	req = dotsContactTestApproval()
	req.Cancelled = true
	if got := dotsContactAdmit(req, dotsContactTestLocal("idle")); got.Action != dotsContactRefuse || got.Reason != "cancelled" {
		t.Fatalf("cancelled question: got %q/%q", got.Action, got.Reason)
	}
}

func TestDotsContactAdmitRefusesApprovalWithoutUserAuthority(t *testing.T) {
	req := dotsContactTestApproval()
	req.Authority = ""
	if got := dotsContactAdmit(req, dotsContactTestLocal("idle")); got.Action != dotsContactRefuse || got.Reason != "no_permission" {
		t.Fatalf("approval without authority: got %q/%q", got.Action, got.Reason)
	}
}

func TestDotsContactAdmitAcceptsCurrentApproval(t *testing.T) {
	got := dotsContactAdmit(dotsContactTestApproval(), dotsContactTestLocal("idle"))
	if got.Action != dotsContactDeliver {
		t.Fatalf("current approval: got %q/%q, want deliver", got.Action, got.Reason)
	}
}

func TestDotsContactAdmitRefusesInvalidEncodingAndEmptyRequest(t *testing.T) {
	req := dotsContactTestInstruction()
	req.Payload = []string{"\xff\xfe broken"}
	if got := dotsContactAdmit(req, dotsContactTestLocal("idle")); got.Action != dotsContactRefuse || got.Reason != "invalid_encoding" {
		t.Fatalf("invalid UTF-8: got %q/%q", got.Action, got.Reason)
	}
	req = dotsContactTestInstruction()
	req.RequestID = ""
	if got := dotsContactAdmit(req, dotsContactTestLocal("idle")); got.Action != dotsContactRefuse || got.Reason != "no_request_id" {
		t.Fatalf("empty request id: got %q/%q", got.Action, got.Reason)
	}
}

func TestDotsContactDigestDoesNotCollideAcrossDelimiters(t *testing.T) {
	if dotsContactDigest([]string{"a", "bc"}) == dotsContactDigest([]string{"ab", "c"}) {
		t.Fatal("digest joins fields with an ambiguous delimiter")
	}
	if dotsContactDigest([]string{"a\x00b"}) == dotsContactDigest([]string{"a", "b"}) {
		t.Fatal("digest collides on an embedded NUL")
	}
}

func TestDotsContactLogicalIDIsOnePathComponent(t *testing.T) {
	for _, id := range []string{"req-1", "../escape", strings.Repeat("x", 500)} {
		got := dotsContactLogicalID(id)
		if strings.ContainsAny(got, "/\\") || len(got) > deliverRequestIDLimit-deliverRetrySuffixRoom {
			t.Fatalf("logical id for %.20q = %q is not a safe path component", id, got)
		}
		if err := deliverPathComponent(got, "logical id"); err != nil {
			t.Fatalf("logical id rejected by the outbox: %v", err)
		}
	}
}
