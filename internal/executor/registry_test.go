package executor

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var regNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func regGrant() Grant {
	return Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "worker-1", OwnerEpoch: 1, ExpiresAt: regNow.Add(time.Hour)}
}

func collectWorld(t *testing.T, kind Kind) (*Registry, *MemoryLedger, Admission, string) {
	t.Helper()
	spool := mkSpool(t)
	a := mustFreeze(t, testTask(kind))
	art := putFile(t, spool, "evidence/summary.md", []byte("123456789"))
	p := testPayload(a)
	p.Artifacts = []Artifact{art}
	r, err := NewResult(a, "completed", p, testProvenance(kind))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "result.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	l := NewMemoryLedger()
	reg := NewRegistry(l, func() time.Time { return regNow })
	return reg, l, a, spool
}

func TestRegistryAdmitCollectEveryProvider(t *testing.T) {
	for _, kind := range []Kind{Codex, Devin, ClaudeCode} {
		t.Run(string(kind), func(t *testing.T) {
			reg, l, a, spool := collectWorld(t, kind)
			ref, err := reg.Admit(a, regGrant())
			if err != nil {
				t.Fatalf("admit: %v", err)
			}
			if ref != ReferenceOf(a) {
				t.Errorf("reference %+v", ref)
			}
			ack, err := reg.Collect(ref, spool, "result.json", testPeer(), DefaultLimits)
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if ack.State != "received" || ack.ExecutionID != "exec-1" || ack.TaskDigest != a.TaskDigest {
				t.Errorf("ack %+v", ack)
			}
			got, gotAck, err := reg.Receipt(ref)
			if err != nil || got.TaskDigest != a.TaskDigest || gotAck != ack {
				t.Errorf("receipt: %v %+v %+v", err, got, gotAck)
			}
			n := len(l.records)
			ack2, err := reg.Collect(ref, spool, "result.json", testPeer(), DefaultLimits)
			if err != nil || !reflect.DeepEqual(ack, ack2) {
				t.Fatalf("replay: %v %+v", err, ack2)
			}
			if len(l.records) != n {
				t.Errorf("a replay appended a record")
			}
			reg2 := NewRegistry(l, func() time.Time { return regNow })
			ack3, err := reg2.Collect(ref, spool, "result.json", Peer{}, DefaultLimits)
			if err == nil {
				t.Errorf("a wrong peer was received: %+v", ack3)
			}
			ack3, err = reg2.Collect(ref, spool, "result.json", testPeer(), DefaultLimits)
			if err != nil || ack3 != ack {
				t.Errorf("a reconstructed registry did not replay: %v %+v", err, ack3)
			}
		})
	}
}

