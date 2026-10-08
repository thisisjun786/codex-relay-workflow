package manage

import (
	"strings"
	"testing"
)

// dotsContactTestQuestion is the open question this manager holds: one concrete action, target and
// scope, under one revision, answered through one authority source.
func dotsContactTestQuestion() *dotsContactQuestion {
	return &dotsContactQuestion{
		ID:              "q-1",
		Revision:        "rev-1",
		Action:          "merge the held branch",
		Target:          "branch-a",
		Scope:           "repo-a",
		AuthoritySource: "answer-user-1",
	}
}

func dotsContactTestLocal(liveness string) dotsContactLocal {
	return dotsContactLocal{
		Repository: "repo-a",
		Manager:    "mgr-1",
		Liveness:   liveness,
		Question:   dotsContactTestQuestion(),
		Ledger:     map[string]dotsContactEntry{},
	}
}

func dotsContactTestInstruction() dotsContactRequest {
	return dotsContactRequest{
		RequestID:  "req-1",
		Repository: "repo-a",
		Manager:    "mgr-1",
		Purpose:    "instruction",
		Text:       "finish the midpoint check",
	}
}

func dotsContactTestApproval() dotsContactRequest {
	return dotsContactRequest{
		RequestID:       "req-2",
		Repository:      "repo-a",
		Manager:         "mgr-1",
		Purpose:         "approval",
		Authority:       "user",
		AuthoritySource: "answer-user-1",
		QuestionID:      "q-1",
		Revision:        "rev-1",
		Action:          "merge the held branch",
		Target:          "branch-a",
		Scope:           "repo-a",
		Text:            "yes, merge it",
	}
}

