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

func request(t *testing.T, p Profile) Request {
	t.Helper()
	return Request{Profile: p, Worktree: t.TempDir(), Prompt: "review the uncommitted changes", Schema: AnswerSchema, Limit: time.Minute, GuideRev: "guide-r1"}
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
			{ID: FindingID("c.go", "third"), Severity: "P3", File: "c.go", Line: 3, Text: "third"},
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
			{ID: FindingID("internal/quote/quote.go", "The helper name does not say what it returns."), Severity: "P3", File: "internal/quote/quote.go", Line: 12, Text: "The helper name does not say what it returns."},
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
		if res.Verdict != VerdictVerified || len(res.Findings) != 1 || res.Findings[0].Impact != "" {
			t.Fatalf("result = %+v, want verified with one finding without impact", res)
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
	for _, k := range []string{"state", "detail", "verdict", "findings", "engine_family", "model", "effort", "guide_rev", "prompt_digest", "fingerprint_before", "fingerprint_after", "elapsed_ms"} {
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
// (/state/crw/crw-lane-b1-issue-child/evidence/bugbot-U1a-r2.json.raw.json, the ids and usage trimmed).
const bugbotNoBugsFixture = `{"type":"result","subtype":"success","is_error":false,"duration_ms":289515,"duration_api_ms":289515,"result":"I'll review only the uncommitted changes. Checking the review tool schema, then starting the Bugbot review.Bugbot found no bugs."}` + "\n"

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
		{ID: FindingID("a.go", "The name is unclear."), Severity: "P3", File: "a.go", Line: 3, Text: "The name is unclear."},
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
