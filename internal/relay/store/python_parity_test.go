package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// pythonParityScript runs named Python test cases to completion, keeps each case's store, and
// dumps every row of the named tables as Python's sqlite3 reads them. "declarations" is the
// declarations module writing a reporting session and a turn declaration into a fresh store;
// "fault_second_occurrence" is FaultLedger.record twice, 60 s apart, so the fault's first_seen_at
// and last_seen_at differ.
const pythonParityScript = `
import importlib, json, os, sqlite3, sys, unittest
tables = json.loads(sys.argv[1])
def dump(path):
    db = sqlite3.connect(path)
    db.row_factory = sqlite3.Row
    rows = {}
    for table in tables:
        found = [dict(row) for row in db.execute("SELECT * FROM " + table + " ORDER BY rowid")]
        if found:
            rows[table] = found
    db.close()
    return rows
out = []
for case_id in sys.argv[2:]:
    if case_id == "declarations":
        from codex_session_relay import declarations
        from codex_session_relay.store import Store
        import tempfile
        path = os.path.join(tempfile.mkdtemp(prefix="decl-"), "relay.sqlite3")
        Store(path).close()
        claim = declarations.record_claim(path, assignment="asg-1", session_id="sess-1",
            dispatch_request_id="dispatch-1", marker_root="/markers", workspace="/work",
            issue_key="CRW-1", at="2026-09-25T00:00:00Z")
        with declarations.Held(path) as held:
            declared = held.settled(held.disposition(assignment="asg-1", session_id="sess-1",
                turn_id="turn-1", outcome="done", declared_at="2026-09-25T00:00:01Z",
                at="2026-09-25T00:00:02Z"))
        assert claim["recorded"] and declared["recorded"], (claim, declared)
        out.append({"case": case_id, "db": path, "rows": dump(path)})
        continue
    if case_id == "fault_second_occurrence":
        from tests import test_faults
        class Second(test_faults.LedgerCase):
            def runTest(self): pass
        case = Second(); case.setUp()
        case.ledger.record(test_faults.omission("k1"))
        case.clock.advance(60)
        case.ledger.record(test_faults.omission("k2"))
        path = str(case.store.path)
        case.store.close()
        out.append({"case": case_id, "db": path, "rows": dump(path)})
        continue
    module, cls, method = case_id.rsplit(".", 2)
    case = getattr(importlib.import_module(module), cls)(method)
    case.setUp()
    getattr(case, method)()
    path = str(case.store.path)
    case.store.close()
    out.append({"case": case_id, "db": path, "rows": dump(path)})
print(json.dumps(out))
`

type parityStore struct {
	Case string                       `json:"case"`
	DB   string                       `json:"db"`
	Raw  map[string][]json.RawMessage `json:"rows"`
}

// pythonParityStores runs the Python cases in an isolated HOME and TMPDIR under t.TempDir.
func pythonParityStores(t *testing.T, tables []string, cases ...string) []parityStore {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("TMPDIR", root)
	encoded, err := json.Marshal(tables)
	if err != nil {
		t.Fatal(err)
	}
	out := pythonStoreValueIn(t, filepath.Join(repositoryRoot(t), "packages/codex-session-relay"), pythonParityScript, append([]string{string(encoded)}, cases...)...)
	var stores []parityStore
	if err := json.Unmarshal([]byte(out[strings.LastIndex(out, "\n")+1:]), &stores); err != nil {
		t.Fatalf("python output %q: %v", out, err)
	}
	return stores
}

// pyRow is one row as Python read it: json.Number for numbers, string, or nil for NULL.
type pyRow map[string]any

func decodeRow(raw json.RawMessage) (pyRow, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var row pyRow
	return row, decoder.Decode(&row)
}

func (r pyRow) text(column string) string { value, _ := r[column].(string); return value }

func (r pyRow) integer(column string) int64 {
	value, _ := r[column].(json.Number).Int64()
	return value
}

func (r pyRow) real(column string) float64 {
	value, _ := r[column].(json.Number).Float64()
	return value
}

// pick is the one row of a listing that match selects.
func pick[T any](rows []T, err error, match func(T) bool) (any, error) {
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if match(row) {
			return row, nil
		}
	}
	return nil, sql.ErrNoRows
}

