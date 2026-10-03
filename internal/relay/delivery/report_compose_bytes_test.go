package delivery

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	py "github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// This test freezes the whole bytes of the two work-report messages (the parent's verification
// request and the child's revision request) and the text of every error building one raises, over
// a table of inputs. Its goldens were recorded on the code before the two compositions shared
// their status table, header lines, unresolved block and message-id derivation, and they stay
// as recorded: a refactor of either composition must leave every byte of them alone.
//
// The input is in memory (Row, Obj, the report map reportRows builds); the completion's only
// outside read is its relationship and scope lookups, answered by a temporary store of its own.

// composed is one composition's input.
type composed struct {
	rid     string
	receipt Obj
	report  map[string]any
}

func (c *composed) rec(key, doc string)       { c.receipt = c.receipt.Set(key, py.Decode(doc)) }
func (c *composed) doc(key, doc string)       { c.report[key] = py.Decode(doc) }
func (c *composed) hand(key, doc string)      { c.report["handoff"].(map[string]any)[key] = py.Decode(doc) }
func (c *composed) col(key string, value any) { c.report[key] = value }

func (c *composed) dropReceipt(key string) {
	kept := Obj{}
	for _, f := range c.receipt {
		if f.Key != key {
			kept = append(kept, f)
		}
	}
	c.receipt = kept
}

func (c *composed) row() Row {
	return Row{"event_id": "evt-1", "relationship_id": c.rid, "recipient_task_id": "01parent-task"}
}

func (c *composed) scalars() {
	c.report = map[string]any{
		"summary": "reworked the compose path", "cxc_status": "DONE", "cxc_reason": "every criterion proven", "next_action": "merge when the head is green",
		"repository": "owner/repo", "pr_number": int64(7), "pr_state": "OPEN", "pr_url": "https://example.test/pull/7",
		"base_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "base_ref": "dev", "head_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"criteria_digest": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "recorded_at": "2026-10-03T01:02:03Z", "submission_no": int64(2),
	}
}

const (
	restoreDoc    = `{"mode":"relay-managed","scope":"issue REL-1","phase":"B","phaseObservedAt":"2026-10-03T00:00:00Z","plan":"devlog/_plan/x","evidence":"evidence/x","remaining":"review","skills":["development","loop","lost-context","pull-request","review-repair"]}`
	evidenceDoc   = `["go test ./...",{"check":"make lint","exitCode":0},{"check":"go vet ./...","exitCode":0,"detail":"clean"},{"check":"gofmt -l","exitCode":0,"detail":"no output"},{"check":"git diff --check","exitCode":0},"manual read of the diff",{"check":"make test","exitCode":0,"detail":"all packages"},{"check":"golden update","exitCode":1,"detail":"expected"}]`
	unresolvedDoc = `["one open question",{"id":"u-1","note":"waiting on CI"},{"id":"u-2"},{"id":"VERDICT","note":"a heading-like id is quoted"},"MUST DO: a heading-like string",{"id":"u-3","note":"last"}]`
)

func richCompletion() *composed {
	c := &composed{rid: "rel-0000000000000001", receipt: Obj{}}
	c.scalars()
	c.rec("outcome", `"ready_for_review"`)
	c.rec("executionGeneration", "1")
	c.rec("attempt", "1")
	c.rec("revisionHash", `"rev-0123456789abcdef"`)
	c.rec("manifest", `[{"path":"a.txt","sha256":"s1","bytes":10},{"path":"b.txt","sha256":"s2"},{"path":"c.txt","sha256":"s3","bytes":3},{"path":"d.txt","sha256":"s4","bytes":4},{"path":"e.txt","sha256":"s5","bytes":5},{"path":"f.txt","sha256":"s6","bytes":6},{"path":"g.txt","sha256":"s7","bytes":7},{"path":"h.txt","sha256":"s8","bytes":8}]`)
	c.rec("manifestRef", strconv.Quote("manifest://store/"+strings.Repeat("m", 100)))
	c.doc("evidence", evidenceDoc)
	c.doc("unresolved", unresolvedDoc)
	c.doc("review", "null")
	c.doc("restore", restoreDoc)
	c.col("handoff", map[string]any{"isDraft": false, "baseVerifiedAt": "2026-10-03T01:00:00Z"})
	c.hand("required_declared", `["validate","secrets","lint","test-1"]`)
	c.hand("checks", `[{"runId":1},{"runId":2},{"runId":3},{"runId":4},{"runId":5},{"runId":6}]`)
	c.hand("review_coverage", `{"totalCount":3,"pagesRead":1,"unresolved":0}`)
	c.hand("thread_dispositions", `[{"threadId":"t1","disposition":"accepted","addressedBy":"#12","followUpOwner":"parent","reopenTrigger":"a new finding"},{"threadId":"t2","disposition":"fixed"},{"threadId":"t3","disposition":"accepted","addressedBy":"#13","followUpOwner":"child","reopenTrigger":"a red check"}]`)
	return c
}