func TestRegistryConflictingResult(t *testing.T) {
	reg, l, a, spool := collectWorld(t, Codex)
	ref, err := reg.Admit(a, regGrant())
	if err != nil {
		t.Fatal(err)
	}
	ack, err := reg.Collect(ref, spool, "result.json", testPeer(), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	r := mustResult(t, a)
	r.Payload.Narrative = "a different ending"
	other, err := EncodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "other.json"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	n := len(l.records)
	q, err := reg.Collect(ref, spool, "other.json", testPeer(), DefaultLimits)
	var ve *ValidationError
	if err == nil || !asValidation(err, &ve) || ve.Code != CodeResultConflict {
		t.Fatalf("a different digest was not quarantined: %v %+v", err, q)
	}
	if q.State != "quarantined" || q.TaskDigest != a.TaskDigest || q.ResultDigest == ack.ResultDigest {
		t.Errorf("quarantine ack %+v", q)
	}
	if len(l.records) != n+1 {
		t.Fatalf("records %d, want %d", len(l.records), n+1)
	}
	q2, err := reg.Collect(ref, spool, "other.json", testPeer(), DefaultLimits)
	if err == nil || q2 != q || len(l.records) != n+1 {
		t.Errorf("the same conflict appended again: %v %+v records %d", err, q2, len(l.records))
	}
	got, gotAck, err := reg.Receipt(ref)
	if err != nil || gotAck != ack || got.Payload.Narrative != "the work is done" {
		t.Errorf("the original final was overwritten: %v %+v", err, gotAck)
	}
	if err := os.Remove(filepath.Join(spool, "evidence", "summary.md")); err != nil {
		t.Fatal(err)
	}
	ack3, err := reg.Collect(ref, spool, "result.json", testPeer(), DefaultLimits)
	if err != nil || ack3 != ack {
		t.Errorf("a replay after artifact retention ended: %v %+v", err, ack3)
	}
}

func TestRegistryStaleAndConcurrentRules(t *testing.T) {
	reg, l, a, spool := collectWorld(t, Devin)
	ref1, err := reg.Admit(a, regGrant())
	if err != nil {
		t.Fatal(err)
	}
	older := regGrant()
	older.OwnerEpoch, older.ExpiresAt = 1, regNow.Add(30*time.Minute)
	if err := func() error { _, e := reg.Admit(a, older); return e }(); err == nil {
		t.Error("an older expiry was renewed backward")
	}
	renamed := regGrant()
	renamed.Owner = "other-worker"
	if err := func() error { _, e := reg.Admit(a, renamed); return e }(); err == nil {
		t.Error("an owner change at the same epoch was allowed")
	}
	n := len(l.records)
	if _, err := reg.Admit(a, regGrant()); err != nil {
		t.Errorf("the same grant is replayed: %v", err)
	}
	if len(l.records) != n {
		t.Error("the same grant appended a record")
	}
	renewed := regGrant()
	renewed.OwnerEpoch, renewed.ExpiresAt = 2, regNow.Add(2*time.Hour)
	if _, err := reg.Admit(a, renewed); err != nil {
		t.Fatalf("a renewal: %v", err)
	}
	t2 := testTask(Devin)
	t2.Identity.ExecutionID = "exec-1"
	t2.Identity.CriteriaDigest = strings.Repeat("9", 64)
	a2, err := Freeze(t2)
	if err != nil {
		t.Fatal(err)
	}
	g2 := regGrant()
	g2.OwnerEpoch = 9
	if _, err := reg.Admit(a2, g2); err == nil {
		t.Error("an execution id was reused for a different task")
	}
	t3 := testTask(Devin)
	t3.Identity.ExecutionID = "exec-2"
	t3.Identity.Attempt = 2
	t3.Identity.AssignmentGeneration = 2
	a3, err := Freeze(t3)
	if err != nil {
		t.Fatal(err)
	}
	g3 := regGrant()
	g3.ExecutionID = "exec-2"
	g3.AssignmentGeneration = 2
	if _, err := reg.Admit(a3, g3); err != nil {
		t.Fatalf("the newer attempt of the task: %v", err)
	}
	if _, _, err := reg.Current("exec-1"); err == nil {
		t.Error("a stale execution still reads current")
	}
	if _, _, err := reg.Current("exec-2"); err != nil {
		t.Errorf("the current execution: %v", err)
	}
	if _, err := reg.Collect(ref1, spool, "result.json", testPeer(), DefaultLimits); err == nil {
		t.Error("a stale execution collected a result")
	}
	var wg sync.WaitGroup
	reg4, l4, a4, spool4 := collectWorld(t, ClaudeCode)
	if _, err := reg4.Admit(a4, regGrant()); err != nil {
		t.Fatal(err)
	}
	ref4 := ReferenceOf(a4)
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reg4.Collect(ref4, spool4, "result.json", testPeer(), DefaultLimits)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent identical collect: %v", err)
		}
	}
	received := 0
	for _, rec := range l4.records {
		if rec.Kind == "received" {
			received++
		}
	}
	if received != 1 {
		t.Errorf("%d received records for one delivery", received)
	}
}

