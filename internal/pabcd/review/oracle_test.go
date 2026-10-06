package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
)

func reviewOracleDecode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func reviewOracleJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return reviewOracleDecode[any](t, raw)
}
func reviewOracleTime(t *testing.T, p *goalplan.Goalplan) {
	t.Helper()
	for i := range p.ReviewRounds {
		r := &p.ReviewRounds[i]
		if r.ClosedAt != nil {
			if _, err := time.Parse("2006-01-02T15:04:05.000Z", *r.ClosedAt); err != nil {
				t.Fatal(err)
			}
			r.ClosedAt = reviewTestPtr(reviewTestTime)
		}
	}
}

// Recorded with Node, replayed without it. JSON object order is not a value.
func TestRecordedOracle(t *testing.T) {
	raw, err := os.ReadFile("testdata/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := reviewOracleDecode[[]struct {
		Name, Op string
		Args     []json.RawMessage
		Expected any
	}](t, raw)
	reviewTestEqual(t, len(cases), 49)
	// The four recorded answers this port changes on purpose (CRW-573, a data-loss defect fixed under the parity rule of
	// 2026-10-03, see known-defects.md): the oracle mints an id that repeats a stored one or is no longer exact. oracle.json keeps
	// what Node answered; the replay expects the refusal, and a tag whose recorded answer already is the refusal fails.
	const stored, limit = "round id %s is already stored: opening another round would overwrite it", "the next round id %s is not an exact integer below 2^53: it could repeat a stored round id"
	changed := map[string]string{
		"order-r9007199254740992":      fmt.Sprintf(stored, "r9007199254740992"),
		"duplicate-id":                 fmt.Sprintf(stored, "r9007199254740992"),
		"order-r100000000000000000000": fmt.Sprintf(stored, "r100000000000000000000"),
		"order-r999999999999999999999": fmt.Sprintf(limit, "r1000000000000000000000"),
	}
	recorded := map[string]bool{}
	for _, c := range cases {
		recorded[c.Name] = true
	}
	for name := range changed {
		if !recorded[name] {
			t.Fatalf("%s is tagged intentionally-changed but is not a recorded case", name)
		}
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Op == "parseSignoff" {
				reviewTestEqual(t, reviewOracleJSON(t, ParseSignoff(reviewOracleDecode[string](t, c.Args[0]))), c.Expected)
				return
			}
			p := reviewOracleDecode[goalplan.Goalplan](t, c.Args[0])
			before := reviewOracleJSON(t, p)
			var actual any
			switch c.Op {
			case "openRound":
				in := reviewOracleDecode[OpenRoundInput](t, c.Args[1])
				in.Now = reviewTestNow
				actual = OpenRound(&p, in)
			case "markLaunching":
				actual = MarkLaunching(&p, reviewOracleDecode[goalplan.ReviewPurpose](t, c.Args[1]), reviewOracleDecode[string](t, c.Args[2]), reviewOracleDecode[string](t, c.Args[3]), nil)
			case "recordVerdict":
				in := reviewOracleDecode[VerdictInput](t, c.Args[1])
				in.Now = reviewTestNow
				actual = RecordVerdict(&p, in)
			case "abortRound":
				r := AbortRound(&p, reviewOracleDecode[goalplan.ReviewPurpose](t, c.Args[1]), reviewOracleDecode[string](t, c.Args[2]))
				if r.Kind == OK {
					reviewOracleTime(t, r.Plan)
					r.Round.ClosedAt = reviewTestPtr(reviewTestTime)
				}
				actual = r
			case "supersedeStaleRounds":
				out, closed := SupersedeStaleRounds(&p, reviewOracleDecode[goalplan.ReviewPurpose](t, c.Args[1]), reviewOracleDecode[string](t, c.Args[2]), reviewOracleDecode[string](t, c.Args[3]))
				if out != &p {
					reviewOracleTime(t, out)
				}
				actual = map[string]any{"plan": out, "closed": closed}
			case "revivedOwner":
				cwd := t.TempDir()
				dir, err := goalplan.GoalplanDir(cwd, p.Slug)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(dir, goalplan.GoalplanFile), c.Args[0], 0600); err != nil {
					t.Fatal(err)
				}
				revived := goalplan.ReadGoalplan(cwd, p.Slug)
				if revived == nil {
					t.Fatal("revival failed")
				}
				var owner any
				if revived.ReviewRounds[0].OwnerSessionID != "" {
					owner = revived.ReviewRounds[0].OwnerSessionID
				}
				_, closed := SupersedeStaleRounds(revived, goalplan.PurposePlanAudit, "", "old")
				actual = map[string]any{"owner": owner, "closed": closed}
			default:
				t.Fatalf("unknown oracle operation %s", c.Op)
			}
			want := c.Expected
			if reason, ok := changed[c.Name]; ok {
				if want = map[string]any{"kind": "invalid_input", "reason": reason}; reflect.DeepEqual(want, c.Expected) {
					t.Fatal("tagged intentionally-changed but the recorded answer is the new one")
				}
			}
			reviewTestEqual(t, reviewOracleJSON(t, actual), want)
			reviewTestEqual(t, reviewOracleJSON(t, p), before)
		})
	}
}
