package delivery

import (
	"strings"
	"testing"
)

// test_intent.py properties INT-1..INT-23. Each runs one list of operations through the real
// intent.py and through this package over the same tree, and every answer - returned records,
// refusal reason AND detail, derived states - is compared whole (testdata/markerops.py).

const (
	intentT0  = "2026-01-01T00:00:00+00:00"
	intentT5  = "2026-01-01T00:05:00+00:00"
	intentT40 = "2026-01-01T00:40:00+00:00"
	session1  = "01child-session"
	task1     = "01child-task"
	rel1      = "rel-0123456789abcdef"
	relOther  = "rel-ffffffffffffffff"
)

func declareOp() markerOp { return markerOp{"op": "declare"} }
func bindOp() markerOp    { return markerOp{"op": "bind", "session": session1, "task": task1} }
func openOp() markerOp    { return markerOp{"op": "open_generation", "relationship_id": rel1} }
func registerOp() markerOp {
	return markerOp{"op": "register", "relationship_id": rel1}
}

func TestINT01_the_derived_state_follows_the_published_facts(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(), markerOp{"op": "state"},
		markerOp{"op": "attempt", "outcome": "accepted", "task_id": task1}, markerOp{"op": "state"},
		bindOp(), markerOp{"op": "state"},
		openOp(), registerOp(), markerOp{"op": "state"},
		// T3: a lost creation response is unknown; a failed one is neither.
		markerOp{"op": "declare", "dispatch": "d-unknown"}, markerOp{"op": "attempt", "dispatch": "d-unknown", "outcome": "unknown"}, markerOp{"op": "state", "dispatch": "d-unknown"},
		markerOp{"op": "declare", "dispatch": "d-failed"}, markerOp{"op": "attempt", "dispatch": "d-failed", "outcome": "failed"}, markerOp{"op": "state", "dispatch": "d-failed"},
	)
	for i, want := range map[int]string{1: IntentDeclared, 3: CreationAccepted, 5: IdentityBound, 8: RelationshipRegistered, 11: CreationUnknown, 14: IntentDeclared} {
		if ok(t, answers[i]) != want {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
}

func TestINT02_two_accepted_task_ids_are_ambiguous_and_ambiguity_outranks_expiry(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(),
		markerOp{"op": "attempt", "outcome": "accepted", "task_id": "task-a"},
		markerOp{"op": "attempt", "outcome": "accepted", "task_id": "task-b"},
		markerOp{"op": "state", "now": intentT5},
		markerOp{"op": "state", "now": intentT40},
	)
	if ok(t, answers[3]) != AmbiguousIdentity || ok(t, answers[4]) != AmbiguousIdentity {
		t.Fatalf("states %v", answers)
	}
}

func TestINT03_expiry_is_anchored_on_declaredat_only(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(), markerOp{"op": "attempt", "outcome": "accepted", "task_id": task1, "at": intentT40}, markerOp{"op": "state", "now": intentT40},
		markerOp{"op": "declare", "dispatch": "d-unknown"}, markerOp{"op": "attempt", "dispatch": "d-unknown", "outcome": "unknown"}, markerOp{"op": "state", "dispatch": "d-unknown", "now": intentT40},
		// Offsets and a naive stamp are compared as instants.
		markerOp{"op": "declare", "dispatch": "d-offset", "declared_at": "2026-01-01T02:00:00+02:00"}, markerOp{"op": "state", "dispatch": "d-offset", "now": "2026-01-01T00:31:00"},
		markerOp{"op": "state", "dispatch": "d-offset", "now": "2026-01-01T00:29:00Z"},
	)
	for i, want := range map[int]string{2: IntentExpired, 5: IntentExpired, 7: IntentExpired, 8: IntentDeclared} {
		if ok(t, answers[i]) != want {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
}

func TestINT04_a_late_fact_cannot_move_the_state_backwards(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(), markerOp{"op": "attempt", "outcome": "accepted", "task_id": task1}, bindOp(), openOp(), registerOp(),
		markerOp{"op": "attempt", "outcome": "accepted", "task_id": task1, "at": intentT5},
		markerOp{"op": "state"},
	)
	if ok(t, answers[6]) != RelationshipRegistered {
		t.Fatalf("state %v", answers[6])
	}
}