func dotsContactWantRefusal(t *testing.T, name string, got dotsContactVerdict, reason string) {
	t.Helper()
	if got.Action != dotsContactRefuse || got.Reason != reason {
		t.Fatalf("%s: got %q/%q, want refuse/%s", name, got.Action, got.Reason, reason)
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
	for _, liveness := range []string{"offline", "unknown", ""} {
		got := dotsContactAdmit(dotsContactTestInstruction(), dotsContactTestLocal(liveness))
		if got.Action != dotsContactWait {
			t.Fatalf("liveness %q: action = %q, want wait", liveness, got.Action)
		}
	}
}

func TestDotsContactAdmitConvergesDuplicateWithSameDigest(t *testing.T) {
	req := dotsContactTestInstruction()
	local := dotsContactTestLocal("idle")
	local.Ledger[req.RequestID] = dotsContactEntry{
		Digest:    dotsContactDigest(req),
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

func TestDotsContactAdmitRefusesSameRequestIDWithAnyDifferentField(t *testing.T) {
	base := dotsContactTestInstruction()
	cases := map[string]func(*dotsContactRequest){
		"payload": func(r *dotsContactRequest) { r.Text = "other text" },
		"scope":   func(r *dotsContactRequest) { r.Scope = "repo-b" },
		"purpose": func(r *dotsContactRequest) {
			r.Purpose, r.Authority, r.AuthoritySource = "approval", "user", "answer-user-1"
			r.QuestionID, r.Revision, r.Action, r.Target, r.Scope = "q-1", "rev-1", "merge the held branch", "branch-a", "repo-a"
		},
	}
	for name, change := range cases {
		req := base
		change(&req)
		local := dotsContactTestLocal("idle")
		local.Ledger[base.RequestID] = dotsContactEntry{Digest: dotsContactDigest(base), Outcome: "accepted"}
		dotsContactWantRefusal(t, name, dotsContactAdmit(req, local), "duplicate_conflict")
	}
}

func TestDotsContactAdmitWaitsBeforeResendingUnknownOutcome(t *testing.T) {
	req := dotsContactTestInstruction()
	local := dotsContactTestLocal("idle")
	local.Ledger[req.RequestID] = dotsContactEntry{Digest: dotsContactDigest(req), Outcome: "unknown"}
	got := dotsContactAdmit(req, local)
	if got.Action != dotsContactWait || got.Reason != "outcome_unknown_reconcile_first" {
		t.Fatalf("got %q/%q, want wait/outcome_unknown_reconcile_first: the same effect must not repeat", got.Action, got.Reason)
	}
}

func TestDotsContactAdmitRefusesWrongRepositoryAndPreviousManager(t *testing.T) {
	req := dotsContactTestInstruction()
	req.Repository = "repo-b"
	dotsContactWantRefusal(t, "wrong repository", dotsContactAdmit(req, dotsContactTestLocal("idle")), "wrong_repository")
	req = dotsContactTestInstruction()
	req.Manager = "mgr-0"
	dotsContactWantRefusal(t, "previous manager", dotsContactAdmit(req, dotsContactTestLocal("idle")), "previous_manager")
}

func TestDotsContactAdmitTakesCancellationAndPauseFromLocalState(t *testing.T) {
	req := dotsContactTestApproval()
	local := dotsContactTestLocal("idle")
	local.Question.Cancelled = true
	dotsContactWantRefusal(t, "cancelled in local view", dotsContactAdmit(req, local), "cancelled")
	local = dotsContactTestLocal("idle")
	local.Question.Paused = true
	dotsContactWantRefusal(t, "paused in local view", dotsContactAdmit(req, local), "paused")
}

func TestDotsContactAdmitRefusesStaleDecision(t *testing.T) {
	req := dotsContactTestApproval()
	req.Revision = "rev-0"
	dotsContactWantRefusal(t, "stale revision", dotsContactAdmit(req, dotsContactTestLocal("idle")), "stale_decision")
	req = dotsContactTestApproval()
	req.QuestionID = "q-0"
	dotsContactWantRefusal(t, "answer to another question", dotsContactAdmit(req, dotsContactTestLocal("idle")), "stale_decision")
}

func TestDotsContactAdmitRequiresConcreteActionTargetScopeAndAuthoritySource(t *testing.T) {
	req := dotsContactTestApproval()
	req.Action = "delete the branch"
	dotsContactWantRefusal(t, "different action", dotsContactAdmit(req, dotsContactTestLocal("idle")), "unbound_decision")
	req = dotsContactTestApproval()
	req.Target = ""
	dotsContactWantRefusal(t, "missing target", dotsContactAdmit(req, dotsContactTestLocal("idle")), "incomplete_decision")
	req = dotsContactTestApproval()
	req.Scope = ""
	dotsContactWantRefusal(t, "missing scope", dotsContactAdmit(req, dotsContactTestLocal("idle")), "incomplete_decision")
	req = dotsContactTestApproval()
	req.AuthoritySource = "answer-other"
	dotsContactWantRefusal(t, "authority source not the recorded answer", dotsContactAdmit(req, dotsContactTestLocal("idle")), "no_permission")
}

func TestDotsContactAdmitRefusesApprovalWithoutUserAuthority(t *testing.T) {
	req := dotsContactTestApproval()
	req.Authority = ""
	dotsContactWantRefusal(t, "no authority", dotsContactAdmit(req, dotsContactTestLocal("idle")), "no_permission")
}

func TestDotsContactAdmitFailsClosedOnIncompleteLocalQuestion(t *testing.T) {
	local := dotsContactTestLocal("idle")
	local.Question = nil
	dotsContactWantRefusal(t, "no open question", dotsContactAdmit(dotsContactTestApproval(), local), "no_open_question")
	blanks := map[string]func(*dotsContactQuestion){
		"id":       func(q *dotsContactQuestion) { q.ID = "" },
		"revision": func(q *dotsContactQuestion) { q.Revision = "" },
		"action":   func(q *dotsContactQuestion) { q.Action = "" },
		"target":   func(q *dotsContactQuestion) { q.Target = "" },
		"scope":    func(q *dotsContactQuestion) { q.Scope = "" },
		"source":   func(q *dotsContactQuestion) { q.AuthoritySource = "" },
	}
	for name, blank := range blanks {
		local := dotsContactTestLocal("idle")
		blank(local.Question)
		req := dotsContactTestApproval()
		req.QuestionID, req.Revision, req.Action, req.Target, req.Scope, req.AuthoritySource = "", "", "", "", "", ""
		dotsContactWantRefusal(t, "blank local "+name+" with blank request", dotsContactAdmit(req, local), "incomplete_decision")
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
	req.Text = "\xff\xfe broken"
	dotsContactWantRefusal(t, "invalid UTF-8", dotsContactAdmit(req, dotsContactTestLocal("idle")), "invalid_encoding")
	req = dotsContactTestInstruction()
	req.RequestID = ""
	dotsContactWantRefusal(t, "empty request id", dotsContactAdmit(req, dotsContactTestLocal("idle")), "no_request_id")
}

func TestDotsContactDigestDoesNotCollideAcrossFieldBoundaries(t *testing.T) {
	first := dotsContactTestInstruction()
	first.Target, first.Scope = "ab", "c"
	second := first
	second.Target, second.Scope = "a", "bc"
	if dotsContactDigest(first) == dotsContactDigest(second) {
		t.Fatal("digest joins fields with an ambiguous boundary")
	}
	nul := dotsContactTestInstruction()
	nul.Text = "a\x00b"
	plain := dotsContactTestInstruction()
	plain.Text = "a"
	if dotsContactDigest(nul) == dotsContactDigest(plain) {
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

func TestDotsContactMessageCarriesTheDecisionAsDeliveryInput(t *testing.T) {
	settings := coreSettings{Model: "m", ReasoningEffort: "none"}
	req := dotsContactTestApproval()
	verdict := dotsContactAdmit(req, dotsContactTestLocal("idle"))
	msg, ok := dotsContactMessage(req, verdict, "thread-a", "parent", settings)
	if !ok {
		t.Fatal("a deliver verdict produced no delivery message")
	}
	if msg.LogicalID != verdict.LogicalID || msg.Thread != "thread-a" || msg.Role != "parent" || msg.Settings != settings {
		t.Fatalf("message identity or settings lost: %+v", msg)
	}
	for _, want := range []string{req.RequestID, req.QuestionID, req.Revision, req.Action, req.Target, req.Scope, req.Text} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("message text lost %q: %q", want, msg.Text)
		}
	}
	refused := dotsContactRefusal("stale_decision")
	if _, ok := dotsContactMessage(req, refused, "thread-a", "parent", settings); ok {
		t.Fatal("a refusal produced a delivery message")
	}
	waiting := dotsContactVerdict{Action: dotsContactWait, Reason: "manager_unreachable", LogicalID: verdict.LogicalID}
	if _, ok := dotsContactMessage(req, waiting, "thread-a", "parent", settings); ok {
		t.Fatal("a wait verdict produced a delivery message")
	}
}
