//go:build dev

package cxcfuzz

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// An absent plan read answers the path of the directory each side read. The oracle reads .codexclaw
// and the port reads .crw, so the two answers differ by the directory name alone, which the rename
// table maps onto the same name (CRW-978 c1).
func TestGoalplanCompareCallsANameOnlyPathDifferenceSame(t *testing.T) {
	goOut := pyjson.Object{{Key: "kind", Value: "absent"}, {Key: "path", Value: "<ROOT>/.crw/goalplans/rec-plan/goalplan.json"}, {Key: "plan", Value: nil}}
	oracleOut := pyjson.Object{{Key: "kind", Value: "absent"}, {Key: "path", Value: "<ROOT>/.codexclaw/goalplans/rec-plan/goalplan.json"}, {Key: "plan", Value: nil}}
	if verdict := goalplanCompare(goOut, oracleOut); verdict.Kind != Same {
		t.Fatalf("a name-only path difference compared %v (%s), want Same", verdict.Kind, verdict.Detail)
	}
}

// A real read difference and a real write difference are two differences: the verdict names both
// fields, and the name-only path difference is not one of them (CRW-978 c1).
func TestGoalplanCompareNamesEachRealDifference(t *testing.T) {
	goOut := pyjson.Object{{Key: "kind", Value: "ok"}, {Key: "path", Value: "<ROOT>/.crw/goalplans/rec-plan/goalplan.json"}, {Key: "plan", Value: "{\"a\": 1}"}, {Key: "written", Value: "w1"}}
	oracleOut := pyjson.Object{{Key: "kind", Value: "ok"}, {Key: "path", Value: "<ROOT>/.codexclaw/goalplans/rec-plan/goalplan.json"}, {Key: "plan", Value: "{\"a\": 2}"}, {Key: "written", Value: "w2"}}
	verdict := goalplanCompare(goOut, oracleOut)
	if verdict.Kind != Differ || !strings.Contains(verdict.Detail, "plan, written") || strings.Contains(verdict.Detail, "path") {
		t.Fatalf("verdict %+v, want a differ naming plan and written and not path", verdict)
	}
}
