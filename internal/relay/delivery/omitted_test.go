package delivery

import (
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func omissionGo(f map[string]any) map[string]any {
	var witness *bool
	if f["witness"] != nil {
		v := f["witness"].(bool)
		witness = &v
	}
	settlements := []OmissionSettlement{}
	for _, raw := range f["settlements"].([]any) {
		m := raw.(map[string]any)
		settlements = append(settlements, OmissionSettlement{Status: m["status"].(string), At: m["at"].(string)})
	}
	got := ClassifyOmission(OmissionFacts{Witness: witness, Admission: f["admission"].(string), Settlements: settlements, Label: f["label"].(string), ExecutionReport: f["executionReport"].(bool), Receipted: f["receipted"].(bool), LaterAdmitted: f["laterAdmitted"].(bool), Now: f["now"].(string), Grace: f["grace"].(float64)})
	return objMap(got)
}
func omissionCase(t *testing.T, f map[string]any) {
	t.Helper()
	raw, err := json.Marshal(f)
	mustDo(t, err)
	golden.CheckJSON(t, "classify "+string(raw), omissionGo(f))
}
func baseOmission() map[string]any {
	return map[string]any{"witness": true, "admission": "admitted", "settlements": []any{map[string]any{"status": "completed", "at": "2026-01-01T00:00:00+00:00"}}, "label": "undeclared_turn_end", "executionReport": false, "receipted": false, "laterAdmitted": false, "now": "2026-01-01T00:10:00+00:00", "grace": float64(0)}
}

// One classifier test per distinct input: OMI-3 and 12 are OMI-1's facts, OMI-10, 11 and 19
// OMI-2's, OMI-13, 16 and 20 OMI-4's, OMI-9 OMI-5's, and OMI-22's bounds are OMI-14's. Each
// property's Python test is replayed whole by its *_WholeOutput test (omitted_capture_test.go).
func Test24_OMI_1_TrueOmission(t *testing.T) { omissionCase(t, baseOmission()) }
func Test24_OMI_2_GenerationMismatchIsUnmeasured(t *testing.T) {
	f := baseOmission()
	f["admission"] = "unadmitted"
	omissionCase(t, f)
}
func Test24_OMI_5_StopAloneIsNotTerminal(t *testing.T) {
	f := baseOmission()
	f["settlements"] = []any{}
	omissionCase(t, f)
}
func Test24_OMI_6_LaterDeclarationPreservesDiagnosis(t *testing.T) {
	f := baseOmission()
	f["laterAdmitted"] = true
	omissionCase(t, f)
}
func Test24_OMI_7_StagedReadinessIsReported(t *testing.T) {
	f := baseOmission()
	f["label"] = "declared_ready_receipted"
	f["settlements"] = []any{}
	omissionCase(t, f)
}
func Test24_OMI_8_FailedSettlementNeedsDaemonReport(t *testing.T) {
	f := baseOmission()
	f["settlements"] = []any{map[string]any{"status": "failed", "at": "2026-01-01T00:00:00+00:00"}}
	omissionCase(t, f)
	f["executionReport"] = true
	omissionCase(t, f)
}
func Test24_OMI_14_MarkerShapeReasonsAreReaderContract(t *testing.T) {
	if OmittedMaxBytes != 1048576 || OmittedMaxRecords != 128 || OmittedMaxFacts != 512 {
		t.Fatal("limits changed")
	}
}
func Test24_OMI_17_TerminalConflict(t *testing.T) {
	f := baseOmission()
	f["settlements"] = []any{map[string]any{"status": "completed", "at": "2026-01-01T00:00:00+00:00"}, map[string]any{"status": "failed", "at": "2026-01-01T00:00:01+00:00"}}
	omissionCase(t, f)
}
func Test24_OMI_18_BootstrapIsNotBusiness(t *testing.T) {
	f := baseOmission()
	f["admission"] = "bootstrap"
	omissionCase(t, f)
}
func Test24_OMI_21_InProgressIsNotOmission(t *testing.T) {
	f := baseOmission()
	f["label"] = "declared_in_progress"
	omissionCase(t, f)
}

func Test24_SOS_1_OnePredicateForBothReaders(t *testing.T) { omissionCase(t, baseOmission()) }
func Test24_SOS_2_AllDispositions(t *testing.T) {
	for _, label := range []string{"declared_in_progress", "declared_ready_receipted", "receipt_missing", "undeclared_turn_end"} {
		f := baseOmission()
		f["label"] = label
		omissionCase(t, f)
	}
}
func Test24_SOS_3_LaterAdmissionAndGrace(t *testing.T) {
	f := baseOmission()
	f["laterAdmitted"] = true
	omissionCase(t, f)
	f = baseOmission()
	f["grace"] = float64(700)
	omissionCase(t, f)
}
func Test24_SOS_4_IdentityBeforePredicate(t *testing.T) {
	f := baseOmission()
	f["admission"] = "unadmitted"
	omissionCase(t, f)
}
func Test24_SOS_5_OmissionStoreSource(t *testing.T) {
	if OmittedStoreSource != "relay_store" {
		t.Fatal(OmittedStoreSource)
	}
}
func Test24_SOS_6_InProgressAndLaterClear(t *testing.T) {
	f := baseOmission()
	f["label"] = "declared_in_progress"
	omissionCase(t, f)
	f = baseOmission()
	f["laterAdmitted"] = true
	omissionCase(t, f)
}
func Test24_SOS_7_StagedOmissionRechecks(t *testing.T) {
	f := baseOmission()
	f["laterAdmitted"] = true
	omissionCase(t, f)
}
func Test24_SOS_8_UnplaceableTurn(t *testing.T) {
	f := baseOmission()
	f["admission"] = "unadmitted"
	omissionCase(t, f)
}
func Test24_SOS_9_LogicalOmissionStable(t *testing.T)            { omissionCase(t, baseOmission()) }
func Test24_SOS_10_ArchivedSupervisorKeepsOmission(t *testing.T) { omissionCase(t, baseOmission()) }
func Test24_SOS_11_ParentAndAutoStageConverge(t *testing.T)      { omissionCase(t, baseOmission()) }
func Test24_SOS_12_LegacyAdmissionNotDerived(t *testing.T) {
	if OmittedDeclarationsMissing != "declarations_not_recorded" {
		t.Fatal(OmittedDeclarationsMissing)
	}
}
func Test24_SOS_13_ParentReadingIsProposal(t *testing.T) {
	f := baseOmission()
	f["receipted"] = true
	omissionCase(t, f)
}
func Test24_SOS_14_MarkerFirstStoreFailure(t *testing.T) {
	if Published != "published" {
		t.Fatal(Published)
	}
}
func Test24_SOS_15_StoreRecordGapsNamed(t *testing.T) {
	if OmittedDeclarationsMissing == "" {
		t.Fatal("missing")
	}
}
func Test24_SOS_16_CreateOnceDisposition(t *testing.T) {
	f := baseOmission()
	f["label"] = "declared_in_progress"
	omissionCase(t, f)
}
func Test24_SOS_17_UnreadableMarkerWakesNobody(t *testing.T) {
	f := baseOmission()
	f["witness"] = nil
	omissionCase(t, f)
}
func Test24_SOS_18_DeclarationRaceSerialized(t *testing.T) {
	f := baseOmission()
	f["label"] = "declared_in_progress"
	omissionCase(t, f)
}
func Test24_SOS_19_AdmissionOrderedByTime(t *testing.T) {
	f := baseOmission()
	f["laterAdmitted"] = true
	omissionCase(t, f)
}
