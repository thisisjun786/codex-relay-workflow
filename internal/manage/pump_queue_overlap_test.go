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
			// both records carried b alone. Neither record can prove the text b holds is the text it sent
			// (its digest covers a's and c's old texts, which are gone), so either one holds the thread.
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
			// A record over [a b] whose a was written again after it carried b by time only; the record over
			// [b] alone carries b's text on disk and proves or disproves a delivery of it by itself.
			name: "partial-and-whole",
			notices: []pumpOverlapNotice{
				{pumpOverlapA, "A-new-after-both", time.Minute},
				{pumpOverlapB, "B-partial-and-whole", -time.Minute},
			},
			records: []pumpOverlapRecord{
				{[]string{pumpOverlapA, pumpOverlapB}, []string{"A-old", "B-partial-and-whole"}, 0, []string{pumpOverlapB}},
				{[]string{pumpOverlapB}, []string{"B-partial-and-whole"}, 0, []string{pumpOverlapB}},
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

// pumpOverlapShapeNamed is the overlap shape with the given name.
func pumpOverlapShapeNamed(t *testing.T, name string) pumpOverlapShape {
	t.Helper()
	for _, shape := range pumpOverlapShapes() {
		if shape.name == name {
			return shape
		}
	}
	t.Fatalf("no overlap shape %q", name)
	return pumpOverlapShape{}
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

// pumpOverlapProvable reports whether a record proves which text it sent: it carried every member of
// its set and each member's text on disk is the text it sent. A record that carried only some members
// (the others were written again after it) cannot: its digest covers texts that are gone, and a member
// older than its stamp may still have been replaced after the old pump read it.
func pumpOverlapProvable(shape pumpOverlapShape, record pumpOverlapRecord) bool {
	if len(record.carries) != len(record.names) {
		return false
	}
	onDisk := map[string]string{}
	for _, notice := range shape.notices {
		onDisk[notice.name] = notice.text
	}
	for i, name := range record.names {
		if onDisk[name] != record.texts[i] {
			return false
		}
	}
	return true
}

// pumpOverlapCoverage classifies each notice under one receipt assignment: delivered when a provable
// record that carried it answers accepted; pending when it is not delivered and a provable record that
// carried it is undetermined; free otherwise (every record that carried it says it never went, or none
// carried it). A record that cannot prove its text holds the whole thread whatever it answers: then
// hold is true and every notice that is not delivered is held, never sent, for the operator.
func pumpOverlapCoverage(shape pumpOverlapShape, kinds []string) (delivered, pending, held map[string]bool, hold bool) {
	delivered, pending, held = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, record := range shape.records {
		if !pumpOverlapProvable(shape, record) {
			hold = true
			continue
		}
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
	if hold {
		pending = map[string]bool{}
		for _, notice := range shape.notices {
			if !delivered[notice.name] {
				held[notice.name] = true
			}
		}
	}
	return delivered, pending, held, hold
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
// turns not_attempted), runs more rounds and checks I1 and I3: a notice a provable attempt delivered
// is never sent and is completed; while any attempt cannot prove its text, every other notice is never
// sent and stays queued under a held pin, whatever that attempt answers; otherwise every other notice
// is sent exactly once, and nothing else stays queued or pinned.
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
	delivered, pending, held, hold := pumpOverlapCoverage(shape, kinds)
	waits := len(pending) > 0 || hold
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
		case held[notice.name]:
			if sent != 0 || completed(notice.name) || !queued(notice.name) {
				t.Errorf("I1 (unprovable): %s was sent %d times, completed %v, queued %v; want held in the queue", notice.name, sent, completed(notice.name), queued(notice.name))
			}
		default:
			if waits && (sent != 0 || !queued(notice.name)) {
				t.Errorf("I3: %s was sent %d times (queued %v) while the thread waits", notice.name, sent, queued(notice.name))
			}
			if !waits && (sent != 1 || !completed(notice.name)) {
				t.Errorf("I3: %s was sent %d times (sent/ %v), want exactly once", notice.name, sent, completed(notice.name))
			}
		}
	}
	pin, pinned := pumpReview776QueueAttempt(t, cfg, thread)
	if pinned != waits {
		t.Errorf("after the first rounds the pin is present=%v, want %v (pending %v, held %v)", pinned, waits, pending, held)
	}
	if isHeld, _ := pin["held"].(bool); isHeld != hold {
		t.Errorf("after the first rounds the pin held=%v, want %v (pending %v, held %v)", isHeld, hold, pending, held)
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
	delivered, _, held, hold = pumpOverlapCoverage(shape, final)
	for _, notice := range shape.notices {
		sent := pumpOverlapSentCount(t, log, notice.text)
		switch {
		case delivered[notice.name]:
			if sent != 0 {
				t.Errorf("I1: %s was delivered by a pre-change attempt and sent again %d times", notice.name, sent)
			}
			if !completed(notice.name) || queued(notice.name) {
				t.Errorf("%s is not completed at the end (sent/ %v, queued %v)", notice.name, completed(notice.name), queued(notice.name))
			}
		case hold:
			// A held pin keeps the thread: every notice not proven delivered waits, unsent and queued.
			if sent != 0 || completed(notice.name) || !queued(notice.name) {
				t.Errorf("%s was sent %d times, completed %v, queued %v behind a held pin", notice.name, sent, completed(notice.name), queued(notice.name))
			}
		default:
			if sent != 1 {
				t.Errorf("I3: %s was sent %d times, want exactly once", notice.name, sent)
			}
			if !completed(notice.name) || queued(notice.name) {
				t.Errorf("%s is not completed at the end (sent/ %v, queued %v)", notice.name, completed(notice.name), queued(notice.name))
			}
		}
	}
	pin, pinned = pumpReview776QueueAttempt(t, cfg, thread)
	if pinned != hold {
		t.Errorf("at the end the pin is present=%v, want %v (held %v): %v", pinned, hold, held, pin)
	}
	if isHeld, _ := pin["held"].(bool); isHeld != hold {
		t.Errorf("at the end the pin held=%v, want %v", isHeld, hold)
	}
	if t.Failed() {
		t.Logf("%s; texts %v; receipts %v then %v; deliveries %q", pumpOverlapDescribe(shape), texts, kinds, final, pumpQueueTestSentMessages(t, log))
	}
}

// Overlapping pre-change records are reconciled as one evidence set. Over two and three overlapping
// records, every receipt answer for every record: a notice a provable record's receipt shows delivered
// is never sent again and is completed (I1); a notice an undetermined record carried is not sent while
// that record is undetermined (I2); a notice every record that carried it shows never went is sent
// exactly once in a new batch and completed (I3). A record that cannot prove its text holds the thread
// whatever it answers (R2): nothing it or any other record covers is sent, and only a notice a provable
// record shows delivered is completed.
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
// was delivered. b must not be sent again. Neither record can prove b's text is the one it sent (each
// carried b by time only), so the thread holds: b is neither completed nor sent, and a and c wait
// behind the held pin.
func TestPumpQueueOverlappingUnsettledRecordsNeverResendADeliveredMember(t *testing.T) {
	pumpOverlapRun(t, pumpOverlapShapeNamed(t, "same-members"), []string{pumpOverlapAccepted, pumpOverlapNotAttempted})
}

// A bridge that cannot be started answers no receipt, so every overlapping record is undetermined:
// nothing is sent, nothing is completed and the pin holds; once the bridge answers, the evidence set
// is reconciled and every notice goes exactly once or is completed.
func TestPumpQueueOverlappingLegacyRecordsHoldWhileTheBridgeIsDown(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, receiptsPath, log := pumpOverlapBridge(t)
	cfg := pumpTestConfig(t, bridge)
	shape := pumpOverlapShapeNamed(t, "mixed3")
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

// pumpOverlapUnprovablePhase is one receipt assignment of the unprovable-overlap test and what the
// rounds under it must leave: the notices completed, the notices still queued, whether a pin is left
// and whether it holds the thread, and the texts sent so far with how often each went.
type pumpOverlapUnprovablePhase struct {
	short, long string
	completed   []string
	queued      []string
	pinned      bool
	held        bool
	sent        map[string]int
}

// An overlap pin can join a record whose accepted answer cannot show which queued notice it carried
// (its whole set looks unchanged, yet its text is not the text on disk) with a record that proves a
// delivery on its own. A notice the provable record shows delivered is completed (I1), on the round its
// answer settles; the unprovable record holds the thread from the start, whatever it answers, so every
// other notice stays queued and unsent for the operator. The longer record over [a b] carried b in the
// second b was written again, so its text is not the text on disk and it is unprovable; the shorter
// record over [a] carries the text on disk.
func TestPumpQueueUnprovableOverlapStillCompletesAProvenDelivery(t *testing.T) {
	a, b := pumpOverlapA, pumpOverlapB
	cases := []struct {
		name        string
		shortLedger string
		phases      []pumpOverlapUnprovablePhase
	}{
		{"short-accepted-in-ledger", deliverStateAccepted, []pumpOverlapUnprovablePhase{
			{short: pumpOverlapUnknown, long: pumpOverlapAccepted, completed: []string{a}, queued: []string{b}, pinned: true, held: true},
		}},
		{"short-accepted-by-receipt", deliverStateUnknown, []pumpOverlapUnprovablePhase{
			{short: pumpOverlapAccepted, long: pumpOverlapAccepted, completed: []string{a}, queued: []string{b}, pinned: true, held: true},
		}},
		{"short-settles-later", deliverStateUnknown, []pumpOverlapUnprovablePhase{
			{short: pumpOverlapUnknown, long: pumpOverlapAccepted, queued: []string{a, b}, pinned: true, held: true},
			{short: pumpOverlapAccepted, long: pumpOverlapAccepted, completed: []string{a}, queued: []string{b}, pinned: true, held: true},
		}},
		{"short-never-went", deliverStateUnknown, []pumpOverlapUnprovablePhase{
			// a is still carried by the unprovable record that went, so it can neither be completed nor
			// sent again: both wait under the held pin.
			{short: pumpOverlapNotAttempted, long: pumpOverlapAccepted, queued: []string{a, b}, pinned: true, held: true},
		}},
		{"long-never-went", deliverStateUnknown, []pumpOverlapUnprovablePhase{
			// The unprovable record's answer that it never went does not show which text of b went
			// nowhere, so b is not sent: it waits under the held pin.
			{short: pumpOverlapAccepted, long: pumpOverlapNotAttempted, completed: []string{a}, queued: []string{b}, pinned: true, held: true},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := pumpTestNow
			e := pumpTestEnv(t, &now)
			bridge, receiptsPath, log := pumpOverlapBridge(t)
			cfg := pumpTestConfig(t, bridge)
			thread := "parent-1"
			dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
			stamp := now.Add(-time.Hour)
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, a, "A-proven"), stamp.Add(-time.Minute))
			// b was written again within the stamp's second, the precision limit of a whole-second stamp.
			pumpQueueTestSetTime(t, pumpQueueTestNotice(t, cfg, thread, b, "B-current"), stamp.Add(500*time.Millisecond))
			longID := pumpQueueTestLegacyRecord(t, cfg, thread, []string{a, b}, []string{"A-proven", "B-old"}, stamp, deliverStateUnknown)
			shortID := pumpQueueTestLegacyRecord(t, cfg, thread, []string{a}, []string{"A-proven"}, stamp, tc.shortLedger)
			for p, phase := range tc.phases {
				pumpOverlapSetReceipts(t, receiptsPath, map[string]string{shortID: phase.short, longID: phase.long})
				for round := 0; round < 3; round++ {
					if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
						t.Fatal(err)
					}
				}
				for _, name := range phase.completed {
					if _, err := os.Lstat(filepath.Join(dir, pumpSentDir, name)); err != nil {
						t.Errorf("phase %d: %s was shown delivered and is not completed: %v", p, name, err)
					}
					if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
						t.Errorf("phase %d: %s is still queued after its completion", p, name)
					}
				}
				for _, name := range phase.queued {
					if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
						t.Errorf("phase %d: %s left the queue: %v", p, name, err)
					}
				}
				for _, text := range []string{"A-proven", "B-current", "B-old"} {
					if got := pumpOverlapSentCount(t, log, text); got != phase.sent[text] {
						t.Errorf("phase %d: %s was sent %d times, want %d", p, text, got, phase.sent[text])
					}
				}
				pin, pinned := pumpReview776QueueAttempt(t, cfg, thread)
				if pinned != phase.pinned {
					t.Errorf("phase %d: pin present=%v, want %v (%v)", p, pinned, phase.pinned, pin)
				}
				if held, _ := pin["held"].(bool); held != phase.held {
					t.Errorf("phase %d: pin held=%v, want %v (%v)", p, held, phase.held, pin)
				}
			}
		})
	}
}

