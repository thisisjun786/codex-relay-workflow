package evidence

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

const otherHead = "cccccccccccccccccccccccccccccccccccccccc"

func dispositionEntry(thread, disposition, head string) map[string]any {
	return map[string]any{"threadId": thread, "disposition": disposition, "evidenceUrl": "https://github.com/o/r/pull/1#" + thread, "head": head, "grade": "P2"}
}

func dispositionDocument(entries ...any) map[string]any {
	return map[string]any{LateDispositionsMember: entries}
}

func TestReadLateDispositions(t *testing.T) {
	t.Run("an entry is read as written", func(t *testing.T) {
		entry := dispositionEntry("T1", "backlog", restateHead)
		entry["grade"] = " P3 (separable) "
		entries, problems := ReadLateDispositions(dispositionDocument(entry, dispositionEntry("T2", "resolved", restateHead)))
		if len(problems) != 0 || len(entries) != 2 {
			t.Fatal(entries, problems)
		}
		want := LateEntry{ThreadID: "T1", Disposition: "backlog", EvidenceURL: "https://github.com/o/r/pull/1#T1", Head: restateHead, Grade: " P3 (separable) "}
		if entries[0] != want {
			t.Fatalf("got %+v want %+v", entries[0], want)
		}
	})
	t.Run("a decoded document keeps its key order and is read the same way", func(t *testing.T) {
		entry := contract.OrderedObject{{Key: "grade", Value: "P2"}, {Key: "head", Value: restateHead}, {Key: "evidenceUrl", Value: "http://example.com/x"}, {Key: "disposition", Value: "answered"}, {Key: "threadId", Value: "T1"}}
		document := contract.OrderedObject{{Key: LateDispositionsMember, Value: []any{entry}}}
		entries, problems := ReadLateDispositions(document)
		if len(problems) != 0 || len(entries) != 1 || entries[0].ThreadID != "T1" {
			t.Fatal(entries, problems)
		}
	})
	t.Run("an empty list is a document that disposes of nothing", func(t *testing.T) {
		entries, problems := ReadLateDispositions(dispositionDocument())
		if len(entries) != 0 || len(problems) != 0 {
			t.Fatal(entries, problems)
		}
	})
	good := dispositionEntry("T2", "answered", restateHead)
	with := func(key string, value any) map[string]any {
		e := dispositionEntry("T1", "answered", restateHead)
		e[key] = value
		return e
	}
	drop := func(key string) map[string]any {
		e := dispositionEntry("T1", "answered", restateHead)
		delete(e, key)
		return e
	}
	for _, tc := range []struct {
		name     string
		document any
		detail   string
	}{
		{"not an object", []any{good}, "the late-thread dispositions document is an object stating lateDispositions, not an array"},
		{"null", nil, "the late-thread dispositions document is an object stating lateDispositions, not null"},
		{"member absent", map[string]any{"x": []any{}}, "the late-thread dispositions document does not state lateDispositions"},
		{"member not a list", map[string]any{LateDispositionsMember: "T1"}, "lateDispositions is a list of entries stating threadId, disposition, evidenceUrl, head, grade, not a string"},
		{"entry not an object", dispositionDocument("T1", good), "late disposition entry 0 is an object stating threadId, disposition, evidenceUrl, head, grade, not a string"},
		{"member absent from an entry", dispositionDocument(drop("grade"), good), "late disposition entry 0 does not state grade"},
		{"member not a string", dispositionDocument(with("threadId", 7), good), "late disposition entry 0 states threadId as a number, not a string; coercing it would let two different values agree"},
		{"member null", dispositionDocument(with("head", nil), good), "late disposition entry 0 states head as null, not a string; coercing it would let two different values agree"},
		{"blank member", dispositionDocument(with("grade", " \t\u00a0"), good), "late disposition entry 0 states a blank grade"},
		{"disposition outside the set", dispositionDocument(with("disposition", "accepted"), good), "late disposition entry 0 states disposition \"accepted\", which is not one of answered, backlog, resolved, refuted"},
		{"evidence not a URL", dispositionDocument(with("evidenceUrl", "see thread"), good), "late disposition entry 0 states evidenceUrl \"see thread\", which is not an http or https URL; the evidence is something a reader can open"},
		{"evidence without a host", dispositionDocument(with("evidenceUrl", "https:///x"), good), "late disposition entry 0 states evidenceUrl \"https:///x\", which is not an http or https URL; the evidence is something a reader can open"},
		{"head not a sha", dispositionDocument(with("head", "main"), good), "late disposition entry 0 states head \"main\", which is not a commit sha"},
		{"a thread twice on one head", dispositionDocument(good, dispositionEntry("T2", "refuted", restateHead)), "late disposition entry 1 repeats the disposition of entry 0: thread \"T2\" on head \"" + restateHead + "\", and two entries for one thread on one head cannot both be the judgement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, problems := ReadLateDispositions(tc.document)
			if len(entries) != 0 || len(problems) != 1 || problems[0].Code != Malformed || problems[0].Detail != tc.detail {
				t.Fatalf("entries %v problems %q want %q", entries, restateLines(problems), tc.detail)
			}
		})
	}
	t.Run("one malformed entry rejects the whole document", func(t *testing.T) {
		entries, problems := ReadLateDispositions(dispositionDocument(good, drop("grade"), with("disposition", "x")))
		if len(entries) != 0 || len(problems) != 2 {
			t.Fatal(entries, restateLines(problems))
		}
	})
	t.Run("one thread under two heads is two judgements", func(t *testing.T) {
		entries, problems := ReadLateDispositions(dispositionDocument(dispositionEntry("T1", "answered", restateHead), dispositionEntry("T1", "answered", otherHead)))
		if len(entries) != 2 || len(problems) != 0 {
			t.Fatal(entries, problems)
		}
	})
}