func TestINT05_bind_is_create_once_and_a_loser_is_recorded(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(), bindOp(), bindOp(), markerOp{"op": "facts"},
		markerOp{"op": "bind", "session": "other-session", "task": "other-task"},
		markerOp{"op": "bind", "session": "second", "task": "second-task"},
		markerOp{"op": "facts"},
	)
	if ok(t, answers[1]).(map[string]any)["outcome"] != Bound || ok(t, answers[2]).(map[string]any)["outcome"] != Unchanged {
		t.Fatalf("binds %v", answers)
	}
	losing := ok(t, answers[4]).(map[string]any)
	if losing["outcome"] != Conflict || losing["boundSessionId"] != session1 {
		t.Fatalf("losing %v", losing)
	}
	facts := ok(t, answers[6]).(map[string]any)["facts"].(map[string]any)
	if facts["bound"].(map[string]any)["sessionId"] != session1 || len(facts["conflicts"].([]any)) != 2 || facts["conflicts"].([]any)[0].(map[string]any)["attemptedSessionId"] != "other-session" {
		t.Fatalf("facts %v", facts)
	}
}

func TestINT06_a_bind_naming_nothing_and_a_blank_dispatch_are_refused(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(),
		markerOp{"op": "bind", "session": "", "task": task1},
		markerOp{"op": "declare", "dispatch": "  "},
	)
	if reasonOf(t, answers[1]) != UnboundGeneration || reasonOf(t, answers[2]) != UnboundGeneration {
		t.Fatalf("refusals %v", answers)
	}
}

