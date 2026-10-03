package evidence

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// These tests pin what RestateProblems answers for a restated record whose threads the fresh
// reading shows. The expected values were recorded from the code before late-thread dispositions
// existed and were green on it; they run unchanged afterwards, so they are the evidence that a
// restatement without a dispositions document is the same as it was.

const (
	restateHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	restateBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func num(n int) json.Number { return json.Number(fmt.Sprint(n)) }

// restateRecord is a coherent handoff record whose review coverage lists the named threads.
func restateRecord(seen ...string) map[string]any {
	listed := make([]any, len(seen))
	for i, id := range seen {
		listed[i] = id
	}
	return map[string]any{
		"baseSha":           restateBase,
		"requiredDeclared":  []any{"dev-gate"},
		"requiredProviders": map[string]any{},
		"checks":            []any{map[string]any{"runId": "run-1", "name": "dev-gate", "headSha": restateHead, "conclusion": "success", "attempt": num(1)}},
		"reviewCoverage":    map[string]any{"hasNextPage": false, "pagesRead": num(1), "totalCount": num(len(seen)), "threadsSeen": listed, "unresolved": num(0)},
	}
}

func restateThread(id, url string, resolved bool) map[string]any {
	return map[string]any{"kind": "reviewThread", "id": id, "url": url, "resolved": resolved}
}

// restateSnapshot is a fresh reading of the same pull request, with these findings.
func restateSnapshot(snapshotProblems []any, findings ...any) map[string]any {
	if snapshotProblems == nil {
		snapshotProblems = []any{}
	}
	return map[string]any{
		"pinned":   map[string]any{"headSha": restateHead, "baseSha": restateBase},
		"gates":    map[string]any{"requiredDeclared": []any{"dev-gate"}, "requiredProviders": map[string]any{}},
		"problems": snapshotProblems,
		"findings": findings,
	}
}

func restateLines(problems []Problem) []string {
	lines := make([]string, len(problems))
	for i, p := range problems {
		lines[i] = p.Code + ": " + p.Detail
	}
	return lines
}

const lateTail = "no longer describes the candidate: "

// restateRecordAbout is restateRecord for a record that is about another head.
func restateRecordAbout(head string, seen ...string) map[string]any {
	record := restateRecord(seen...)
	record["checks"] = []any{map[string]any{"runId": "run-1", "name": "dev-gate", "headSha": head, "conclusion": "success", "attempt": num(1)}}
	return record
}

func lateMessage(count int, labels string) string {
	return fmt.Sprintf("late_finding: %d review thread(s) on this head are not in the record's threadsSeen, so the record did not see them and ", count) + lateTail + labels
}

func TestRestateProblemsPinnedBeforeDispositions(t *testing.T) {
	many := []any{}
	for _, n := range []int{6, 5, 4, 3, 2, 1} {
		many = append(many, restateThread(fmt.Sprintf("T%d", n), fmt.Sprintf("https://x/t%d", n), n%2 == 0))
	}
	for _, tc := range []struct {
		name     string
		head     string
		record   any
		snapshot any
		want     []string
	}{
		{"a current record has no problem", restateHead, restateRecord("T1"), restateSnapshot(nil, restateThread("T1", "u/T1", true)), nil},
		{"one late thread is named by its url", restateHead, restateRecord(), restateSnapshot(nil, restateThread("T1", "u/T1", true)), []string{lateMessage(1, "u/T1")}},
		{"a late thread without a url is named by its id", restateHead, restateRecord(), restateSnapshot(nil, restateThread("T1", "", true)), []string{lateMessage(1, "T1")}},
		{"resolved or not makes no difference", restateHead, restateRecord("T3"), restateSnapshot(nil, restateThread("T9", "u/T9", false), restateThread("T3", "u/T3", true), restateThread("T2", "u/T2", true)), []string{lateMessage(2, "u/T2, u/T9")}},
		{"six late threads name the first five in order", restateHead, restateRecord(), restateSnapshot(nil, many...), []string{lateMessage(6, "https://x/t1, https://x/t2, https://x/t3, https://x/t4, https://x/t5")}},
		{"a review and a comment are never late threads", restateHead, restateRecord(), restateSnapshot(nil, map[string]any{"kind": "review", "id": "R1", "url": "u/R1"}, map[string]any{"kind": "comment", "id": "C1", "url": "u/C1"}), nil},
		{"a record that is not an object", restateHead, []any{}, restateSnapshot(nil), []string{"malformed_evidence: a handoff record is an object, not an array"}},
		{"a malformed record stops before the late judgement", restateHead, func() any { r := restateRecord(); r["checks"] = "x"; return r }(), restateSnapshot(nil, restateThread("T1", "u/T1", true)), []string{"malformed_evidence: the restated checks are a list of check runs, not a string"}},
		{"the snapshot's own problems come before the late finding", restateHead, restateRecord(), restateSnapshot([]any{map[string]any{"code": "unreadable", "detail": "reading the thing failed"}}, restateThread("T1", "u/T1", true)), []string{"unreadable: reading the thing failed", lateMessage(1, "u/T1")}},
		{"a moved head comes before the snapshot's problems and the late finding", "cccccccccccccccccccccccccccccccccccccccc", restateRecordAbout("cccccccccccccccccccccccccccccccccccccccc"), restateSnapshot([]any{map[string]any{"code": "unreadable", "detail": "reading the thing failed"}}, restateThread("T1", "u/T1", true)), []string{"candidate_moved: the record is about head \"cccccccccccccccccccccccccccccccccccccccc\" and the forge now reports \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\", so the record describes a commit that is no longer the candidate", "unreadable: reading the thing failed", lateMessage(1, "u/T1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := restateLines(RestateProblems(tc.head, tc.record, tc.snapshot))
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