func TestRegistryFailsClosed(t *testing.T) {
	reg, _, a, spool := collectWorld(t, Codex)
	ref, err := reg.Admit(a, regGrant())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(spool, "evidence", "summary.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Collect(ref, spool, "result.json", testPeer(), DefaultLimits); err == nil {
		t.Error("a result whose artifact is missing was received")
	}
	if _, _, err := reg.Receipt(ref); err == nil {
		t.Error("a receipt exists for a refused delivery")
	}
	fail := &failLedger{records: []Record{}}
	reg2 := NewRegistry(fail, func() time.Time { return regNow })
	if _, err := reg2.Admit(a, regGrant()); err == nil {
		t.Error("a failing ledger admitted")
	}
	corrupt := NewMemoryLedger()
	_ = corrupt.Transact(func(recs []Record, app func(Record) error) error {
		return app(Record{Kind: "mystery"})
	})
	reg3 := NewRegistry(corrupt, func() time.Time { return regNow })
	if _, err := reg3.Admit(a, regGrant()); err == nil {
		t.Error("a corrupt history was ignored")
	}
	reg4 := NewRegistry(nil, nil)
	if _, err := reg4.Admit(a, regGrant()); err == nil {
		t.Error("a nil ledger admitted")
	}
	if _, _, err := reg4.Current("x"); err == nil {
		t.Error("a nil ledger answered")
	}
}

type failLedger struct {
	mu      sync.Mutex
	records []Record
}

func (l *failLedger) Transact(fn func([]Record, func(Record) error) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fn(l.records, func(Record) error { return errors.New("the ledger is gone") })
}

func corruptWorld(t *testing.T, kinds ...Record) (*MemoryLedger, Admission, Reference) {
	t.Helper()
	l := NewMemoryLedger()
	reg := NewRegistry(l, func() time.Time { return regNow })
	a := mustFreeze(t, testTask(Codex))
	ref, err := reg.Admit(a, regGrant())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range kinds {
		if err := l.Transact(func(_ []Record, app func(Record) error) error { return app(rec) }); err != nil {
			t.Fatalf("append corrupt record: %v", err)
		}
	}
	return l, a, ref
}

func TestRegistryCorruptHistoryFailsClosed(t *testing.T) {
	spool := mkSpool(t)
	a := mustFreeze(t, testTask(Codex))
	art := putFile(t, spool, "evidence/summary.md", []byte("123456789"))
	p := testPayload(a)
	p.Artifacts = []Artifact{art}
	good := func() Result {
		r, err := NewResult(a, "completed", p, testProvenance(Codex))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}()
	goodAck := Acknowledgement{State: "received", ExecutionID: "exec-1", TaskDigest: a.TaskDigest, ResultDigest: strings.Repeat("3", 64)}
	received := func() Record {
		return Record{Kind: "received", Result: &good, Acknowledgement: &goodAck}
	}
	badID := good
	badID.Identity.TaskID = "task-9"
	badAckID := goodAck
	badAckID.ExecutionID = "exec-9"
	badAckState := goodAck
	badAckState.State = "maybe"
	badAckDigest := goodAck
	badAckDigest.ResultDigest = "short"
	quarResult := good
	quarResult.Payload.Narrative = "other"
	quarAck := Acknowledgement{State: "quarantined", ExecutionID: "exec-1", TaskDigest: a.TaskDigest, ResultDigest: strings.Repeat("4", 64)}
	wrongRenew := regGrant()
	wrongRenew.OwnerEpoch = 2
	wrongTask := testTask(ClaudeCode)
	wrongAdm, err := Freeze(wrongTask)
	if err != nil {
		t.Fatal(err)
	}
	backGrant := regGrant()
	backGrant.OwnerEpoch = 0
	oldTask := testTask(Codex)
	oldTask.Identity.ExecutionID = "exec-2"
	oldTask.Identity.AssignmentGeneration = 2
	oldAdm, err := Freeze(oldTask)
	if err != nil {
		t.Fatal(err)
	}
	oldGrant := regGrant()
	oldGrant.ExecutionID, oldGrant.AssignmentGeneration = "exec-2", 2
	cases := []struct {
		name    string
		records []Record
	}{
		{"result wrong identity", []Record{received(), {Kind: "received", Result: &badID, Acknowledgement: &goodAck}}},
		{"ack wrong execution", []Record{{Kind: "received", Result: &good, Acknowledgement: &badAckID}}},
		{"ack wrong state", []Record{{Kind: "received", Result: &good, Acknowledgement: &badAckState}}},
		{"ack bad digest", []Record{{Kind: "received", Result: &good, Acknowledgement: &badAckDigest}}},
		{"two received", []Record{received(), received()}},
		{"quarantine without final", []Record{{Kind: "quarantined", Result: &quarResult, Acknowledgement: &quarAck}}},
		{"quarantine same digest", []Record{received(), {Kind: "quarantined", Result: &good, Acknowledgement: &Acknowledgement{State: "quarantined", ExecutionID: "exec-1", TaskDigest: a.TaskDigest, ResultDigest: goodAck.ResultDigest}}}},
		{"renewed mismatched admission", []Record{{Kind: "grant_renewed", Admission: &wrongAdm, Grant: &wrongRenew}}},
		{"backwards grant epoch", []Record{{Kind: "grant_renewed", Admission: &a, Grant: &backGrant}}},
		{"admitted backwards attempt", []Record{{Kind: "admitted", Admission: &oldAdm, Grant: &oldGrant}}},
		{"missing result pointer", []Record{{Kind: "received", Acknowledgement: &goodAck}}},
		{"extra pointer on admitted", []Record{{Kind: "admitted", Admission: &a, Grant: &backGrant, Result: &good}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, _, ref := corruptWorld(t, c.records...)
			reg := NewRegistry(l, func() time.Time { return regNow })
			if _, _, err := reg.Current("exec-1"); err == nil {
				t.Error("a corrupt history still answers Current")
			}
			if _, _, err := reg.Receipt(ref); err == nil {
				t.Error("a corrupt history still answers Receipt")
			}
			if _, err := reg.Collect(ref, spool, "result.json", testPeer(), DefaultLimits); err == nil {
				t.Error("a corrupt history still collects")
			}
		})
	}
}

