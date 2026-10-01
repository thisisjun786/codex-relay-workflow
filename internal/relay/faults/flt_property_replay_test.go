package faults

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// These tests use f1ReplayCLI, which runs each command line and compares exit status,
// stdout, stderr, and every table row with the golden.

func replayObservation(t *testing.T, ctx context.Context, gd string, class, severity, key string) map[string]any {
	t.Helper()
	return f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", fmt.Sprintf(`{"schema":"fault-observation/1","product":"crw","faultClass":%q,"severity":%q,"signature":{"relationship":"rel","turn":"turn"},"occurrenceKey":%q,"scope":{"projectKey":"P"},"detail":"detail","evidence":[{"kind":"row","ref":"row:1","observed":{"state":"failed"}}]}`, class, severity, key)})
}

// FLT-1: identity and duplicate observations compare complete replies and rows.
func Test22_FLT_1_IdentityWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	replayObservation(t, ctx, gd, "report_omitted", Broken, "one")
	replayObservation(t, ctx, gd, "report_omitted", Broken, "one")
	replayObservation(t, ctx, gd, "report_omitted", Broken, "two")
}

// FLT-2, FLT-3, FLT-4, FLT-14: registry, evidence bounds, suppression, and
// malformed-observation refusals compare complete replies and rows.
func Test22_FLT_2_3_4_14_RecordWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	for _, args := range [][]string{
		{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"invented","severity":"broken","signature":{"x":1},"occurrenceKey":"one"}`},
		{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"evidence"},"occurrenceKey":"one","evidence":[{"kind":"row","ref":"one"},{"kind":"row","ref":"two"},{"kind":"row","ref":"three"},{"kind":"row","ref":"four"},{"kind":"row","ref":"five"},{"kind":"row","ref":"six"},{"kind":"row","ref":"seven"},{"kind":"row","ref":"eight"},{"kind":"row","ref":"nine"}]}`},
		{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"bad"},"occurrenceKey":"one","cleared":"false"}`},
	} {
		f1ReplayCLI(t, ctx, gd, args)
	}
	for _, key := range []string{"one", "two", "three"} {
		replayObservation(t, ctx, gd, "delivery_stalled", Degraded, key)
	}
}

// FLT-5, FLT-6, FLT-7, FLT-8, FLT-19, FLT-23, FLT-24: fixes, pruning,
// remediation executions, clearing, recurrence, and resolution compare the
// complete command replies and all rows after every transition.
func Test22_FLT_5_6_7_8_19_23_24_LifecycleWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	observed := replayObservation(t, ctx, gd, "report_omitted", Broken, "one")
	id := observed["faultId"].(string)
	f1ReplayCLI(t, ctx, gd, []string{"fault-fix", "--fault", id, "--ref", "PR-1"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-resolve", "--fault", id})
	f1ReplayCLI(t, ctx, gd, []string{"fault-reverify", "--fault", id, "--method", "suite", "--ref", "check-1", "--outcome", "failed"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-reverify", "--fault", id, "--method", "suite", "--ref", "check-2", "--outcome", "passed"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-resolve", "--fault", id})
	replayObservation(t, ctx, gd, "report_omitted", Broken, "two")
	f1ReplayCLI(t, ctx, gd, []string{"fault-prune", "--fault", id, "--keep", "1"})
}

// FLT-9, 10, 13, 15, 16, 18, 20, 21, 22 and 32 (sweep rotation, bounds, readings, derivation and
// source clearing) are the TestF1 sweep tests' (f1_*_test.go), and FLT-11, 12, 25, 26 and 27
// (target changes and the publication protocol) TestF1_FLT_25_26_27_LifecycleWholeCLI's.

// FLT-17: opening a pre-existing database that lacks the fault tables. Here the runtimes
// differed by decision, and Go's documented behaviour is pinned (docs/port/decisions.md 14 and
// 30): Python's Store.__init__ ran the DDL on every open and re-created each missing IF NOT
// EXISTS table in the store it owned, while Go validates the required tables and never repairs a
// store - it refuses the command as a host error and leaves the store as it was. Go's twin is
// then the store Python repaired as Go would have it had it created it (the same tables and
// rows, which the golden holds), and the same transitions compare their complete output and
// tables.
func Test22_FLT_17_SchemaWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	missing := []string{"fault_ledger", "fault_occurrences", "fault_timeline", "fault_remediations", "fault_publications", "fault_targets", "fault_cursors"}
	seed := []string{f1Relationship}
	for _, table := range missing {
		seed = append(seed, "DROP TABLE "+table)
	}
	f1Seed(t, ctx, gd, seed)
	target := []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"}

	// Go: refused, and nothing in the store changes.
	before := flt17Rows(t, ctx, gd)
	var stdout, stderr bytes.Buffer
	code, handled := executeAsCLI(ctx, append([]string{"--state", gd, "--json"}, target...), &stdout, &stderr)
	const refused = "{\n  \"error\": \"host\",\n  \"detail\": \"ownership refused: required table missing: fault_ledger\"\n}\n"
	if !handled || code != 3 || stdout.String() != refused || stderr.Len() != 0 {
		t.Fatalf("Go on a store missing the fault tables: handled=%t exit %d\nstdout %q\nstderr %q\nwant exit 3 stdout %q", handled, code, stdout.String(), stderr.String(), refused)
	}
	if after := flt17Rows(t, ctx, gd); !reflect.DeepEqual(before, after) {
		t.Fatalf("Go's refusal changed the store:\nbefore %v\nafter  %v", before, after)
	}
	for _, table := range missing {
		if _, ok := before[table]; ok {
			t.Fatalf("the seeded Go store still has %s", table)
		}
	}

	// Go's twin is the store Python repaired as Go would have it had it created it: the frozen
	// fixture fenced for Go, with every table, holding the rows the seed left. The golden holds
	// its tables but schema_meta, which names the owning runtime.
	if err := os.RemoveAll(gd); err != nil {
		t.Fatal(err)
	}
	f1Twin(t, gd)
	s, err := store.Open(ctx, filepath.Join(gd, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Q(ctx).ExecContext(ctx, f1Relationship); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	twin := flt17Rows(t, ctx, gd)
	delete(twin, "schema_meta")
	checkGolden(t, "the twin's tables", nil, runPathsOf(t, filepath.Dir(gd)), twin)
	f1ReplayCLI(t, ctx, gd, target)
	replayObservation(t, ctx, gd, "report_omitted", Broken, "schema")
	f1ReplayCLI(t, ctx, gd, []string{"fault-next"})
}

// flt17Rows is every table of the store at dir and its rows, read without writing it.
func flt17Rows(t *testing.T, ctx context.Context, dir string) map[string][]string {
	t.Helper()
	tables := map[string][]string{}
	readStore(t, ctx, filepath.Join(dir, "relay.sqlite3"), func(ctx context.Context, s *store.Store) error {
		names, err := s.All(ctx, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
		if err != nil {
			return err
		}
		for _, name := range names {
			key := text(name, "name")
			rows, err := s.All(ctx, "SELECT * FROM "+testsupport.QuoteIdent(key)+" ORDER BY rowid")
			if err != nil {
				return err
			}
			tables[key] = []string{}
			for _, r := range rows {
				tables[key] = append(tables[key], fmt.Sprint(r))
			}
		}
		return nil
	})
	return tables
}

func Test22_FLT_12_AwaitingTargetWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	replayObservation(t, ctx, gd, "report_omitted", Broken, "without-target")
	f1ReplayCLI(t, ctx, gd, []string{"fault-next"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-next"})
}

func Test22_FLT_35_NestedListingBoundWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	answer := replayObservation(t, ctx, gd, "report_omitted", Broken, "listing")
	id := answer["faultId"].(string)
	for i := 0; i < 23; i++ {
		f1ReplayCLI(t, ctx, gd, []string{"fault-fix", "--fault", id, "--ref", fmt.Sprintf("PR #%d", i)})
	}
	f1ReplayCLI(t, ctx, gd, []string{"fault-show", "--limit", "1"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-show", "--fault", id})
	for i := 0; i < 4; i++ {
		f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", fmt.Sprintf(`{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"page-%d"},"occurrenceKey":"one"}`, i)})
	}
	first := f1ReplayCLI(t, ctx, gd, []string{"fault-show", "--limit", "2"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"inserted-between-pages"},"occurrenceKey":"one"}`})
	second := f1ReplayCLI(t, ctx, gd, []string{"fault-show", "--limit", "2", "--after", fmt.Sprint(first["next"])})
	f1ReplayCLI(t, ctx, gd, []string{"fault-show", "--limit", "10", "--after", fmt.Sprint(second["next"])})
}