// The producer's --queue write is an atomic rename, so it can replace a notice between the round's
// read of the queue and the legacy lookup. The lookup must judge the text the round read on the time
// of the file that text came from: a pre-change record that carried the older text is then still
// adopted, the older text is completed or reconciled under the record's own id and never goes again
// under a new one, and the newer notice is a notice of its own that goes exactly once.
func TestPumpQueueLegacyLookupJudgesTheTimeReadWithTheText(t *testing.T) {
	cases := []struct {
		name, state, receipt string
		replace              bool
	}{
		{"accepted-in-ledger", deliverStateAccepted, "", true},
		{"accepted-by-receipt", deliverStateUnknown, pumpOverlapAccepted, true},
		{"accepted-unchanged", deliverStateAccepted, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := pumpTestNow
			e := pumpTestEnv(t, &now)
			bridge, receiptsPath, log := pumpOverlapBridge(t)
			cfg := pumpTestConfig(t, bridge)
			thread, name := "parent-1", pumpOverlapA
			dir := filepath.Join(cfg.StateDir, pumpQueueDir, thread)
			stamp := now.Add(-time.Hour)
			path := pumpQueueTestNotice(t, cfg, thread, name, "A-already-delivered")
			pumpQueueTestSetTime(t, path, stamp.Add(-time.Minute))
			id := pumpQueueTestLegacyRecord(t, cfg, thread, []string{name}, []string{"A-already-delivered"}, stamp, tc.state)
			pumpOverlapSetReceipts(t, receiptsPath, map[string]string{id: tc.receipt})

			// The round reads the queue as FlushThread does.
			names, err := pumpQueueSortedNames(dir)
			if err != nil {
				t.Fatal(err)
			}
			texts, modTimes, err := pumpReview776QueueReadNotices(dir, names)
			if err != nil {
				t.Fatal(err)
			}
			if !modTimes[0].Equal(stamp.Add(-time.Minute)) {
				t.Fatalf("the time read with the text is %v, want %v", modTimes[0], stamp.Add(-time.Minute))
			}
			whole := pumpReview776QueueBatch{names: names, texts: texts, body: pumpReview776QueueBody(texts), modTimes: modTimes}
			if tc.replace {
				// The producer replaces the notice after the read, the way --queue writes: a new file renamed
				// onto the name, written after the record.
				if err := os.WriteFile(path+".tmp", []byte("B-new-undelivered"), 0o600); err != nil {
					t.Fatal(err)
				}
				pumpQueueTestSetTime(t, path+".tmp", stamp.Add(time.Minute))
				if err := os.Rename(path+".tmp", path); err != nil {
					t.Fatal(err)
				}
			}
			st := pumpTestReadStatePtr(t, cfg)
			action, err := pumpReview776QueueAdoptLegacy(context.Background(), e, cfg, st, dir, thread, whole)
			if err != nil {
				t.Fatal(err)
			}
			if action == pumpReview776QueueLegacyNone {
				t.Fatalf("the record that carried the text the round read was not adopted")
			}
			if _, pinned := st.QueueAttempt[thread]; !pinned {
				t.Fatalf("the adopted record left no pin")
			}
			// The later rounds settle the pin and deliver what is left.
			for round := 0; round < 3; round++ {
				if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
					t.Fatal(err)
				}
			}
			if sent := pumpOverlapSentCount(t, log, "A-already-delivered"); sent != 0 {
				t.Errorf("the text the pre-change attempt delivered was sent again %d times", sent)
			}
			wantB := 0
			if tc.replace {
				wantB = 1
			}
			if sent := pumpOverlapSentCount(t, log, "B-new-undelivered"); sent != wantB {
				t.Errorf("the newer notice was sent %d times, want %d", sent, wantB)
			}
			raw, err := os.ReadFile(filepath.Join(dir, pumpSentDir, name))
			if err != nil {
				t.Fatalf("nothing was completed under sent/: %v", err)
			}
			if !tc.replace && string(raw) != "A-already-delivered" {
				t.Errorf("sent/ holds %q, want the delivered text", raw)
			}
			if names := pumpQueueTestNames(t, cfg, thread); len(names) != 0 {
				t.Errorf("notices left queued: %v", names)
			}
			if pin, pinned := pumpReview776QueueAttempt(t, cfg, thread); pinned {
				t.Errorf("a pin is left: %v", pin)
			}
		})
	}
}

