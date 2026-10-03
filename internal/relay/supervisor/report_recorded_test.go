package supervisor

import (
	"context"
	"encoding/json"
	"testing"
)

func compareRecordedPython(t *testing.T, id string, first, second, prior any) {
	t.Helper()
	compareReportCapture(t, id, map[string]any{"first": first, "second": second, "prior": prior})
}
func Test24_SR_3_RecordOnceAndSuppress(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	ctx := context.Background()
	o := f.obligation(t)
	first, err := f.c.RecordReport(ctx, o, f.at, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.c.RecordReport(ctx, o, f.at, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first["recorded"] != true || second["recorded"] != false || first["seq"] != second["seq"] {
		t.Fatalf("first=%v second=%v", first, second)
	}
	yes, reason, err := f.c.Reportable(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if yes || reason != "already_reported_under_this_obligation" {
		t.Fatalf("reportable=%t reason=%q", yes, reason)
	}
	prior, err := f.c.PriorReport(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	compareRecordedPython(t, "SR-3", first, second, prior)
}
func Test24_SR_17_ReportJournalNamesMessage(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	ctx := context.Background()
	o := f.obligation(t)
	message := "m-1"
	result, err := f.c.RecordReport(ctx, o, f.at, &message, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result["recorded"] != true {
		t.Fatal(result)
	}
	rows, err := f.s.Journal(ctx, "supervisor_report", o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Subject != o.ID {
		t.Fatal(rows)
	}
	var detail map[string]any
	if err = json.Unmarshal([]byte(rows[0].Detail), &detail); err != nil {
		t.Fatal(err)
	}
	if detail["messageId"] != message || detail["kind"] != "completion" {
		t.Fatal(detail)
	}
	prior, err := f.c.PriorReport(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prior["detail"].(map[string]any)["messageId"] != message {
		t.Fatal(prior)
	}
	second, err := f.c.RecordReport(ctx, o, f.at, &message, nil)
	if err != nil {
		t.Fatal(err)
	}
	compareRecordedPython(t, "SR-17", result, second, prior)
}