func one[T any](row T, err error) (any, error) { return row, err }

const everyRow = 1 << 30

// parityReaders read the row a Python row names back through the typed Go query for its table.
var parityReaders = map[string]func(context.Context, *Store, pyRow) (any, error){
	"scope_bindings": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ScopeBinding(c, r.text("binding_id")))
	},
	"scope_links": func(c context.Context, s *Store, r pyRow) (any, error) { return one(s.ScopeLink(c, r.text("link_id"))) },
	"scope_directives": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ScopeDirective(c, r.text("directive_id")))
	},
	"linkage_conflicts": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.LinkageConflicts(c, r.text("scope_kind"), r.text("scope_key"))
		return pick(rows, err, func(x LinkageConflictsRow) bool { return x.ID == r.integer("id") })
	},
	"relationship_scope": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.RelationshipScope(c, r.text("relationship_id")))
	},
	"coordination_conflicts": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.CoordinationConflicts(c, r.text("domain"), r.text("subject"))
		return pick(rows, err, func(x CoordinationConflictsRow) bool { return x.ID == r.integer("id") })
	},
	"merge_turns": func(c context.Context, s *Store, r pyRow) (any, error) { return one(s.MergeTurn(c, r.text("turn_id"))) },
	"merge_turn_ledger": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.MergeLedgerEntry(c, r.text("turn_id"), r.text("idempotency_key")))
	},
	"merge_turn_checks": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.MergeChecks(c, r.text("turn_id"))
		return pick(rows, err, func(x MergeTurnChecksRow) bool { return x.CheckID == r.text("check_id") })
	},
	"execution_slots": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.ExecutionSlotTenures(c, r.text("subject_kind"), r.text("subject_key"))
		return pick(rows, err, func(x ExecutionSlotsRow) bool { return x.SlotID == r.text("slot_id") })
	},
	"execution_limits": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ExecutionLimit(c, r.text("limit_id")))
	},
	"execution_usage": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ExecutionUsage(c, r.text("scope_kind"), r.text("scope_key"), r.text("dimension")))
	},
	"edit_regions": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.EditRegion(c, r.text("region_id")))
	},
	"edit_agreements": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.EditAgreement(c, r.text("agreement_id")))
	},
	"edit_followups": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.EditFollowup(c, r.text("followup_id")))
	},
	"edit_revision_marks": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.EditRevisionMark(c, r.text("repository"), r.text("from_revision")))
	},
	"edit_reaffirmations": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.EditReaffirmation(c, r.text("agreement_id")))
	},
	"supervisor_messages": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.SupervisorMessage(c, r.text("message_id")))
	},
	"supervisor_attempts": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.SupervisorAttempts(c, r.text("message_id"))
		return pick(rows, err, func(x SupervisorAttemptsRow) bool { return x.RequestID == r.text("request_id") })
	},
	"supervisor_readbacks": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.SupervisorReadback(c, r.text("message_id")))
	},
	"fault_ledger": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultLedger(c, r.text("fault_id")))
	},
	"fault_adoptions": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultAdoption(c, r.text("fault_id")))
	},
	"fault_aliases": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultAlias(c, r.text("alias_id")))
	},
	"fault_budget_uses": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultBudgetUse(c, r.text("product"), r.text("kind"), r.text("ref")))
	},
	"fault_cursors": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.FaultCursors(c)
		return pick(rows, err, func(x FaultCursorsRow) bool { return x.Source == r.text("source") })
	},
	"fault_limits": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultLimit(c, r.text("product"), r.text("kind")))
	},
	"fault_links": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultLink(c, r.text("fault_id")))
	},
	"fault_notifications": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultNotification(c, r.text("notification_id")))
	},
	"fault_occurrences": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.FaultOccurrences(c, r.text("fault_id"), everyRow, false)
		return pick(rows, err, func(x FaultOccurrencesRow) bool { return x.OccurrenceID == r.text("occurrence_id") })
	},
	"fault_overtaken_deliveries": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.OvertakenDelivery(c, r.text("event_id")))
	},
	"fault_policies": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultPolicy(c, r.text("product"), r.text("fault_class"), r.text("severity")))
	},
	"fault_publication_attempts": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.FaultPublicationAttempts(c, r.text("publication_id"), everyRow)
		return pick(rows, err, func(x FaultPublicationAttemptsRow) bool { return x.AttemptID == r.integer("attempt_id") })
	},
	"fault_publication_payloads": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultPublicationPayload(c, r.text("publication_id")))
	},
	"fault_publications": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultPublication(c, r.text("publication_id")))
	},
	"fault_remediations": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.FaultRemediations(c, r.text("fault_id"), everyRow)
		return pick(rows, err, func(x FaultRemediationsRow) bool { return x.RemediationID == r.text("remediation_id") })
	},
	"fault_target_projects": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultTargetProject(c, r.text("scope_key")))
	},
	"fault_targets": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.FaultTargetTeam(c, r.text("scope_key")))
	},
	"fault_timeline": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.FaultTimeline(c, r.text("fault_id"))
		return pick(rows, err, func(x FaultTimelineRow) bool { return x.Seq == r.integer("seq") })
	},
	"product_registry": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ProductRegistry(c, r.text("product_key")))
	},
	"product_bindings": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.ProductBindings(c, r.text("product_key"))
		return pick(rows, err, func(x ProductBindingsRow) bool { return x.Kind == r.text("kind") && x.Ref == r.text("ref") })
	},
	"routing_policy": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.RoutingPolicy(c, r.text("policy_key")))
	},
	"incident_routes": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.IncidentRoute(c, r.text("fault_id")))
	},
	"route_incidents": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.RouteIncidents(c, r.text("fault_id"))
		return pick(rows, err, func(x RouteIncidentsRow) bool { return x.IncidentID == r.text("incident_id") })
	},
	"sync_targets": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.SyncTarget(c, r.text("relationship_id"), r.text("target")))
	},
	"sync_outbox": func(c context.Context, s *Store, r pyRow) (any, error) { return one(s.SyncJob(c, r.text("sync_id"))) },
	"managed_start_requests": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ManagedStartRequest(c, r.text("request_id")))
	},
	"recipient_rate": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.RecipientRate(c, r.text("recipient_task_id"), r.real("window_start")))
	},
	"recipient_lifecycle": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.RecipientLifecycle(c, r.text("task_id")))
	},
	"poll_observations": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.PollObservation(c, r.text("relationship_id"), r.integer("execution_generation"), r.text("turn_id")))
	},
	"delivery_intent": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.DeliveryIntent(c, r.text("event_id")))
	},
	"authorized_settings": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.AuthorizedSettings(c, r.text("task_id")))
	},
	"attempt_settings_violations": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.SettingsViolation(c, r.text("request_id")))
	},
	"canonical_criteria": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.CanonicalCriteria(c, r.text("relationship_id"))
		return pick(rows, err, func(x CanonicalCriteriaRow) bool { return x.CriterionID == r.text("criterion_id") })
	},
	"verification_mode": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.VerificationMode(c, r.text("relationship_id")))
	},
	"claim_context": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ClaimContext(c, r.text("event_id")))
	},
	"verdict_context": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.VerdictContext(c, r.text("event_id")))
	},
	"assignment_marks": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.AssignmentMarks(c, r.text("relationship_id"))
		return pick(rows, err, func(x AssignmentMarksRow) bool {
			return x.Mark == r.text("mark") && x.EventID == r.text("event_id")
		})
	},
	"reporting_sessions": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ReportingSession(c, r.text("assignment_id"), r.text("session_id")))
	},
	"turn_declarations": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.TurnDeclaration(c, r.text("assignment_id"), r.text("session_id"), r.text("turn_id")))
	},
}

