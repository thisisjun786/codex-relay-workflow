package faults

import (
	"context"
	"fmt"
	"testing"
)

// These tests use f1ReplayCLI, which executes the same command against Go and
// live Python and compares exit status, stdout, stderr, and every fault_* row.

func replayObservation(t *testing.T, ctx context.Context, gd, pd string, class, severity, key string) map[string]any {
	t.Helper()
	return f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", fmt.Sprintf(`{"schema":"fault-observation/1","product":"crw","faultClass":%q,"severity":%q,"signature":{"relationship":"rel","turn":"turn"},"occurrenceKey":%q,"scope":{"projectKey":"P"},"detail":"detail","evidence":[{"kind":"row","ref":"row:1","observed":{"state":"failed"}}]}`, class, severity, key)})
}

// FLT-1: identity and duplicate observations compare complete replies and rows.
func Test22_FLT_1_IdentityWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "one")
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "one")
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "two")
}

// FLT-2, FLT-3, FLT-4, FLT-14: registry, evidence bounds, suppression, and
// malformed-observation refusals compare complete replies and rows.
func Test22_FLT_2_3_4_14_RecordWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	for _, args := range [][]string{
		{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"invented","severity":"broken","signature":{"x":1},"occurrenceKey":"one"}`},
		{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"evidence"},"occurrenceKey":"one","evidence":[{"kind":"row","ref":"one"},{"kind":"row","ref":"two"},{"kind":"row","ref":"three"},{"kind":"row","ref":"four"},{"kind":"row","ref":"five"},{"kind":"row","ref":"six"},{"kind":"row","ref":"seven"},{"kind":"row","ref":"eight"},{"kind":"row","ref":"nine"}]}`},
		{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"bad"},"occurrenceKey":"one","cleared":"false"}`},
	} {
		f1ReplayCLI(t, ctx, gd, pd, args)
	}
	for _, key := range []string{"one", "two", "three"} {
		replayObservation(t, ctx, gd, pd, "delivery_stalled", Degraded, key)
	}
}

// FLT-5, FLT-6, FLT-7, FLT-8, FLT-19, FLT-23, FLT-24: fixes, pruning,
// remediation executions, clearing, recurrence, and resolution compare the
// complete command replies and all rows after every transition.
func Test22_FLT_5_6_7_8_19_23_24_LifecycleWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	observed := replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "one")
	id := observed["faultId"].(string)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-fix", "--fault", id, "--ref", "PR-1"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-resolve", "--fault", id})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-reverify", "--fault", id, "--method", "suite", "--ref", "check-1", "--outcome", "failed"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-reverify", "--fault", id, "--method", "suite", "--ref", "check-2", "--outcome", "passed"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-resolve", "--fault", id})
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "two")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-prune", "--fault", id, "--keep", "1"})
}

// FLT-9, FLT-10, FLT-13, FLT-15, FLT-16, FLT-18, FLT-20, FLT-21, FLT-22,
// FLT-32: sweep rotation, bounds, readings, derivation and source clearing.
func Test22_FLT_9_10_13_15_16_18_20_21_22_32_SweepWholeOutput(t *testing.T) {
	TestF1_FLT_18_21_22_SweepSourcesWholeCLI(t)
	TestF1_FLT_18_21_InFlightWholeCLI(t)
	TestF1_FLT_21_SourceBeyondPageWholeCLI(t)
	TestF1_FLT_32_ReadingsContinuationWholeCLI(t)
	TestF1SweepBoundsAndScopeWholeCLI(t)
	TestF1SweepReadingsWholeCLI(t)
}

// FLT-11, FLT-12, FLT-25, FLT-26, FLT-27: target changes and the complete
// publication protocol compare command bytes and every fault table row.
func Test22_FLT_11_12_25_26_27_PublicationWholeOutput(t *testing.T) {
	testFLT252627LifecycleWholeCLI(t)
}

// FLT-17: Python and Go opening a pre-existing database are exercised through
// a cloned store; the resulting schema and complete ledger output are compared.
func Test22_FLT_17_SchemaWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	seed := []string{f1Relationship}
	for _, table := range []string{"fault_ledger", "fault_occurrences", "fault_timeline", "fault_remediations", "fault_publications", "fault_targets", "fault_cursors"} {
		seed = append(seed, "DROP TABLE "+table)
	}
	f1SeedBoth(t, ctx, gd, pd, seed)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "schema")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-next"})
}

func Test22_FLT_12_AwaitingTargetWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "without-target")
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-next"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-next"})
}

func Test22_FLT_35_NestedListingBoundWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	answer := replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "listing")
	id := answer["faultId"].(string)
	for i := 0; i < 23; i++ {
		f1ReplayCLI(t, ctx, gd, pd, []string{"fault-fix", "--fault", id, "--ref", fmt.Sprintf("PR #%d", i)})
	}
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-show", "--limit", "1"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-show", "--fault", id})
	for i := 0; i < 4; i++ {
		f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", fmt.Sprintf(`{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"page-%d"},"occurrenceKey":"one"}`, i)})
	}
	first := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-show", "--limit", "2"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"inserted-between-pages"},"occurrenceKey":"one"}`})
	second := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-show", "--limit", "2", "--after", fmt.Sprint(first["next"])})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-show", "--limit", "10", "--after", fmt.Sprint(second["next"])})
}

func Test22_FLT_10_GenerationTenCursorWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
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
	f1SeedBoth(t, ctx, gd, pd, seed)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE relationships SET execution_generation=10 WHERE relationship_id='rel-31'", "INSERT INTO poll_observations VALUES('rel-31',10,'turn-ten',NULL,NULL,'stamp','failed')"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
}

func Test22_FLT_2_RegistryAndScopeRefusalsWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	replayObservation(t, ctx, gd, pd, "invented", Broken, "unregistered")
	f1SeedBoth(t, ctx, gd, pd, []string{legacyScopeFault})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", scopeConflictObservation("b", "first")})
	answer := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", scopeConflictObservation("safe", "first")})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", scopeConflictObservation("b", "rescope")})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-move", "--fault", answer["faultId"].(string), "--scope", `{"projectKey":"b"}`})
}

// FLT-29, FLT-30, FLT-31, FLT-34, FLT-35: round-trip and listing command
// outputs are compared byte-for-byte with live Python. FLT-33's documented Go
// replacement is covered by its static-registry test and CLI oracle.
func Test22_FLT_29_30_31_34_35_CLIWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	observed := replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "listing")
	id := observed["faultId"].(string)
	for _, args := range [][]string{
		{"fault-show"}, {"fault-show", "--fault", id}, {"fault-next"},
		{"fault-show", "--limit", "0"}, {"fault-show", "--fault", id, "--product", "crw"},
	} {
		f1ReplayCLI(t, ctx, gd, pd, args)
	}
}
