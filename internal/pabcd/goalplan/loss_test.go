package goalplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// revivalLossPlan is a plan revival keeps whole; each case below breaks one thing in it.
const revivalLossPlan = `{"objective":"o","slug":"demo","createdAt":"2026-01-01T00:00:00.000Z","updatedAt":"2026-01-01T00:00:00.000Z","activeWorkPhaseId":null,
"workPhases":[{"id":"wp1","title":"t","status":"pending","tasks":[{"id":"t1","title":"one","status":"pending"},{"id":"t2","title":"two","status":"pending"}],"criteriaIds":["c-1"]}],
"criteria":[{"id":"c-1","scenario":"s","expectedEvidence":"","capturedEvidence":null,"status":"open"}],"host":{"armed":false,"armedAt":null,"source":"none"},
"reviewRounds":[{"roundId":"r1","purpose":"plan_audit","planPath":"p","planSha256":"x","status":"approved","lane":{"launchId":"l1"},"openedAt":"2026-01-01T00:00:00.000Z","planFiles":[{"path":"kept.md","sha256":"abc"},{"path":"other.md","sha256":"def"}]}],
"decisions":[{"id":"q1","question":"q","status":"open","askedAt":"2026-01-01T00:00:00.000Z"}]}`

func revivalLossReason(path string) string {
	return "goalplan 'demo' holds " + path + " that this build cannot keep; refusing to rewrite it"
}

func revivalLossFirst(m map[string]any, key string) map[string]any {
	return m[key].([]any)[0].(map[string]any)
}
func revivalLossPhase(m map[string]any) map[string]any { return revivalLossFirst(m, "workPhases") }
func revivalLossRound(m map[string]any) map[string]any { return revivalLossFirst(m, "reviewRounds") }
func revivalLossTask(m map[string]any, i int) map[string]any {
	return revivalLossPhase(m)["tasks"].([]any)[i].(map[string]any)
}

// revivalLossEdit decodes revivalLossPlan, applies edit and encodes the result again.
func revivalLossEdit(t *testing.T, edit func(m map[string]any)) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(revivalLossPlan), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// revivalLossLock takes the write lock of slug with a spy as the writer and reports whether the spy ran.
func revivalLossLock(t *testing.T, cwd, slug string) (GoalplanWriteLockResult[string], bool) {
	t.Helper()
	called := false
	res, err := WithGoalplanWriteLock(cwd, slug, func(*Goalplan) (string, error) { called = true; return "ran", nil }, &GoalplanWriteLockOptions{RetryDelaysMs: []int{}})
	if err != nil {
		t.Fatal(err)
	}
	return res, called
}