func TestINT07_intent_declaration_is_create_once_on_every_semantic_field(t *testing.T) {
	base := func(field string, value any) markerOp {
		op := markerOp{"op": "declare", "criteria_source": "doc-a", "baseline_revision": "rev-1", "issue_key": "REL-1", "db_path": "/first/relay.sqlite3"}
		if field != "" {
			op[field] = value
		}
		return op
	}
	answers := sameOps(t, nil,
		base("", nil),
		base("db_path", "/corrected/relay.sqlite3"),
		base("criteria_source", "doc-b"),
		base("baseline_revision", "rev-2"),
		base("issue_key", "REL-9"),
		base("", nil),
		markerOp{"op": "facts"},
	)
	if ok(t, answers[0]).(map[string]any)["outcome"] != Published || ok(t, answers[5]).(map[string]any)["outcome"] != Unchanged {
		t.Fatalf("outcomes %v", answers)
	}
	for i := 1; i <= 4; i++ {
		if ok(t, answers[i]).(map[string]any)["outcome"] != Conflict {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
	if ok(t, answers[6]).(map[string]any)["facts"].(map[string]any)["intent"].(map[string]any)["dbPath"] != "/first/relay.sqlite3" {
		t.Fatal("the stored intent was rewritten")
	}
}

func TestINT08_the_dispatch_request_id_is_stored_only_as_its_hash(t *testing.T) {
	answers := sameOps(t, nil, declareOp(), markerOp{"op": "facts"})
	intent := ok(t, answers[1]).(map[string]any)["facts"].(map[string]any)["intent"].(map[string]any)
	if strings.Contains(renderPlain(intent), "dispatch-request-1") || intent["dispatchRequestIdHash"] != AssignmentID("dispatch-request-1") {
		t.Fatalf("intent %v", intent)
	}
}

func renderPlain(v any) string { return strings.ReplaceAll(pyReprValue(fromJSON(v)), " ", "") }

func TestINT09_registration_is_confirmed_against_the_relay_store(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(), bindOp(),
		// Another dispatch's relationship.
		markerOp{"op": "open_generation", "relationship_id": rel1}, markerOp{"op": "register", "relationship_id": rel1, "register_dispatch": "a-different-dispatch"},
		// An unrelated relationship carrying the right dispatch id.
		markerOp{"op": "register", "relationship_id": relOther},
		// A dispatch whose generation has advanced.
		markerOp{"op": "open_generation", "relationship_id": "rel-stale", "generation": 1, "current": 2}, markerOp{"op": "register", "relationship_id": "rel-stale"},
		markerOp{"op": "facts"},
		// The current generation registers.
		markerOp{"op": "register", "relationship_id": rel1}, markerOp{"op": "facts"},
	)
	for i, want := range map[int]string{3: RelationshipConflict, 4: RelationshipConflict, 6: StaleGeneration} {
		if reasonOf(t, answers[i]) != want {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
	if _, published := ok(t, answers[7]).(map[string]any)["facts"].(map[string]any)["relationship"]; published {
		t.Fatal("a refusal published")
	}
	if ok(t, answers[8]).(map[string]any)["outcome"] != Published {
		t.Fatalf("current %v", answers[8])
	}
}

func TestINT10_an_unreadable_missing_or_held_store_refuses_registration(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(), bindOp(),
		markerOp{"op": "register", "relationship_id": rel1, "db_path": "<tree>/no-such-store.sqlite3"},
		markerOp{"op": "exists", "path": "no-such-store.sqlite3"},
		markerOp{"op": "register", "relationship_id": rel1, "db_path": "<tree>/state/no-such-store.sqlite3"},
		markerOp{"op": "exists", "path": "state/no-such-store.sqlite3"},
		openOp(), markerOp{"op": "hold_begin"},
		registerOp(),
		markerOp{"op": "hold_end"},
		markerOp{"op": "facts"},
	)
	for _, i := range []int{2, 4, 8} {
		if reasonOf(t, answers[i]) != UnregisteredRelationship {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
	if ok(t, answers[3]) != false || ok(t, answers[5]) != false {
		t.Fatal("the refusal created the store it could not find")
	}
	if detail := answers[8].(map[string]any)["detail"].(string); !strings.Contains(detail, "write lock could not be taken") {
		t.Fatalf("detail %q", detail)
	}
	if _, published := ok(t, answers[10]).(map[string]any)["facts"].(map[string]any)["relationship"]; published {
		t.Fatal("published without the hold")
	}
}

func TestINT11_registration_is_create_once_and_names_a_contradiction(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(), bindOp(), openOp(), registerOp(), registerOp(),
		markerOp{"op": "open_generation", "relationship_id": relOther},
		markerOp{"op": "register", "relationship_id": relOther},
		markerOp{"op": "facts"},
	)
	if ok(t, answers[3]).(map[string]any)["outcome"] != Published || ok(t, answers[4]).(map[string]any)["outcome"] != Unchanged || ok(t, answers[6]).(map[string]any)["outcome"] != Conflict {
		t.Fatalf("outcomes %v", answers)
	}
	if ok(t, answers[7]).(map[string]any)["facts"].(map[string]any)["relationship"].(map[string]any)["relationshipId"] != rel1 {
		t.Fatal("the stored registration moved")
	}
}

func TestINT12_a_malformed_assignment_id_is_refused(t *testing.T) {
	var ops []markerOp
	ops = append(ops, declareOp())
	for _, bad := range []string{"../escape", "not-hex", "", strings.Repeat("A", 64)} {
		ops = append(ops, markerOp{"op": "attempt", "assignment": bad, "outcome": "accepted", "task_id": task1})
	}
	answers := sameOps(t, nil, ops...)
	for i := 1; i < len(answers); i++ {
		if reasonOf(t, answers[i]) != UnknownGeneration {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
}

func TestINT13_the_registration_fact_records_its_generation(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(), bindOp(), openOp(),
		registerOp(), registerOp(), markerOp{"op": "facts"},
		// A legacy fact without the field replays unchanged and is not rewritten.
		markerOp{"op": "declare", "dispatch": "legacy"},
		markerOp{"op": "open_generation", "relationship_id": "rel-legacy", "generation_dispatch": "legacy"},
		markerOp{"op": "publish", "dispatch": "legacy", "path": "relationship.json", "confined": true, "payload": map[string]any{"relationshipId": "rel-legacy", "at": intentT0}},
		markerOp{"op": "register", "dispatch": "legacy", "register_dispatch": "legacy", "relationship_id": "rel-legacy"},
		markerOp{"op": "facts", "dispatch": "legacy"},
		markerOp{"op": "open_generation", "relationship_id": relOther, "generation_dispatch": "legacy"},
		markerOp{"op": "register", "dispatch": "legacy", "register_dispatch": "legacy", "relationship_id": relOther},
		// A stored fact naming another generation is a contradiction.
		markerOp{"op": "declare", "dispatch": "other-gen"},
		markerOp{"op": "open_generation", "relationship_id": "rel-other-gen", "generation_dispatch": "other-gen"},
		markerOp{"op": "publish", "dispatch": "other-gen", "path": "relationship.json", "confined": true, "payload": map[string]any{"relationshipId": "rel-other-gen", "executionGeneration": 2, "at": intentT0}},
		markerOp{"op": "register", "dispatch": "other-gen", "register_dispatch": "other-gen", "relationship_id": "rel-other-gen"},
		markerOp{"op": "facts", "dispatch": "other-gen"},
	)
	first := ok(t, answers[3]).(map[string]any)
	if first["outcome"] != Published || first["executionGeneration"] != float64(1) || ok(t, answers[4]).(map[string]any)["outcome"] != Unchanged {
		t.Fatalf("fresh %v %v", first, answers[4])
	}
	if ok(t, answers[9]).(map[string]any)["outcome"] != Unchanged || ok(t, answers[12]).(map[string]any)["outcome"] != Conflict {
		t.Fatalf("legacy %v %v", answers[9], answers[12])
	}
	if _, has := ok(t, answers[10]).(map[string]any)["facts"].(map[string]any)["relationship"].(map[string]any)["executionGeneration"]; has {
		t.Fatal("legacy fact rewritten")
	}
	conflict := ok(t, answers[16]).(map[string]any)
	stored := ok(t, answers[17]).(map[string]any)["facts"].(map[string]any)["relationship"].(map[string]any)
	if conflict["outcome"] != Conflict || conflict["executionGeneration"] != float64(1) || stored["executionGeneration"] != float64(2) {
		t.Fatalf("other generation %v %v", conflict, stored)
	}
}

func TestINT14_the_registration_hold_records_nothing_in_the_relay(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(), bindOp(), openOp(),
		markerOp{"op": "relay_tables"},
		registerOp(),
		markerOp{"op": "relay_tables"},
	)
	if ok(t, answers[4]).(map[string]any)["outcome"] != Published {
		t.Fatalf("register %v", answers[4])
	}
	requireSameJSON(t, "relay tables across the hold", ok(t, answers[5]), ok(t, answers[3]))
}

func TestINT15_the_claimant_comes_from_the_path_and_the_body_must_agree(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(),
		markerOp{"op": "claim", "session": session1},
		markerOp{"op": "claimant", "fact": "claims/" + session1 + "/claim.json"},
		markerOp{"op": "publish", "path": "claims/impostor/claim.json", "payload": map[string]any{"dispatchRequestId": "dispatch-request-1", "sessionId": session1, "at": intentT0}},
		markerOp{"op": "claimant", "fact": "claims/impostor/claim.json"},
		markerOp{"op": "declare", "dispatch": "path-only"},
		markerOp{"op": "publish", "dispatch": "path-only", "path": "claims/" + session1 + "/claim.json", "payload": map[string]any{"at": intentT0}},
		markerOp{"op": "facts", "dispatch": "path-only"},
	)
	if ok(t, answers[2]) != session1 || ok(t, answers[4]) != nil {
		t.Fatalf("claimants %v %v", answers[2], answers[4])
	}
	claims := ok(t, answers[7]).(map[string]any)["facts"].(map[string]any)["claims"].([]any)
	if len(claims) != 1 {
		t.Fatalf("a path-only claim still exists: %v", claims)
	}
	if Claimant(fromJSON(claims[0]).(Obj)) != nil {
		t.Fatal("a path-only claim owns nothing")
	}
}

func TestINT16_a_claim_must_correlate_with_the_intents_dispatch(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(),
		markerOp{"op": "claim", "session": session1, "claim_dispatch": "not-the-dispatch-id"},
		markerOp{"op": "facts"},
		markerOp{"op": "publish", "path": "claims/" + session1 + "/claim.json", "payload": map[string]any{"dispatchRequestId": "not-the-dispatch-id", "sessionId": session1, "at": intentT0}},
		markerOp{"op": "correlated", "session": session1},
		markerOp{"op": "publish", "path": "claims/second/claim.json", "payload": map[string]any{"dispatchRequestId": "dispatch-request-1", "sessionId": "second", "at": intentT0}},
		markerOp{"op": "correlated", "session": "second"},
	)
	if reasonOf(t, answers[1]) != RelationshipConflict || !strings.Contains(answers[1].(map[string]any)["detail"].(string), "different assignment") {
		t.Fatalf("refusal %v", answers[1])
	}
	if claims := ok(t, answers[2]).(map[string]any)["facts"].(map[string]any)["claims"].([]any); len(claims) != 0 {
		t.Fatal("the refused claim was written")
	}
	if ok(t, answers[4]) != false || ok(t, answers[6]) != true {
		t.Fatalf("correlation %v %v", answers[4], answers[6])
	}
}

func TestINT17_a_contest_clears_only_by_the_bound_identity_and_a_matching_digest(t *testing.T) {
	second := "claims/second/claim.json"
	answers := sameOps(t, nil,
		declareOp(), markerOp{"op": "claim", "session": session1}, bindOp(),
		markerOp{"op": "contested"},
		markerOp{"op": "publish", "path": second, "payload": map[string]any{"dispatchRequestId": "dispatch-request-1", "sessionId": "second", "at": intentT5}},
		markerOp{"op": "contested"},
		markerOp{"op": "resolution", "task": task1, "session": session1, "facts": []any{second}, "digest": "zero"},
		markerOp{"op": "covered", "fact": second},
		markerOp{"op": "contested"},
		markerOp{"op": "resolution", "task": task1, "session": "second", "facts": []any{second}},
		markerOp{"op": "contested"},
		markerOp{"op": "resolution", "task": task1, "session": session1, "facts": []any{second}},
		markerOp{"op": "covered", "fact": second},
		markerOp{"op": "contested"},
		// T16: a competing claim leaving its session blank still competes.
		markerOp{"op": "declare", "dispatch": "blank"}, markerOp{"op": "claim", "dispatch": "blank", "claim_dispatch": "blank", "session": session1},
		markerOp{"op": "bind", "dispatch": "blank", "session": session1, "task": task1},
		markerOp{"op": "publish", "dispatch": "blank", "path": "claims/blank/claim.json", "payload": map[string]any{"dispatchRequestId": "blank", "sessionId": "", "at": intentT5}},
		markerOp{"op": "contested", "dispatch": "blank"},
	)
	for i, want := range map[int]bool{3: false, 5: true, 7: false, 8: true, 10: true, 12: true, 13: false, 18: true} {
		if ok(t, answers[i]) != want {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
}

func TestINT18_pre_bind_ambiguity_survives_disagreeing_or_unaccepted_resolutions(t *testing.T) {
	everything := []any{"attempts/0", "attempts/1", "claims/" + session1 + "/claim.json", "claims/second/claim.json"}
	answers := sameOps(t, nil,
		declareOp(),
		markerOp{"op": "attempt", "outcome": "accepted", "task_id": "task-a"}, markerOp{"op": "attempt", "outcome": "accepted", "task_id": "task-b"},
		markerOp{"op": "claim", "session": session1}, markerOp{"op": "claim", "session": "second"},
		markerOp{"op": "resolution", "task": "task-a", "session": session1, "facts": everything, "reason": "disagreeing"},
		markerOp{"op": "resolution", "task": "task-b", "session": session1, "facts": everything, "reason": "disagreeing"},
		markerOp{"op": "state"},
		markerOp{"op": "declare", "dispatch": "unaccepted"},
		markerOp{"op": "attempt", "dispatch": "unaccepted", "outcome": "accepted", "task_id": "task-a"}, markerOp{"op": "attempt", "dispatch": "unaccepted", "outcome": "unknown", "task_id": "task-b"},
		markerOp{"op": "claim", "dispatch": "unaccepted", "claim_dispatch": "unaccepted", "session": session1}, markerOp{"op": "claim", "dispatch": "unaccepted", "claim_dispatch": "unaccepted", "session": "second"},
		markerOp{"op": "resolution", "dispatch": "unaccepted", "task": "task-b", "session": session1, "facts": everything, "reason": "unconfirmed"},
		markerOp{"op": "state", "dispatch": "unaccepted"},
		// The control: one agreeing resolution covering everything does clear it.
		markerOp{"op": "declare", "dispatch": "agreeing"},
		markerOp{"op": "attempt", "dispatch": "agreeing", "outcome": "accepted", "task_id": "task-a"}, markerOp{"op": "attempt", "dispatch": "agreeing", "outcome": "accepted", "task_id": "task-b"},
		markerOp{"op": "claim", "dispatch": "agreeing", "claim_dispatch": "agreeing", "session": session1}, markerOp{"op": "claim", "dispatch": "agreeing", "claim_dispatch": "agreeing", "session": "second"},
		markerOp{"op": "resolution", "dispatch": "agreeing", "task": "task-a", "session": session1, "facts": everything},
		markerOp{"op": "state", "dispatch": "agreeing"},
	)
	if ok(t, answers[7]) != AmbiguousIdentity || ok(t, answers[14]) != AmbiguousIdentity || ok(t, answers[21]) != CreationAccepted {
		t.Fatalf("states %v %v %v", answers[7], answers[14], answers[21])
	}
}

func TestINT19_a_malformed_fact_is_reported_by_its_field_path(t *testing.T) {
	ops := []markerOp{
		declareOp(),
		markerOp{"op": "write_raw", "path": "claims/" + session1 + "/claim.json", "text": `"bare"`}, markerOp{"op": "malformed"},
		markerOp{"op": "declare", "dispatch": "array"}, markerOp{"op": "publish", "dispatch": "array", "path": "attempts/0.json", "payload": map[string]any{"outcome": "accepted", "taskId": []any{"a", "b"}}}, markerOp{"op": "malformed", "dispatch": "array"},
		markerOp{"op": "declare", "dispatch": "null"}, markerOp{"op": "publish", "dispatch": "null", "path": "resolutions/0.json", "payload": map[string]any{"chosenTaskId": nil, "chosenSessionId": session1, "adjudicated": []any{}}}, markerOp{"op": "malformed", "dispatch": "null"},
		markerOp{"op": "declare", "dispatch": "nested"}, markerOp{"op": "publish", "dispatch": "nested", "path": "resolutions/0.json", "payload": map[string]any{"chosenTaskId": task1, "chosenSessionId": session1, "adjudicated": []any{"not-a-record"}}}, markerOp{"op": "malformed", "dispatch": "nested"},
	}
	// Raw JSON text, so 1.0 stays a float on both sides.
	for i, value := range []string{"true", "1.0", `"1"`, "null", "0", "-1", "1", ""} {
		dispatch := "stamp-" + string(rune('a'+i))
		stamp := ""
		if value != "" {
			stamp = `, "executionGeneration": ` + value
		}
		ops = append(ops, markerOp{"op": "declare", "dispatch": dispatch}, markerOp{"op": "write_raw", "dispatch": dispatch, "path": "relationship.json", "text": `{"relationshipId": "` + rel1 + `"` + stamp + `, "at": "` + intentT0 + `"}`}, markerOp{"op": "malformed", "dispatch": dispatch})
	}
	answers := sameOps(t, nil, ops...)
	want := []any{"claims", "attempts.taskId", "resolutions.chosenTaskId", "resolutions.adjudicated"}
	for k, i := range []int{2, 5, 8, 11} {
		if ok(t, answers[i]) != want[k] {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
	for k := range 8 {
		got := ok(t, answers[12+3*k+2])
		if (k < 6 && got != "relationship.executionGeneration") || (k >= 6 && got != nil) {
			t.Fatalf("stamp %d %v", k, got)
		}
	}
}

func TestINT20_a_counter_that_is_not_a_count_is_corruption(t *testing.T) {
	answers := sameOps(t, nil,
		markerOp{"op": "counters", "value": map[string]any{"holdsThisTurn": 0}},
		markerOp{"op": "counters", "value": nil},
		markerOp{"op": "counters", "value": map[string]any{"holdsThisTurn": nil}},
		markerOp{"op": "counters", "value": map[string]any{"holdsThisTurn": -1}},
		markerOp{"op": "counters", "value": map[string]any{"holdsThisTurn": "1"}},
		markerOp{"op": "counters", "value": map[string]any{"holdsThisTurn": true}},
		markerOp{"op": "counters", "value": []any{}},
	)
	for i, want := range []any{nil, nil, "counters.holdsThisTurn", "counters.holdsThisTurn", "counters.holdsThisTurn", "counters.holdsThisTurn", "counters"} {
		if ok(t, answers[i]) != want {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
}

func TestINT21_an_identity_used_as_a_directory_name_is_refused_at_every_writer(t *testing.T) {
	ops := []markerOp{declareOp()}
	bad := []string{"../escape", "a/b", "..", ".", "", "  ", "../../escaped", "a\\b"}
	for _, b := range bad {
		ops = append(ops,
			markerOp{"op": "claim", "session": b},
			markerOp{"op": "disposition", "session": b, "turn": "turn-1", "outcome": "interrupted"},
			markerOp{"op": "disposition", "session": session1, "turn": b, "outcome": "interrupted"})
	}
	ops = append(ops, markerOp{"op": "exists_in", "path": "claims"}, markerOp{"op": "exists_in", "path": "dispositions"}, markerOp{"op": "exists", "path": "escaped"}, markerOp{"op": "exists", "path": "markers/escaped"})
	answers := sameOps(t, nil, ops...)
	for i := 1; i <= 3*len(bad); i++ {
		if reasonOf(t, answers[i]) != UnboundGeneration {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
	for _, a := range answers[len(answers)-4:] {
		if ok(t, a) != false {
			t.Fatalf("written: %v", answers[len(answers)-4:])
		}
	}
}

func TestINT22_a_disposition_outside_the_vocabulary_is_refused(t *testing.T) {
	answers := sameOps(t, nil, declareOp(),
		markerOp{"op": "disposition", "session": session1, "turn": "turn-1", "outcome": "done"},
		markerOp{"op": "disposition", "session": session1, "turn": "turn-1", "outcome": "interrupted"},
		markerOp{"op": "disposition", "session": session1, "turn": "turn-1", "outcome": "interrupted"},
		markerOp{"op": "disposition", "session": session1, "turn": "turn-1", "outcome": "failed"},
	)
	if reasonOf(t, answers[1]) != OutcomeInconsistent {
		t.Fatalf("refusal %v", answers[1])
	}
	for i, want := range map[int]string{2: Published, 3: Unchanged, 4: Conflict} {
		if ok(t, answers[i]).(map[string]any)["published"] != want {
			t.Fatalf("answer %d %v", i, answers[i])
		}
	}
}

func TestINT23_assignment_selection_for_a_workspace(t *testing.T) {
	answers := sameOps(t, nil,
		// No intent: not selectable.
		markerOp{"op": "write_raw", "dispatch": "half-built", "path": "attempts/.keep", "text": ""},
		declareOp(),
		markerOp{"op": "select", "session": session1},
		// A claim is consulted before recency.
		markerOp{"op": "claim", "session": session1},
		markerOp{"op": "declare", "dispatch": "dispatch-request-2", "issue_key": "REL-2", "declared_at": intentT5},
		markerOp{"op": "select", "session": session1},
		// Without a claim, the newest declaration wins.
		markerOp{"op": "select", "session": "stranger"},
		// Instants, not printed strings.
		markerOp{"op": "declare", "dispatch": "offset-earlier", "declared_at": "2026-01-02T01:00:00+02:00"},
		markerOp{"op": "declare", "dispatch": "offset-later", "declared_at": "2026-01-02T00:30:00+00:00"},
		markerOp{"op": "select", "session": "stranger"},
		// An unmanaged workspace selects nothing.
		markerOp{"op": "select", "session": session1, "workspace": "<tree>/elsewhere"},
	)
	name := func(i int) any { return ok(t, answers[i]).(map[string]any)["assignment"] }
	if name(2) != AssignmentID("dispatch-request-1") || name(5) != AssignmentID("dispatch-request-1") || name(6) != AssignmentID("dispatch-request-2") || name(9) != AssignmentID("offset-later") || name(10) != nil {
		t.Fatalf("selections %v %v %v %v %v", name(2), name(5), name(6), name(9), name(10))
	}
}

// The publish path confines every marker write: a symlinked parent inside the subtree cannot
// carry a create-once write outside the root (marker.confined; the ValueError text is Python's).
func TestINT21_confinement_refuses_a_symlinked_parent_outside_the_root(t *testing.T) {
	answers := sameOps(t, nil,
		markerOp{"op": "write_raw", "target": "<tree>/outside/.keep", "text": ""},
		markerOp{"op": "write_raw", "target": "<tree>/markers/.keep", "text": ""},
		markerOp{"op": "symlink", "to": "<tree>/outside", "link": "<tree>/markers/escape"},
		markerOp{"op": "publish", "target": "<tree>/markers/escape/x/fact.json", "payload": map[string]any{"a": 1}, "confined": true},
		markerOp{"op": "exists", "path": "outside/x"},
	)
	if msg, _ := answers[3].(map[string]any)["error"].(string); !strings.HasPrefix(msg, "ValueError: refusing to write outside the marker root") || ok(t, answers[4]) != false {
		t.Fatalf("confinement %v %v", answers[3], answers[4])
	}
}