func Test22_FLT_10_GenerationTenCursorWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	seed := []string{}
	// Fill a page ending at generation 9, then move that relationship to 10.
	for i := 0; i < 32; i++ {
		rel := fmt.Sprintf("rel-%02d", i)
		seed = append(seed,
			fmt.Sprintf("INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES('%s','ISSUE','active','parent','host','child','host',9,'[]','[]','stamp','stamp')", rel),
			fmt.Sprintf("INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at) VALUES('%s',9,'dispatch-%s','bound','turn','stamp')", rel, rel),
			fmt.Sprintf("INSERT INTO poll_observations VALUES('%s',9,'turn',NULL,NULL,'stamp','failed')", rel))
	}
	// Generation 10 is already within the rotation ceiling, though not current yet.
	seed = append(seed, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at) VALUES('rel-31',10,'dispatch-ten','bound','turn-ten','stamp')")
	f1Seed(t, ctx, gd, seed)
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
	f1Seed(t, ctx, gd, []string{"UPDATE relationships SET execution_generation=10 WHERE relationship_id='rel-31'", "INSERT INTO poll_observations VALUES('rel-31',10,'turn-ten',NULL,NULL,'stamp','failed')"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
}

func Test22_FLT_2_RegistryAndScopeRefusalsWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	replayObservation(t, ctx, gd, "invented", Broken, "unregistered")
	f1Seed(t, ctx, gd, []string{legacyScopeFault})
	f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", scopeConflictObservation("b", "first")})
	answer := f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", scopeConflictObservation("safe", "first")})
	f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", scopeConflictObservation("b", "rescope")})
	f1ReplayCLI(t, ctx, gd, []string{"fault-move", "--fault", answer["faultId"].(string), "--scope", `{"projectKey":"b"}`})
}

// FLT-29, FLT-30, FLT-31, FLT-34, FLT-35: round-trip and listing command
// outputs are compared byte-for-byte with the golden. FLT-33's documented Go
// replacement is covered by its static-registry test and CLI oracle.
func Test22_FLT_29_30_31_34_35_CLIWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	observed := replayObservation(t, ctx, gd, "report_omitted", Broken, "listing")
	id := observed["faultId"].(string)
	for _, args := range [][]string{
		{"fault-show"}, {"fault-show", "--fault", id}, {"fault-next"},
		{"fault-show", "--limit", "0"}, {"fault-show", "--fault", id, "--product", "crw"},
	} {
		f1ReplayCLI(t, ctx, gd, args)
	}
}