// revivalLossRefused puts raw in the plan file of a fresh workspace and requires the lock to refuse with reason, the writer not
// called, the file untouched and the lock released; displayed says whether the display read still returns a plan.
func revivalLossRefused(t *testing.T, raw, reason string, displayed bool) {
	t.Helper()
	cwd, dir := readWorkspace(t)
	file := filepath.Join(dir, GoalplanFile)
	writeReadFile(t, file, raw)
	res, called := revivalLossLock(t, cwd, "demo")
	if res.Kind != "unreadable" || res.Reason != reason || res.Value != nil || called {
		t.Fatalf("lock %+v called=%v, want unreadable %q", res, called, reason)
	}
	if b, _ := os.ReadFile(file); string(b) != raw {
		t.Fatal("the plan file changed")
	}
	if _, err := os.Stat(filepath.Join(dir, GoalplanLockDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the lock was not released", err)
	}
	if got := ReadGoalplanDetailed(cwd, "demo"); (got.Plan != nil) != displayed {
		t.Fatalf("display read %+v", got)
	}
}

// revivalLossPasses requires the lock to call the writer for raw and leave the file alone.
func revivalLossPasses(t *testing.T, raw string) {
	t.Helper()
	cwd, dir := readWorkspace(t)
	file := filepath.Join(dir, GoalplanFile)
	writeReadFile(t, file, raw)
	if res, called := revivalLossLock(t, cwd, "demo"); res.Kind != "ok" || !called {
		t.Fatalf("lock %+v called=%v", res, called)
	}
	if b, _ := os.ReadFile(file); string(b) != raw {
		t.Fatal("the plan file changed")
	}
}

func TestRevivalLossRefusesWhatARewriteWouldDrop(t *testing.T) {
	for _, c := range []struct {
		name, path string
		edit       func(m map[string]any)
	}{
		{"second task without a title", "workPhases[0].tasks[1]", func(m map[string]any) { delete(revivalLossTask(m, 1), "title") }},
		{"numeric criteriaIds entry", "workPhases[0].criteriaIds[1]", func(m map[string]any) {
			revivalLossPhase(m)["criteriaIds"] = []any{"c-1", 7}
		}},
		{"unknown plan key", "evil", func(m map[string]any) { m["evil"] = 1 }},
		{"unknown work phase key", "workPhases[0].evil", func(m map[string]any) { revivalLossPhase(m)["evil"] = 1 }},
		{"unknown task key", "workPhases[0].tasks[0].evil", func(m map[string]any) { revivalLossTask(m, 0)["evil"] = 1 }},
		{"unknown criterion key", "criteria[0].evil", func(m map[string]any) { revivalLossFirst(m, "criteria")["evil"] = 1 }},
		{"unknown decision key", "decisions[0].evil", func(m map[string]any) { revivalLossFirst(m, "decisions")["evil"] = 1 }},
		{"unknown host key", "host.evil", func(m map[string]any) { m["host"].(map[string]any)["evil"] = 1 }},
		{"a cased twin of a known key", "ReviewRounds", func(m map[string]any) { m["ReviewRounds"] = []any{1} }},
		{"plan files with one malformed entry", "reviewRounds[0].planFiles", func(m map[string]any) {
			revivalLossRound(m)["planFiles"] = []any{map[string]any{"path": "kept.md", "sha256": "abc"}, map[string]any{"path": "bad.md", "sha256": ""}}
		}},
		{"an unknown task status", "workPhases[0].tasks[0].status", func(m map[string]any) { revivalLossTask(m, 0)["status"] = "doing" }},
		{"a dirty that is not a boolean", "reviewRounds[0].lane.sourceIdentity.dirty", func(m map[string]any) {
			revivalLossRound(m)["lane"].(map[string]any)["sourceIdentity"] = map[string]any{"kind": "resolved", "commitSha": "c", "dirty": "yes", "capturedAt": "2026-01-01T00:00:00.000Z", "treeHash": "t"}
		}},
		{"a fractional schemaVersion", "schemaVersion", func(m map[string]any) { m["schemaVersion"] = 2.9 }},
		{"an updatedAt below the top level", "finalGate.updatedAt", func(m map[string]any) {
			m["finalGate"] = map[string]any{"status": "pending", "qaRequired": false, "updatedAt": 5}
		}},
	} {
		t.Run(c.name, func(t *testing.T) { revivalLossRefused(t, revivalLossEdit(t, c.edit), revivalLossReason(c.path), true) })
	}
}

func TestRevivalLossLetsALosslessPlanThrough(t *testing.T) {
	for name, edit := range map[string]func(m map[string]any){
		"as it is": func(m map[string]any) {},
		"nulls where the writer omits or fills": func(m map[string]any) {
			revivalLossPhase(m)["blockedReason"] = nil
			revivalLossRound(m)["closedAt"] = nil
			revivalLossRound(m)["lane"].(map[string]any)["reviewerSession"] = nil
			m["finalGate"] = nil
			m["createdAt"] = nil
		},
		"a top-level updatedAt the writer refreshes": func(m map[string]any) { m["updatedAt"] = 5 },
		"a schemaVersion written as 1e0":             func(m map[string]any) { m["schemaVersion"] = json.Number("1e0") },
		"U+FFFD and U+2028 as they are":              func(m map[string]any) { revivalLossTask(m, 0)["title"] = "a\ufffdb\u2028c" },
	} {
		t.Run(name, func(t *testing.T) { revivalLossPasses(t, revivalLossEdit(t, edit)) })
	}
}

func TestRevivalLossNumber(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"9007199254740993", "9007199254740992", false}, {"1e21", "1e+21", true}, {"1e21", "1000000000000000000000", true},
		{"-0", "0", true}, {"0.1", "0.10", true}, {"0e9223372036854775808", "0", true}, {"-0e-9223372036854775808", "0", true},
		{"1e-9223372036854775808", "0", false}, {"1e9223372036854775808", "1e9223372036854775808", true}, {"1e9223372036854775808", "2e9223372036854775808", false},
		{"1.50", "1.5", true}, {"10", "1e1", true}, {"1." + strings.Repeat("0", 1000001), "1", true}, {"1.0000000000000000001", "1", false},
	} {
		if got := revivalLossNumber(json.Number(c.a), json.Number(c.b)); got != c.want {
			t.Errorf("revivalLossNumber(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// The lone surrogate forms never reach the comparison: a surrogate escape is refused by the read, and a byte sequence that is not
// UTF-8 (a lone 0xff, a WTF-8 surrogate) would read as U+FFFD on both sides, so the lock refuses it by its offset.
func TestRevivalLossRefusesSurrogatesAndInvalidUTF8(t *testing.T) {
	title := func(s string) string { return strings.Replace(revivalLossPlan, `"title":"one"`, `"title":"`+s+`"`, 1) }
	for _, escape := range []string{`\ud800`, `abc\ud83d`} {
		raw := title(escape)
		revivalLossRefused(t, raw, fmt.Sprintf("unpaired JSON surrogate at byte %d would lose stored text", strings.Index(raw, `\ud8`)), false)
	}
	for _, bad := range []string{"a\xffb", "a\xed\xa0\x80b"} {
		raw := title(bad)
		revivalLossRefused(t, raw, fmt.Sprintf("goalplan 'demo' holds invalid UTF-8 at byte %d that this build cannot keep; refusing to rewrite it", strings.Index(raw, bad[1:2])), true)
	}
}

// Decoding keeps the last value of a key an object states twice, so the earlier one would be erased by a write.
func TestRevivalLossRefusesARepeatedKey(t *testing.T) {
	for _, c := range []struct{ name, old, new, path string }{
		{"a top-level key", `"objective":"o"`, `"objective":"first","objective":"o"`, "objective"},
		{"the review rounds of a plan", `"decisions":[`, `"reviewRounds":[],"decisions":[`, "reviewRounds"},
		{"a key of a task", `"id":"t1"`, `"id":"x","id":"t1"`, "workPhases[0].tasks[0].id"},
		{"a key of a review round", `"roundId":"r1"`, `"roundId":"r0","roundId":"r1"`, "reviewRounds[0].roundId"},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := strings.Replace(revivalLossPlan, c.old, c.new, 1)
			if raw == revivalLossPlan {
				t.Fatal("the plan was not edited")
			}
			revivalLossRefused(t, raw, "goalplan 'demo' holds the repeated key "+c.path+" that this build cannot keep; refusing to rewrite it", true)
		})
	}
	for text, want := range map[string]string{`{"a":1,"a":2}`: "a", `{"a":{"b":1,"b":2}}`: "a.b", `[{"a":1},{"a":1,"a":1}]`: "[1].a", `{"x.y":1,"x.y":2}`: `["x.y"]`,
		`{"a":[1,{"b":1}],"c":{"b":1}}`: "", `[]`: "", `5`: ""} {
		if got := revivalLossDuplicate(text); got != want {
			t.Errorf("revivalLossDuplicate(%s) = %q, want %q", text, got, want)
		}
	}
}

func TestRevivalLossRefusesTheRecordedLosses(t *testing.T) {
	want := map[string]string{"phase_criteriaIds_mixed": "workPhases[0].criteriaIds[1]", "extra_keys_dropped": "criteria[0].evil",
		"decision_open_extra_keys_dropped": "decisions[0].evil", "decision_decided_evil_dropped": "decisions[0].evil"}
	for _, name := range []string{"null", "number", "string", "array", "empty_object", "id_missing", "id_number", "title_missing", "title_number", "skipped_with_bad_dependsOn"} {
		want["task_entry_"+name] = "workPhases[0].tasks[1]"
	}
	plans := loadOraclePlans(t).Plans
	for _, name := range slices.Sorted(maps.Keys(want)) {
		t.Run(name, func(t *testing.T) {
			c := plans[name]
			if got := revivalLoss(decodePlan(t, c.Raw)); got != want[name] {
				t.Fatalf("revivalLoss = %q, want %q", got, want[name])
			}
			revivalLossRefused(t, strings.ReplaceAll(c.Raw, "rec-plan", "demo"), revivalLossReason(want[name]), true)
		})
	}
}

// No false refusal: a plan the writer wrote is covered by its own re-encoding, so the lock calls its writer on it again.
func TestRevivalLossLetsWhatTheWriterWroteThrough(t *testing.T) {
	readable, refused := 0, 0
	plans := loadOraclePlans(t).Plans
	for _, name := range slices.Sorted(maps.Keys(plans)) {
		c := plans[name]
		if c.Slug != "rec-plan" {
			continue
		}
		cwd, dir := readWorkspace(t)
		writeReadFile(t, filepath.Join(dir, GoalplanFile), strings.ReplaceAll(c.Raw, "rec-plan", "demo"))
		plan := ReadGoalplan(cwd, "demo")
		if plan == nil {
			continue
		}
		readable++
		if res, _ := revivalLossLock(t, cwd, "demo"); res.Kind != "ok" {
			refused++
		}
		if err := WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(name, err)
		}
		if res, called := revivalLossLock(t, cwd, "demo"); res.Kind != "ok" || !called {
			t.Errorf("%s: the plan the writer wrote was refused: %+v", name, res)
		}
	}
	if readable < 200 || refused < 14 || refused == readable {
		t.Fatalf("%d readable recordings, %d refused as stored", readable, refused)
	}
	t.Logf("%d readable recordings, %d refused as stored", readable, refused)

	web, desktop, two, three := SurfaceWeb, SurfaceDesktop, 2.0, 3.0
	for _, in := range []NewGoalplanInput{
		{Objective: "Ship the export"},
		{Objective: "Ship with criteria", Criteria: []NewGoalplanCriterion{{Scenario: "a", ExpectedEvidence: "e"}, {Scenario: "b", Surface: &web}, {Scenario: "c", Surface: &desktop, Presented: PresentedNative}}},
		{Objective: "Schema two", SchemaVersion: &two},
		{Objective: "Schema three", SchemaVersion: &three},
	} {
		cwd, plan := t.TempDir(), BuildGoalplan(in)
		if err := WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		if res, called := revivalLossLock(t, cwd, plan.Slug); res.Kind != "ok" || !called {
			t.Errorf("%q: the plan the builder made was refused: %+v", in.Objective, res)
		}
	}

	// The recorded builds of the writer tests, each written and locked again. criteria-host is excluded by name: its criterion with an
	// explicitly empty surface is written as "surface":"" and revival drops that key (known-defects.md, CRW-604 section).
	var records struct{ Builds []struct{ ID string } }
	raw, err := os.ReadFile(filepath.Join("testdata", "write", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &records); err != nil || len(records.Builds) != 11 {
		t.Fatalf("recorded builds %d: %v", len(records.Builds), err)
	}
	values := map[string]float64{"zero": 0, "negative": -7, "nan": math.NaN(), "inf": math.Inf(1), "negative-inf": math.Inf(-1), "two": 2, "fraction": 2.9, "three": 3, "future": 99}
	for _, b := range records.Builds {
		if b.ID == "criteria-host" {
			continue
		}
		in := NewGoalplanInput{Objective: "Hello, World!!", Now: func() string { return "2026-01-01T00:00:00.000Z" }}
		if n, ok := values[b.ID]; ok {
			in.SchemaVersion = &n
		}
		cwd, plan := t.TempDir(), BuildGoalplan(in)
		if err := WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		if res, called := revivalLossLock(t, cwd, plan.Slug); res.Kind != "ok" || !called {
			t.Errorf("recorded build %s: the plan the builder made was refused: %+v", b.ID, res)
		}
	}
}

func TestRevivalLossOrderAndShapes(t *testing.T) {
	tasks := func(m map[string]any) []any { return revivalLossPhase(m)["tasks"].([]any) }
	for _, c := range []struct {
		name, want string
		edit       func(m map[string]any)
	}{
		{"the sorted first of two unknown plan keys", "a", func(m map[string]any) { m["b"], m["a"] = 1, 1 }},
		{"criteria sorts before evil", "criteria[0].x", func(m map[string]any) { m["evil"] = 1; revivalLossFirst(m, "criteria")["x"] = 1 }},
		{"criteriaIds sorts before tasks", "workPhases[0].criteriaIds[1]", func(m map[string]any) {
			revivalLossPhase(m)["criteriaIds"] = []any{"c-1", 7}
			delete(revivalLossTask(m, 1), "title")
		}},
		{"a dropped middle task is reported at the first position that differs", "workPhases[0].tasks[1]", func(m map[string]any) {
			revivalLossPhase(m)["tasks"] = []any{tasks(m)[0], 5, tasks(m)[1]}
		}},
		{"a host that is a string", "host", func(m map[string]any) { m["host"] = "external" }},
		{"tasks that are not a list", "workPhases[0].tasks", func(m map[string]any) { revivalLossPhase(m)["tasks"] = "x" }},
		{"criteriaIds that is not a list", "workPhases[0].criteriaIds", func(m map[string]any) { revivalLossPhase(m)["criteriaIds"] = "x" }},
		{"a key that is not an identifier", `["a.b"]`, func(m map[string]any) { m["a.b"] = 1 }},
		{"a key with a newline under a phase", `workPhases[0]["x\ny"]`, func(m map[string]any) { revivalLossPhase(m)["x\ny"] = 1 }},
	} {
		parsed := decodePlan(t, revivalLossEdit(t, c.edit))
		before := compact(t, parsed)
		if got := revivalLoss(parsed); got != c.want {
			t.Errorf("%s: revivalLoss = %q, want %q", c.name, got, c.want)
		}
		if compact(t, parsed) != before {
			t.Errorf("%s: revivalLoss changed its argument", c.name)
		}
	}
	// A plan revival refuses has nothing to rewrite: the lock never reaches the check, and revivalLoss has no loss to name.
	for _, raw := range []string{"null", `"x"`, "[]", "5", `{"objective":"o","slug":"demo","workPhases":"x","criteria":[],"host":{}}`, `{"objective":"o","slug":"demo","workPhases":[5],"criteria":[],"host":{}}`} {
		if got := revivalLoss(decodePlan(t, raw)); got != "" {
			t.Errorf("revivalLoss(%s) = %q", raw, got)
		}
	}
}

func TestRevivalLossCovers(t *testing.T) {
	for _, c := range []struct {
		name      string
		file, enc any
		want      string
	}{
		{"a null array element is not replaced by a value", []any{nil}, []any{"x"}, "a[0]"},
		{"a file array shorter than its re-encoding", []any{}, []any{"x"}, "a"},
		{"a number is not a string", json.Number("1"), "1", "a"},
		{"a boolean is not a string", true, "true", "a"},
		{"a null key is covered by an absent key", map[string]any{"k": nil}, map[string]any{}, ""},
		{"a key the re-encoding lacks", map[string]any{"k": "v"}, map[string]any{}, "a.k"},
		{"an object in place of an array", map[string]any{}, []any{}, "a"},
	} {
		if got := revivalLossCovers(c.file, c.enc, "a"); got != c.want {
			t.Errorf("%s: revivalLossCovers = %q, want %q", c.name, got, c.want)
		}
	}
}
