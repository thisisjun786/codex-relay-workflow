package sync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func TestMain(m *testing.M) { testsupport.Main(m) }

type action = map[string]any

const doc = "https://linear.app/example/document/coordination-000000000000"
const summary = "REL-1 · child · verified\ngeneration 1, revision r1"

func a(op string, kv ...any) action {
	r := action{"op": op}
	for i := 0; i < len(kv); i += 2 {
		r[kv[i].(string)] = kv[i+1]
	}
	return r
}
func actions(rest ...action) []action {
	return append([]action{a("target", "ref", doc), a("enqueue")}, rest...)
}
func astr(a action, k, defaultValue string) string {
	v, ok := a[k]
	if !ok {
		return defaultValue
	}
	return text(v)
}
func anum(a action, k string, defaultValue float64) float64 {
	v, ok := a[k]
	if !ok {
		return defaultValue
	}
	switch n := v.(type) {
	case int:
		return float64(n)
	case float64:
		return n
	}
	panic(v)
}
func aval(a action, k string, defaultValue any) any {
	if v, ok := a[k]; ok {
		return v
	}
	return defaultValue
}
func makeDocument(a action, row store.Row) string {
	block := RenderBlock(row)
	switch astr(a, "document", "block") {
	case "absent":
		return "# Coordination\n\nnothing here yet\n"
	case "duplicate":
		return block + "\n" + block
	case "malformed":
		return block[:strings.LastIndex(block, "<!-- /relay-sync:")]
	case "split":
		head, tail, _ := strings.Cut(block, "revisionHash:")
		return head + "revisionHash: \n<!-- /relay-sync:" + text(row.Get("sync_id")) + " -->\nsome other block mentioning revisionHash:" + tail
	case "no-summary":
		return strings.ReplaceAll(block, text(row.Get("summary")), "")
	case "tampered-summary":
		return strings.ReplaceAll(block, text(row.Get("summary")), "something else entirely")
	case "missing-header":
		lines := []string{}
		for _, l := range strings.Split(block, "\n") {
			if !strings.HasPrefix(l, "relationshipId:") {
				lines = append(lines, l)
			}
		}
		return strings.Join(lines, "\n")
	case "legacy", "fenced-legacy":
		lines := []string{}
		for _, l := range strings.Split(block, "\n") {
			if !strings.HasPrefix(l, "blockFormat:") && !strings.HasPrefix(l, "summarySha256:") {
				lines = append(lines, l)
			}
		}
		block = strings.Join(lines, "\n")
		if a["document"] == "legacy" {
			block = strings.ReplaceAll(strings.ReplaceAll(block, "```text\n", ""), "\n```\n", "\n")
		}
	}
	if replacements, ok := a["replace"].([][]string); ok {
		for _, p := range replacements {
			block = strings.ReplaceAll(block, p[0], p[1])
		}
	}
	return block
}