// A notice the producer replaces while it is read is read again, so the text and the time always come
// from one file; a notice that is a symlink is still refused.
func TestPumpQueueReadNoticePairsTheTextWithItsOwnFile(t *testing.T) {
	now := pumpTestNow
	_ = pumpTestEnv(t, &now)
	dir := t.TempDir()
	path := filepath.Join(dir, pumpOverlapA)
	if err := os.WriteFile(path, []byte(" A \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := now.Add(-time.Hour)
	pumpQueueTestSetTime(t, path, at)
	checked, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The name now holds another file than the one checked: the handle read reports it as not the same.
	if err := os.WriteFile(path+".tmp", []byte("B"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
	if _, _, same, err := pumpReview776QueueReadHandle(path, checked); err != nil || same {
		t.Errorf("a replaced notice read as the checked one (same=%v, err=%v)", same, err)
	}
	pumpQueueTestSetTime(t, path, at)
	text, modified, err := pumpReview776QueueReadNotice(dir, pumpOverlapA)
	if err != nil || text != "B" || !modified.Equal(at) {
		t.Errorf("read %q at %v (%v), want %q at %v", text, modified, err, "B", at)
	}
	link := filepath.Join(dir, pumpOverlapB)
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pumpReview776QueueReadNotice(dir, pumpOverlapB); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlinked notice was read: %v", err)
	}
	if _, _, err := pumpReview776QueueLegacyScan(&Config{StateDir: dir}, "parent-1", pumpReview776QueueBatch{names: []string{pumpOverlapA}, texts: []string{"B"}}); err == nil {
		t.Errorf("a snapshot without the times read with its texts was judged")
	}
}
