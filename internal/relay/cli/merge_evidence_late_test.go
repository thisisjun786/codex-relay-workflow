package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// The scripted forge reports this head for the pull request.
const (
	lateHead      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	lateOtherHead = "cccccccccccccccccccccccccccccccccccccccc"
)

func lateEntry(thread, disposition, head string) map[string]any {
	return map[string]any{"threadId": thread, "disposition": disposition, "evidenceUrl": "https://github.com/owner/repo/pull/7#discussion_" + thread, "head": head, "grade": "P2"}
}

// lateDoc is the coordinator's dispositions document.
func lateDoc(entries ...any) map[string]any { return map[string]any{"lateDispositions": entries} }

func lateWrite(t *testing.T, name string, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// lateRecord is a receipt written before the late threads arrived: a reading of the pull
// request while it had no review thread at all.
func lateRecord(t *testing.T) string {
	t.Helper()
	_, first := runMerge(t, scriptedForge{})
	return lateWrite(t, "record.json", first)
}

// lateRestate restates that record against a forge that now shows two resolved threads, T1 and T2.
func lateRestate(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	return runMerge(t, scriptedForge{late: true}, append([]string{"--restate", lateRecord(t)}, args...)...)
}

func lateRestatement(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	rest, ok := p["restatement"].(map[string]any)
	if !ok {
		t.Fatalf("no restatement in %v", p)
	}
	return rest
}

func lateCodes(t *testing.T, p map[string]any) string {
	t.Helper()
	var codes []string
	for _, raw := range lateRestatement(t, p)["problems"].([]any) {
		codes = append(codes, raw.(map[string]any)["code"].(string))
	}
	return strings.Join(codes, ",")
}

func lateDetail(t *testing.T, p map[string]any, code string) string {
	t.Helper()
	for _, raw := range lateRestatement(t, p)["problems"].([]any) {
		if one := raw.(map[string]any); one["code"] == code {
			return one["detail"].(string)
		}
	}
	t.Fatalf("no %s problem in %v", code, p["restatement"])
	return ""
}

func lateExec(t *testing.T, s scriptedForge, args ...string) (int, string, string) {
	t.Helper()
	old := forgeRunner
	forgeRunner = func(context.Context) evidence.Runner { return s.run }
	defer func() { forgeRunner = old }()
	var out, stderr bytes.Buffer
	code := Execute(context.Background(), append([]string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7"}, args...), &out, &stderr)
	return code, out.String(), stderr.String()
}

// A thread that arrives on the same head after the receipt is a late finding until the coordinator
// records a disposition for it, and then the restatement passes.
func TestLateDispositionClearsLateFinding(t *testing.T) {
	t.Run("without a disposition both threads are late findings", func(t *testing.T) {
		code, p := lateRestate(t)
		rest := lateRestatement(t, p)
		if code != 2 || rest["current"] != false {
			t.Fatal(code, rest)
		}
		if _, given := rest["lateDispositions"]; given {
			t.Fatalf("lateDispositions appears without the flag: %v", rest)
		}
		detail := lateDetail(t, p, "late_finding")
		if !strings.Contains(detail, "2 review thread(s)") || !strings.Contains(detail, "u/T1") || !strings.Contains(detail, "u/T2") {
			t.Fatal(detail)
		}
	})
	t.Run("a same-head disposition for each late thread passes", func(t *testing.T) {
		entries := []any{lateEntry("T1", "answered", lateHead), lateEntry("T2", "backlog", lateHead)}
		entries[1].(map[string]any)["grade"] = "P3 (separable, follow-up filed)"
		code, p := lateRestate(t, "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(entries...)))
		rest := lateRestatement(t, p)
		if code != 0 || rest["current"] != true || len(rest["problems"].([]any)) != 0 {
			t.Fatal(code, rest)
		}
		results := rest["lateDispositions"].([]any)
		if len(results) != 2 {
			t.Fatal(results)
		}
		for i, want := range []struct{ thread, disposition, grade string }{{"T1", "answered", "P2"}, {"T2", "backlog", "P3 (separable, follow-up filed)"}} {
			got := results[i].(map[string]any)
			if got["threadId"] != want.thread || got["disposition"] != want.disposition || got["grade"] != want.grade || got["head"] != lateHead || got["effect"] != "applied" || got["evidenceUrl"] == "" {
				t.Fatalf("result %d: %v", i, got)
			}
			if _, reasoned := got["reason"]; reasoned {
				t.Fatalf("an applied result carries a reason: %v", got)
			}
		}
	})
	t.Run("a disposition for one thread leaves the other late", func(t *testing.T) {
		code, p := lateRestate(t, "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(lateEntry("T1", "refuted", lateHead))))
		if code != 2 || lateRestatement(t, p)["current"] != false {
			t.Fatal(code, p["restatement"])
		}
		detail := lateDetail(t, p, "late_finding")
		if !strings.Contains(detail, "1 review thread(s)") || !strings.Contains(detail, "u/T2") || strings.Contains(detail, "u/T1") {
			t.Fatal(detail)
		}
	})
	t.Run("every disposition in the closed set is accepted", func(t *testing.T) {
		for _, disposition := range []string{"answered", "backlog", "resolved", "refuted"} {
			code, p := lateRestate(t, "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(lateEntry("T1", disposition, lateHead), lateEntry("T2", disposition, lateHead))))
			if code != 0 || lateRestatement(t, p)["current"] != true {
				t.Fatal(disposition, code, p["restatement"])
			}
		}
	})
	t.Run("one thread under two heads applies only the entry for the head under restatement", func(t *testing.T) {
		code, p := lateRestate(t, "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(lateEntry("T1", "answered", lateOtherHead), lateEntry("T1", "refuted", lateHead), lateEntry("T2", "answered", lateHead))))
		results := lateRestatement(t, p)["lateDispositions"].([]any)
		if code != 0 || len(results) != 3 || results[0].(map[string]any)["reason"] != "other_head" || results[1].(map[string]any)["effect"] != "applied" || results[1].(map[string]any)["disposition"] != "refuted" {
			t.Fatal(code, results)
		}
	})
	t.Run("the grade and the evidence are echoed exactly as written", func(t *testing.T) {
		entry := lateEntry("T1", "answered", lateHead)
		entry["grade"] = " P2 "
		entry["evidenceUrl"] = "https://github.com/owner/repo/pull/7#discussion_r1 "
		code, p := lateRestate(t, "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(entry, lateEntry("T2", "answered", lateHead))))
		got := lateRestatement(t, p)["lateDispositions"].([]any)[0].(map[string]any)
		if code != 0 || got["grade"] != " P2 " || got["evidenceUrl"] != entry["evidenceUrl"] {
			t.Fatal(code, got)
		}
	})
	t.Run("--restate-head names the head the entries are matched against", func(t *testing.T) {
		// The record is about aaaa..., so naming cccc... moves the candidate: an entry for cccc... is
		// applied to the threads but cannot make a moved candidate current, and an entry for the head
		// the record really names no longer matches.
		code, p := lateRestate(t, "--restate-head", lateOtherHead, "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(lateEntry("T1", "answered", lateOtherHead), lateEntry("T2", "answered", lateHead))))
		rest := lateRestatement(t, p)
		results := rest["lateDispositions"].([]any)
		codes := lateCodes(t, p)
		if code != 2 || rest["current"] != false || !strings.Contains(codes, "candidate_moved") || results[0].(map[string]any)["effect"] != "applied" || results[1].(map[string]any)["reason"] != "other_head" {
			t.Fatal(code, codes, results)
		}
		if detail := lateDetail(t, p, "late_finding"); !strings.Contains(detail, "1 review thread(s)") || !strings.Contains(detail, "u/T2") {
			t.Fatal(detail)
		}
	})
	t.Run("a disposition does not stand in for resolving the thread", func(t *testing.T) {
		// T1 is unresolved on the forge. The fresh reading refuses it as review_incomplete whatever
		// the coordinator recorded, so the coordinator resolves it first.
		code, p := runMerge(t, scriptedForge{late: true, unresolved: true}, "--restate", lateRecord(t), "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(lateEntry("T1", "answered", lateHead), lateEntry("T2", "answered", lateHead))))
		rest := lateRestatement(t, p)
		codes := lateCodes(t, p)
		if code != 2 || rest["current"] != false || !strings.Contains(codes, "review_incomplete") || strings.Contains(codes, "late_finding") {
			t.Fatal(code, codes, rest)
		}
	})
}

// A disposition for another head has no effect, and a malformed document is refused.
func TestLateDispositionRefusals(t *testing.T) {
	t.Run("a disposition for another head has no effect", func(t *testing.T) {
		for name, head := range map[string]string{"another commit": lateOtherHead, "the same sha in capitals": strings.ToUpper(lateHead), "a 7-character prefix of the sha": lateHead[:7]} {
			t.Run(name, func(t *testing.T) {
				code, p := lateRestate(t, "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(lateEntry("T1", "answered", head), lateEntry("T2", "answered", head))))
				rest := lateRestatement(t, p)
				if code != 2 || rest["current"] != false || lateCodes(t, p) != "late_finding" {
					t.Fatal(code, lateCodes(t, p), rest)
				}
				for _, raw := range rest["lateDispositions"].([]any) {
					got := raw.(map[string]any)
					if got["effect"] != "ignored" || got["reason"] != "other_head" || got["head"] != head {
						t.Fatal(got)
					}
				}
			})
		}
	})
	t.Run("an entry naming a padded thread id matches no thread", func(t *testing.T) {
		code, p := lateRestate(t, "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(lateEntry(" T1", "answered", lateHead), lateEntry("T2", "answered", lateHead))))
		rest := lateRestatement(t, p)
		got := rest["lateDispositions"].([]any)[0].(map[string]any)
		if code != 2 || got["reason"] != "unknown_thread" || got["threadId"] != " T1" || !strings.Contains(lateDetail(t, p, "late_finding"), "u/T1") {
			t.Fatal(code, got, rest)
		}
	})
	t.Run("a disposition for a thread that is not late or not there has no effect", func(t *testing.T) {
		// The record saw T1 and T2, so neither is late; the entries only report why they changed nothing.
		_, first := runMerge(t, scriptedForge{late: true})
		record := lateWrite(t, "record.json", first)
		code, p := runMerge(t, scriptedForge{late: true}, "--restate", record, "--late-dispositions", lateWrite(t, "dispositions.json", lateDoc(lateEntry("T1", "answered", lateHead), lateEntry("T9", "answered", lateHead))))
		rest := lateRestatement(t, p)
		if code != 0 || rest["current"] != true {
			t.Fatal(code, rest)
		}
		results := rest["lateDispositions"].([]any)
		if r := results[0].(map[string]any); r["effect"] != "ignored" || r["reason"] != "not_late" {
			t.Fatal(r)
		}
		if r := results[1].(map[string]any); r["effect"] != "ignored" || r["reason"] != "unknown_thread" {
			t.Fatal(r)
		}
	})
	t.Run("a malformed document is refused and covers nothing", func(t *testing.T) {
		// Every malformed shape and its exact message is pinned in the evidence package; these are the
		// ones that reach the command through a file.
		other := lateEntry("T2", "answered", lateHead)
		with := func(key string, value any) map[string]any {
			e := lateEntry("T1", "answered", lateHead)
			e[key] = value
			return e
		}
		for name, document := range map[string]any{
			"a member missing":          lateDoc(with("grade", nil), other),
			"unknown disposition":       lateDoc(with("disposition", "accepted"), other),
			"evidence scheme":           lateDoc(with("evidenceUrl", "file:///etc/passwd"), other),
			"thread id is a number":     lateDoc(with("threadId", 1), other),
			"grade is a no-break space": lateDoc(with("grade", " "), other),
			"a thread twice":            lateDoc(lateEntry("T1", "answered", lateHead), lateEntry("T1", "backlog", lateHead), other),
			"the member is absent":      map[string]any{"dispositions": []any{lateEntry("T1", "answered", lateHead)}},
			"the member is null":        map[string]any{"lateDispositions": nil},
		} {
			t.Run(name, func(t *testing.T) {
				code, p := lateRestate(t, "--late-dispositions", lateWrite(t, "dispositions.json", document))
				rest := lateRestatement(t, p)
				codes := lateCodes(t, p)
				if code != 2 || rest["current"] != false || !strings.Contains(codes, "malformed_evidence") || !strings.Contains(codes, "late_finding") {
					t.Fatal(code, codes, rest)
				}
				if got := rest["lateDispositions"].([]any); len(got) != 0 {
					t.Fatalf("a rejected document applied entries: %v", got)
				}
			})
		}
	})
}

func TestLateDispositionUsage(t *testing.T) {
	file := lateWrite(t, "dispositions.json", lateDoc(lateEntry("T1", "answered", lateHead)))
	t.Run("the flag grades a restatement and needs one", func(t *testing.T) {
		code, out, _ := lateExec(t, scriptedForge{late: true}, "--late-dispositions", file)
		if code != 4 || !strings.Contains(out, "--restate") {
			t.Fatal(code, out)
		}
	})
	t.Run("an empty value is not given", func(t *testing.T) {
		code, out, stderr := lateExec(t, scriptedForge{}, "--late-dispositions=")
		if code != 0 || strings.Contains(out, "lateDispositions") {
			t.Fatal(code, out, stderr)
		}
	})
	t.Run("a file that cannot be read", func(t *testing.T) {
		code, out, _ := lateExec(t, scriptedForge{late: true}, "--restate", lateRecord(t), "--late-dispositions", filepath.Join(t.TempDir(), "missing.json"))
		if code != 4 || !strings.Contains(out, "could not be read") {
			t.Fatal(code, out)
		}
	})
	t.Run("a file that is not JSON", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dispositions.json")
		if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, _ := lateExec(t, scriptedForge{late: true}, "--restate", lateRecord(t), "--late-dispositions", path)
		if code != 4 || !strings.Contains(out, "not JSON") {
			t.Fatal(code, out)
		}
	})
	t.Run("a JSON document that is not an object", func(t *testing.T) {
		code, out, _ := lateExec(t, scriptedForge{late: true}, "--restate", lateRecord(t), "--late-dispositions", lateWrite(t, "dispositions.json", []any{lateEntry("T1", "answered", lateHead)}))
		if code != 4 || !strings.Contains(out, "JSON object") {
			t.Fatal(code, out)
		}
	})
}

// A duplicate key in the document keeps its last value, as every reader of these files does, and
// that last value is what is validated and echoed.
func TestLateDispositionDuplicateKeys(t *testing.T) {
	entry := func(thread string) string {
		return `"disposition": "answered", "evidenceUrl": "https://github.com/owner/repo/pull/7#d", "head": "` + lateHead + `", "grade": "P2", "threadId": ` + thread
	}
	for name, tc := range map[string]struct {
		raw  string
		code int
	}{
		"a number overwritten by a string":   {`{"lateDispositions": [{"threadId": 1, ` + entry(`"T1"`) + `}, {` + entry(`"T2"`) + `}]}`, 0},
		"a string overwritten by a number":   {`{"lateDispositions": [{"threadId": "T1", ` + entry("1") + `}, {` + entry(`"T2"`) + `}]}`, 2},
		"the member overwritten by a list":   {`{"lateDispositions": "x", "lateDispositions": [{` + entry(`"T1"`) + `}, {` + entry(`"T2"`) + `}]}`, 0},
		"the member overwritten by a string": {`{"lateDispositions": [{` + entry(`"T1"`) + `}, {` + entry(`"T2"`) + `}], "lateDispositions": "x"}`, 2},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dispositions.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			code, p := lateRestate(t, "--late-dispositions", path)
			if code != tc.code {
				t.Fatal(code, p["restatement"])
			}
			if tc.code == 2 && !strings.Contains(lateCodes(t, p), "malformed_evidence") {
				t.Fatal(lateCodes(t, p))
			}
		})
	}
}
