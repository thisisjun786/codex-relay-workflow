package skill

import "fmt"

// hookReplaySite is one return of a decision function that replay requires a fixture to execute.
// function keeps the name the decision fixtures and hook-contract.md use for that function;
// ordinal counts its returns in source order from 1; label says what that return answers.
type hookReplaySite struct {
	function string
	ordinal  int
	label    string
}

func (s hookReplaySite) key() string { return s.function + ":" + fmt.Sprint(s.ordinal) }

// hookReturnSites is replay's return-site denominator, owned here rather than read from any
// source file at run time. A return is marked reached by a marker call on the decision path:
// markHookReplay in this package, reach and MarkReturn in internal/relay/delivery (MarkReturn
// also from internal/relay/hook), and probeState's answer closure for observe_state.
// TestHookReturnSitesEqualTheSourceMarkers derives the (function, ordinal) set from those marker
// calls in the Go source and requires it to equal this table, so a return added without a row,
// or a row left behind by a removed return, fails the tests instead of shrinking the denominator.
var hookReturnSites = []hookReplaySite{
	{"_ambiguity_resolved", 1, "unresolved: no resolution names both a task and a session"},
	{"_ambiguity_resolved", 2, "unresolved: the resolutions name different identities"},
	{"_ambiguity_resolved", 3, "unresolved: the chosen task or session is not in the record"},
	{"_ambiguity_resolved", 4, "resolved exactly when every accepted attempt and claim is covered"},

	{"_claimant", 1, "no owner: factId is not a claims/<session>[/claim.json] path"},
	{"_claimant", 2, "no owner: the session segment is . or .."},
	{"_claimant", 3, "no owner: the body does not name the path's session"},
	{"_claimant", 4, "owner: the session the path and the body both name"},

	{"_correlated", 1, "not correlated: the claim has a correlation problem"},
	{"_correlated", 2, "correlated"},

	{"_correlation_problem", 1, "claim_absent: this session owns no claim"},
	{"_correlation_problem", 2, "claim_dispatch_unnamed: the claim names no dispatch request id"},
	{"_correlation_problem", 3, "intent_dispatch_unnamed: the intent publishes no dispatch hash"},
	{"_correlation_problem", 4, "intent_assignment_mismatch: the intent hash is not this assignment"},
	{"_correlation_problem", 5, "claim_dispatch_unnamed: the dispatch request id cannot be hashed"},
	{"_correlation_problem", 6, "correlated, or claim_dispatch_mismatch when the hashes differ"},

	{"_covered", 1, "uncovered: the fact carries no factId"},
	{"_covered", 2, "covered: a resolution adjudicated this factId and digest"},
	{"_covered", 3, "uncovered: no resolution adjudicated this fact"},

	{"classify_declaration", 1, "declared_<outcome>: a releasing disposition for this turn"},
	{"classify_declaration", 2, "declared_ready_receipted: ready with a matching receipt"},
	{"classify_declaration", 3, "receipt_missing: ready without a matching receipt"},
	{"classify_declaration", 4, "undeclared_turn_end: no disposition for this turn"},

	{"decide", 1, "release: the observation is not an omission"},
	{"decide", 2, "release unresolved_handoff: the hold bound is reached"},
	{"decide", 3, "release hold_in_flight: this turn already took its hold"},
	{"decide", 4, "release hold_in_flight: a continuation is already running"},
	{"decide", 5, "block: hold the omission"},

	{"derive_assignment_state", 1, "relationship_registered or identity_bound: the session is bound"},
	{"derive_assignment_state", 2, "ambiguous_identity: competing tasks or claims are unresolved"},
	{"derive_assignment_state", 3, "intent_expired: the binding window has passed"},
	{"derive_assignment_state", 4, "creation_unknown: no accepted and some unknown creation"},
	{"derive_assignment_state", 5, "creation_accepted or intent_declared"},

	{"identity_contested", 1, "uncontested: nothing is bound"},
	{"identity_contested", 2, "contested exactly when a competing fact is uncovered for the bound identity"},

	{"observe_state", 1, "state_unreadable: a store could not be read"},
	{"observe_state", 2, "marker_malformed: a published fact has the wrong shape"},
	{"observe_state", 3, "unmanaged: no assignment for this workspace"},
	{"observe_state", 4, "dispatch_uncorrelated: unbound and no matching dispatch request id"},
	{"observe_state", 5, "correlated_unbound: correlated but not yet bound"},
	{"observe_state", 6, "bound_identity_unnamed: the bind names no session"},
	{"observe_state", 7, "marker_claimed_by_other_session: another session is bound"},
	{"observe_state", 8, "declared_<outcome>: the bound child declared this turn"},
	{"observe_state", 9, "marker_unclaimed: bound but this session has not claimed"},
	{"observe_state", 10, "claim_uncorrelated: the bound session's claim does not correlate"},
	{"observe_state", 11, "managed_unregistered: no relationship is registered"},
	{"observe_state", 12, "receipt_missing: the store has no record of the current generation"},
	{"observe_state", 13, "receipt_missing: the registration does not name the current generation"},
	{"observe_state", 14, "receipt_missing: no receipt at the current head"},
	{"observe_state", 15, "undeclared_turn_end: no usable disposition for this turn"},

	{"resolve_assignment", 1, "none: no assignment has published an intent"},
	{"resolve_assignment", 2, "the latest published assignment, preferring those this session claimed"},

	{"selected_marker", 1, "the observation's own marker: no workspace listing"},
	{"selected_marker", 2, "the assignment resolved from the workspace listing"},
}
