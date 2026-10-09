package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The overlap fake bridge answers every get_operation from a receipt table keyed by request id, so
// two pre-change attempts over overlapping notice sets can carry different answers in one round. The
// table is a file the test rewrites between rounds, so an answer that was undetermined can settle
// later. It accepts every send and steer, and reports the parent as active so a batch goes at once.
const (
	pumpOverlapReceiptsEnv = "CRW_MANAGE_TEST_OVERLAP_RECEIPTS"
	pumpOverlapLogEnv      = "CRW_MANAGE_TEST_OVERLAP_LOG"
	pumpOverlapFakeRun     = "^TestPumpQueueOverlapFakeBridge$"
	// pumpOverlapDown is a receipt-table key that makes the whole bridge fail to start.
	pumpOverlapDown = "*down*"
)

// The receipt kinds a pre-change attempt can answer with.
const (
	pumpOverlapAccepted     = "accepted"
	pumpOverlapNotAttempted = "not_attempted"
	pumpOverlapRefused      = "refused"
	pumpOverlapUnknown      = "unknown"
	pumpOverlapUnreachable  = "unreachable"
)

var pumpOverlapKinds = []string{pumpOverlapAccepted, pumpOverlapNotAttempted, pumpOverlapRefused, pumpOverlapUnknown, pumpOverlapUnreachable}

// TestPumpQueueOverlapFakeBridge is the overlap fake bridge when this binary is re-executed with a
// receipt table.
func TestPumpQueueOverlapFakeBridge(t *testing.T) {
	path := os.Getenv(pumpOverlapReceiptsEnv)
	if path == "" {
		t.Skip("not the overlap fake bridge")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		os.Exit(91)
	}
	receipts := map[string]string{}
	if err := json.Unmarshal(raw, &receipts); err != nil {
		os.Exit(92)
	}
	if receipts[pumpOverlapDown] != "" {
		os.Exit(3)
	}
	log := os.Getenv(pumpOverlapLogEnv)
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var message map[string]any
		if err := decoder.Decode(&message); err != nil {
			os.Exit(0)
		}
		id, request := message["id"]
		if method, _ := message["method"].(string); method == "initialize" {
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": deliverProtocol, "capabilities": map[string]any{},
				"serverInfo": map[string]any{"name": "overlap-fake-bridge", "version": "1"}}})
			continue
		}
		if !request {
			continue
		}
		params, _ := message["params"].(map[string]any)
		tool, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		deliverFakeLog(log, tool, args)
		var payload map[string]any
		switch tool {
		case deliverToolActive:
			payload = map[string]any{"observation": "active", "activeTurnId": "turn-1"}
		case deliverToolSteer:
			payload = map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}
		case deliverToolSend:
			payload = map[string]any{"status": "accepted", "delivery": "turn_started"}
		case deliverToolOperation:
			requestID, _ := args["request_id"].(string)
			switch receipts[requestID] {
			case pumpOverlapAccepted:
				payload = map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}
			case pumpOverlapNotAttempted:
				payload = map[string]any{"status": "not_attempted"}
			case pumpOverlapRefused:
				payload = map[string]any{"status": "refused"}
			case pumpOverlapUnreachable:
				// The receipt read fails in transport: the bridge answers the call with a JSON-RPC error.
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "the ledger is unreachable"}})
				continue
			default:
				payload = map[string]any{"status": "outcome_unknown"}
			}
		default:
			payload = map[string]any{"status": "outcome_unknown"}
		}
		text, err := json.Marshal(payload)
		if err != nil {
			os.Exit(93)
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(text)}}}})
	}
}