// sameValue compares one Go field with the value Python's sqlite3 read from the same cell.
func sameValue(goValue reflect.Value, python any) (bool, string) {
	switch v := goValue.Interface().(type) {
	case string:
		text, ok := python.(string)
		return ok && text == v, fmt.Sprintf("%q", v)
	case int64:
		number, ok := python.(json.Number)
		parsed, err := number.Int64()
		return ok && err == nil && parsed == v, fmt.Sprint(v)
	case float64:
		number, ok := python.(json.Number)
		parsed, err := number.Float64()
		return ok && err == nil && parsed == v, fmt.Sprint(v)
	case sql.NullString:
		if !v.Valid {
			return python == nil, "NULL"
		}
		return sameValue(reflect.ValueOf(v.String), python)
	case sql.NullInt64:
		if !v.Valid {
			return python == nil, "NULL"
		}
		return sameValue(reflect.ValueOf(v.Int64), python)
	case sql.NullFloat64:
		if !v.Valid {
			return python == nil, "NULL"
		}
		return sameValue(reflect.ValueOf(v.Float64), python)
	}
	return false, fmt.Sprintf("unsupported %T", goValue.Interface())
}

// compareRow checks every column of the Go row against Python's, in DDL order.
func compareRow(t *testing.T, where string, goRow any, python pyRow) {
	t.Helper()
	value := reflect.ValueOf(goRow)
	columns := reflect.TypeOf(goRow)
	if len(python) != value.NumField() {
		t.Errorf("%s: Python row has %d columns, Go row %d", where, len(python), value.NumField())
	}
	for i := 0; i < value.NumField(); i++ {
		column := snake(columns.Field(i).Name)
		pythonValue, present := python[column]
		if !present {
			t.Errorf("%s: Go field %s has no Python column %s", where, columns.Field(i).Name, column)
			continue
		}
		if same, rendered := sameValue(value.Field(i), pythonValue); !same {
			t.Errorf("%s.%s: Go read %s, Python %v", where, column, rendered, pythonValue)
		}
	}
}

