package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// pyRow is one row of a store fixture as Python's sqlite3 read it: json.Number for a number, a
// string, or nil for NULL.
type pyRow map[string]any

func (r pyRow) text(column string) string { value, _ := r[column].(string); return value }

func (r pyRow) integer(column string) int64 {
	value, _ := r[column].(json.Number).Int64()
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

// parityReaders read the row a fixture row names back through the typed Go query for its table.
// A table the product reads with its own SQL elsewhere has no typed store query and no entry:
// its rows are in the fixture (the Python run dumped every table) but not read back here.
var parityReaders = map[string]func(context.Context, *Store, pyRow) (any, error){
	"linkage_conflicts": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.LinkageConflicts(c, r.text("scope_kind"), r.text("scope_key"))
		return pick(rows, err, func(x LinkageConflictsRow) bool { return x.ID == r.integer("id") })
	},
	"relationship_scope": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.RelationshipScope(c, r.text("relationship_id")))
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
	"product_bindings": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.ProductBindings(c, r.text("product_key"))
		return pick(rows, err, func(x ProductBindingsRow) bool { return x.Kind == r.text("kind") && x.Ref == r.text("ref") })
	},
	"route_incidents": func(c context.Context, s *Store, r pyRow) (any, error) {
		rows, err := s.RouteIncidents(c, r.text("fault_id"))
		return pick(rows, err, func(x RouteIncidentsRow) bool { return x.IncidentID == r.text("incident_id") })
	},
	"sync_targets": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.SyncTarget(c, r.text("relationship_id"), r.text("target")))
	},
	"managed_start_requests": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ManagedStartRequest(c, r.text("request_id")))
	},
	"reporting_sessions": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.ReportingSession(c, r.text("assignment_id"), r.text("session_id")))
	},
	"turn_declarations": func(c context.Context, s *Store, r pyRow) (any, error) {
		return one(s.TurnDeclaration(c, r.text("assignment_id"), r.text("session_id"), r.text("turn_id")))
	},
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

// parityStore is the store one Python test case left, run to completion by the retired Python
// implementation: every row but schema_meta, as the fixture parity-<group>.json.gz holds it.
type parityStore struct {
	Case  string    `json:"case"`
	Store storeRows `json:"store"`
}

// dumpedRow is one row of a table in a store fixture, as Python's sqlite3 read it: json.Number
// for a number, a string, nil for NULL. The rowid a table without an INTEGER PRIMARY KEY was
// dumped with is not a column.
func dumpedRow(t *testing.T, table tableRows, cells []cell) pyRow {
	t.Helper()
	row := pyRow{}
	for i, column := range table.Columns {
		if column == "rowid" {
			continue
		}
		value, err := cells[i].value()
		if err != nil {
			t.Fatal(err)
		}
		switch v := value.(type) {
		case int64:
			row[column] = json.Number(strconv.FormatInt(v, 10))
		case float64:
			row[column] = json.Number(strconv.FormatFloat(v, 'g', -1, 64))
		default:
			row[column] = v
		}
	}
	return row
}

// rowFields is a typed Go row as its columns, in the DDL's names: text, an integer, a real, or
// nil for NULL.
func rowFields(t *testing.T, goRow any) map[string]any {
	t.Helper()
	value, columns := reflect.ValueOf(goRow), reflect.TypeOf(goRow)
	out := map[string]any{}
	for i := 0; i < value.NumField(); i++ {
		column := snake(columns.Field(i).Name)
		switch v := value.Field(i).Interface().(type) {
		case string, int64, float64:
			out[column] = v
		case sql.NullString:
			out[column] = nullable(v.Valid, v.String)
		case sql.NullInt64:
			out[column] = nullable(v.Valid, v.Int64)
		case sql.NullFloat64:
			out[column] = nullable(v.Valid, v.Float64)
		default:
			t.Fatalf("%s: unsupported %T", column, v)
		}
	}
	return out
}

func nullable[T any](valid bool, value T) any {
	if !valid {
		return nil
	}
	return value
}

// runParity opens a Go store holding each store the group's Python cases wrote (the fixture) and
// reads every row of the group's tables that have a typed query back through it: what Go reads is
// the golden, which is what Python's sqlite3 read from the same rows. Each of those tables must
// have been read at least once.
func runParity(t *testing.T, group string, tables []string) {
	t.Helper()
	var stores []parityStore
	readFixture(t, "parity-"+group+".json", &stores)
	ctx := context.Background()
	read := map[string]any{}
	compared := map[string]int{}
	for i, fixture := range stores {
		s := restoreStore(t, filepath.Join(t.TempDir(), fmt.Sprintf("case-%d", i), "relay.sqlite3"), fixture.Store)
		for _, table := range fixture.Store.Tables {
			reader := parityReaders[table.Table]
			if reader == nil || !slices.Contains(tables, table.Table) {
				continue
			}
			for j, cells := range table.Rows {
				where := fmt.Sprintf("%s %s[%d]", fixture.Case, table.Table, j)
				goRow, err := reader(ctx, s, dumpedRow(t, table, cells))
				if err != nil {
					t.Errorf("%s: Go query: %v", where, err)
					continue
				}
				read[where] = rowFields(t, goRow)
				compared[table.Table]++
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	checkJSON(t, "rows read back", read)
	for _, table := range tables {
		if _, readable := parityReaders[table]; readable && compared[table] == 0 {
			t.Errorf("no Python case wrote %s, so its Go query was never compared", table)
		}
	}
	t.Logf("rows compared per table: %v", compared)
}

func TestPythonParity_linkage(t *testing.T) {
	t.Parallel()
	runParity(t, "linkage", []string{"scope_bindings", "scope_links", "scope_directives", "linkage_conflicts", "relationship_scope", "coordination_conflicts"})
}

func TestPythonParity_merge_turn(t *testing.T) {
	t.Parallel()
	runParity(t, "merge-turn", []string{"merge_turns", "merge_turn_ledger", "merge_turn_checks"})
}

func TestPythonParity_capacity(t *testing.T) {
	t.Parallel()
	runParity(t, "capacity", []string{"execution_slots", "execution_limits", "execution_usage"})
}

func TestPythonParity_supervisor(t *testing.T) {
	t.Parallel()
	runParity(t, "supervisor", []string{"supervisor_messages", "supervisor_attempts", "supervisor_readbacks"})
}

func TestPythonParity_product_routing(t *testing.T) {
	t.Parallel()
	runParity(t, "product-routing", []string{"product_registry", "product_bindings", "routing_policy", "incident_routes", "route_incidents"})
}

func TestPythonParity_sync(t *testing.T) {
	t.Parallel()
	runParity(t, "sync", []string{"sync_targets", "sync_outbox"})
}

func TestPythonParity_managed(t *testing.T) {
	t.Parallel()
	runParity(t, "managed", []string{"managed_start_requests"})
}

func TestPythonParity_remaining(t *testing.T) {
	t.Parallel()
	runParity(t, "remaining", []string{"recipient_rate", "recipient_lifecycle", "poll_observations", "delivery_intent", "authorized_settings",
		"attempt_settings_violations", "canonical_criteria", "verification_mode", "claim_context", "verdict_context",
		"assignment_marks", "reporting_sessions", "turn_declarations"})
}