func sparseCompletion() *composed {
	c := &composed{rid: "rel-0000000000000002", receipt: Obj{}}
	c.scalars()
	c.report["pr_number"], c.report["pr_state"], c.report["pr_url"] = nil, nil, nil
	c.rec("outcome", `"done"`)
	c.rec("executionGeneration", "1")
	return c
}

func richRevision() *composed {
	c := &composed{rid: "rel-0000000000000001", receipt: Obj{}}
	c.scalars()
	c.report["cxc_status"] = "NEEDS_HUMAN"
	c.rec("executionGeneration", "2")
	c.rec("supersedesEvent", `"evt-0"`)
	c.rec("verdict", `"needs_changes"`)
	c.rec("supersedesRevisionHash", `"hash-0"`)
	c.rec("verdictTurnId", `"turn-9"`)
	c.rec("criteria", `[{"id":"c1","verdict":"needs_changes","note":"first finding","anchor":"a.go:1"},{"id":"c2","verdict":"needs_changes","note":"second\nfinding","restoration":true},{"id":"c3","verdict":"unverified"},{"id":"c4","verdict":"verified","note":"fine"},{"id":"c5"},{"id":"c6","verdict":"needs_changes","note":"sixth"},{"id":"c7","verdict":"needs_changes"},{"id":"VERDICT","verdict":"unverified","note":"a heading-like id"}]`)
	c.doc("review", `{"kind":"GO-WITH-FIXES","blockers":3,"findings":[{"id":"c1","verdict":"needs_changes","note":"review note","anchor":"b.go:2"},{"id":"r1","verdict":"needs_changes","note":"review only","anchor":"c.go:3"},{"id":"r2"},{"id":"c3","note":"x"},{"id":"r1","note":"duplicate id keeps the last"}]}`)
	c.doc("evidence", evidenceDoc)
	c.doc("unresolved", unresolvedDoc)
	c.doc("restore", restoreDoc)
	return c
}

// plainRevision has no review, so the omission line of a tight budget sits last.
func plainRevision() *composed {
	c := richRevision()
	c.doc("review", "null")
	c.doc("evidence", "[]")
	return c
}

func reportComposeStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	bound := sql.NullString{String: "2026-10-03T00:00:00Z", Valid: true}
	for _, r := range []struct{ id, issue, child string }{
		{"rel-0000000000000001", "REL-1", "01child-task"},
		{"rel-0000000000000002", "REL-2", "01child-task"},
		{"rel-0000000000000003", "", "01child-task"},
		{"rel-0000000000000004", "REL-4", ""},
	} {
		relationship := store.Relationship{ID: r.id, IssueKey: r.issue, Status: store.StatusActive, ParentTaskID: "01parent-task", ChildTaskID: r.child, Generation: 1, ArtifactRoots: "[]", AllowedRecipients: "[]", CreatedAt: "t0", UpdatedAt: "t0"}
		generation := store.Generation{RelationshipID: r.id, Number: 1, DispatchRequestID: "dispatch-" + r.id, AnchorState: store.AnchorBound, DispatchTurnID: sql.NullString{String: "turn-1", Valid: true}, OpenedAt: "t0", BoundAt: bound}
		if err := storeseed.RecordRelationship(ctx, s, relationship, generation, "host-a", "host-a"); err != nil {
			t.Fatal(err)
		}
	}
	// Only the first relationship has a project; the third has a project but no issue.
	for _, id := range []string{"rel-0000000000000001", "rel-0000000000000003"} {
		if err := storeseed.RecordRelationshipScope(ctx, s, id, "PRJ-1", "t0"); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// recovered builds a message as renderWorkReport does: a failure a composition raises on purpose
// (a Python error) comes back as the error, any other panic stays a panic.
func recovered(compose func() (string, error)) (text string, err error) {
	defer py.RecoverPython(&err)
	return compose()
}

type kind struct {
	name    string
	compose func(c *composed, budget int) (string, error)
}

func kinds(s *store.Store) (completion, revision kind) {
	completion = kind{"completion", func(c *composed, budget int) (string, error) {
		return recovered(func() (string, error) {
			return composeWorkCompletion(context.Background(), s, c.row(), c.receipt, "req-1", c.report, budget)
		})
	}}
	revision = kind{"revision", func(c *composed, budget int) (string, error) {
		return recovered(func() (string, error) { return composeWorkRevision(c.row(), c.receipt, "req-1", c.report, budget) })
	}}
	return
}

// describe is what a golden holds for one build: the message, or the error's text. A budget
// refusal names its budget, so that number is masked and checked against the budget instead.
func describe(k kind, c *composed, budget int) string {
	text, err := k.compose(c, budget)
	if err == nil {
		return text
	}
	return "error: " + strings.Replace(err.Error(), "budget of "+strconv.Itoa(budget)+" bytes", "budget of <budget> bytes", 1)
}

// smallest is the least budget that still composes.
func smallest(k kind, c *composed) int {
	for budget := 0; ; budget++ {
		if _, err := k.compose(c, budget); err == nil {
			return budget
		}
	}
}

// scan composes every budget from the message's full length down to 0 and checks one golden per
// run of equal results: each optional section dropped in rank order, each "... N more" cut of an
// essential section, and the refusal, with no budget chosen by hand.
func scan(t *testing.T, name string, k kind, c *composed) {
	t.Helper()
	full, err := k.compose(c, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	type run struct {
		high, low int
		value     string
	}
	var runs []run
	for budget := len(full); budget >= 0; budget-- {
		value := describe(k, c, budget)
		if n := len(runs); n > 0 && runs[n-1].value == value {
			runs[n-1].low = budget
			continue
		}
		runs = append(runs, run{budget, budget, value})
	}
	for _, r := range runs {
		golden.Check(t, fmt.Sprintf("%s budgets %d-%d", name, r.high, r.low), []byte(r.value))
	}
	t.Logf("%s: %d distinct results over %d budgets", name, len(runs), len(full)+1)
}

type row struct {
	name string
	edit func(c *composed)
}

func checkRows(t *testing.T, k kind, base func() *composed, rows []row) {
	t.Helper()
	for _, r := range rows {
		for _, budget := range []int{6000, 900} {
			c := base()
			r.edit(c)
			golden.Check(t, fmt.Sprintf("%s %s @%d", k.name, r.name, budget), []byte(describe(k, c, budget)))
		}
	}
}

func TestReportComposeBytes(t *testing.T) {
	t.Parallel()
	completion, revision := kinds(reportComposeStore(t))
	unknownSkill := `{"skills":["unknown"]}`
	surrogate := "a" + string(rune(92)) + "ud800b"

	t.Run("budget scan", func(t *testing.T) {
		scan(t, "completion rich", completion, richCompletion())
		scan(t, "completion sparse", completion, sparseCompletion())
		scan(t, "revision rich", revision, richRevision())
		scan(t, "revision without a review", revision, plainRevision())
	})

	t.Run("status matrix", func(t *testing.T) {
		for _, k := range []struct {
			kind kind
			base func() *composed
		}{{completion, richCompletion}, {revision, richRevision}} {
			for _, status := range []any{"DONE", "NOOP", "BLOCKED", "UNSAFE", "NEEDS_HUMAN", "BUDGET_EXHAUSTED", "IN_PROGRESS", nil} {
				at := func() *composed { c := k.base(); c.col("cxc_status", status); return c }
				least := smallest(k.kind, at())
				for _, budget := range []int{6000, 1200, least, least - 1} {
					key := fmt.Sprintf("%s %v @%d", k.kind.name, status, budget)
					golden.Check(t, key, []byte(describe(k.kind, at(), budget)))
				}
			}
		}
	})

	t.Run("completion rows", func(t *testing.T) {
		checkRows(t, completion, richCompletion, []row{
			{"as recorded", func(c *composed) {}},
			{"outcome blocked_needs_input", func(c *composed) { c.rec("outcome", `"blocked_needs_input"`) }},
			{"outcome other", func(c *composed) { c.rec("outcome", `"failed"`) }},
			{"outcome absent", func(c *composed) { c.dropReceipt("outcome") }},
			{"relationship issue only", func(c *composed) { c.rid = "rel-0000000000000002" }},
			{"relationship without an issue", func(c *composed) { c.rid = "rel-0000000000000003" }},
			{"relationship without a child", func(c *composed) { c.rid = "rel-0000000000000004" }},
			{"relationship absent", func(c *composed) { c.rid = "rel-0000000000000099" }},
			{"evidence absent", func(c *composed) { c.col("evidence", nil) }},
			{"evidence empty", func(c *composed) { c.doc("evidence", "[]") }},
			{"evidence dict without check", func(c *composed) { c.doc("evidence", `[{"exitCode":0}]`) }},
			{"evidence dict with null check", func(c *composed) { c.doc("evidence", `[{"check":null,"exitCode":3,"detail":"d"}]`) }},
			{"unresolved absent", func(c *composed) { c.col("unresolved", nil) }},
			{"unresolved empty", func(c *composed) { c.doc("unresolved", "[]") }},
			{"submission as text", func(c *composed) { c.col("submission_no", "2") }},
			{"submission absent", func(c *composed) { c.col("submission_no", nil) }},
			{"pull request absent", func(c *composed) { c.col("pr_number", nil); c.col("pr_state", nil); c.col("pr_url", nil) }},
			{"pull request without state or url", func(c *composed) { c.col("pr_state", ""); c.col("pr_url", nil) }},
			{"base sha only", func(c *composed) { c.col("base_ref", nil) }},
			{"base ref only", func(c *composed) { c.col("base_sha", nil) }},
			{"no base, head or criteria", func(c *composed) {
				c.col("base_ref", nil)
				c.col("base_sha", nil)
				c.col("head_sha", "")
				c.col("criteria_digest", nil)
			}},
			{"handoff absent", func(c *composed) { delete(c.report, "handoff") }},
			{"handoff required none", func(c *composed) { c.hand("required_declared", "[]") }},
			{"handoff required as text", func(c *composed) { c.hand("required_declared", `"validate"`) }},
			{"handoff required as object", func(c *composed) { c.hand("required_declared", `{"validate":1}`) }},
			{"handoff without checks or coverage", func(c *composed) {
				c.hand("checks", "null")
				c.hand("review_coverage", "null")
				c.hand("thread_dispositions", "null")
			}},
			{"manifest absent", func(c *composed) { c.dropReceipt("manifest") }},
			{"manifest empty", func(c *composed) { c.rec("manifest", "[]") }},
			{"manifest ref absent", func(c *composed) { c.dropReceipt("manifestRef") }},
			{"manifest ref 240 bytes", func(c *composed) { c.rec("manifestRef", strconv.Quote(strings.Repeat("r", 240))) }},
			{"manifest ref 241 bytes", func(c *composed) { c.rec("manifestRef", strconv.Quote(strings.Repeat("r", 241))) }},
			{"manifest ref cut inside a character", func(c *composed) {
				c.rec("manifestRef", strconv.Quote(strings.Repeat("r", 239)+strings.Repeat("é", 5)))
			}},
			{"manifest ref invalid utf-8", func(c *composed) { c.receipt = c.receipt.Set("manifestRef", "bad\xff\xfe") }},
			{"manifest ref lone surrogate", func(c *composed) { c.receipt = c.receipt.Set("manifestRef", surrogate) }},
			{"restore absent", func(c *composed) { c.col("restore", nil) }},
			{"restore only a mode", func(c *composed) { c.doc("restore", `{"mode":"direct"}`) }},
			{"restore skills empty", func(c *composed) { c.doc("restore", `{"mode":"direct","skills":[]}`) }},
		})
		checkRows(t, completion, sparseCompletion, []row{{"sparse as recorded", func(c *composed) {}}})
	})

	t.Run("revision rows", func(t *testing.T) {
		checkRows(t, revision, richRevision, []row{
			{"as recorded", func(c *composed) {}},
			{"no findings and no review", func(c *composed) { c.dropReceipt("criteria"); c.col("review", nil) }},
			{"receipt findings only", func(c *composed) { c.col("review", nil) }},
			{"review findings only", func(c *composed) { c.dropReceipt("criteria") }},
			{"review without findings", func(c *composed) { c.doc("review", `{"kind":"FAIL"}`) }},
			{"every finding verified", func(c *composed) {
				c.rec("criteria", `[{"id":"c1","verdict":"verified"},{"id":"c2","verdict":"verified"}]`)
				c.col("review", nil)
			}},
			{"unverified findings only", func(c *composed) { c.rec("criteria", `[{"id":"c1","verdict":"unverified"}]`); c.col("review", nil) }},
			{"undecided findings only", func(c *composed) { c.rec("criteria", `[{"id":"c1"},{"id":"c2","note":"n"}]`); c.col("review", nil) }},
			{"one undecided finding", func(c *composed) { c.rec("criteria", `[{"id":"c1"}]`); c.col("review", nil) }},
			{"review PASS", func(c *composed) { c.doc("review", `{"kind":"PASS","findings":[{"id":"c1","note":"n"}]}`) }},
			{"review FAIL", func(c *composed) { c.doc("review", `{"kind":"FAIL","findings":[{"id":"x1","note":"n"}]}`) }},
			{"review GO-WITH-FIXES 1", func(c *composed) { c.doc("review", `{"kind":"GO-WITH-FIXES","blockers":1}`) }},
			{"review GO-WITH-FIXES 2", func(c *composed) { c.doc("review", `{"kind":"GO-WITH-FIXES","blockers":2}`) }},
			{"review GO-WITH-FIXES 9999", func(c *composed) { c.doc("review", `{"kind":"GO-WITH-FIXES","blockers":9999}`) }},
			{"scope without pull request", func(c *composed) { c.col("pr_number", nil) }},
			{"scope without base, head, criteria", func(c *composed) { c.col("base_sha", nil); c.col("head_sha", nil); c.col("criteria_digest", nil) }},
			{"scope base ref only", func(c *composed) { c.col("base_sha", "") }},
			{"generation absent", func(c *composed) { c.dropReceipt("executionGeneration") }},
			{"evidence absent", func(c *composed) { c.col("evidence", nil) }},
			{"evidence dicts without check", func(c *composed) { c.doc("evidence", `[{"exitCode":0},{"check":"c"}]`) }},
			{"unresolved absent", func(c *composed) { c.col("unresolved", nil) }},
			{"restore absent", func(c *composed) { c.col("restore", nil) }},
			{"submission as text", func(c *composed) { c.col("submission_no", "2") }},
			{"submission absent", func(c *composed) { c.col("submission_no", nil) }},
		})
	})

	t.Run("failures", func(t *testing.T) {
		completionFailures := []row{
			{"evidence a number", func(c *composed) { c.doc("evidence", "7") }},
			{"evidence item a number", func(c *composed) { c.doc("evidence", "[3]") }},
			{"unresolved item a number", func(c *composed) { c.doc("unresolved", "[5]") }},
			{"review coverage a number", func(c *composed) { c.hand("review_coverage", "3") }},
			{"required a number", func(c *composed) { c.hand("required_declared", "3") }},
			{"required item a number", func(c *composed) { c.hand("required_declared", "[3]") }},
			{"checks a number", func(c *composed) { c.hand("checks", "5") }},
			{"thread dispositions a number", func(c *composed) { c.hand("thread_dispositions", "1") }},
			{"thread disposition item a number", func(c *composed) { c.hand("thread_dispositions", "[1]") }},
			{"manifest a number", func(c *composed) { c.rec("manifest", "2") }},
			{"manifest item a number", func(c *composed) { c.rec("manifest", "[1]") }},
			{"manifest item empty", func(c *composed) { c.rec("manifest", "[{}]") }},
			{"manifest item without sha256", func(c *composed) { c.rec("manifest", `[{"path":"x"}]`) }},
			{"restore a number", func(c *composed) { c.doc("restore", "5") }},
			{"restore skills a number", func(c *composed) { c.doc("restore", `{"skills":3}`) }},
			{"restore skill unhashable", func(c *composed) { c.doc("restore", `{"skills":[["x"]]}`) }},
			{"restore skill unknown", func(c *composed) { c.doc("restore", unknownSkill) }},
			// Two faults: the one the composition reaches first is the one reported.
			{"order evidence before restore", func(c *composed) { c.doc("evidence", "7"); c.doc("restore", "5") }},
			{"order evidence before unresolved", func(c *composed) { c.doc("evidence", "7"); c.doc("unresolved", "[5]") }},
			{"order unresolved before manifest", func(c *composed) { c.doc("unresolved", "[5]"); c.rec("manifest", "2") }},
			{"order coverage before required", func(c *composed) { c.hand("review_coverage", "3"); c.hand("required_declared", "3") }},
			{"order required before checks", func(c *composed) { c.hand("required_declared", "[3]"); c.hand("checks", "5") }},
			{"order checks before dispositions", func(c *composed) { c.hand("checks", "5"); c.hand("thread_dispositions", "1") }},
			{"order handoff before manifest", func(c *composed) { c.hand("checks", "5"); c.rec("manifest", "[{}]") }},
			{"order manifest path before sha256", func(c *composed) { c.rec("manifest", "[{}]") }},
			{"order manifest before restore", func(c *composed) { c.rec("manifest", "[{}]"); c.doc("restore", unknownSkill) }},
		}
		for _, r := range completionFailures {
			c := richCompletion()
			r.edit(c)
			golden.Check(t, "completion "+r.name, []byte(describe(completion, c, 6000)))
		}
		// A budget too small for the required parts is refused once every build fault is passed.
		golden.Check(t, "completion refusal", []byte(describe(completion, richCompletion(), 10)))

		revisionFailures := []row{
			{"review a number", func(c *composed) { c.col("review", int64(3)) }},
			{"review findings a number", func(c *composed) { c.doc("review", `{"kind":"PASS","findings":3}`) }},
			{"review finding without an id", func(c *composed) { c.doc("review", `{"kind":"PASS","findings":[{"x":1}]}`) }},
			{"review finding id unhashable", func(c *composed) { c.doc("review", `{"kind":"PASS","findings":[{"id":[1]}]}`) }},
			{"review finding a number", func(c *composed) { c.doc("review", `{"kind":"PASS","findings":[4]}`) }},
			{"review without a kind", func(c *composed) { c.doc("review", `{"blockers":1}`) }},
			{"review kind unknown", func(c *composed) { c.doc("review", `{"kind":"MAYBE"}`) }},
			{"GO-WITH-FIXES blockers zero", func(c *composed) { c.doc("review", `{"kind":"GO-WITH-FIXES","blockers":0}`) }},
			{"GO-WITH-FIXES blockers negative", func(c *composed) { c.doc("review", `{"kind":"GO-WITH-FIXES","blockers":-2}`) }},
			{"GO-WITH-FIXES blockers absent", func(c *composed) { c.doc("review", `{"kind":"GO-WITH-FIXES"}`) }},
			{"GO-WITH-FIXES blockers 10000", func(c *composed) { c.doc("review", `{"kind":"GO-WITH-FIXES","blockers":10000}`) }},
			{"GO-WITH-FIXES blockers a boolean", func(c *composed) { c.doc("review", `{"kind":"GO-WITH-FIXES","blockers":true}`) }},
			{"GO-WITH-FIXES blockers a fraction", func(c *composed) { c.doc("review", `{"kind":"GO-WITH-FIXES","blockers":1.5}`) }},
			{"PASS with blockers", func(c *composed) { c.doc("review", `{"kind":"PASS","blockers":1}`) }},
			{"FAIL with blockers zero", func(c *composed) { c.doc("review", `{"kind":"FAIL","blockers":0}`) }},
			{"evidence a number", func(c *composed) { c.doc("evidence", "7") }},
			{"evidence item a number", func(c *composed) { c.doc("evidence", "[3]") }},
			{"unresolved item a number", func(c *composed) { c.doc("unresolved", "[5]") }},
			{"restore a number", func(c *composed) { c.doc("restore", "5") }},
			{"restore skill unknown", func(c *composed) { c.doc("restore", unknownSkill) }},
			// Two faults: the one the composition reaches first is the one reported.
			{"order review before evidence", func(c *composed) { c.col("review", int64(3)); c.doc("evidence", "7") }},
			{"order findings before evidence", func(c *composed) { c.doc("review", `{"kind":"PASS","findings":[{"id":[1]}]}`); c.doc("evidence", "7") }},
			{"order finding id before finding shape", func(c *composed) { c.doc("review", `{"kind":"PASS","findings":[{"x":1},4]}`) }},
			{"order evidence before unresolved", func(c *composed) { c.doc("evidence", "7"); c.doc("unresolved", "[5]") }},
			{"order unresolved before restore", func(c *composed) { c.doc("unresolved", "[5]"); c.doc("restore", `{"skills":3}`) }},
			{"order restore before verdict", func(c *composed) { c.doc("restore", unknownSkill); c.doc("review", `{"kind":"MAYBE"}`) }},
		}
		for _, r := range revisionFailures {
			c := richRevision()
			r.edit(c)
			golden.Check(t, "revision "+r.name, []byte(describe(revision, c, 6000)))
		}
		golden.Check(t, "revision refusal", []byte(describe(revision, richRevision(), 10)))
		verdictBeforeRefusal := richRevision()
		verdictBeforeRefusal.doc("review", `{"kind":"MAYBE"}`)
		golden.Check(t, "revision verdict before refusal", []byte(describe(revision, verdictBeforeRefusal, 10)))
	})
}
