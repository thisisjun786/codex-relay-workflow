package reviewadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeRun is a tool call the test scripts: what it prints, its exit, a start error, and an optional file it writes into the worktree.
type fakeRun struct {
	stdout, stderr string
	exit           int
	err            error
	touch          string
}

func (f fakeRun) run(_ context.Context, _ Profile, req Request) ([]byte, []byte, int, error) {
	if f.touch != "" {
		if err := os.WriteFile(filepath.Join(req.Worktree, f.touch), []byte("changed"), 0o644); err != nil {
			return nil, nil, -1, err
		}
	}
	return []byte(f.stdout), []byte(f.stderr), f.exit, f.err
}

// hashFiles is a fingerprint over the names and contents of the files under dir.
func hashFiles(_ context.Context, dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		h.Write([]byte(rel + "\x00"))
		h.Write(data)
		h.Write([]byte{0})
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

func jsonProfile() Profile {
	return JSONProfile("fake", []string{"fake-tool"}, "grok-4.7-medium", "medium")
}

// The range a request of a ranged profile (the committed mode) names in the tests: the commits 3f2a91c.. and 8be04d7.. the committed
// fixtures name.
const (
	rangeBase = "3f2a91c0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9"
	rangeHead = "8be04d7a1b2c3d4e5f60718293a4b5c6d7e8f901"
)

func request(t *testing.T, p Profile) Request {
	t.Helper()
	req := Request{Profile: p, Worktree: t.TempDir(), Prompt: "review the uncommitted changes", Schema: AnswerSchema, Limit: time.Minute, GuideRev: "guide-r1"}
	if p.Ranged {
		req.Base, req.Head = rangeBase, rangeHead
	}
	return req
}

func check(t *testing.T, p Profile, f fakeRun) Result {
	t.Helper()
	res, err := Adapter{Run: f.run, Fingerprint: hashFiles}.Check(t.Context(), request(t, p))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

func wantState(t *testing.T, res Result, state State) {
	t.Helper()
	if res.State != state {
		t.Fatalf("state = %q (detail %q), want %q", res.State, res.Detail, state)
	}
}

// bugbotEnvelope is the one-line JSON cursor-agent prints, with the given result text (the shape of /scratch/lt/bugbot-smoke.json.raw.json).
func bugbotEnvelope(t *testing.T, result string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "duration_ms": 63347, "result": result, "session_id": "s", "request_id": "r"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

const bugbotTable = "I'll review only the uncommitted changes with Bugbot. First I'm checking how to launch that reviewer.Bugbot found 1 finding in the uncommitted changes.\n\n" +
	"| Severity | Location (file:line) | Finding |\n| --- | --- | --- |\n" +
	"| medium | internal/quote/quote.go:25 | The new `firstByte` helper has an empty `if s == \"\"` branch and then indexes `s[0]`, so an empty string panics with an index out of range. |"

func TestAdapterNormalizesToolOutputToTheSchema(t *testing.T) {
	t.Run("severity words become grades and the fields are kept", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[
			{"severity":"high","file":"a.go","line":1,"text":"first"},
			{"severity":"Medium","file":"b.go","line":2,"text":"second"},
			{"severity":"low","file":"c.go","line":3,"text":"third"},
			{"severity":"P0","file":"d.go","line":0,"text":"fourth"}]}`})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictNeedsChanges {
			t.Fatalf("verdict = %q", res.Verdict)
		}
		want := []Finding{
			{ID: FindingID("a.go", "first"), Severity: "P1", File: "a.go", Line: 1, Text: "first"},
			{ID: FindingID("b.go", "second"), Severity: "P2", File: "b.go", Line: 2, Text: "second"},
			{ID: FindingID("c.go", "third"), Severity: "P3", Impact: "minor_separable", File: "c.go", Line: 3, Text: "third"},
			{ID: FindingID("d.go", "fourth"), Severity: "P0", File: "d.go", Line: 0, Text: "fourth"},
		}
		if !slices.Equal(res.Findings, want) {
			t.Fatalf("findings = %+v\nwant %+v", res.Findings, want)
		}
	})
	t.Run("a verified answer with a minor finding", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"verified","findings":[{"severity":"nit","file":"a.go","line":4,"text":"naming"}]}`})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictVerified || len(res.Findings) != 1 || res.Findings[0].Severity != "P3" {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("findings of one identity collapse into the more severe", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[
			{"severity":"low","file":"a.go","line":10,"text":"The  loop never ends"},
			{"severity":"other","file":"b.go","line":1,"text":"unrelated"},
			{"severity":"high","file":"a.go","line":50,"text":"the loop\nnever ends"}]}`})
		if res.State != StateInvalid {
			t.Fatalf("an unknown severity word: state = %q, want invalid", res.State)
		}
		res = check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[
			{"severity":"low","file":"a.go","line":10,"text":"The  loop never ends","impact":"minor_separable"},
			{"severity":"medium","file":"b.go","line":1,"text":"unrelated"},
			{"severity":"high","file":"a.go","line":50,"text":"the loop\nnever ends"}]}`})
		wantState(t, res, StateOK)
		want := []Finding{
			{ID: FindingID("a.go", "the loop never ends"), Severity: "P1", Impact: "minor_separable", File: "a.go", Line: 10, Text: "The  loop never ends"},
			{ID: FindingID("b.go", "unrelated"), Severity: "P2", File: "b.go", Line: 1, Text: "unrelated"},
		}
		if !slices.Equal(res.Findings, want) {
			t.Fatalf("findings = %+v\nwant %+v", res.Findings, want)
		}
	})
	for name, stdout := range map[string]string{
		"an unknown key":         `{"verdict":"verified","findings":[],"note":"x"}`,
		"an unknown finding key": `{"verdict":"verified","findings":[{"severity":"low","file":"a.go","line":1,"text":"t","why":"x"}]}`,
		"trailing data":          `{"verdict":"verified","findings":[]} {"verdict":"verified","findings":[]}`,
		"not JSON":               `the change looks fine`,
		"another verdict":        `{"verdict":"approved","findings":[]}`,
		"a negative line":        `{"verdict":"verified","findings":[{"severity":"low","file":"a.go","line":-1,"text":"t"}]}`,
	} {
		t.Run("invalid: "+name, func(t *testing.T) {
			res := check(t, jsonProfile(), fakeRun{stdout: stdout})
			wantState(t, res, StateInvalid)
			if res.Verdict != "" || res.Findings != nil || res.Detail == "" {
				t.Fatalf("an invalid answer keeps no judgment: %+v", res)
			}
		})
	}
	t.Run("a Bugbot table", func(t *testing.T) {
		res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, bugbotTable)})
		wantState(t, res, StateOK)
		text := "The new `firstByte` helper has an empty `if s == \"\"` branch and then indexes `s[0]`, so an empty string panics with an index out of range."
		want := []Finding{{ID: FindingID("internal/quote/quote.go", text), Severity: "P2", File: "internal/quote/quote.go", Line: 25, Text: text}}
		if res.Verdict != VerdictNeedsChanges || !slices.Equal(res.Findings, want) {
			t.Fatalf("result = %+v\nwant needs_changes with %+v", res, want)
		}
	})
	t.Run("a Bugbot table of P3 findings with a security one is needs_changes", func(t *testing.T) {
		table := "Bugbot found 2 findings in the uncommitted changes.\n\n" +
			"| Severity | Location (file:line) | Finding |\n| --- | --- | --- |\n" +
			"| low | internal/quote/quote.go:12 | The helper name does not say what it returns. |\n" +
			"| low security | internal/quote/token.go:30 | The token is written to the debug log. |"
		res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, table)})
		wantState(t, res, StateOK)
		want := []Finding{
			{ID: FindingID("internal/quote/quote.go", "The helper name does not say what it returns."), Severity: "P3", Impact: "minor_separable", File: "internal/quote/quote.go", Line: 12, Text: "The helper name does not say what it returns."},
			{ID: FindingID("internal/quote/token.go", "The token is written to the debug log."), Severity: "P3", Impact: "safety_or_data", File: "internal/quote/token.go", Line: 30, Text: "The token is written to the debug log."},
		}
		if res.Verdict != VerdictNeedsChanges || !slices.Equal(res.Findings, want) {
			t.Fatalf("result = %+v\nwant needs_changes with %+v", res, want)
		}
	})
	t.Run("a Bugbot table of plain P3 findings stays verified", func(t *testing.T) {
		table := "Bugbot found 1 finding in the uncommitted changes.\n\n" +
			"| Severity | Location (file:line) | Finding |\n| --- | --- | --- |\n" +
			"| nit | internal/quote/quote.go:12 | The helper name does not say what it returns. |"
		res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, table)})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictVerified || len(res.Findings) != 1 || res.Findings[0].Impact != "minor_separable" {
			t.Fatalf("result = %+v, want verified with one minor_separable finding", res)
		}
	})
	t.Run("Bugbot found no bugs", func(t *testing.T) {
		res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, "I'll review only the uncommitted changes. Checking the Task tool schema, then launching Bugbot on that diff.Bugbot is available, so I'm launching it on the uncommitted diff only.Bugbot found no bugs.")})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictVerified || len(res.Findings) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
	for name, text := range map[string]string{
		"partial coverage":            "Bugbot found no bugs in the 41 uncommitted files it reviewed.\n\nIt left 74 of the 115 changed files unchecked, including `internal/bridge/thread_title.go`. Say if you want a follow-up review of those omitted files.",
		"a removed finding":           "Bugbot reviewed the uncommitted changes and reported 1 finding. The findings validator removed it, so this review was not clean. There are no validated findings to list.",
		"prose of neither shape":      "I could not launch the reviewer.",
		"a table and unchecked files": bugbotTable + "\n\nIt left 3 files unchecked.",
	} {
		t.Run("Bugbot invalid: "+name, func(t *testing.T) {
			wantState(t, check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, text)}), StateInvalid)
		})
	}
	t.Run("Bugbot output that is not an envelope", func(t *testing.T) {
		wantState(t, check(t, BugbotProfile(), fakeRun{stdout: "Bugbot found no bugs."}), StateInvalid)
		wantState(t, check(t, BugbotProfile(), fakeRun{stdout: `{"type":"result"}`}), StateInvalid)
	})
}