// pumpOverlapBridge writes the overlap fake bridge and returns its path, its receipt table and its
// call log.
func pumpOverlapBridge(t *testing.T) (bridge, receipts, log string) {
	t.Helper()
	dir := t.TempDir()
	receipts = filepath.Join(dir, "receipts.json")
	log = filepath.Join(dir, "calls.jsonl")
	bridge = filepath.Join(dir, "bridge")
	pumpOverlapSetReceipts(t, receipts, map[string]string{})
	script := "#!/bin/sh\n" + pumpOverlapReceiptsEnv + "=" + coreShellQuote(receipts) + " " +
		pumpOverlapLogEnv + "=" + coreShellQuote(log) + " exec " + coreShellQuote(os.Args[0]) +
		" -test.run " + coreShellQuote(pumpOverlapFakeRun) + "\n"
	syscall.ForkLock.RLock()
	err := os.WriteFile(bridge, []byte(script), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return bridge, receipts, log
}

// pumpOverlapSetReceipts replaces the receipt table the fake bridge reads on its next start.
func pumpOverlapSetReceipts(t *testing.T, path string, receipts map[string]string) {
	t.Helper()
	raw, err := json.Marshal(receipts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

// pumpOverlapNotice is one queued notice of an overlap shape: its name, its text on disk and its
// modification time as an offset from the shape's base stamp.
type pumpOverlapNotice struct {
	name, text string
	modified   time.Duration
}

// pumpOverlapRecord is one pre-change ledger record of an overlap shape: the names its id hashes,
// the texts the attempt sent, its stamp as an offset from the base stamp, and the notices it carried
// (the members whose text on disk is still the text it sent). The carried set is declared, not
// derived, so the test's model of who covers what does not depend on the code under test.
type pumpOverlapRecord struct {
	names, texts []string
	created      time.Duration
	carries      []string
}

// pumpOverlapShape is a queue and the overlapping pre-change records left over it.
type pumpOverlapShape struct {
	name    string
	notices []pumpOverlapNotice
	records []pumpOverlapRecord
}

const (
	pumpOverlapA = "aaaaaaaaaaaaaaaa.txt"
	pumpOverlapB = "bbbbbbbbbbbbbbbb.txt"
	pumpOverlapC = "cccccccccccccccc.txt"
	pumpOverlapZ = "zzzzzzzzzzzzzzzz.txt"
)

// pumpOverlapShapes are the overlaps a pre-change queue can leave: two records whose carried sets are
// the same members, nested records, records that share only some members, and records that came from
// the name order and from the oldest-first order; then three records nested and three mixed.
func pumpOverlapShapes() []pumpOverlapShape {
	return []pumpOverlapShape{
		{
			// The reviewer's reproduction: a and c were written again after both attempts, b was not, so
			// both records carried b alone.
			name: "same-members",
			notices: []pumpOverlapNotice{
				{pumpOverlapA, "A-new-after-both", time.Minute},
				{pumpOverlapB, "B-carried-by-both", -time.Minute},
				{pumpOverlapC, "C-new-after-both", time.Minute},
			},
			records: []pumpOverlapRecord{
				{[]string{pumpOverlapA, pumpOverlapB}, []string{"A-old", "B-carried-by-both"}, 0, []string{pumpOverlapB}},
				{[]string{pumpOverlapA, pumpOverlapB, pumpOverlapC}, []string{"A-old", "B-carried-by-both", "C-old"}, 10 * time.Second, []string{pumpOverlapB}},
			},
		},
		{
			name: "nested",
			notices: []pumpOverlapNotice{
				{pumpOverlapA, "A-nested", -time.Minute},
				{pumpOverlapB, "B-nested", -time.Minute},
				{pumpOverlapC, "C-nested", -time.Minute},
			},
			records: []pumpOverlapRecord{
				{[]string{pumpOverlapA, pumpOverlapB}, []string{"A-nested", "B-nested"}, 0, []string{pumpOverlapA, pumpOverlapB}},
				{[]string{pumpOverlapA, pumpOverlapB, pumpOverlapC}, []string{"A-nested", "B-nested", "C-nested"}, 10 * time.Second, []string{pumpOverlapA, pumpOverlapB, pumpOverlapC}},
			},
		},
		{
			// c is the oldest and a the newest: [a b] is a name-ordered prefix, [b c] an oldest-first one.
			name: "partial-overlap",
			notices: []pumpOverlapNotice{
				{pumpOverlapA, "A-partial", -time.Minute},
				{pumpOverlapB, "B-partial", -2 * time.Minute},
				{pumpOverlapC, "C-partial", -3 * time.Minute},
			},
			records: []pumpOverlapRecord{
				{[]string{pumpOverlapA, pumpOverlapB}, []string{"A-partial", "B-partial"}, 0, []string{pumpOverlapA, pumpOverlapB}},
				{[]string{pumpOverlapB, pumpOverlapC}, []string{"B-partial", "C-partial"}, 0, []string{pumpOverlapB, pumpOverlapC}},
			},
		},
		{
			// z sorts last but is the oldest: [z] is an oldest-first set, [a z] the name-ordered one.
			name: "different-order",
			notices: []pumpOverlapNotice{
				{pumpOverlapA, "A-order", -time.Minute},
				{pumpOverlapZ, "Z-order", -3 * time.Minute},
			},
			records: []pumpOverlapRecord{
				{[]string{pumpOverlapZ}, []string{"Z-order"}, -2 * time.Minute, []string{pumpOverlapZ}},
				{[]string{pumpOverlapA, pumpOverlapZ}, []string{"A-order", "Z-order"}, 0, []string{pumpOverlapA, pumpOverlapZ}},
			},
		},
		{
			name: "nested3",
			notices: []pumpOverlapNotice{
				{pumpOverlapA, "A-nested3", -time.Minute},
				{pumpOverlapB, "B-nested3", -time.Minute},
				{pumpOverlapC, "C-nested3", -time.Minute},
			},
			records: []pumpOverlapRecord{
				{[]string{pumpOverlapA}, []string{"A-nested3"}, 0, []string{pumpOverlapA}},
				{[]string{pumpOverlapA, pumpOverlapB}, []string{"A-nested3", "B-nested3"}, 10 * time.Second, []string{pumpOverlapA, pumpOverlapB}},
				{[]string{pumpOverlapA, pumpOverlapB, pumpOverlapC}, []string{"A-nested3", "B-nested3", "C-nested3"}, 20 * time.Second, []string{pumpOverlapA, pumpOverlapB, pumpOverlapC}},
			},
		},
		{
			name: "mixed3",
			notices: []pumpOverlapNotice{
				{pumpOverlapA, "A-mixed3", -time.Minute},
				{pumpOverlapB, "B-mixed3", -2 * time.Minute},
				{pumpOverlapC, "C-mixed3", -3 * time.Minute},
			},
			records: []pumpOverlapRecord{
				{[]string{pumpOverlapA, pumpOverlapB}, []string{"A-mixed3", "B-mixed3"}, 0, []string{pumpOverlapA, pumpOverlapB}},
				{[]string{pumpOverlapB, pumpOverlapC}, []string{"B-mixed3", "C-mixed3"}, 0, []string{pumpOverlapB, pumpOverlapC}},
				{[]string{pumpOverlapA, pumpOverlapB, pumpOverlapC}, []string{"A-mixed3", "B-mixed3", "C-mixed3"}, 0, []string{pumpOverlapA, pumpOverlapB, pumpOverlapC}},
			},
		},
	}
}

// pumpOverlapCombos is every assignment of a receipt kind to n records.
func pumpOverlapCombos(n int) [][]string {
	combos := [][]string{{}}
	for i := 0; i < n; i++ {
		var next [][]string
		for _, combo := range combos {
			for _, kind := range pumpOverlapKinds {
				next = append(next, append(append([]string(nil), combo...), kind))
			}
		}
		combos = next
	}
	return combos
}

// pumpOverlapCoverage classifies each notice under one receipt assignment: delivered when a record
// that carried it answers accepted, pending when none does and one that carried it is undetermined,
// free otherwise (every record that carried it says it never went, or none carried it).
func pumpOverlapCoverage(shape pumpOverlapShape, kinds []string) (delivered, pending map[string]bool) {
	delivered, pending = map[string]bool{}, map[string]bool{}
	for i, record := range shape.records {
		for _, name := range record.carries {
			switch kinds[i] {
			case pumpOverlapAccepted:
				delivered[name] = true
			case pumpOverlapUnknown, pumpOverlapUnreachable:
				pending[name] = true
			}
		}
	}
	for name := range delivered {
		delete(pending, name)
	}
	return delivered, pending
}

// pumpOverlapSentCount is how many deliveries the fake bridge saw that carried text.
func pumpOverlapSentCount(t *testing.T, log, text string) int {
	t.Helper()
	count := 0
	for _, message := range pumpQueueTestSentMessages(t, log) {
		count += strings.Count(message, text)
	}
	return count
}

// pumpOverlapRun sets one shape up, runs rounds of the queue under the first receipt assignment,
// checks I1 and I2, then settles every undetermined receipt (unknown turns accepted, unreachable
// turns not_attempted), runs more rounds and checks I1 and I3: a notice an attempt delivered is
// never sent and is completed, every other notice is sent exactly once, and nothing stays queued or
// pinned.
func pumpOverlapRun(t *testing.T, shape pumpOverlapShape, kinds []string) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, receiptsPath, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	thread := "parent-1"
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	stamp := now.Add(-time.Hour)
	for _, notice := range shape.notices {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, notice.name, notice.text), stamp.Add(notice.modified))
	}
	texts := map[string]string{}
	for _, notice := range shape.notices {
		texts[notice.name] = notice.text
	}
	ids := make([]string, len(shape.records))
	for i, record := range shape.records {
		ids[i] = pumpQueueTestLegacyRecord(t, cfg, thread, record.names, record.texts, stamp.Add(record.created), deliverStateUnknown)
	}
	receipts := map[string]string{}
	for i, id := range ids {
		receipts[id] = kinds[i]
	}
	pumpOverlapSetReceipts(t, receiptsPath, receipts)
	flush := func(rounds int) {
		for round := 0; round < rounds; round++ {
			if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
				t.Fatal(err)
			}
		}
	}
	queued := func(name string) bool {
		_, err := os.Lstat(filepath.Join(dir, name))
		return err == nil
	}
	completed := func(name string) bool {
		_, err := os.Lstat(filepath.Join(dir, pumpSentDir, name))
		return err == nil
	}

	flush(3)
	delivered, pending := pumpOverlapCoverage(shape, kinds)
	for _, notice := range shape.notices {
		sent := pumpOverlapSentCount(t, log, notice.text)
		switch {
		case delivered[notice.name]:
			if sent != 0 {
				t.Errorf("I1: %s was delivered by a pre-change attempt and sent again %d times", notice.name, sent)
			}
			if !completed(notice.name) || queued(notice.name) {
				t.Errorf("I1: %s was delivered by a pre-change attempt and is not completed (sent/ %v, queued %v)", notice.name, completed(notice.name), queued(notice.name))
			}
		case pending[notice.name]:
			if sent != 0 {
				t.Errorf("I2: %s was sent %d times while an attempt that carried it is undetermined", notice.name, sent)
			}
			if !queued(notice.name) {
				t.Errorf("I2: %s left the queue while an attempt that carried it is undetermined", notice.name)
			}
		default:
			if sent > 1 {
				t.Errorf("I3: %s was sent %d times", notice.name, sent)
			}
			if len(pending) == 0 && (sent != 1 || !completed(notice.name)) {
				t.Errorf("I3: %s was sent %d times (sent/ %v), want exactly once", notice.name, sent, completed(notice.name))
			}
		}
	}
	if _, pinned := pumpReview776QueueAttempt(t, cfg, thread); pinned != (len(pending) > 0) {
		t.Errorf("after the first rounds the pin is present=%v, want %v (pending %v)", pinned, len(pending) > 0, pending)
	}
	if t.Failed() {
		return
	}

	settled := map[string]string{}
	for id, kind := range receipts {
		switch kind {
		case pumpOverlapUnknown:
			kind = pumpOverlapAccepted
		case pumpOverlapUnreachable:
			kind = pumpOverlapNotAttempted
		}
		settled[id] = kind
	}
	pumpOverlapSetReceipts(t, receiptsPath, settled)
	final := make([]string, len(kinds))
	for i, id := range ids {
		final[i] = settled[id]
	}
	flush(3)
	delivered, _ = pumpOverlapCoverage(shape, final)
	for _, notice := range shape.notices {
		sent := pumpOverlapSentCount(t, log, notice.text)
		if delivered[notice.name] {
			if sent != 0 {
				t.Errorf("I1: %s was delivered by a pre-change attempt and sent again %d times", notice.name, sent)
			}
		} else if sent != 1 {
			t.Errorf("I3: %s was sent %d times, want exactly once", notice.name, sent)
		}
		if !completed(notice.name) || queued(notice.name) {
			t.Errorf("%s is not completed at the end (sent/ %v, queued %v)", notice.name, completed(notice.name), queued(notice.name))
		}
	}
	if names := pumpQueueTestNames(t, cfg, thread); len(names) != 0 {
		t.Errorf("notices left queued at the end: %v", names)
	}
	if pin, pinned := pumpReview776QueueAttempt(t, cfg, thread); pinned {
		t.Errorf("a pin is left at the end: %v", pin)
	}
	if t.Failed() {
		t.Logf("%s; texts %v; receipts %v then %v; deliveries %q", pumpOverlapDescribe(shape), texts, kinds, final, pumpQueueTestSentMessages(t, log))
	}
}