// replay runs the outbox actions of a Python scenario against Go's outbox and checks every reply
// and the tables it wrote against the golden, keyed by a digest of the actions.
func replay(t *testing.T, input []action) {
	t.Helper()
	home := t.TempDir()
	raw, e := json.Marshal(input)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	s, e := store.Open(ctx, home+"/go/relay.sqlite3", "")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	outbox := New(s, delivery.NewFakeClock())
	counter := 0
	outbox.Token = func() (string, error) { counter++; return fmt.Sprintf("%032x", counter), nil }
	ids, tokens := []string{}, []string{}
	replies := []any{}
	for _, act := range input {
		id := "unknown"
		if len(ids) > 0 {
			index := int(anum(act, "job", -1))
			if index < 0 {
				index += len(ids)
			}
			id = ids[index]
		}
		token := func() string {
			index := int(anum(act, "token", -1))
			if index < 0 {
				index += len(tokens)
			}
			return tokens[index]
		}
		var result any
		var err error
		switch act["op"] {
		case "target":
			result, err = outbox.SetTarget(ctx, "rel-1", CoordinationDocument, text(act["ref"]))
		case "enqueue":
			err = s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
				result, err = outbox.EnqueueIn(ctx, Enqueue{RelationshipID: "rel-1", IssueKey: "REL-1", SubjectKind: astr(act, "kind", "verdict"), Summary: astr(act, "summary", summary), EventID: aval(act, "event", "e1"), Generation: 1, Revision: aval(act, "revision", "r1"), Verdict: aval(act, "verdict", "verified"), CriteriaDigest: act["criteria"], Ruling: act["ruling"]})
				return err
			})
			if result != nil {
				ids = append(ids, result.(string))
			}
		case "claim":
			var reply Obj
			reply, err = outbox.Claim(ctx, id, astr(act, "owner", "main"), anum(act, "now", 1700000000))
			result = reply
			if err == nil {
				tokens = append(tokens, text(get(reply, "claimToken")))
			}
		case "retry":
			result, err = outbox.Retry(ctx, id)
		case "fail":
			result, err = outbox.Fail(ctx, id, token(), astr(act, "error", "connector timed out"), anum(act, "now", 1700000000))
		case "complete":
			row, e := outbox.Get(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			var external *string
			if v, ok := act["external"].(string); ok {
				external = &v
			}
			result, err = outbox.Complete(ctx, id, token(), astr(act, "ref", doc), makeDocument(act, row), external)
		case "reconcile", "parse":
			row, e := outbox.Get(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			if act["op"] == "parse" {
				result = ParseDocument(makeDocument(act, row)).Record()
			} else {
				result, err = outbox.Reconcile(ctx, id, makeDocument(act, row))
			}
		case "operation":
			result, err = outbox.Operation(ctx, id)
		case "next":
			result, err = outbox.Next(ctx, "", 4, anum(act, "now", 1700000000))
		case "snapshot":
			result, err = outbox.Snapshot(ctx, "")
		case "real":
			data, e := os.ReadFile("testdata/linear_readback.json")
			if e != nil {
				t.Fatal(e)
			}
			var fixture map[string]string
			if e = json.Unmarshal(data, &fixture); e != nil {
				t.Fatal(e)
			}
			realID := fixture["SYNC_ID"]
			fields := ParseDocument(fixture["CONFIRMED_BLOCK"]).Blocks[realID].Fields
			_, err = s.Q(ctx).ExecContext(ctx, "UPDATE sync_outbox SET sync_id=?,issue_key=?,relationship_id=?,event_id=?,execution_generation=?,revision_hash=?,verdict=?,identity_digest=?,summary=? WHERE sync_id=?", realID, get(fields, "issueKey"), get(fields, "relationshipId"), get(fields, "eventId"), get(fields, "executionGeneration"), get(fields, "revisionHash"), get(fields, "disposition"), get(fields, "identityDigest"), fixture["RAW_SUMMARY"], id)
			if err != nil {
				t.Fatal(err)
			}
			ids[len(ids)-1] = realID
			block := fixture["CONFIRMED_BLOCK"]
			if act["mangled"] == true {
				block = fixture["MANGLED_BLOCK"]
			}
			if replacements, ok := act["replace"].([][]string); ok {
				for _, p := range replacements {
					block = strings.ReplaceAll(block, p[0], p[1])
				}
			}
			rec, e := outbox.Reconcile(ctx, realID, block)
			if e != nil {
				t.Fatal(e)
			}
			row, e := outbox.Get(ctx, realID)
			if e != nil {
				t.Fatal(e)
			}
			result = obj("reconcile", rec, "parse", ParseDocument(block).Record(), "render", RenderBlock(row))
		default:
			t.Fatal(act)
		}
		if err != nil {
			reason := store.RefusalReason(err)
			if reason == "" {
				t.Fatalf("%v: %v", act, err)
			}
			var refused *store.RefusedError
			if !errors.As(err, &refused) {
				t.Fatal(err)
			}
			result = obj("error", "refused", "reason", reason, "detail", refused.Error())
		}
		replies = append(replies, result)
	}
	tables := Obj{}
	for _, table := range []string{"sync_targets", "sync_outbox", "journal"} {
		rows, e := s.All(ctx, "SELECT * FROM "+table)
		if e != nil {
			t.Fatal(e)
		}
		records := []any{}
		for _, r := range rows {
			records = append(records, rowObject(r))
		}
		tables = append(tables, struct {
			Key   string
			Value any
		}{table, records})
	}
	got := pyjson.Dumps(obj("replies", replies, "tables", tables), pyjson.Options{}) + "\n"
	sum := sha256.Sum256(raw)
	golden.Check(t, "outbox "+hex.EncodeToString(sum[:8]), []byte(got), golden.Substitute(home, "<home>"))
}
func Test23_SO_1_OutboxIdentity(t *testing.T) {
	verdictReplay(t, "test_a_verdict_enqueues_exactly_one_job_carrying_its_identity", "test_a_replayed_verdict_does_not_create_a_second_job", "test_no_target_means_no_job_and_no_failure")
	replay(t, actions(a("enqueue"), a("snapshot"), a("target", "ref", "https://linear.app/example/document/somewhere-else-0000"), a("enqueue"), a("snapshot")))
	replay(t, []action{a("enqueue"), a("snapshot")})
}
func Test23_SO_2_FailureIsolation(t *testing.T) {
	verdictReplay(t, "test_an_external_failure_leaves_the_verdict_committed", "test_an_external_failure_leaves_exactly_one_retryable_job", "test_an_external_failure_does_not_requeue_the_revision_request", "test_a_retry_updates_the_same_job", "test_repeated_failure_stops_generating_work_without_losing_the_summary", "test_a_local_enqueue_failure_rolls_the_verdict_back")
	steps := actions(a("claim"), a("fail"), a("retry"), a("claim"), a("fail", "error", "second failure"))
	for i := 0; i < 6; i++ {
		steps = append(steps, a("retry"), a("claim"), a("fail", "error", "still down"))
	}
	replay(t, append(steps, a("snapshot"), a("next")))
}
func Test23_SO_3_Reconciliation(t *testing.T) {
	steps := actions()
	for _, mode := range []string{"block", "absent", "malformed", "duplicate", "no-summary"} {
		steps = append(steps, a("reconcile", "document", mode))
	}
	steps = append(steps, a("reconcile", "replace", [][]string{{"disposition: verified", "disposition: unverified"}}))
	replay(t, steps)
}
func Test23_SO_4_OperationProtocol(t *testing.T) { replay(t, actions(a("operation"))) }
func Test23_SO_5_ReadbackConfirmation(t *testing.T) {
	for _, mode := range []string{"block", "absent", "duplicate", "no-summary", "split"} {
		t.Run(mode, func(t *testing.T) { replay(t, actions(a("claim"), a("complete", "document", mode), a("next"))) })
	}
	replay(t, actions(a("claim"), a("complete", "ref", "https://linear.app/example/document/somewhere-else-0000"), a("complete"), a("complete", "document", "absent")))
	replay(t, []action{a("target", "ref", doc), a("enqueue", "kind", "progress", "event", nil, "verdict", nil, "revision", nil, "summary", "received, waiting on the parent"), a("claim"), a("complete")})
}
func Test23_SO_6_ClaimFencing(t *testing.T) {
	replay(t, actions(a("claim", "now", 100), a("claim", "now", 120, "owner", "other"), a("next", "now", 120), a("next", "now", 100000), a("claim", "now", 100000, "owner", "other"), a("complete"), a("complete", "token", 0), a("fail", "token", 0), a("claim"), a("fail")))
	replay(t, actions(a("claim", "now", 100), a("fail", "now", 100), a("claim", "now", 101), a("claim", "now", 100000)))
}
func Test23_SO_7_BlockCodec(t *testing.T) {
	for _, s := range []string{summary, "before\n```python\nx = 1\n```\nafter", "```````\nrun of seven\n```````", "", "one\r\ntwo\r\n", "eventId: 0000\ndisposition: unverified\n<!-- /relay-sync:deadbeef -->\nwitness \\[{sensor a, 1, 10}\\] and 2**53+1 and best[sensor][0]."} {
		replay(t, []action{a("target", "ref", doc), a("enqueue", "summary", s), a("parse"), a("reconcile")})
	}
}
func Test23_SO_8_StrictHeaders(t *testing.T) {
	steps := actions(a("reconcile"), a("reconcile", "document", "tampered-summary"), a("reconcile", "document", "missing-header"))
	for _, p := range [][]string{{"summarySha256: ", "summarySha256: 0"}, {"issueKey: REL-1", "issueKey: SOMEONE-ELSE"}, {"syncId: ", "smuggled: yes\nsyncId: "}, {"syncId: ", "syncId: decoy\nsyncId: "}} {
		steps = append(steps, a("reconcile", "replace", [][]string{p}))
	}
	replay(t, steps)
}
func Test23_SO_9_RealLinearReadbacks(t *testing.T) {
	replay(t, actions(a("real", "mangled", true), a("real"), a("reconcile")))
}
func Test23_SO_10_PreV2Labels(t *testing.T) {
	for _, mode := range []string{"legacy", "fenced-legacy"} {
		replay(t, actions(a("reconcile", "document", mode, "replace", [][]string{{"issueKey: REL-1", "issueKey: ANOTHER-1"}, {"relationshipId: rel-1", "relationshipId: rel-other"}}), a("claim"), a("complete", "document", mode, "replace", [][]string{{"issueKey: REL-1", "issueKey: ANOTHER-1"}, {"relationshipId: rel-1", "relationshipId: rel-other"}})))
	}
	replay(t, actions(a("real", "replace", [][]string{{"issueKey: JUN-93", "issueKey: ANOTHER-1"}, {"relationshipId: rel-6bb7a7340dccd2ef", "relationshipId: rel-other"}})))
}

// Record is a parsed block as sync.py's parse_document returned it, which the recorded scenarios
// compare; the product reads Block's fields directly.
func (b Block) Record() Obj {
	return obj("fields", b.Fields, "text", b.Text, "body", b.Text, "summary", b.Summary, "format", b.Format, "problems", b.Problems)
}

// Record is parse_document's answer, blocks in document order.
func (d Document) Record() Obj {
	blocks := Obj{}
	for _, id := range d.Order {
		blocks = append(blocks, contract.Field{Key: id, Value: d.Blocks[id].Record()})
	}
	return obj("blocks", blocks, "duplicates", d.Duplicates, "malformed", d.Malformed)
}