func TestAdapterQuotaTextIsQuotaNotNoAnswer(t *testing.T) {
	p := jsonProfile()
	p.QuotaTexts = []string{"quota", "Usage Limit"}
	t.Run("a failed call with quota text on stderr", func(t *testing.T) {
		res := check(t, p, fakeRun{exit: 1, stderr: "error: you have reached your USAGE LIMIT, try again later"})
		wantState(t, res, StateQuota)
		if !strings.Contains(res.Detail, "usage limit") {
			t.Fatalf("detail = %q, want the quota text named", res.Detail)
		}
	})
	t.Run("a blank answer with quota text on stderr", func(t *testing.T) {
		wantState(t, check(t, p, fakeRun{stdout: " \n", stderr: "quota exceeded"}), StateQuota)
	})
	t.Run("a valid answer that quotes the word", func(t *testing.T) {
		res := check(t, p, fakeRun{stdout: `{"verdict":"needs_changes","findings":[{"severity":"high","file":"q.go","line":3,"text":"the quota check is inverted"}]}`, stderr: "warning: quota at 90%"})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictNeedsChanges || len(res.Findings) != 1 {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("a quota text only counts as a whole word", func(t *testing.T) {
		for stderr, state := range map[string]State{
			"HTTP 429 Too Many Requests":  StateQuota,
			"error: status=429":           StateQuota,
			"exit status 1429":            StateToolFailed,
			"request 4290abc failed":      StateToolFailed,
			"please upgrade your plan":    StateQuota,
			"the upgraded client crashed": StateToolFailed,
		} {
			if res := check(t, BugbotProfile(), fakeRun{exit: 1, stderr: stderr}); res.State != state {
				t.Errorf("stderr %q: state = %q, want %q", stderr, res.State, state)
			}
		}
	})
	t.Run("Bugbot: a failed call with the usage limit", func(t *testing.T) {
		wantState(t, check(t, BugbotProfile(), fakeRun{exit: 1, stderr: "Error: You've hit your usage limit."}), StateQuota)
	})
}

func TestAdapterEmptyOutputIsNoAnswer(t *testing.T) {
	for name, c := range map[string]struct {
		p     Profile
		f     fakeRun
		state State
		in    string
	}{
		"whitespace on stdout":             {jsonProfile(), fakeRun{stdout: " \n\t"}, StateNoAnswer, ""},
		"a Bugbot envelope with no text":   {BugbotProfile(), fakeRun{stdout: `{"type":"result","is_error":false,"result":""}`}, StateNoAnswer, ""},
		"a Bugbot envelope with blanks":    {BugbotProfile(), fakeRun{stdout: `{"type":"result","is_error":false,"result":"  \n"}`}, StateNoAnswer, ""},
		"a failed call without quota":      {jsonProfile(), fakeRun{exit: 2, stderr: "panic: boom"}, StateToolFailed, "exit 2"},
		"a run that did not start":         {jsonProfile(), fakeRun{exit: -1, err: fmt.Errorf("%w: exec: \"fake-tool\": executable file not found", ErrNotStarted)}, StateToolFailed, "did not start"},
		"a run that broke after the start": {jsonProfile(), fakeRun{stdout: "{", exit: -1, err: errors.New("the tool wrote more than 8388608 bytes")}, StateToolFailed, "broke after the tool started: the tool wrote more"},
		"a run past the time limit":        {jsonProfile(), fakeRun{exit: -1, err: context.DeadlineExceeded}, StateToolFailed, "time limit"},
	} {
		t.Run(name, func(t *testing.T) {
			res := check(t, c.p, c.f)
			wantState(t, res, c.state)
			if !strings.Contains(res.Detail, c.in) {
				t.Fatalf("detail = %q, want it to name %q", res.Detail, c.in)
			}
			if res.Verdict != "" || res.Findings != nil {
				t.Fatalf("no answer keeps no judgment: %+v", res)
			}
		})
	}
}

func TestAdapterRefusesAnAnswerWhenTheWorktreeChanged(t *testing.T) {
	valid := `{"verdict":"needs_changes","findings":[{"severity":"high","file":"a.go","line":1,"text":"t"}]}`
	for name, f := range map[string]fakeRun{
		"a valid answer":           {stdout: valid, touch: "new.go"},
		"a failed call with quota": {exit: 1, stderr: "usage limit", touch: "new.go"},
		"a blank answer":           {stdout: "", touch: "new.go"},
		"an invalid answer":        {stdout: "nope", touch: "new.go"},
	} {
		t.Run(name, func(t *testing.T) {
			p := jsonProfile()
			p.QuotaTexts = []string{"usage limit"}
			res := check(t, p, f)
			wantState(t, res, StateWorktreeChanged)
			if res.Verdict != "" || res.Findings != nil {
				t.Fatalf("the answer of a call that changed the worktree is refused: %+v", res)
			}
			if res.FingerprintBefore == "" || res.FingerprintAfter == "" || res.FingerprintBefore == res.FingerprintAfter {
				t.Fatalf("fingerprints before %q after %q", res.FingerprintBefore, res.FingerprintAfter)
			}
		})
	}
	t.Run("an unchanged worktree keeps the answer", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: valid})
		wantState(t, res, StateOK)
		if res.FingerprintBefore != res.FingerprintAfter {
			t.Fatalf("fingerprints differ: %+v", res)
		}
	})
}

func TestAdapterRecordsTheEngineFamilyAndGuideRev(t *testing.T) {
	clock := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	now := func() time.Time { clock = clock.Add(750 * time.Millisecond); return clock }
	a := Adapter{Run: fakeRun{stdout: `{"verdict":"verified","findings":[]}`}.run, Fingerprint: hashFiles, Now: now}
	req := request(t, jsonProfile())
	res, err := a.Check(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	wantState(t, res, StateOK)
	sum := sha256.Sum256([]byte(req.Prompt))
	if res.EngineFamily != "grok" || res.Model != "grok-4.7-medium" || res.Effort != "medium" || res.GuideRev != "guide-r1" ||
		res.PromptDigest != hex.EncodeToString(sum[:]) || res.FingerprintBefore == "" || res.FingerprintAfter != res.FingerprintBefore || res.ElapsedMS != 750 {
		t.Fatalf("result = %+v", res)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"state", "detail", "verdict", "findings", "engine_family", "model", "effort", "guide_rev", "prompt_digest", "fingerprint_before", "fingerprint_after", "elapsed_ms", "cache_read_tokens", "commands"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("the record has no %q: %s", k, raw)
		}
	}

	p := jsonProfile()
	p.Family = "xai"
	req.Profile = p
	if res, err = a.Check(t.Context(), req); err != nil || res.EngineFamily != "xai" {
		t.Fatalf("a declared family: %+v %v", res, err)
	}
	if b := BugbotProfile(); b.Model != "grok-4.7-medium" || b.Effort != "medium" || b.Family != "" {
		t.Fatalf("Bugbot profile = %+v", b)
	}

	// A record of a call that gave no answer still names who was asked.
	res, err = Adapter{Run: fakeRun{exit: 1}.run, Fingerprint: hashFiles}.Check(t.Context(), request(t, BugbotProfile()))
	if err != nil || res.State != StateToolFailed || res.EngineFamily != "grok" || res.GuideRev != "guide-r1" || res.FingerprintBefore == "" {
		t.Fatalf("a failed call: %+v %v", res, err)
	}

	for name, mutate := range map[string]func(*Adapter, *Request){
		"an empty guide rev":       func(_ *Adapter, r *Request) { r.GuideRev = "" },
		"an empty effort":          func(_ *Adapter, r *Request) { r.Profile.Effort = "" },
		"an empty model":           func(_ *Adapter, r *Request) { r.Profile.Model = " " },
		"an empty worktree":        func(_ *Adapter, r *Request) { r.Worktree = "" },
		"an empty schema":          func(_ *Adapter, r *Request) { r.Schema = nil },
		"a schema that is not one": func(_ *Adapter, r *Request) { r.Schema = []byte(`{"type":7}`) },
		"no time limit":            func(_ *Adapter, r *Request) { r.Limit = 0 },
		"no answer reader":         func(_ *Adapter, r *Request) { r.Profile.Answer = nil },
		"no run":                   func(a *Adapter, _ *Request) { a.Run = nil },
		"no fingerprint":           func(a *Adapter, _ *Request) { a.Fingerprint = nil },
	} {
		t.Run(name, func(t *testing.T) {
			ran := false
			a := Adapter{Run: func(context.Context, Profile, Request) ([]byte, []byte, int, error) {
				ran = true
				return nil, nil, 0, nil
			}, Fingerprint: hashFiles}
			r := request(t, jsonProfile())
			mutate(&a, &r)
			if _, err := a.Check(t.Context(), r); err == nil {
				t.Fatal("Check accepted the request")
			}
			if ran {
				t.Fatal("the tool ran for a request Check refuses")
			}
		})
	}
}