// Overlapping pre-change records are reconciled as one evidence set. Over two and three overlapping
// records, every receipt answer for every record: a notice any record's receipt shows delivered is
// never sent again and is completed (I1); a notice an undetermined record carried is not sent while
// that record is undetermined (I2); a notice every record that carried it shows never went is sent
// exactly once in a new batch and completed (I3).
func TestPumpQueueOverlappingLegacyRecordsKeepTheInvariants(t *testing.T) {
	for _, shape := range pumpOverlapShapes() {
		for _, kinds := range pumpOverlapCombos(len(shape.records)) {
			t.Run(shape.name+"/"+strings.Join(kinds, "+"), func(t *testing.T) {
				pumpOverlapRun(t, shape, kinds)
			})
		}
	}
}

// The reviewer's reproduction, by name: two unsettled records over [a b] and [a b c], a and c written
// again after both, b not. The longer record's receipt says it never went, the shorter one's says it
// was delivered. b must not be sent again; a and c are new notices and go once.
func TestPumpQueueOverlappingUnsettledRecordsNeverResendADeliveredMember(t *testing.T) {
	pumpOverlapRun(t, pumpOverlapShapes()[0], []string{pumpOverlapAccepted, pumpOverlapNotAttempted})
}

// A bridge that cannot be started answers no receipt, so every overlapping record is undetermined:
// nothing is sent, nothing is completed and the pin holds; once the bridge answers, the evidence set
// is reconciled and every notice goes exactly once or is completed.
func TestPumpQueueOverlappingLegacyRecordsHoldWhileTheBridgeIsDown(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, receiptsPath, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	shape := pumpOverlapShapes()[5]
	thread := "parent-1"
	dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
	stamp := now.Add(-time.Hour)
	for _, notice := range shape.notices {
		pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, notice.name, notice.text), stamp.Add(notice.modified))
	}
	ids := make([]string, len(shape.records))
	for i, record := range shape.records {
		ids[i] = pumpQueueTestLegacyRecord(t, cfg, thread, record.names, record.texts, stamp.Add(record.created), deliverStateUnknown)
	}
	pumpOverlapSetReceipts(t, receiptsPath, map[string]string{pumpOverlapDown: "yes"})
	for round := 0; round < 3; round++ {
		if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
			t.Fatal(err)
		}
	}
	if sent := pumpQueueTestSentMessages(t, log); len(sent) != 0 {
		t.Errorf("a notice was sent while the bridge was down: %q", sent)
	}
	if names := pumpQueueTestNames(t, cfg, thread); len(names) != len(shape.notices) {
		t.Errorf("a notice left the queue while the bridge was down: %v", names)
	}
	// [b c] answers accepted, the others never went: b and c are completed, a goes once.
	pumpOverlapSetReceipts(t, receiptsPath, map[string]string{ids[0]: pumpOverlapNotAttempted, ids[1]: pumpOverlapAccepted, ids[2]: pumpOverlapNotAttempted})
	for round := 0; round < 3; round++ {
		if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
			t.Fatal(err)
		}
	}
	for _, notice := range shape.notices {
		want := 0
		if notice.name == pumpOverlapA {
			want = 1
		}
		if sent := pumpOverlapSentCount(t, log, notice.text); sent != want {
			t.Errorf("%s was sent %d times, want %d", notice.name, sent, want)
		}
		if _, err := os.Lstat(filepath.Join(dir, pumpSentDir, notice.name)); err != nil {
			t.Errorf("%s was not completed: %v", notice.name, err)
		}
	}
	if pin, pinned := pumpReview776QueueAttempt(t, cfg, thread); pinned {
		t.Errorf("a pin is left at the end: %v", pin)
	}
}

// pumpOverlapDescribe names a shape's records for a failure message.
func pumpOverlapDescribe(shape pumpOverlapShape) string {
	var parts []string
	for _, record := range shape.records {
		parts = append(parts, fmt.Sprintf("%v carries %v", record.names, record.carries))
	}
	return strings.Join(parts, "; ")
}
