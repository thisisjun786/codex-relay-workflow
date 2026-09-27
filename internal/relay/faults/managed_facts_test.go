package faults

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"strings"
	"testing"
)

func Test22_FLF_4_ManagedAnswerDoesNotInventAChild(t *testing.T) {
	for _, tc := range []struct {
		status        string
		answer        row
		want, without string
	}{
		{"settings_unverified", row{store.Column{Name: "detail", Value: `{"retainedChildTaskId":"child-1"}`}}, "created", "no child"},
		{"failed", row{store.Column{Name: "detail", Value: `{"retainedChildTaskId":"child-new"}`}}, "child-new", "no child"},
		{"unknown", row{store.Column{Name: "detail", Value: `{"retainedChildTaskId":null}`}}, "not established", "no child"},
		{"failed", nil, "not established", "no child"},
	} {
		_, actual, impact := managedAnswerFacts("REL-MANAGED", tc.status, tc.answer)
		if !strings.Contains(actual, tc.want) || tc.without != "" && strings.Contains(actual, tc.without) && tc.status != "failed" {
			t.Fatalf("%s: %s / %s", tc.status, actual, impact)
		}
	}
}
func Test22_FLF_5_ManagedRecordNamesItsClearConditions(t *testing.T) {
	clears := classes["managed_start_failed"].clears
	for _, part := range []string{"accepted", "attach", "creation-stage answer"} {
		if !strings.Contains(clears, part) {
			t.Fatalf("missing clear condition %s in %q", part, clears)
		}
	}
}