func TestFindingIdentityIgnoresLineNumbers(t *testing.T) {
	id := FindingID("a.go", "The loop never ends")
	if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("id %q is not 16 hex", id)
	}
	for _, same := range []string{"the loop never ends", "  The\tloop\n never   ENDS "} {
		if FindingID("a.go", same) != id {
			t.Fatalf("%q has another id", same)
		}
	}
	if FindingID("b.go", "The loop never ends") == id || FindingID("a.go", "The loop ends") == id {
		t.Fatal("another file or text has the same id")
	}
	at := func(line int) Result {
		return check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[{"severity":"high","file":"a.go","line":` + fmt.Sprint(line) + `,"text":"The loop never ends"}]}`})
	}
	r10, r50 := at(10), at(50)
	if len(r10.Findings) != 1 || len(r50.Findings) != 1 || r10.Findings[0].ID != id || r50.Findings[0].ID != id || r10.Findings[0].Line != 10 || r50.Findings[0].Line != 50 {
		t.Fatalf("line 10: %+v; line 50: %+v", r10.Findings, r50.Findings)
	}
}

// TestEvidenceMissingFindingKeepsItsQuestion (CRW-1490): a finding of the kind evidence_missing carries the evidence question the
// check wants answered ({question, files_needed}), and the adapter's record keeps both: the lane asks the question of an explorer
// and checks the head again instead of sending the finding to a fixer. The metadata is part of the answer a tool may give, not of
// the judgment, so it survives on the Finding the record hands the lane. Two findings of one id collapse as ever - but a copy that
// is an ordinary finding wins the kind over the evidence_missing one: a merged finding is never evidence-only because one copy of
// it was.
func TestEvidenceMissingFindingKeepsItsQuestion(t *testing.T) {
	const missing = `{"severity":"P2","impact":"criterion_unmet","file":"node-A-1.txt","line":1,"text":"the verification evidence is missing","kind":"evidence_missing","evidence_question":{"question":"Which command proves the node behavior?","files_needed":["node-A-1.txt"]}}`
	t.Run("the metadata survives the record", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[` + missing + `]}`})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictNeedsChanges || len(res.Findings) != 1 {
			t.Fatalf("result = %+v", res)
		}
		raw, err := json.Marshal(res.Findings[0])
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m["kind"] != "evidence_missing" {
			t.Fatalf("the finding's kind did not survive: %s", raw)
		}
		eq, _ := m["evidence_question"].(map[string]any)
		files, _ := eq["files_needed"].([]any)
		if eq["question"] != "Which command proves the node behavior?" || len(files) != 1 || files[0] != "node-A-1.txt" {
			t.Fatalf("the evidence question did not survive: %s", raw)
		}
	})
	t.Run("a same-id ordinary finding is not evidence-only", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[` + missing +
			`,{"severity":"P1","impact":"core_function","file":"node-A-1.txt","line":9,"text":"the verification evidence is missing"}]}`})
		wantState(t, res, StateOK)
		if len(res.Findings) != 1 || res.Findings[0].Severity != "P1" {
			t.Fatalf("findings = %+v, want the merged finding at the more severe grade", res.Findings)
		}
		raw, err := json.Marshal(res.Findings[0])
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m["kind"] == "evidence_missing" {
			t.Fatalf("the merged finding is evidence-only because one copy of it was: %s", raw)
		}
	})
	t.Run("an ordinary finding first stays ordinary when an evidence copy follows", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[
			{"severity":"P1","impact":"core_function","file":"node-A-1.txt","line":9,"text":"the verification evidence is missing"},` + missing + `]}`})
		wantState(t, res, StateOK)
		if len(res.Findings) != 1 {
			t.Fatalf("findings = %+v", res.Findings)
		}
		raw, err := json.Marshal(res.Findings[0])
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m["kind"] == "evidence_missing" {
			t.Fatalf("the merged finding took the evidence kind of the later copy: %s", raw)
		}
	})
	t.Run("a null evidence question is no question", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[
			{"severity":"P2","impact":"core_function","file":"a.go","line":1,"text":"t","evidence_question":null}]}`})
		wantState(t, res, StateOK)
		if len(res.Findings) != 1 || res.Findings[0].EvidenceQuestion != nil {
			t.Fatalf("findings = %+v", res.Findings)
		}
	})
	for name, finding := range map[string]string{
		"evidence_missing without a question":    `{"severity":"P2","file":"a.go","line":1,"text":"t","kind":"evidence_missing"}`,
		"evidence_missing with a blank question": `{"severity":"P2","file":"a.go","line":1,"text":"t","kind":"evidence_missing","evidence_question":{"question":"  ","files_needed":["a.go"]}}`,
		"a question on an ordinary finding":      `{"severity":"P2","file":"a.go","line":1,"text":"t","kind":"correctness","evidence_question":{"question":"q","files_needed":[]}}`,
		"a question on a finding of no kind":     `{"severity":"P2","file":"a.go","line":1,"text":"t","evidence_question":{"question":"q","files_needed":[]}}`,
	} {
		t.Run("invalid: "+name, func(t *testing.T) {
			res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[` + finding + `]}`})
			wantState(t, res, StateInvalid)
		})
	}
}

func TestExecRunRunsInTheWorktreeAndKillsOnTheLimit(t *testing.T) {
	dir := t.TempDir()
	p := JSONProfile("sh", []string{"/bin/sh", "-c", `pwd; printf '%s' "$1"; echo oops >&2; exit 3`, "sh"}, "m", "e")
	stdout, stderr, exit, err := ExecRun(t.Context(), p, Request{Worktree: dir, Prompt: "the prompt"})
	if err != nil || exit != 3 || string(stderr) != "oops\n" {
		t.Fatalf("exit %d err %v stderr %q", exit, err, stderr)
	}
	real, _ := filepath.EvalSymlinks(dir)
	if got := string(stdout); got != real+"\nthe prompt" && got != dir+"\nthe prompt" {
		t.Fatalf("stdout = %q", got)
	}

	p = JSONProfile("sleep", []string{"/bin/sh", "-c", "sleep 30 & sleep 30"}, "m", "e")
	start := time.Now()
	res, err := Adapter{Run: ExecRun, Fingerprint: hashFiles}.Check(t.Context(), Request{Profile: p, Worktree: dir, Prompt: "x", Schema: AnswerSchema, Limit: 300 * time.Millisecond, GuideRev: "g"})
	if err != nil {
		t.Fatal(err)
	}
	wantState(t, res, StateToolFailed)
	if !strings.Contains(res.Detail, "time limit") || time.Since(start) > 10*time.Second {
		t.Fatalf("detail %q after %s", res.Detail, time.Since(start))
	}

	_, _, _, err = ExecRun(t.Context(), JSONProfile("none", nil, "m", "e"), Request{Worktree: dir})
	if !errors.Is(err, ErrNotStarted) {
		t.Fatalf("a profile without argv: %v", err)
	}
	_, _, _, err = ExecRun(t.Context(), JSONProfile("missing", []string{filepath.Join(dir, "no-such-tool")}, "m", "e"), Request{Worktree: dir})
	if !errors.Is(err, ErrNotStarted) {
		t.Fatalf("a missing binary: %v", err)
	}
}

