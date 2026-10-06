package migrate

// classify_records_test.go holds the red-first cases of CRW-682: the state-copy preflight judged three families of
// retained record with its own, looser shape checks, so it admitted records the readers that consume them refuse.
// Each judgement now delegates to the reader that owns the record.
//
// The cases build synthetic roots only. isolate puts HOME, CODEX_HOME, CRW_HOME, CODEXCLAW_HOME and TMPDIR in
// temporary directories, so nothing here reads or writes the real ~/.codex, ~/.crw or ~/.codexclaw. A refusal is
// proved to write nothing by invRowRefusal, which fingerprints the source and the destination around it.

import (
	"errors"
	"path/filepath"
	"testing"
)

// invSessionRecord is a session record the state reader accepts. restore requires a phase of the known set, so a
// body with no phase, a truncated one, or an array is unreadable to ReadStateStrict and refuses the preflight.
const invSessionRecord = `{"phase":"IDLE"}`

// TestClassifyRecordsDispatchReconcileIsUnresolved proves review finding 1: reconcile is unresolved, not terminal.
// internal/role/dispatch_ledger.go:779-780, 808, 841 read it as "check whether the child exists, stop, hand it
// over", so a stopped record with an attempt still in reconcile refuses the scope instead of being copied.
func TestClassifyRecordsDispatchReconcileIsUnresolved(t *testing.T) {
	for _, record := range []string{"stopped", "complete", "main-direct"} {
		t.Run(record, func(t *testing.T) {
			invClassifyRecords(t, map[string]string{
				"sessions/s1.json":       invSessionRecord,
				"dispatches/s1/d-1.json": invDispatchFull("s1", "d-1", record, "reconcile"),
			}, ReasonActive, "")
		})
	}
}

// invDispatchCandidate is invDispatchFull with the candidate object spelled out and, when extra is non-empty, extra
// members appended to the attempt. Every case below is store-shaped except for the one defect it names, so the
// refusal it expects comes from the check under test rather than from an earlier one.
func invDispatchCandidate(session, id, record, attempt, candidate, extra string) string {
	return `{"version":1,"sessionId":"` + session + `","id":"` + id + `","role":"reviewer","candidates":[` + candidate + `],"attempts":[{"id":"a-1","candidate":` + candidate + `,"claimed":false,"agentId":null,"observedModel":null,"code":null,"taskFailure":null,"status":"` + attempt + `","reconciliation":null,"spawnIssued":false,"toolUseId":null` + extra + `}],"status":"` + record + `"}`
}

// TestClassifyRecordsDispatchShape proves review finding 2: the preflight drops its own shape checks and uses the
// ledger's reader, so an input dispatchPinnedDecode refuses refuses the scope and a store-shaped record copies.
//
// invDispatchMismatch is a store-shaped record whose attempt candidate differs from the candidate at its index.
func invDispatchMismatch(session, id, record, attempt, candidate, attemptCandidate string) string {
	return `{"version":1,"sessionId":"` + session + `","id":"` + id + `","role":"reviewer","candidates":[` + candidate + `],"attempts":[{"id":"a-1","candidate":` + attemptCandidate + `,"claimed":false,"agentId":null,"observedModel":null,"code":null,"taskFailure":null,"status":"` + attempt + `","reconciliation":null,"spawnIssued":false,"toolUseId":null}],"status":"` + record + `"}`
}

func TestClassifyRecordsDispatchShape(t *testing.T) {
	empty := `{"version":1,"sessionId":"s1","id":"d-1","role":"reviewer","candidates":[],"attempts":[{"id":"a-1","candidate":{},"claimed":false,"spawnIssued":false,"status":"complete"}],"status":"complete"}`
	nullID := invDispatchCandidate("s1", "d-1", "complete", "complete", `{"model":null,"effort":null}`, `,"id":null`)
	noSpawn := invDispatchCandidate("s1", "d-1", "complete", "complete", `{"model":null,"effort":null}`, `,"spawnIssued":null`)
	cases := []struct {
		name   string
		record string
		want   Reason // "" means the record copies
	}{
		{"empty candidates refuses unreadable", empty, ReasonUnreadable},
		{"null attempt id refuses unreadable", nullID, ReasonUnreadable},
		{"attempt candidate differs refuses unreadable", invDispatchMismatch("s1", "d-1", "complete", "complete", `{"model":"one","effort":null}`, `{"model":"other","effort":null}`), ReasonUnreadable},
		{"missing spawnIssued refuses unreadable", noSpawn, ReasonUnreadable},
		{"store-shaped complete record copies", invDispatchFull("s1", "d-1", "complete", "complete"), ""},
		{"store-shaped stopped record copies", invDispatchFull("s1", "d-1", "stopped", "failed"), ""},
		{"store-shaped main-direct record copies", invDispatchFull("s1", "d-1", "main-direct", "complete"), ""},
		{"store-shaped active record refuses active", invDispatchFull("s1", "d-1", "active", "complete"), ReasonActive},
		{"store-shaped in-flight attempt refuses active", invDispatchFull("s1", "d-1", "complete", "running"), ReasonActive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws, src, dst := invRowProject(t)
			invTree(t, src, map[string]string{"sessions/s1.json": invSessionRecord, "dispatches/s1/d-1.json": c.record})
			if c.want != "" {
				invRowRefusal(t, ws, src, dst, c.want)
				return
			}
			p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
			must(t, err)
			invWant(t, p, ScopeProject, "dispatches/s1/d-1.json", DispCopy)
		})
	}
}