// snake is the DDL column name of a generated row field.
func snake(field string) string {
	for _, pair := range [][2]string{{"ID", "Id"}, {"SHA", "Sha"}, {"TS", "Ts"}, {"PR", "Pr"}, {"CWD", "Cwd"}, {"CXC", "Cxc"}} {
		field = strings.ReplaceAll(field, pair[0], pair[1])
	}
	var out strings.Builder
	for i, r := range field {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		out.WriteRune(r)
	}
	return out.String()
}

// runParity opens each Python-written store with Go and reads every row of the group's tables
// back through the typed queries; every table must have been read at least once.
func runParity(t *testing.T, tables []string, cases ...string) []parityStore {
	t.Helper()
	stores := pythonParityStores(t, tables, cases...)
	compared := map[string]int{}
	for _, python := range stores {
		s, err := Open(context.Background(), python.DB, "")
		if err != nil {
			t.Fatalf("%s: Go cannot open the Python store: %v", python.Case, err)
		}
		ctx := context.Background()
		for table, raws := range python.Raw {
			read := parityReaders[table]
			if read == nil {
				t.Fatalf("no Go reader for %s", table)
			}
			for i, raw := range raws {
				row, err := decodeRow(raw)
				if err != nil {
					t.Fatal(err)
				}
				where := fmt.Sprintf("%s %s[%d]", python.Case, table, i)
				goRow, err := read(ctx, s, row)
				if err != nil {
					t.Errorf("%s: Go query: %v", where, err)
					continue
				}
				compareRow(t, where, goRow, row)
				compared[table]++
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range tables {
		if compared[table] == 0 {
			t.Errorf("no Python case wrote %s, so its Go query was never compared", table)
		}
	}
	t.Logf("rows compared per table: %v", compared)
	return stores
}

func TestPythonParity_linkage(t *testing.T) {
	runParity(t, []string{"scope_bindings", "scope_links", "scope_directives", "linkage_conflicts", "relationship_scope", "coordination_conflicts"},
		"tests.test_linkage.Directives.test_only_the_execution_supervisor_may_instruct",
		"tests.test_linkage.Attachment.test_attaching_twice_converges",
		"tests.test_merge_turn.TheBaseTheTargetActuallyReads.test_only_a_landed_turn_is_restated")
}

func TestPythonParity_merge_turn(t *testing.T) {
	runParity(t, []string{"merge_turns", "merge_turn_ledger", "merge_turn_checks"},
		"tests.test_merge_turn.TheBaseTheTargetActuallyReads.test_a_restatement_states_why")
}

func TestPythonParity_capacity(t *testing.T) {
	runParity(t, []string{"execution_slots", "execution_limits", "execution_usage"},
		"tests.test_capacity.StatingABoundIsNotAnybodysCall.test_the_canonical_store_ceiling_does_apply",
		"tests.test_capacity.CountsAndDeclaredCeilingsAreDifferentKindsOfFact.test_only_an_observation_gives_a_resource_dimension_a_value")
}

func TestPythonParity_regions(t *testing.T) {
	runParity(t, []string{"edit_regions", "edit_agreements", "edit_followups", "edit_revision_marks", "edit_reaffirmations"},
		"tests.test_edit_regions.AReaffirmationCarriesWhatWasAgreed.test_both_conditions_survive_a_second_carry",
		"tests.test_edit_regions.WorkNobodyTookIsNobodysWork.test_nobody_can_report_done_what_nobody_took")
}

func TestPythonParity_supervisor(t *testing.T) {
	runParity(t, []string{"supervisor_messages", "supervisor_attempts", "supervisor_readbacks"},
		"tests.test_supervisor_channel.TheRoundtrip.test_the_whole_record_reads_back_as_one_answer")
}

func TestPythonParity_fault(t *testing.T) {
	stores := runParity(t, []string{"fault_ledger", "fault_aliases", "fault_targets", "fault_target_projects", "fault_occurrences",
		"fault_timeline", "fault_adoptions", "fault_policies", "fault_remediations", "fault_publications",
		"fault_publication_attempts", "fault_publication_payloads", "fault_links", "fault_budget_uses", "fault_limits",
		"fault_notifications", "fault_cursors", "fault_overtaken_deliveries"},
		"tests.test_product_routing.ControlGroups.test_a_false_completion_stays_flagged_for_reverification_on_its_owner",
		"tests.test_fault_contract.ASweepJudgesOnlyWhatItsRowsCanShow.test_a_fault_moved_out_of_this_workspace_is_still_judged_under_its_id",
		"tests.test_fault_contract.ListingsArePaged.test_limits_are_read_a_page_at_a_time",
		"tests.test_fault_contract.B12_SuppressionPolicyIsAdjustable.test_a_degraded_threshold_can_be_lowered_prospectively",
		"tests.test_fault_contract.ExistenceQuestionsAreBounded.test_one_question_judges_a_bounded_number_and_the_next_continues",
		"fault_second_occurrence")
	// A fault seen twice must be among the compared rows, so a reader that swapped first_seen_at and
	// last_seen_at cannot pass on rows where the two happen to be equal.
	distinct := 0
	for _, store := range stores {
		for _, raw := range store.Raw["fault_ledger"] {
			row, err := decodeRow(raw)
			must(t, err)
			if row.text("first_seen_at") != row.text("last_seen_at") {
				distinct++
			}
		}
	}
	if distinct == 0 {
		t.Fatal("no Python-written fault_ledger row has first_seen_at != last_seen_at")
	}
}

func TestPythonParity_product_routing(t *testing.T) {
	runParity(t, []string{"product_registry", "product_bindings", "routing_policy", "incident_routes", "route_incidents"},
		"tests.test_product_routing.Projects.test_an_existing_suitable_project_is_reused")
}

func TestPythonParity_sync(t *testing.T) {
	runParity(t, []string{"sync_targets", "sync_outbox"},
		"tests.test_sync_outbox.BlockParsing.test_blocks_round_trip")
}

func TestPythonParity_managed(t *testing.T) {
	runParity(t, []string{"managed_start_requests"},
		"tests.test_managed_start.ManagedEntry.test_paused_child_is_not_sent_business")
}

func TestPythonParity_remaining(t *testing.T) {
	runParity(t, []string{"recipient_rate", "recipient_lifecycle", "poll_observations", "delivery_intent", "authorized_settings",
		"attempt_settings_violations", "canonical_criteria", "verification_mode", "claim_context", "verdict_context",
		"assignment_marks", "reporting_sessions", "turn_declarations"},
		"tests.test_assignment.CriteriaCurrency.test_an_existing_mark_stops_being_state_when_the_criteria_change",
		"tests.test_fault_notices.WhatTheSixthAuditFound.test_a_pause_after_the_reservation_is_kept",
		"tests.test_diagnostics.RefusedBeforeTheQueue.test_a_scoped_status_filters_the_pending_intents_too",
		"tests.test_settings_preservation.ViolationAnnotatesItDoesNotReclassify.test_an_annotated_dispatch_is_not_retried",
		"declarations")
}
