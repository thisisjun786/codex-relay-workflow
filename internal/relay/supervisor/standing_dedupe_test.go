package supervisor

import (
	"context"
	"testing"
)

func Test24_SR_13_ConfirmedOmissionNotStanding(t *testing.T) {
	f := fixture24(t)
	reading := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "reason": "terminal_without_report", "relationshipId": "rel-1", "executionGeneration": float64(1), "selectors": map[string]any{"turn": "turn-7"}}
	for _, query := range []string{
		"INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')",
		"INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES ('confirmed','rel-1','REL-1','coordination_document','doc-1','verdict','turn-7','digest','summary','confirmed','t','t')",
	} {
		if _, err := f.s.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	o := ObservationObligation(reading)
	answer, err := f.c.Selection(context.Background(), *o, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer["standing"] != "discharged" || answer["reason"] != "already_in_the_record_the_supervisor_reads" {
		t.Fatal(answer)
	}
	project, err := f.c.Standing(context.Background(), "PRJ-1", []any{reading})
	if err != nil {
		t.Fatal(err)
	}
	if len(project["standing"].([]any)) != 1 || len(project["gaps"].([]any)) != 0 {
		t.Fatal(project)
	}
}
func Test24_SR_12_StandingCountsFactsNotReadings(t *testing.T) {
	f := fixture24(t)
	good := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "reason": "terminal_without_report", "relationshipId": "rel-1", "executionGeneration": float64(1), "selectors": map[string]any{"turn": "turn-7"}}
	bad := map[string]any{"schema": "reporting-observation/1", "reportingState": "something", "relationshipId": "rel-1", "selectors": map[string]any{"turn": "turn-7"}}
	foreign := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": "rel-elsewhere", "selectors": map[string]any{"turn": "turn-7"}}
	for _, tc := range []struct {
		name           string
		readings       []any
		standing, gaps int
	}{
		{"two identical omissions", []any{good, good}, 2, 0},
		{"one bad reading twice", []any{bad, bad}, 1, 1},
		{"foreign readings ignored", []any{foreign, foreign}, 1, 0},
		{"loops coexist", []any{good, bad, bad}, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, err := f.c.Standing(context.Background(), "PRJ-1", tc.readings)
			if err != nil {
				t.Fatal(err)
			}
			if len(answer["standing"].([]any)) != tc.standing || len(answer["gaps"].([]any)) != tc.gaps {
				t.Fatalf("%s", jsonText(answer))
			}
		})
	}
}