// TestClassifyRecordsSessionRecord proves review finding 3 for sessions/*.json: the preflight uses the state
// reader's own verdict, so a file ReadStateStrict calls unreadable refuses the scope while one it reads copies.
func TestClassifyRecordsSessionRecord(t *testing.T) {
	t.Run("truncated session refuses unreadable", func(t *testing.T) {
		invClassifyRecords(t, map[string]string{"sessions/s1.json": "{"}, ReasonUnreadable, "")
	})
	t.Run("session without a phase refuses unreadable", func(t *testing.T) {
		invClassifyRecords(t, map[string]string{"sessions/s1.json": "{}"}, ReasonUnreadable, "")
	})
	t.Run("session array refuses unreadable", func(t *testing.T) {
		invClassifyRecords(t, map[string]string{"sessions/s1.json": "[]"}, ReasonUnreadable, "")
	})
	t.Run("valid session copies", func(t *testing.T) {
		invClassifyRecords(t, map[string]string{"sessions/s1.json": invSessionRecord}, "", "sessions/s1.json")
	})
}

// TestClassifyRecordsSingleObject proves review finding 3 for the single-object JSON records the issue names: the
// whole bytes must be one JSON object, so an array, a truncated object or trailing data refuses the scope.
func TestClassifyRecordsSingleObject(t *testing.T) {
	bad := []struct{ name, path, body string }{
		{"goalplan array refuses unreadable", "goalplans/x/goalplan.json", "[]"},
		{"goalplan truncated refuses unreadable", "goalplans/x/goalplan.json", "{"},
		{"goalplan trailing data refuses unreadable", "goalplans/x/goalplan.json", "{} {}"},
		{"freeze array refuses unreadable", "interview/freeze.json", "[]"},
		{"evidence attempt truncated refuses unreadable", "evidence-attempts/a.json", "{"},
		{"unrecordable truncated refuses unreadable", "evidence-unrecordable/a-1.json", "{"},
		{"source array refuses unreadable", "sources/s1.json", "[]"},
		{"objective kind array refuses unreadable", "objective-kind/s1.json", "[]"},
		{"divergence mode array refuses unreadable", "divergence/s1.mode.json", "[]"},
		{"attest truncated refuses unreadable", "attest.json", "{"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			invClassifyRecords(t, map[string]string{"sessions/s1.json": invSessionRecord, c.path: c.body}, ReasonUnreadable, "")
		})
	}
	good := []string{"goalplans/x/goalplan.json", "interview/freeze.json", "evidence-attempts/a.json",
		"evidence-unrecordable/a-1.json", "sources/s1.json", "objective-kind/s1.json",
		"divergence/s1.mode.json", "attest.json"}
	for _, path := range good {
		t.Run("object copies: "+path, func(t *testing.T) {
			invClassifyRecords(t, map[string]string{"sessions/s1.json": invSessionRecord, path: "{}"}, "", path)
		})
	}
}

// TestClassifyRecordsUserSingleObject proves the user-table single-object rows the issue names are judged too.
func TestClassifyRecordsUserSingleObject(t *testing.T) {
	for _, c := range []struct{ name, path, body string }{
		{"config array refuses unreadable", "config.json", "[]"},
		{"subagents truncated refuses unreadable", "subagents.json", "{"},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := isolate(t)
			u, v := filepath.Join(base, "eu"), filepath.Join(base, "ev")
			mkdirs(t, u, v)
			invTree(t, u, map[string]string{c.path: c.body})
			invUserRefusal(t, u, v, ReasonUnreadable)
		})
	}
	t.Run("user objects copy", func(t *testing.T) {
		base := isolate(t)
		u, v := filepath.Join(base, "eu"), filepath.Join(base, "ev")
		mkdirs(t, u, v)
		invTree(t, u, map[string]string{"config.json": "{}", "subagents.json": `{"roles":{}}`})
		p, err := invClassify(t, Options{Scope: ScopeUser, FromHome: u, ToHome: v})
		must(t, err)
		invWant(t, p, ScopeUser, "config.json", DispCopy)
		invWant(t, p, ScopeUser, "subagents.json", DispCopy)
	})
}