func TestRestateWithDispositions(t *testing.T) {
	late := func(extra ...any) map[string]any {
		return restateSnapshot(nil, append([]any{restateThread("T1", "u/T1", true), restateThread("T2", "u/T2", true)}, extra...)...)
	}
	t.Run("without a document there are no results", func(t *testing.T) {
		problems, results := RestateWithDispositions(restateHead, restateRecord(), late(), nil)
		if results != nil || len(problems) != 1 || problems[0].Code != LateFinding {
			t.Fatal(restateLines(problems), results)
		}
	})
	t.Run("an entry for each late thread on this head clears the late finding", func(t *testing.T) {
		problems, results := RestateWithDispositions(restateHead, restateRecord(), late(), dispositionDocument(dispositionEntry("T1", "answered", restateHead), dispositionEntry("T2", "backlog", restateHead)))
		if len(problems) != 0 || len(results) != 2 {
			t.Fatal(restateLines(problems), results)
		}
		for _, r := range results {
			if r.(map[string]any)["effect"] != LateApplied {
				t.Fatal(r)
			}
		}
	})
	t.Run("a thread without an entry stays late and is the only one named", func(t *testing.T) {
		problems, _ := RestateWithDispositions(restateHead, restateRecord(), late(), dispositionDocument(dispositionEntry("T1", "answered", restateHead)))
		if got := restateLines(problems); len(got) != 1 || got[0] != lateMessage(1, "u/T2") {
			t.Fatal(got)
		}
	})
	t.Run("an entry for another head changes nothing", func(t *testing.T) {
		problems, results := RestateWithDispositions(restateHead, restateRecord(), late(), dispositionDocument(dispositionEntry("T1", "answered", otherHead), dispositionEntry("T2", "answered", otherHead)))
		if got := restateLines(problems); len(got) != 1 || got[0] != lateMessage(2, "u/T1, u/T2") {
			t.Fatal(got)
		}
		for _, r := range results {
			if got := r.(map[string]any); got["effect"] != LateIgnored || got["reason"] != LateOtherHead {
				t.Fatal(got)
			}
		}
	})
	t.Run("the head under restatement is the head given, not the one the forge shows", func(t *testing.T) {
		// A record about another head is a moved candidate whatever the entries say.
		problems, results := RestateWithDispositions(otherHead, restateRecordAbout(otherHead), late(), dispositionDocument(dispositionEntry("T1", "answered", otherHead), dispositionEntry("T2", "answered", otherHead)))
		lines := restateLines(problems)
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "candidate_moved: ") || results[0].(map[string]any)["effect"] != LateApplied {
			t.Fatal(lines, results)
		}
	})
	t.Run("readings of entries that dispose of nothing, in order of precedence", func(t *testing.T) {
		snapshot := restateSnapshot(nil, restateThread("T1", "u/T1", true))
		_, results := RestateWithDispositions(restateHead, restateRecord("T1"), snapshot, dispositionDocument(
			dispositionEntry("T1", "answered", restateHead),
			dispositionEntry("T9", "answered", restateHead),
			dispositionEntry("T1", "answered", otherHead),
			dispositionEntry("T9", "answered", otherHead),
		))
		var got []string
		for _, r := range results {
			one := r.(map[string]any)
			got = append(got, one["effect"].(string)+":"+one["reason"].(string))
		}
		if want := "ignored:not_late ignored:unknown_thread ignored:other_head ignored:other_head"; strings.Join(got, " ") != want {
			t.Fatalf("got %q want %q", strings.Join(got, " "), want)
		}
	})
	t.Run("only a review thread can be disposed of", func(t *testing.T) {
		snapshot := restateSnapshot(nil, map[string]any{"kind": "comment", "id": "C1", "url": "u/C1"}, map[string]any{"kind": "review", "id": "R1", "url": "u/R1"})
		problems, results := RestateWithDispositions(restateHead, restateRecord(), snapshot, dispositionDocument(dispositionEntry("C1", "answered", restateHead), dispositionEntry("R1", "answered", restateHead)))
		for _, r := range results {
			if got := r.(map[string]any); got["reason"] != LateUnknownThread {
				t.Fatal(got)
			}
		}
		if len(problems) != 0 {
			t.Fatal(restateLines(problems))
		}
	})
	t.Run("a malformed document keeps every thread late and says why", func(t *testing.T) {
		bad := dispositionEntry("T1", "answered", restateHead)
		delete(bad, "grade")
		problems, results := RestateWithDispositions(restateHead, restateRecord(), late(), dispositionDocument(bad, dispositionEntry("T2", "answered", restateHead)))
		want := []string{"malformed_evidence: late disposition entry 0 does not state grade", lateMessage(2, "u/T1, u/T2")}
		if got := restateLines(problems); strings.Join(got, "\n") != strings.Join(want, "\n") || len(results) != 0 || results == nil {
			t.Fatal(got, results)
		}
	})
	t.Run("a malformed record is reported with a malformed document and judges nothing", func(t *testing.T) {
		record := restateRecord()
		record["checks"] = "x"
		problems, results := RestateWithDispositions(restateHead, record, late(), map[string]any{"x": 1})
		want := []string{"malformed_evidence: the restated checks are a list of check runs, not a string", "malformed_evidence: the late-thread dispositions document does not state lateDispositions"}
		if got := restateLines(problems); strings.Join(got, "\n") != strings.Join(want, "\n") || len(results) != 0 || results == nil {
			t.Fatal(got, results)
		}
		problems, results = RestateWithDispositions(restateHead, []any{}, late(), dispositionDocument())
		if got := restateLines(problems); len(got) != 1 || len(results) != 0 || results == nil {
			t.Fatal(got, results)
		}
	})
	t.Run("an empty head is a malformed record and judges nothing", func(t *testing.T) {
		problems, results := RestateWithDispositions("", restateRecord(), late(), dispositionDocument(dispositionEntry("T1", "answered", restateHead)))
		if len(problems) == 0 || problems[0].Code != Malformed || len(results) != 0 {
			t.Fatal(restateLines(problems), results)
		}
	})
}