// TestVerifiedWithABlockingImpactIsVerdictInvalid pins the adapter to the one blocking predicate of the flow (premerge.Blocks,
// CRW-1438): an impact outside the closed set is invalid, a security finding without an impact is safety_or_data, and a verified
// answer over any blocking finding is invalid, the adapter's form of verdict_invalid.
func TestVerifiedWithABlockingImpactIsVerdictInvalid(t *testing.T) {
	cases := []struct {
		name, verdict, severity, impact string
		state                           State
		wantImpact                      string
	}{
		{"verified over a P3 core_function finding", VerdictVerified, "low", "core_function", StateInvalid, ""},
		{"verified over a P2 finding without impact", VerdictVerified, "medium", "", StateInvalid, ""},
		{"verified over a P3 minor_separable finding", VerdictVerified, "low", "minor_separable", StateOK, "minor_separable"},
		{"needs_changes over a P3 core_function finding", VerdictNeedsChanges, "low", "core_function", StateOK, "core_function"},
		{"an impact outside the closed set", VerdictNeedsChanges, "low", "cosmetic", StateInvalid, ""},
		{"verified over a security P3 finding without impact", VerdictVerified, "low security", "", StateInvalid, ""},
		{"needs_changes over a security P3 finding without impact", VerdictNeedsChanges, "low security", "", StateOK, "safety_or_data"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			finding := map[string]any{"severity": c.severity, "file": "a.go", "line": 7, "text": "the finding"}
			if c.impact != "" {
				finding["impact"] = c.impact
			}
			answer, err := json.Marshal(map[string]any{"verdict": c.verdict, "findings": []any{finding}})
			if err != nil {
				t.Fatal(err)
			}
			res := check(t, jsonProfile(), fakeRun{stdout: string(answer)})
			wantState(t, res, c.state)
			id := FindingID("a.go", "the finding")
			if c.state == StateInvalid {
				if res.Verdict != "" || len(res.Findings) != 0 {
					t.Fatalf("an invalid record kept a judgment: %+v", res)
				}
				if c.verdict == VerdictVerified && c.impact != "cosmetic" && !strings.Contains(res.Detail, id) {
					t.Fatalf("detail %q does not name the finding %s", res.Detail, id)
				}
				if c.impact == "cosmetic" && !strings.Contains(res.Detail, "cosmetic") {
					t.Fatalf("detail %q does not name the impact", res.Detail)
				}
				return
			}
			if res.Verdict != c.verdict || len(res.Findings) != 1 || res.Findings[0].Impact != c.wantImpact || res.Findings[0].ID != id {
				t.Fatalf("result = %+v, want verdict %s and one finding of impact %q", res, c.verdict, c.wantImpact)
			}
		})
	}
	t.Run("a collapsed security duplicate makes the kept finding safety_or_data", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"verified","findings":[
			{"severity":"low","file":"a.go","line":1,"text":"leak"},
			{"severity":"low vuln","file":"a.go","line":9,"text":"leak"}]}`})
		wantState(t, res, StateInvalid)
		if !strings.Contains(res.Detail, FindingID("a.go", "leak")) {
			t.Fatalf("detail %q does not name the finding", res.Detail)
		}
	})
	t.Run("a blocking impact of a duplicate is not hidden by an earlier minor_separable", func(t *testing.T) {
		res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"verified","findings":[
			{"severity":"low","impact":"minor_separable","file":"a.go","line":1,"text":"leak"},
			{"severity":"low security","file":"a.go","line":9,"text":"leak"}]}`})
		wantState(t, res, StateInvalid)
		res = check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"needs_changes","findings":[
			{"severity":"low","impact":"minor_separable","file":"a.go","line":1,"text":"leak"},
			{"severity":"low","impact":"core_function","file":"a.go","line":9,"text":"leak"}]}`})
		wantState(t, res, StateOK)
		if len(res.Findings) != 1 || res.Findings[0].Impact != "core_function" || res.Findings[0].Line != 1 {
			t.Fatalf("findings = %+v, want one at line 1 of impact core_function", res.Findings)
		}
	})
}

// TestAdapterRefusesASecondJSONValue: an answer is exactly one JSON value; a second value or trailing data after the first,
// whatever its shape, makes the answer invalid, in the JSON profile and in Bugbot's envelope.
func TestAdapterRefusesASecondJSONValue(t *testing.T) {
	for name, stdout := range map[string]string{
		"a second answer":           `{"verdict":"verified","findings":[]} {"verdict":"needs_changes","findings":[]}`,
		"a closing brace":           `{"verdict":"verified","findings":[]}}`,
		"a closing bracket":         "{\"verdict\":\"verified\",\"findings\":[]}\n]",
		"a second value on a line":  "{\"verdict\":\"verified\",\"findings\":[]}\n\"more\"\n",
		"trailing words after JSON": `{"verdict":"verified","findings":[]} done`,
	} {
		t.Run("JSON: "+name, func(t *testing.T) {
			res := check(t, jsonProfile(), fakeRun{stdout: stdout})
			wantState(t, res, StateInvalid)
			if res.Verdict != "" || res.Findings != nil {
				t.Fatalf("an invalid answer keeps no judgment: %+v", res)
			}
		})
	}
	t.Run("JSON: one answer and trailing whitespace is ok", func(t *testing.T) {
		wantState(t, check(t, jsonProfile(), fakeRun{stdout: "{\"verdict\":\"verified\",\"findings\":[]}\n \t\n"}), StateOK)
	})
	env := strings.TrimSpace(bugbotEnvelope(t, "Bugbot found no bugs."))
	for name, stdout := range map[string]string{
		"a second envelope": env + "\n" + env + "\n",
		"a closing brace":   env + "}\n",
		"a closing bracket": env + "]",
	} {
		t.Run("Bugbot: "+name, func(t *testing.T) {
			wantState(t, check(t, BugbotProfile(), fakeRun{stdout: stdout}), StateInvalid)
		})
	}
}

// TestBugbotErrorEnvelopeIsQuotaOrToolFailed: cursor-agent can exit 0 with an envelope whose is_error is set. Its text is the
// tool's own report: a quota text there is quota, any other text is the tool's failure, never an invalid answer.
func TestBugbotErrorEnvelopeIsQuotaOrToolFailed(t *testing.T) {
	envelope := func(result string) string {
		b, err := json.Marshal(map[string]any{"type": "result", "subtype": "error", "is_error": true, "result": result})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	t.Run("a usage limit", func(t *testing.T) {
		res := check(t, BugbotProfile(), fakeRun{stdout: envelope("You've hit your usage limit. Upgrade to continue.")})
		wantState(t, res, StateQuota)
		if !strings.Contains(res.Detail, "usage limit") {
			t.Fatalf("detail = %q, want the quota text named", res.Detail)
		}
	})
	t.Run("another error", func(t *testing.T) {
		res := check(t, BugbotProfile(), fakeRun{stdout: envelope("Connection to the model server was lost.")})
		wantState(t, res, StateToolFailed)
		if !strings.Contains(res.Detail, "Connection to the model server was lost.") {
			t.Fatalf("detail = %q, want the tool's report", res.Detail)
		}
		if res.Verdict != "" || res.Findings != nil {
			t.Fatalf("a failed call keeps no judgment: %+v", res)
		}
	})
	t.Run("an error with no text", func(t *testing.T) {
		wantState(t, check(t, BugbotProfile(), fakeRun{stdout: envelope("")}), StateToolFailed)
	})
}

// TestExecRunKillsLeftoverChildrenAfterTheToolExits: a tool that exits 0 after starting a background child that keeps its stdout
// open gives its answer; the child is killed with the group instead of holding the call until the streams time out.
func TestExecRunKillsLeftoverChildrenAfterTheToolExits(t *testing.T) {
	dir := t.TempDir()
	script := `sleep 30 & printf '%s' '{"verdict":"verified","findings":[]}'; exit 0`
	p := JSONProfile("bg", []string{"/bin/sh", "-c", script, "sh"}, "m", "e")
	start := time.Now()
	res, err := Adapter{Run: ExecRun, Fingerprint: hashFiles}.Check(t.Context(), Request{Profile: p, Worktree: dir, Prompt: "x", Schema: AnswerSchema, Limit: 20 * time.Second, GuideRev: "g"})
	if err != nil {
		t.Fatal(err)
	}
	wantState(t, res, StateOK)
	if res.Verdict != VerdictVerified {
		t.Fatalf("result = %+v", res)
	}
	if took := time.Since(start); took >= killGrace {
		t.Fatalf("the call waited %s for the leftover child", took)
	}

	t.Run("a stream held outside the group ends the call after the grace", func(t *testing.T) {
		// setsid moves the child out of the tool's group, so the group kill misses it and it holds stdout for 5s; the tool exits
		// only once the child has left the group (it wrote ready).
		script := `setsid sh -c ': > ready; exec sleep 5' & while [ ! -e ready ]; do sleep 0.01; done; printf '%s' '{"verdict":"verified","findings":[]}'; exit 0`
		p := JSONProfile("escaped", []string{"/bin/sh", "-c", script, "sh"}, "m", "e")
		start := time.Now()
		stdout, _, exit, err := ExecRun(t.Context(), p, Request{Worktree: t.TempDir(), Prompt: "x"})
		took := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "stayed open") || exit != -1 || took < killGrace || took > 4*time.Second {
			t.Fatalf("exit %d err %v after %s, want the streams error after the grace", exit, err, took)
		}
		if string(stdout) != `{"verdict":"verified","findings":[]}` {
			t.Fatalf("stdout = %q, want what the tool wrote", stdout)
		}
	})
}

// bugbotNoBugsFixture is a real cursor-agent envelope of a clean Bugbot review
// (the bugbot-U1a-r2.json.raw.json evidence of the b1 lane, the ids and usage trimmed).
const bugbotNoBugsFixture = `{"type":"result","subtype":"success","is_error":false,"duration_ms":289515,"duration_api_ms":289515,"result":"I'll review only the uncommitted changes. Checking the review tool schema, then starting the Bugbot review.Bugbot found no bugs."}` + "\n"

// bugbotNoBugsScopeFixture is a real cursor-agent envelope of a clean Bugbot review that names its scope after the sentence
// (/state/crw/crw-lane-n1-relay-layer-words/evidence/bugbot-n1-3a-r1.json.raw.json, the ids and usage trimmed).
const bugbotNoBugsScopeFixture = `{"type":"result","subtype":"success","is_error":false,"duration_ms":199008,"duration_api_ms":199008,"result":"I'll review only the uncommitted changes with Bugbot. Checking the Task tool schema, then launching the reviewer.Bugbot found no bugs in the uncommitted changes."}` + "\n"

// TestBugbotNoBugsMustBeAffirmative: only prose whose last sentence says that Bugbot found no bugs is a clean review; prose that
// merely contains the words, negated or hedged, is not an answer.
func TestBugbotNoBugsMustBeAffirmative(t *testing.T) {
	t.Run("a real clean review", func(t *testing.T) {
		res := check(t, BugbotProfile(), fakeRun{stdout: bugbotNoBugsFixture})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictVerified || len(res.Findings) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
	for _, text := range []string{
		"Bugbot found no bugs",
		"Bugbot found no bugs!",
		"Launching the reviewer.\n\nBugbot found no bugs.\n",
		"I'll review the diff.  Bugbot found no bugs.",
	} {
		t.Run("affirmative: "+text, func(t *testing.T) {
			res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, text)})
			wantState(t, res, StateOK)
			if res.Verdict != VerdictVerified {
				t.Fatalf("result = %+v", res)
			}
		})
	}
	for _, text := range []string{
		"I could not determine whether Bugbot found no bugs.",
		"I could not determine whether Bugbot found no bugs",
		"It is unclear if Bugbot found no bugs; the run was cut short.",
		"Bugbot found no bugs.\n\nThe review then crashed before it covered the tests.",
		"Bugbot found no bugs? I could not confirm it.",
		"Bugbot never said it found no bugs.",
	} {
		t.Run("not an answer: "+text, func(t *testing.T) {
			res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, text)})
			wantState(t, res, StateInvalid)
		})
	}
}

// TestBugbotReadsEveryFindingsTable: findings in a second table of the same header are findings, so a blocking one there makes
// the review needs_changes.
func TestBugbotReadsEveryFindingsTable(t *testing.T) {
	header := "| Severity | Location (file:line) | Finding |\n| --- | --- | --- |\n"
	text := "Bugbot found 2 findings.\n\n" + header +
		"| low | a.go:3 | The name is unclear. |\n\nA second pass found more.\n\n" + header +
		"| high | b.go:9 | The loop never ends. |"
	res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, text)})
	wantState(t, res, StateOK)
	want := []Finding{
		{ID: FindingID("a.go", "The name is unclear."), Severity: "P3", Impact: "minor_separable", File: "a.go", Line: 3, Text: "The name is unclear."},
		{ID: FindingID("b.go", "The loop never ends."), Severity: "P1", File: "b.go", Line: 9, Text: "The loop never ends."},
	}
	if res.Verdict != VerdictNeedsChanges || !slices.Equal(res.Findings, want) {
		t.Fatalf("result = %+v\nwant needs_changes with %+v", res, want)
	}
	t.Run("an empty second table is an error", func(t *testing.T) {
		bad := "Bugbot found 1 finding.\n\n" + header + "| low | a.go:3 | The name is unclear. |\n\nMore:\n\n" + header
		wantState(t, check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, bad)}), StateInvalid)
	})
}

// TestBugbotRefusesEveryPartialReview: prose outside the findings tables that says the review covered only part of the change
// (skipped files, files it did not or could not review, a partial or truncated review, a change too large, a review that timed
// out) makes the result no answer, even when the tables hold only P3 findings that would be verified. The same words inside a
// finding of a table are finding text and leave a complete review as it is.
func TestBugbotRefusesEveryPartialReview(t *testing.T) {
	p3 := "Bugbot found 1 finding in the uncommitted changes.\n\n" +
		"| Severity | Location (file:line) | Finding |\n| --- | --- | --- |\n" +
		"| low | internal/quote/quote.go:12 | The helper name does not say what it returns. |"
	for _, tail := range []string{
		"It skipped 74 changed files.",
		"It Skipped 3 files that were too large.",
		"Bugbot skipped the generated files.",
		"I did not review internal/bridge.",
		"I didn't review the tests.",
		"Bugbot could not review 12 of the files.",
		"It couldn't review the vendored tree.",
		"It only reviewed the first 40 files.",
		"This is a partial review of the change.",
		"The review was partial.",
		"The diff was truncated before the review.",
		"The change is too large to review in one pass.",
		"The reviewer timed out before it reached the tests.",
		"It left 41 of the 115 changed files out of the review.",
		"Some files were not reviewed.",
		"The review is incomplete.",
	} {
		t.Run(tail, func(t *testing.T) {
			res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, p3+"\n\n"+tail)})
			wantState(t, res, StateInvalid)
			if !strings.Contains(res.Detail, "not whole") {
				t.Fatalf("detail = %q, want the partial review named", res.Detail)
			}
			res = check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, tail+"\n\nBugbot found no bugs.")})
			wantState(t, res, StateInvalid)
		})
	}
	t.Run("the words inside a finding are finding text", func(t *testing.T) {
		text := "Bugbot found 1 finding in the uncommitted changes.\n\n" +
			"| Severity | Location (file:line) | Finding |\n| --- | --- | --- |\n" +
			"| low | internal/quote/quote.go:12 | The loop skipped 3 files and did not review the rest: a partial, truncated read that timed out before it was too large. |"
		res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, text)})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictVerified || len(res.Findings) != 1 {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("a real complete review reads the same", func(t *testing.T) {
		wantState(t, check(t, BugbotProfile(), fakeRun{stdout: bugbotNoBugsFixture}), StateOK)
		wantState(t, check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, bugbotTable)}), StateOK)
	})
}

// TestExecRunStopsAToolAtTheOutputCap: a tool that writes past the output cap and would go on forever is stopped at the first
// overflow, well before the time limit, and the call is the output-cap failure, not a timeout.
func TestExecRunStopsAToolAtTheOutputCap(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			script := `while :; do cat /dev/zero; done`
			if stream == "stderr" {
				script = `while :; do cat /dev/zero >&2; done`
			}
			p := JSONProfile("runaway", []string{"/bin/sh", "-c", script, "sh"}, "m", "e")
			limit := 20 * time.Second
			ctx, cancel := context.WithTimeout(t.Context(), limit)
			defer cancel()
			start := time.Now()
			_, _, exit, err := ExecRun(ctx, p, Request{Worktree: t.TempDir(), Prompt: "x"})
			if took := time.Since(start); !errors.Is(err, ErrOutputCap) || exit != -1 || took > 5*time.Second {
				t.Fatalf("exit %d err %v after %s, want the output-cap failure at once", exit, err, took)
			}
			start = time.Now()
			res, err := Adapter{Run: ExecRun, Fingerprint: hashFiles}.Check(t.Context(), Request{Profile: p, Worktree: t.TempDir(), Prompt: "x", Schema: AnswerSchema, Limit: limit, GuideRev: "g"})
			if err != nil {
				t.Fatal(err)
			}
			wantState(t, res, StateToolFailed)
			if took := time.Since(start); !strings.Contains(res.Detail, "output cap") || strings.Contains(res.Detail, "time limit") || took > 5*time.Second {
				t.Fatalf("detail %q after %s, want the output-cap failure well before the limit of %s", res.Detail, took, limit)
			}
		})
	}
}

// TestAdapterRecordsACanonicalEngineFamily: a declared family is recorded trimmed and in lower case, the form execution.ModelFamily
// gives an author's family; a declared family that is blank after the trim or is no family name (a provider prefix, a dash, a
// space), and a model whose family is blank, are a request Check refuses before the tool runs.
func TestAdapterRecordsACanonicalEngineFamily(t *testing.T) {
	a := Adapter{Run: fakeRun{stdout: `{"verdict":"verified","findings":[]}`}.run, Fingerprint: hashFiles}
	p := jsonProfile()
	p.Family = " Claude "
	res, err := a.Check(t.Context(), request(t, p))
	if err != nil || res.EngineFamily != "claude" {
		t.Fatalf("a declared ' Claude ': %+v %v", res, err)
	}
	p = jsonProfile()
	p.Model = "Anthropic/Claude-Opus-5"
	if res, err = a.Check(t.Context(), request(t, p)); err != nil || res.EngineFamily != "claude" {
		t.Fatalf("the family of a model: %+v %v", res, err)
	}
	for name, edit := range map[string]func(*Profile){
		"a blank family":         func(p *Profile) { p.Family = " \t" },
		"a prefixed family":      func(p *Profile) { p.Family = "anthropic/claude" },
		"a model id as a family": func(p *Profile) { p.Family = "claude-opus" },
		"a family with a space":  func(p *Profile) { p.Family = "cla ude" },
		"a model of no family":   func(p *Profile) { p.Model = "-opus" },
	} {
		t.Run(name, func(t *testing.T) {
			ran := false
			a := Adapter{Run: func(context.Context, Profile, Request) ([]byte, []byte, int, error) {
				ran = true
				return nil, nil, 0, nil
			}, Fingerprint: hashFiles}
			p := jsonProfile()
			edit(&p)
			if res, err := a.Check(t.Context(), request(t, p)); err == nil || ran {
				t.Fatalf("Check took the request (ran %v): %+v %v", ran, res, err)
			}
		})
	}
}

// TestBugbotNoBugsTakesTheScopePhrase: the affirmative sentence followed only by a short scope phrase that names the scope the
// request asks for, all uncommitted changes of the worktree ("in the uncommitted changes", "on the diff", "across the working
// tree"), is a clean review; a phrase that names another or a narrower scope (staged, committed, changed or modified files, a
// number of files, "the files it could open"), more prose after it, a negated or hedged sentence, and a partial-review word stay
// refused.
func TestBugbotNoBugsTakesTheScopePhrase(t *testing.T) {
	t.Run("the real clean review with a scope phrase", func(t *testing.T) {
		res := check(t, BugbotProfile(), fakeRun{stdout: bugbotNoBugsScopeFixture})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictVerified || len(res.Findings) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
	for _, text := range []string{
		"Launching the reviewer.Bugbot found no bugs in the uncommitted changes.",
		"Bugbot found no bugs in uncommitted changes",
		"Bugbot found no bugs on the diff!",
		"Bugbot found no bugs across the working tree.",
		"Bugbot found no bugs in the working tree.",
		"Bugbot found no bugs for the uncommitted files.",
		"Bugbot found no bugs on the working-tree diff.",
		"Bugbot found no bugs in the working tree code.",
		"BUGBOT FOUND NO BUGS IN THE UNCOMMITTED CHANGES.",
		"Bugbot found no bugs in the code.",
	} {
		t.Run("affirmative: "+text, func(t *testing.T) {
			res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, text)})
			wantState(t, res, StateOK)
			if res.Verdict != VerdictVerified {
				t.Fatalf("result = %+v", res)
			}
		})
	}
	for _, text := range []string{
		"Bugbot found no bugs in the files it could open.",
		"Bugbot found no bugs for the staged files.",
		"Bugbot found no bugs in the committed changes.",
		"Bugbot found no bugs in the changed files.",
		"Bugbot found no bugs in the modified files.",
		"Bugbot found no bugs in the staged changes.",
		"Bugbot found no bugs in the first 40 files.",
		"Bugbot found no bugs in 41 files.",
		"Bugbot found no bugs in the uncommitted changes it reviewed.",
		"Bugbot found no bugs in the uncommitted changes, but the run was cut off.",
		"Bugbot found no bugs in the uncommitted changes.\n\nThe review then crashed before it covered the tests.",
		"Bugbot found no bugs in the uncommitted changes?",
		"I could not determine whether Bugbot found no bugs in the uncommitted changes.",
		"It skipped 74 changed files.Bugbot found no bugs in the uncommitted changes.",
		"Bugbot found no bugs in the 41 uncommitted files it reviewed.",
		"Bugbot found no bugs in the diff of the first 40 files.",
	} {
		t.Run("not an answer: "+text, func(t *testing.T) {
			wantState(t, check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, text)}), StateInvalid)
		})
	}
}

// TestBugbotCommittedProfileReadsTheRangeScope (CRW-1468): the committed-mode profile is the same Bugbot call as BugbotProfile, but
// its clean-answer reader takes the scope phrases of a committed range that name the requested range ("in the changes between <base>
// and <head>", "in <base>..<head>") and refuses the uncommitted, staged or working-tree scope, in the scope phrase or anywhere in the
// prose outside the tables (a range check that reviewed the working tree reviewed the clean checkout, not the range), and a clean
// sentence that names no range; BugbotProfile keeps refusing "committed"; a findings table reads the same in both. A range named by
// symbolic refs (HEAD~1..HEAD, a branch) is refused: the request names its range by commit ids.
func TestBugbotCommittedProfileReadsTheRangeScope(t *testing.T) {
	committed, uncommitted := BugbotCommittedProfile(), BugbotProfile()
	if committed.Name == uncommitted.Name || !slices.Equal(committed.Argv, uncommitted.Argv) || committed.Model != uncommitted.Model ||
		committed.Effort != uncommitted.Effort || committed.Family != uncommitted.Family || !slices.Equal(committed.QuotaTexts, uncommitted.QuotaTexts) || committed.Answer == nil ||
		!committed.Ranged || uncommitted.Ranged {
		t.Fatalf("the committed profile %+v is not the Bugbot call of %+v under its own name", committed, uncommitted)
	}
	for _, text := range []string{
		"Bugbot found no bugs in the changes between 3f2a91c and 8be04d7.",
		"Bugbot found no bugs in the changes between 3f2a91c0d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9 and 8be04d7.",
		"Bugbot found no bugs in the changes from 3f2a91c to 8be04d7!",
		"Bugbot found no bugs in the diff between `3f2a91c` and `8be04d7`.",
		"Bugbot found no bugs in the commits between 3f2a91c and 8be04d7.",
		"Bugbot found no bugs in 3f2a91c..8be04d7.",
		"Bugbot found no bugs in `3f2a91c..8be04d7`.",
		"Bugbot found no bugs in the range 3f2a91c..8be04d7.",
		"Bugbot found no bugs in the diff 3f2a91c..8be04d7.",
		"Bugbot found no bugs in the changes between 3f2a91c and 8be04d7 (git diff 3f2a91c..8be04d7).",
		"BUGBOT FOUND NO BUGS IN THE CHANGES BETWEEN 3F2A91C AND 8BE04D7.",
	} {
		t.Run("committed clean: "+text, func(t *testing.T) {
			res := check(t, committed, fakeRun{stdout: bugbotEnvelope(t, text)})
			wantState(t, res, StateOK)
			if res.Verdict != VerdictVerified || len(res.Findings) != 0 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
	for _, text := range []string{
		// a clean sentence that names no range of commits (or "the base" and "the head", which name none) ties the answer to no range:
		// it may be about any (TestBugbotCommittedReaderTakesOnlyTheRequestedRange)
		"Bugbot found no bugs.",
		"Bugbot found no bugs in the changes between the base and the head.",
		"Bugbot found no bugs in the committed changes.",
		"I'll review the changes between 3f2a91c and 8be04d7.Bugbot found no bugs in the committed changes.",
		"Bugbot found no bugs on the diff.",
		"Bugbot found no bugs in the uncommitted changes.",
		"Bugbot found no bugs in the working tree.",
		"Bugbot found no bugs on the working-tree diff.",
		"Bugbot found no bugs in the working tree code.",
		"Bugbot found no bugs for the uncommitted files.",
		"I'll review only the uncommitted changes. Checking the review tool schema, then starting the Bugbot review.Bugbot found no bugs.",
		"I'll review the working tree.Bugbot found no bugs in the committed changes.",
		"Bugbot found no bugs in the staged changes.",
		"I reviewed the staged changes.Bugbot found no bugs.",
		"I reviewed the unstaged changes.Bugbot found no bugs in the committed changes.",
		"Nothing is in the staging area.Bugbot found no bugs.",
		"Bugbot found no bugs in the changes between 3f2a91c and 8be04d7 it reviewed.",
		"Bugbot found no bugs in the changes between 3f2a91c and the files it could open.",
		"Bugbot found no bugs in the first 40 files.",
		"Bugbot found no bugs in HEAD..HEAD.",
		"Bugbot found no bugs in the changes between HEAD~1 and HEAD.",
		"Bugbot found no bugs in the changes between main and 8be04d7.",
		"Bugbot found no bugs in origin/main..8be04d7.",
		"Bugbot found no bugs in the changes between 3f2a91 and 8be04d7.",
		"Bugbot found no bugs in the changes between 3f2a91c and 8be04d7?",
		"It skipped 74 changed files.Bugbot found no bugs in the changes between 3f2a91c and 8be04d7.",
		"Bugbot found no bugs in the changes between 3f2a91c and 8be04d7.\n\nThe review then crashed before it covered the tests.",
	} {
		t.Run("committed not an answer: "+text, func(t *testing.T) {
			wantState(t, check(t, committed, fakeRun{stdout: bugbotEnvelope(t, text)}), StateInvalid)
		})
	}
	t.Run("the real uncommitted clean reviews are no committed answer", func(t *testing.T) {
		wantState(t, check(t, committed, fakeRun{stdout: bugbotNoBugsFixture}), StateInvalid)
		wantState(t, check(t, committed, fakeRun{stdout: bugbotNoBugsScopeFixture}), StateInvalid)
		wantState(t, check(t, uncommitted, fakeRun{stdout: bugbotNoBugsFixture}), StateOK)
		wantState(t, check(t, uncommitted, fakeRun{stdout: bugbotNoBugsScopeFixture}), StateOK)
	})
	for _, text := range []string{
		"Bugbot found no bugs in the committed changes.",
		"Bugbot found no bugs in the changes between 3f2a91c and 8be04d7.",
		"Bugbot found no bugs in 3f2a91c..8be04d7.",
	} {
		t.Run("uncommitted not an answer: "+text, func(t *testing.T) {
			wantState(t, check(t, uncommitted, fakeRun{stdout: bugbotEnvelope(t, text)}), StateInvalid)
		})
	}
	t.Run("a findings table reads the same in both", func(t *testing.T) {
		// the committed answer names the range it was asked about; the tables are the same
		table := "\n\n| Severity | Location (file:line) | Finding |\n| --- | --- | --- |\n" +
			"| medium | internal/quote/quote.go:25 | An empty string panics with an index out of range. |\n" +
			"| low | internal/quote/quote.go:12 | The helper name does not say what it returns. |"
		a := check(t, committed, fakeRun{stdout: bugbotEnvelope(t, "Bugbot found 2 findings in the changes between 3f2a91c and 8be04d7."+table)})
		b := check(t, uncommitted, fakeRun{stdout: bugbotEnvelope(t, "Bugbot found 2 findings."+table)})
		wantState(t, a, StateOK)
		wantState(t, b, StateOK)
		if a.Verdict != VerdictNeedsChanges || a.Verdict != b.Verdict || len(a.Findings) != 2 || !slices.Equal(a.Findings, b.Findings) {
			t.Fatalf("committed %+v\nuncommitted %+v", a, b)
		}
	})
	t.Run("a committed findings table whose prose names the working tree is no answer", func(t *testing.T) {
		wantState(t, check(t, committed, fakeRun{stdout: bugbotEnvelope(t, bugbotTable)}), StateInvalid)
		wantState(t, check(t, uncommitted, fakeRun{stdout: bugbotEnvelope(t, bugbotTable)}), StateOK)
	})
	t.Run("a committed error envelope is the tool's report", func(t *testing.T) {
		b, err := json.Marshal(map[string]any{"type": "result", "subtype": "error", "is_error": true, "result": "You've hit your usage limit."})
		if err != nil {
			t.Fatal(err)
		}
		wantState(t, check(t, committed, fakeRun{stdout: string(b) + "\n"}), StateQuota)
	})
}

// checkRange is check of a committed range: the request names base..head, as crw.unitCheck's committed mode does.
func checkRange(t *testing.T, p Profile, base, head string, f fakeRun) Result {
	t.Helper()
	req := request(t, p)
	req.Base, req.Head = base, head
	res, err := Adapter{Run: f.run, Fingerprint: hashFiles}.Check(t.Context(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

// TestBugbotCommittedReaderTakesOnlyTheRequestedRange (CRW-1468): the committed reader is given the range the request asks about and
// reads an answer as the judgment of that range only. A clean answer whose sentence names another range (a narrower B..C of the
// requested A..C, or another pair of commits), or that names no range at all, is no answer (invalid), and so is an answer whose prose
// names another range anywhere outside the findings tables; the requested range, by its commit ids or their prefixes, reads clean.
func TestBugbotCommittedReaderTakesOnlyTheRequestedRange(t *testing.T) {
	const base, head = "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	committed := BugbotCommittedProfile()
	for _, text := range []string{
		"Bugbot found no bugs in the changes between 3f2a91c and 8be04d7.",
		"Bugbot found no bugs in 9999999..aaaaaaa.",
		"Bugbot found no bugs.",
		"Bugbot found no bugs in the committed changes.",
		"Bugbot found no bugs in the changes between 1111111 and 3333333.",
		"Bugbot found no bugs in the changes between 3333333 and 2222222.",
		"Bugbot found no bugs in the changes between 2222222 and 1111111.",
		"Bugbot found no bugs in the changes between the base and the head.",
		"I reviewed 3333333..2222222.Bugbot found no bugs in the changes between 1111111 and 2222222.",
		"I'll review the changes between 1111111 and 2222222.Bugbot found no bugs in the committed changes.",
	} {
		t.Run("not the requested range: "+text, func(t *testing.T) {
			wantState(t, checkRange(t, committed, base, head, fakeRun{stdout: bugbotEnvelope(t, text)}), StateInvalid)
		})
	}
	for _, text := range []string{
		"Bugbot found no bugs in the changes between 1111111 and 2222222.",
		"Bugbot found no bugs in the changes between " + base + " and " + head + ".",
		"Bugbot found no bugs in 1111111aa..2222222bb.",
		"Bugbot found no bugs in the changes between `1111111` and `2222222` (git diff 1111111..2222222).",
	} {
		t.Run("the requested range: "+text, func(t *testing.T) {
			res := checkRange(t, committed, base, head, fakeRun{stdout: bugbotEnvelope(t, text)})
			wantState(t, res, StateOK)
			if res.Verdict != VerdictVerified || len(res.Findings) != 0 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
	table := "\n\n| Severity | Location (file:line) | Finding |\n| --- | --- | --- |\n| low | internal/quote/quote.go:12 | The helper name does not say what it returns. |"
	t.Run("a findings table of the requested range", func(t *testing.T) {
		res := checkRange(t, committed, base, head, fakeRun{stdout: bugbotEnvelope(t, "Bugbot found 1 finding in the changes between 1111111 and 2222222."+table)})
		wantState(t, res, StateOK)
		if len(res.Findings) != 1 {
			t.Fatalf("result = %+v", res)
		}
	})
	for _, prose := range []string{"Bugbot found 1 finding.", "Bugbot found 1 finding in the changes between 3333333 and 2222222."} {
		t.Run("a findings table of no range or another: "+prose, func(t *testing.T) {
			wantState(t, checkRange(t, committed, base, head, fakeRun{stdout: bugbotEnvelope(t, prose+table)}), StateInvalid)
		})
	}
	t.Run("a request of the committed profile that names no range is refused", func(t *testing.T) {
		req := request(t, committed)
		req.Base, req.Head = "", ""
		if _, err := (Adapter{Run: fakeRun{stdout: bugbotEnvelope(t, "Bugbot found no bugs.")}.run, Fingerprint: hashFiles}).Check(t.Context(), req); err == nil ||
			!strings.Contains(err.Error(), "names none") {
			t.Fatalf("Check of a committed profile with no range: %v", err)
		}
	})
}

// TestBugbotReadsNegatedPartialStatements (CRW-1505): a whole sentence that denies the omissions the partial-review wordings name
// ("No files were skipped.", "nothing was dropped") is no report of a partial review: the wordings are read sentence by sentence,
// and a sentence the denial covers is passed by, while a denial inside a bigger sentence ("I cannot say that no files were
// skipped."), a qualified one ("no files were skipped except ...") and a sentence that is itself partial still read partial. The
// committed-mode reader reads them the same, and a findings table keeps its findings.
func TestBugbotReadsNegatedPartialStatements(t *testing.T) {
	clean := []string{
		"No files were skipped.",
		"No findings were removed.",
		"No findings were dropped.",
		"No findings were omitted.",
		"No findings were removed, dropped, or omitted.",
		"Nothing was skipped.",
		"None of the changed files were skipped.",
		"The files were not skipped.",
		"Findings have not been dropped.",
		"Bugbot did not skip any files.",
		"The validator didn't remove any findings.",
		`The review was not "not clean".`,
		"NO FILES WERE SKIPPED.\nNo findings were dropped.",
		"No files were\nskipped.",
		"No findings were removed,\ndropped, or omitted.",
	}
	for _, text := range clean {
		t.Run("uncommitted clean: "+text, func(t *testing.T) {
			res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, text+"Bugbot found no bugs.")})
			wantState(t, res, StateOK)
			if res.Verdict != VerdictVerified || len(res.Findings) != 0 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
	const base, head = "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "2222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	committed := BugbotCommittedProfile()
	cleanRange := "Bugbot found no bugs in the changes between 1111111 and 2222222."
	for _, text := range clean {
		t.Run("committed clean: "+text, func(t *testing.T) {
			res := checkRange(t, committed, base, head, fakeRun{stdout: bugbotEnvelope(t, text+cleanRange)})
			wantState(t, res, StateOK)
			if res.Verdict != VerdictVerified || len(res.Findings) != 0 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
	t.Run("a denial after a findings table keeps the finding", func(t *testing.T) {
		text := "Bugbot found 1 finding in the changes between 1111111 and 2222222.\n\n" +
			"| Severity | Location (file:line) | Finding |\n| --- | --- | --- |\n" +
			"| nit | internal/quote/quote.go:12 | The helper name does not say what it returns. |\n\n" +
			"No files were skipped."
		res := checkRange(t, committed, base, head, fakeRun{stdout: bugbotEnvelope(t, text)})
		wantState(t, res, StateOK)
		if res.Verdict != VerdictVerified || len(res.Findings) != 1 || res.Findings[0].Impact != "minor_separable" {
			t.Fatalf("result = %+v, want verified with one minor_separable finding", res)
		}
	})
	partial := []string{
		"No files were skipped. One finding was dropped.",
		"No findings were dropped; some files were skipped.",
		"No files were skipped except the generated files.",
		"No files were skipped unless they were too large.",
		"I cannot say that no files were skipped.",
		"The review was not clean.",
		"No files were not skipped.",
		"Not all files were checked; some files were not reviewed.",
		"No files were skipped\nexcept the generated files.",
		"No files were skipped\r\nunless they were too large.",
		"No files were skipped?",
	}
	for _, text := range partial {
		t.Run("uncommitted not whole: "+text, func(t *testing.T) {
			res := check(t, BugbotProfile(), fakeRun{stdout: bugbotEnvelope(t, text+"Bugbot found no bugs.")})
			wantState(t, res, StateInvalid)
			if !strings.Contains(res.Detail, "not whole") {
				t.Fatalf("detail = %q, want not whole", res.Detail)
			}
		})
		t.Run("committed not whole: "+text, func(t *testing.T) {
			res := checkRange(t, committed, base, head, fakeRun{stdout: bugbotEnvelope(t, text+cleanRange)})
			wantState(t, res, StateInvalid)
			if !strings.Contains(res.Detail, "not whole") {
				t.Fatalf("detail = %q, want not whole", res.Detail)
			}
		})
	}
}

// TestMergeFindingsKeepsTheHigherSeverityAndTheBlockingImpact: MergeFindings merges the findings of several lists (the two passes of
// a crosscheck round) into one finding per id: the first copy's file, line and text, the higher severity, and a blocking impact over
// a non-blocking or empty one, whichever list holds which copy. Findings of other ids keep their order, and the inputs are not changed.
func TestMergeFindingsKeepsTheHigherSeverityAndTheBlockingImpact(t *testing.T) {
	id := FindingID("a.go", "The loop never ends")
	minor := Finding{ID: id, Severity: "P3", Impact: "minor_separable", File: "a.go", Line: 3, Text: "The loop never ends"}
	core := Finding{ID: id, Severity: "P1", Impact: "core_function", File: "a.go", Line: 40, Text: "the loop  never ends"}
	other := Finding{ID: FindingID("b.go", "naming"), Severity: "P3", File: "b.go", Line: 1, Text: "naming"}
	for _, c := range []struct {
		name  string
		a, b  []Finding
		first Finding
	}{
		{"the minor copy first", []Finding{minor}, []Finding{core, other}, minor},
		{"the blocking copy first", []Finding{core, other}, []Finding{minor}, core},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, b := slices.Clone(c.a), slices.Clone(c.b)
			got := MergeFindings(a, b)
			merged := c.first
			merged.Severity, merged.Impact = "P1", "core_function"
			want := []Finding{merged, other}
			if !slices.Equal(got, want) {
				t.Fatalf("merged = %+v\nwant %+v", got, want)
			}
			if !slices.Equal(a, c.a) || !slices.Equal(b, c.b) {
				t.Fatalf("MergeFindings changed its inputs: %+v %+v", a, b)
			}
		})
	}
	t.Run("an empty impact takes the other copy's", func(t *testing.T) {
		bare := Finding{ID: id, Severity: "P2", File: "a.go", Line: 3, Text: "The loop never ends"}
		got := MergeFindings([]Finding{bare}, []Finding{minor})
		if len(got) != 1 || got[0].Severity != "P2" || got[0].Impact != "minor_separable" || got[0].Line != 3 {
			t.Fatalf("merged = %+v", got)
		}
		got = MergeFindings([]Finding{minor}, []Finding{bare})
		if len(got) != 1 || got[0].Severity != "P2" || got[0].Impact != "minor_separable" {
			t.Fatalf("merged = %+v", got)
		}
	})
	t.Run("no findings is an empty list", func(t *testing.T) {
		if got := MergeFindings(nil, []Finding{}); got == nil || len(got) != 0 {
			t.Fatalf("merged = %#v, want an empty list", got)
		}
	})
}

// meteredProfile is a tool that prints {"answer": <document or null>, "usage": {"cache": N, "commands": M}} (usage absent when the
// tool reports none), with a usage reader and the floors 100000 cached tokens, 3 commands and 60 s.
func meteredProfile() Profile {
	return Profile{
		Name: "metered", Argv: []string{"fake-tool"}, Model: "gpt-6-astra", Effort: "high", QuotaTexts: []string{"usage limit"},
		Answer: func(stdout []byte, _ Request) ([]byte, error) {
			var out struct {
				Answer json.RawMessage `json:"answer"`
			}
			if err := json.Unmarshal(stdout, &out); err != nil {
				return nil, err
			}
			if string(out.Answer) == "null" {
				return nil, nil
			}
			return out.Answer, nil
		},
		Usage: func(stdout []byte, _ Request) (Usage, bool) {
			var out struct {
				Usage *struct {
					Cache    int64 `json:"cache"`
					Commands int   `json:"commands"`
				} `json:"usage"`
			}
			if json.Unmarshal(stdout, &out) != nil || out.Usage == nil {
				return Usage{}, false
			}
			return Usage{CacheRead: out.Usage.Cache, Commands: out.Usage.Commands}, true
		},
		MinCacheRead: 100000, MinCommands: 3, MinElapsed: 60 * time.Second,
	}
}

// meteredOutput is what meteredProfile's tool prints: answer (a JSON document, or "null" for a blank one) and, when cache >= 0, the
// usage it reports.
func meteredOutput(answer string, cache int64, commands int) string {
	if cache < 0 {
		return `{"answer":` + answer + `}`
	}
	return fmt.Sprintf(`{"answer":%s,"usage":{"cache":%d,"commands":%d}}`, answer, cache, commands)
}

// checkFor runs one check of p on f whose call takes d on a fake clock.
func checkFor(t *testing.T, p Profile, f fakeRun, d time.Duration) Result {
	t.Helper()
	return checkForWorktree(t, p, t.TempDir(), f, d)
}

func checkForWorktree(t *testing.T, p Profile, dir string, f fakeRun, d time.Duration) Result {
	t.Helper()
	clock := time.Date(2026, 10, 16, 4, 0, 0, 0, time.UTC)
	calls := 0
	now := func() time.Time {
		calls++
		if calls == 1 {
			return clock
		}
		return clock.Add(d)
	}
	req := request(t, p)
	req.Worktree = dir
	res, err := Adapter{Run: f.run, Fingerprint: hashFiles, Now: now}.Check(t.Context(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

// TestAdapterVoidsAPassThatReadNoRepository: a profile with floors (cached tokens, commands, elapsed time) turns an answer that would
// be ok into not_read when the call ran shorter than its floor, reported no usage, read fewer cached tokens or ran fewer commands
// than the floor: such a pass read no repository and has no verdict. The record keeps the elapsed time and the usage the tool
// reported in every state; a profile without floors is never voided, and the states before ok keep their order.
func TestAdapterVoidsAPassThatReadNoRepository(t *testing.T) {
	valid := `{"verdict":"verified","findings":[]}`
	t.Run("a shallow pass is not read", func(t *testing.T) {
		res := checkFor(t, meteredProfile(), fakeRun{stdout: meteredOutput(valid, 2304, 1)}, 20*time.Second)
		wantState(t, res, StateNotRead)
		if res.ElapsedMS != 20000 || res.CacheReadTokens != 2304 || res.Commands != 1 {
			t.Fatalf("elapsed %d ms, cache read %d, commands %d; want 20000, 2304, 1", res.ElapsedMS, res.CacheReadTokens, res.Commands)
		}
		for _, n := range []string{"2304 tokens", "1 commands", "20 s"} {
			if !strings.Contains(res.Detail, n) {
				t.Fatalf("detail %q does not name %q", res.Detail, n)
			}
		}
		if res.Verdict != "" || res.Findings != nil {
			t.Fatalf("a void pass carries a verdict: %+v", res)
		}
	})
	t.Run("a pass over every floor is ok", func(t *testing.T) {
		res := checkFor(t, meteredProfile(), fakeRun{stdout: meteredOutput(valid, 150000, 4)}, 70*time.Second)
		wantState(t, res, StateOK)
		if res.ElapsedMS != 70000 || res.CacheReadTokens != 150000 || res.Commands != 4 || res.Verdict != VerdictVerified {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("each floor alone voids the pass", func(t *testing.T) {
		for name, c := range map[string]struct {
			cache    int64
			commands int
			d        time.Duration
		}{
			"too few cached tokens": {99999, 4, 70 * time.Second},
			"too few commands":      {150000, 2, 70 * time.Second},
			"too short":             {150000, 4, 59 * time.Second},
		} {
			t.Run(name, func(t *testing.T) {
				res := checkFor(t, meteredProfile(), fakeRun{stdout: meteredOutput(valid, c.cache, c.commands)}, c.d)
				wantState(t, res, StateNotRead)
				if res.CacheReadTokens != c.cache || res.Commands != c.commands || res.ElapsedMS != c.d.Milliseconds() {
					t.Fatalf("result = %+v", res)
				}
			})
		}
	})
	t.Run("no usage is not read", func(t *testing.T) {
		res := checkFor(t, meteredProfile(), fakeRun{stdout: meteredOutput(valid, -1, 0)}, 70*time.Second)
		wantState(t, res, StateNotRead)
		if res.CacheReadTokens != 0 || res.Commands != 0 || res.ElapsedMS != 70000 || !strings.Contains(res.Detail, "no usage") {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("a profile without floors is never voided and still records the usage", func(t *testing.T) {
		p := meteredProfile()
		p.MinCacheRead, p.MinCommands, p.MinElapsed = 0, 0, 0
		res := checkFor(t, p, fakeRun{stdout: meteredOutput(valid, 2304, 1)}, 20*time.Second)
		wantState(t, res, StateOK)
		if res.ElapsedMS != 20000 || res.CacheReadTokens != 2304 || res.Commands != 1 {
			t.Fatalf("result = %+v", res)
		}
		res = checkFor(t, p, fakeRun{stdout: meteredOutput(valid, -1, 0)}, 20*time.Second)
		wantState(t, res, StateOK)
	})
	t.Run("the states before ok keep their order and record the usage", func(t *testing.T) {
		res := checkFor(t, meteredProfile(), fakeRun{stdout: meteredOutput("null", 2304, 1)}, 20*time.Second)
		wantState(t, res, StateNoAnswer)
		if res.ElapsedMS != 20000 || res.CacheReadTokens != 2304 || res.Commands != 1 {
			t.Fatalf("result = %+v", res)
		}
		res = checkFor(t, meteredProfile(), fakeRun{stdout: meteredOutput(`{"verdict":"approve","findings":[]}`, 2304, 1)}, 20*time.Second)
		wantState(t, res, StateInvalid)
		res = checkFor(t, meteredProfile(), fakeRun{stdout: meteredOutput(valid, 2304, 1), touch: "x.go"}, 20*time.Second)
		wantState(t, res, StateWorktreeChanged)
		res = checkFor(t, meteredProfile(), fakeRun{exit: 1, stderr: "usage limit"}, 20*time.Second)
		wantState(t, res, StateQuota)
	})
}

// TestFirstLineCutsAtARuneBoundary: firstLine keeps at most 200 bytes of the first line and never cuts a rune in two, so the text
// it puts in a record's detail stays valid UTF-8 whatever the tool printed.
func TestFirstLineCutsAtARuneBoundary(t *testing.T) {
	ascii := strings.Repeat("a", 199)
	for _, c := range []struct {
		name, in, want string
	}{
		{"a short line is kept whole", "the tool failed\nsecond line", "the tool failed"},
		{"a two-byte rune across the cut is dropped", ascii + "é and more", ascii},
		{"a three-byte rune across the cut is dropped", strings.Repeat("a", 198) + "한글", strings.Repeat("a", 198)},
		{"a four-byte rune across the cut is dropped", strings.Repeat("a", 197) + "😀", strings.Repeat("a", 197)},
		{"a rune that ends at the cut is kept", strings.Repeat("a", 198) + "é tail", strings.Repeat("a", 198) + "é"},
		{"a line of only multibyte runes", strings.Repeat("한", 100), strings.Repeat("한", 66)},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := firstLine([]byte(c.in))
			if got != c.want {
				t.Fatalf("firstLine = %q (%d bytes), want %q (%d bytes)", got, len(got), c.want, len(c.want))
			}
			if !utf8.ValidString(got) || len(got) > 200 {
				t.Fatalf("firstLine = %q: valid UTF-8 %v, %d bytes", got, utf8.ValidString(got), len(got))
			}
		})
	}
}

// TestNormalizeRecordsAnImpactlessP3AsMinorSeparable: a P3 finding of a new answer that names no impact is recorded
// minor_separable, after the security rule (a security P3 stays safety_or_data) and after the duplicate collapse (the collapsed
// grade decides); a P0-P2 finding without impact keeps an empty impact and still blocks a verified verdict.
func TestNormalizeRecordsAnImpactlessP3AsMinorSeparable(t *testing.T) {
	for _, c := range []struct {
		name, verdict, findings string
		state                   State
		severity, impact        string
	}{
		{"needs_changes over an impactless P3", VerdictNeedsChanges, `{"severity":"low","file":"a.go","line":1,"text":"leak"}`, StateOK, "P3", "minor_separable"},
		{"verified over an impactless P3", VerdictVerified, `{"severity":"low","file":"a.go","line":1,"text":"leak"}`, StateOK, "P3", "minor_separable"},
		{"an impactless P2 stays empty", VerdictNeedsChanges, `{"severity":"medium","file":"a.go","line":1,"text":"leak"}`, StateOK, "P2", ""},
		{"an impactless P2 still blocks verified", VerdictVerified, `{"severity":"medium","file":"a.go","line":1,"text":"leak"}`, StateInvalid, "", ""},
		{"a P3 with an impact keeps it", VerdictNeedsChanges, `{"severity":"low","impact":"core_function","file":"a.go","line":1,"text":"leak"}`, StateOK, "P3", "core_function"},
		{"a security P3 stays safety_or_data", VerdictNeedsChanges, `{"severity":"low security","file":"a.go","line":1,"text":"leak"}`, StateOK, "P3", "safety_or_data"},
		{"an impactless P3 collapsed into a P2 copy stays empty", VerdictNeedsChanges, `{"severity":"low","file":"a.go","line":1,"text":"leak"},
			{"severity":"medium","file":"a.go","line":9,"text":"leak"}`, StateOK, "P2", ""},
		{"an impactless P3 collapsed with a core_function copy takes it", VerdictNeedsChanges, `{"severity":"low","file":"a.go","line":1,"text":"leak"},
			{"severity":"low","impact":"core_function","file":"a.go","line":9,"text":"leak"}`, StateOK, "P3", "core_function"},
		{"an impactless P3 collapsed with a security copy is safety_or_data", VerdictNeedsChanges, `{"severity":"low","file":"a.go","line":1,"text":"leak"},
			{"severity":"low vuln","file":"a.go","line":9,"text":"leak"}`, StateOK, "P3", "safety_or_data"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := check(t, jsonProfile(), fakeRun{stdout: `{"verdict":"` + c.verdict + `","findings":[` + c.findings + `]}`})
			wantState(t, res, c.state)
			if c.state != StateOK {
				return
			}
			if len(res.Findings) != 1 || res.Findings[0].Severity != c.severity || res.Findings[0].Impact != c.impact {
				t.Fatalf("findings = %+v, want one %s finding of impact %q", res.Findings, c.severity, c.impact)
			}
		})
	}
	t.Run("the rule is in normalize, so every profile that normalizes gets it", func(t *testing.T) {
		got, err := normalize(VerdictNeedsChanges, []Finding{
			{ID: "p3", Severity: "P3", File: "a.go", Line: 1, Text: "naming"},
			{ID: "p1", Severity: "P1", File: "a.go", Line: 2, Text: "crash"},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Impact != "minor_separable" || got[1].Impact != "" {
			t.Fatalf("normalized = %+v, want the P3 minor_separable and the P1 without impact", got)
		}
	})
}