// TestClassifyRecordsJsonlDamagedLineCopies proves the JSONL rows stay unjudged: their Go readers skip a damaged
// line, so one damaged line is not a reason to refuse the whole scope. render-observations.jsonl is the exception
// the issue names (its hook reader refuses the whole file on one damaged line) and is covered below.
func TestClassifyRecordsJsonlDamagedLineCopies(t *testing.T) {
	damaged := "{\"ok\":true}\n{ this line is damaged\n{\"ok\":true}\n"
	for _, rel := range []string{"ledger.jsonl", "interviews/s1.jsonl", "metrics.jsonl",
		"divergence/candidates.jsonl", "bg/ledger.jsonl", "goalplans/x/ledger.jsonl"} {
		t.Run(rel, func(t *testing.T) {
			invClassifyRecords(t, map[string]string{"sessions/s1.json": invSessionRecord, rel: damaged}, "", rel)
		})
	}
}

// TestClassifyRecordsRenderObservations proves the issue's unless clause: render-observations.jsonl is the one JSONL
// row whose dev reader refuses the whole file on one damaged line (hook.NativeObservationLedgerMalformed, the check
// the oracle's Stop hook consults at render-observations.ts:161 and hook.ts:1910), so that file is judged the same
// way and a damaged line refuses the scope.
func TestClassifyRecordsRenderObservations(t *testing.T) {
	t.Run("a damaged line refuses unreadable", func(t *testing.T) {
		invClassifyRecords(t, map[string]string{
			"sessions/s1.json":          invSessionRecord,
			"render-observations.jsonl": "{\"ok\":true}\n{ this line is damaged\n",
		}, ReasonUnreadable, "")
	})
	t.Run("a non-object line refuses unreadable", func(t *testing.T) {
		invClassifyRecords(t, map[string]string{"sessions/s1.json": invSessionRecord, "render-observations.jsonl": "[]\n"}, ReasonUnreadable, "")
	})
	t.Run("every line an object copies", func(t *testing.T) {
		invClassifyRecords(t, map[string]string{
			"sessions/s1.json":          invSessionRecord,
			"render-observations.jsonl": "{\"kind\":\"observation\",\"detail\":\"d\"}\n{\"kind\":\"observation\",\"detail\":\"e\"}\n",
		}, "", "render-observations.jsonl")
	})
}

// TestClassifyRecordsRefusalsWriteNothing proves every new refusal is a whole-scope refusal that writes nothing.
// invRowRefusal fingerprints both trees around the classification.
func TestClassifyRecordsRefusalsWriteNothing(t *testing.T) {
	for _, c := range []struct {
		name    string
		entries map[string]string
		want    Reason
	}{
		{"unresolved dispatch", map[string]string{"sessions/s1.json": invSessionRecord, "dispatches/s1/d-1.json": invDispatchFull("s1", "d-1", "stopped", "reconcile")}, ReasonActive},
		{"unreadable dispatch", map[string]string{"sessions/s1.json": invSessionRecord, "dispatches/s1/d-1.json": "{"}, ReasonUnreadable},
		{"unreadable session", map[string]string{"sessions/s1.json": "{"}, ReasonUnreadable},
		{"unreadable object", map[string]string{"sessions/s1.json": invSessionRecord, "interview/freeze.json": "[]"}, ReasonUnreadable},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws, src, dst := invRowProject(t)
			invTree(t, src, c.entries)
			invRowRefusal(t, ws, src, dst, c.want)
		})
	}
}

// invClassifyRecords classifies one project root holding entries and requires the refusal want, or the copy named
// by copied when want is empty.
func invClassifyRecords(t *testing.T, entries map[string]string, want Reason, copied string) {
	t.Helper()
	ws, src, dst := invRowProject(t)
	invTree(t, src, entries)
	if want != "" {
		invRowRefusal(t, ws, src, dst, want)
		return
	}
	p, err := invClassify(t, Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	invWant(t, p, ScopeProject, copied, DispCopy)
}

// invUserRefusal classifies a user pair and requires the refusal want, proving the refusal changed neither tree.
func invUserRefusal(t *testing.T, from, to string, want Reason) {
	t.Helper()
	beforeFrom, beforeTo := fingerprint(t, from), fingerprint(t, to)
	r, err := Open(Options{Scope: ScopeUser, FromHome: from, ToHome: to})
	must(t, err)
	defer r.Close()
	_, err = classify(r)
	var ref *RefusedError
	if !errors.As(err, &ref) || ref.Reason != want {
		t.Fatalf("classify = %v, want the refusal %q", err, want)
	}
	if fingerprint(t, from) != beforeFrom || fingerprint(t, to) != beforeTo {
		t.Error("the refusal changed a user tree")
	}
}