func TestRegistryFailedAppendYieldsNoAcknowledgement(t *testing.T) {
	spool := mkSpool(t)
	a := mustFreeze(t, testTask(Codex))
	art := putFile(t, spool, "evidence/summary.md", []byte("123456789"))
	p := testPayload(a)
	p.Artifacts = []Artifact{art}
	r, err := NewResult(a, "completed", p, testProvenance(Codex))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := EncodeResult(r)
	if err := os.WriteFile(filepath.Join(spool, "result.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	good := NewMemoryLedger()
	reg := NewRegistry(good, func() time.Time { return regNow })
	ref, err := reg.Admit(a, regGrant())
	if err != nil {
		t.Fatal(err)
	}
	fail := &failLedger{records: good.records}
	reg2 := NewRegistry(fail, func() time.Time { return regNow })
	if ack, err := reg2.Collect(ref, spool, "result.json", testPeer(), DefaultLimits); err == nil || ack != (Acknowledgement{}) {
		t.Errorf("a failed append returned ack %+v err %v", ack, err)
	}
	renew := regGrant()
	renew.OwnerEpoch, renew.ExpiresAt = 2, regNow.Add(2*time.Hour)
	if newRef, err := reg2.Admit(a, renew); err == nil || newRef != (Reference{}) {
		t.Errorf("a failed renewal returned ref %+v err %v", newRef, err)
	}
	goodReg := NewRegistry(good, func() time.Time { return regNow })
	ack, err := goodReg.Collect(ref, spool, "result.json", testPeer(), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	other := r
	other.Payload.Narrative = "a different ending"
	oraw, _ := EncodeResult(other)
	if err := os.WriteFile(filepath.Join(spool, "other.json"), oraw, 0o600); err != nil {
		t.Fatal(err)
	}
	fail.records = good.records
	q, err := reg2.Collect(ref, spool, "other.json", testPeer(), DefaultLimits)
	var ve *ValidationError
	if err == nil || q != (Acknowledgement{}) || asValidation(err, &ve) && ve.Code == CodeResultConflict {
		t.Errorf("a failed quarantine append returned ack %+v err %v", q, err)
	}
	got, gotAck, err := goodReg.Receipt(ref)
	if err != nil || gotAck != ack || got.Payload.Narrative != "the work is done" {
		t.Errorf("the stored final changed: %v %+v", err, gotAck)
	}
}
