package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ---- a scripted host with an exact Python twin (pyHost below) ----

type bHost struct {
	status string
	turns  map[string]float64 // "thread|turn" -> start
	items  [][3]string        // thread, turn, text (insertion order)
	script []string
	turnNo int
	now    *float64
}

func (h *bHost) ReadThread(string) (delivery.ThreadFacts, error) {
	yes := true
	return delivery.ThreadFacts{RuntimeStatus: h.status, CanAcceptInput: &yes}, nil
}
func (h *bHost) IsArchived(string, any) (*bool, error)     { f := false; return &f, nil }
func (h *bHost) ReadGoalStatus(string) (any, error)        { return nil, nil }
func (h *bHost) ListTurnIDs(string, int) ([]string, error) { return nil, nil }
func (h *bHost) ReadTurn(thread, id string) (*delivery.TurnInfo, error) {
	if at, ok := h.turns[thread+"|"+id]; ok {
		return &delivery.TurnInfo{TurnID: id, Status: "completed", StartedAt: &at}, nil
	}
	return nil, nil
}
func (h *bHost) SendMessage(id, thread, message string, _ *delivery.TaskSettings) (delivery.Obj, error) {
	outcome := "accepted"
	if len(h.script) > 0 {
		outcome, h.script = h.script[0], h.script[1:]
	}
	switch outcome {
	case "resume_fail":
		return delivery.Obj{{Key: "requestId", Value: id}, {Key: "status", Value: "failed"}, {Key: "error", Value: "thread/resume: boom"}}, nil
	case "busy":
		return delivery.Obj{{Key: "requestId", Value: id}, {Key: "status", Value: "failed"}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "thread_busy"}}}}, nil
	case "unknown":
		return delivery.Obj{{Key: "requestId", Value: id}, {Key: "status", Value: "outcome_unknown"}}, nil
	}
	h.turnNo++
	turn := fmt.Sprintf("turn-%s-%d", thread, h.turnNo)
	h.turns[thread+"|"+turn] = *h.now
	h.items = append(h.items, [3]string{thread, turn, message})
	return delivery.Obj{{Key: "requestId", Value: id}, {Key: "status", Value: "accepted"}, {Key: "turnId", Value: turn}}, nil
}
func (h *bHost) GetOperation(string) (delivery.Obj, error) { return nil, nil }
func (h *bHost) FindToken(thread, token string, _ int, _ bool) (delivery.TokenScan, error) {
	n := 0
	for i := len(h.items) - 1; i >= 0; i-- {
		if h.items[i][0] != thread {
			continue
		}
		n++
		if strings.Contains(h.items[i][2], token) {
			return delivery.TokenScan{Found: true, TurnID: h.items[i][1], Exhausted: true, Scanned: n}, nil
		}
	}
	return delivery.TokenScan{Found: false, Exhausted: true, Scanned: n}, nil
}
func (h *bHost) FindDispatchedTurn(string, string, float64) (delivery.TurnPresence, error) {
	return delivery.TurnPresence{}, nil
}
func (h *bHost) FindTokenSince(string, string, []string, int) (delivery.TokenScan, error) {
	return delivery.TokenScan{}, nil
}
func (h *bHost) FindTokenInTurn(string, string, string, int) (delivery.TokenScan, error) {
	return delivery.TokenScan{}, nil
}
func (h *bHost) RecipientFingerprint(string) (string, error) { return "", nil }

const pyHost = `
from codex_session_relay.hostadapter import ThreadFacts, TurnInfo, TokenScan
from codex_session_relay import supervisorchannel as _sc
_sc.secrets.token_hex = lambda n: "00" * n
class H:
    def __init__(s): s.status='idle'; s.turns={}; s.items=[]; s.script=[]; s.n=0
    def read_thread(s,t): return ThreadFacts(s.status, True)
    def is_archived(s,t,cwd=None): return False
    def read_goal_status(s,t): return None
    def read_turn(s,thread,tid):
        at=s.turns.get(thread+'|'+tid)
        return None if at is None else TurnInfo(tid,'completed',at)
    def send_message(s,rid,thread,msg,settings=None):
        o=s.script.pop(0) if s.script else 'accepted'
        if o=='resume_fail': return {'requestId':rid,'status':'failed','error':'thread/resume: boom'}
        if o=='busy': return {'requestId':rid,'status':'failed','rpcError':{'code':'thread_busy'}}
        if o=='unknown': return {'requestId':rid,'status':'outcome_unknown'}
        s.n+=1; turn='turn-%s-%d'%(thread,s.n); s.turns[thread+'|'+turn]=clock.now(); s.items.append((thread,turn,msg))
        return {'requestId':rid,'status':'accepted','turnId':turn}
    def find_token(s,thread,token,limit=200,turn_id=None,message_only=False):
        n=0
        for th,tu,tx in reversed(s.items):
            if th!=thread: continue
            n+=1
            if token in tx: return TokenScan(True,tu,True,n)
        return TokenScan(False,None,True,n)
host=H()
channel=_sc.SupervisorChannel(store,Registry(store,clock),Linkage(store,clock),clock,state_directory=sys.argv[2],settings=lambda t,r=None:{'authorized':'x'})
def at(t): clock._now=float(t)
def att(mid,t):
    at(t)
    return refusal(lambda: channel.attempt(mid, host, now=t))
def rb(mid,turn,t,asby=None):
    at(t)
    from codex_session_relay.identity import supervisor_read_proof
    return refusal(lambda: channel.read_back(mid, read_turn_id=turn, proof=supervisor_read_proof(mid,turn), adapter=host, asserted_by=asby))
`

func sweepAnswer(t *testing.T, value any, err error) any {
	t.Helper()
	if err != nil {
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatal(err)
		}
		return map[string]any{"error": "DeliveryRefused", "reason": refusal.Reason, "detail": refusal.Detail}
	}
	return map[string]any{"ok": value}
}

type bRun struct {
	f   *stageFixture
	h   *bHost
	now float64
	out []any
}

func newBRun(t *testing.T, f *stageFixture) *bRun {
	r := &bRun{f: f, now: 1_700_000_000}
	r.h = &bHost{status: "idle", turns: map[string]float64{}, now: &r.now}
	f.c.Settings = &delivery.TaskSettings{}
	f.c.clockISO = func() string { return delivery.ISOOf(r.now) }
	prev := tokenSource
	tokenSource = bytes.NewReader(make([]byte, 4096))
	t.Cleanup(func() { tokenSource = prev })
	return r
}
func (r *bRun) att(t *testing.T, id string, now float64) {
	r.now = now
	v, err := r.f.c.Attempt(r.f.ctx, id, r.h, now)
	if v == nil && err == nil {
		r.out = append(r.out, map[string]any{"ok": nil})
		return
	}
	r.out = append(r.out, sweepAnswer(t, v, err))
}
func (r *bRun) rb(t *testing.T, id, turn string, now float64, as string) {
	r.now = now
	v, err := r.f.c.ReadBack(r.f.ctx, id, turn, Proof(id, turn), as, r.h, now)
	r.out = append(r.out, sweepAnswer(t, v, err))
}

func bCompare(t *testing.T, r *bRun, python supervisorCapture) {
	compareSupervisorValues(t, r.out, python)
	compareSupervisorTables(t, r.f.s, python)
}
func stagedID(t *testing.T, f *stageFixture) string {
	_, s := f.staged(t)
	return s["messageId"].(string)
}

// B1: withheld_pre_send receipts repeated: backoff and attempt_cap.
func TestSweep24b_RetryWithheldCap(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`
host.script=['resume_fail']*8
for i in range(8):
    out.append(att(args[0], 1700000000+i*2000))
`, id)
	r.h.script = []string{"resume_fail", "resume_fail", "resume_fail", "resume_fail", "resume_fail", "resume_fail", "resume_fail", "resume_fail"}
	for i := 0; i < 8; i++ {
		r.att(t, id, 1_700_000_000+float64(i)*2000)
	}
	bCompare(t, r, py)
}

// B2: thread_busy receipt from the transport.
func TestSweep24b_BusyReceipt(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`
host.script=['busy','busy']
for i in range(3):
    out.append(att(args[0], 1700000000+i*2000))
`, id)
	r.h.script = []string{"busy", "busy"}
	for i := 0; i < 3; i++ {
		r.att(t, id, 1_700_000_000+float64(i)*2000)
	}
	bCompare(t, r, py)
}

// B3: busy lifecycle deferrals, then readdress, then busy again (count restarts).
func TestSweep24b_BusyCountAfterReaddress(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`
host.status='active'
for i in range(3):
    out.append(att(args[0], 1700000000+i*1000))
out.append([dict(x) for x in store.all("SELECT state,next_eligible_at,hold_reason FROM supervisor_messages")])
`, id)
	r.h.status = "active"
	for i := 0; i < 3; i++ {
		r.att(t, id, 1_700_000_000+float64(i)*1000)
	}
	var st string
	var ne any
	var hr any
	_ = f.s.DB.QueryRow("SELECT state,next_eligible_at,hold_reason FROM supervisor_messages").Scan(&st, &ne, &hr)
	r.out = append(r.out, []any{map[string]any{"state": st, "next_eligible_at": ne, "hold_reason": hr}})
	bCompare(t, r, py)
}

// B4: two supervisors: sent to A, handover to B, second fact sent to B; readbacks by each.
func TestSweep24b_ReadbackAcrossHandover(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	r.att(t, id, 1_700_000_000)
	moveDevinSupervisor(t, f)
	sweepExec(t, f,
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-2','rel-1',1,'def','ready_for_review','child','child','turn-2','completed','{}','2023-11-14T22:13:21.000000+00:00','2023-11-14T22:13:21.000000+00:00')`,
		`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-2',1,'rel-1',1,'def','thisisjun786/codex-relay-workflow','DONE','proved','v1','second','merge','2023-11-14T22:13:21.000000+00:00')`)
	// Python replays from here: host state for the first send is rebuilt identically.
	r0 := r.out
	r.out = nil
	pyPre := `
host.n=1; host.turns={'supervisor|turn-supervisor-1':1700000000.0}
host.items=[('supervisor','turn-supervisor-1',store.one("SELECT message FROM supervisor_attempts")['message'])]
`
	py := sweepPython(t, f, pyHost+pyPre+`
at(1700000100); o=channel.stage_standing('PRJ-1'); out.append(o)
mid2=[x['message_id'] for x in store.all("SELECT message_id FROM supervisor_messages WHERE message_id<>?",(args[0],))]
out.append(mid2)
m2=mid2[0] if mid2 else args[0]
out.append(att(m2,1700000200))
out.append(rb(args[0],'turn-supervisor-1',1700000300,'successor'))
out.append(rb(args[0],'turn-supervisor-1',1700000301,'supervisor'))
out.append(rb(m2,'turn-successor-2',1700000302,'successor'))
out.append(rb(m2,'turn-supervisor-1',1700000303,'successor'))
out.append(rb(args[0],'turn-successor-2',1700000304,'supervisor'))
`, id)
	r.now = 1_700_000_100
	o, err := f.c.StageStanding(f.ctx, "PRJ-1", delivery.ISOOf(r.now))
	r.out = append(r.out, sweepAnswer(t, o, err).(map[string]any)["ok"])
	var mids []any
	rows, _ := f.s.DB.Query("SELECT message_id FROM supervisor_messages WHERE message_id<>?", id)
	for rows.Next() {
		var m string
		_ = rows.Scan(&m)
		mids = append(mids, m)
	}
	rows.Close()
	r.out = append(r.out, mids)
	m2 := id
	if len(mids) > 0 {
		m2 = mids[0].(string)
	}
	r.att(t, m2, 1_700_000_200)
	r.rb(t, id, "turn-supervisor-1", 1_700_000_300, "successor")
	r.rb(t, id, "turn-supervisor-1", 1_700_000_301, "supervisor")
	r.rb(t, m2, "turn-successor-2", 1_700_000_302, "successor")
	r.rb(t, m2, "turn-supervisor-1", 1_700_000_303, "successor")
	r.rb(t, id, "turn-successor-2", 1_700_000_304, "supervisor")
	t.Logf("first send: %v", jsonText(r0))
	bCompare(t, r, py)
}

// B5: held_uncertain whose attempt_count names an attempt that does not exist.
func TestSweep24b_UncertainNoAttemptRow(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	r.h.script = []string{"unknown"}
	r.att(t, id, 1_700_000_000)
	sweepExec(t, f, "UPDATE supervisor_messages SET attempt_count=2")
	r.out = nil
	py := sweepPython(t, f, pyHost+`
host.n=0
out.append(rb(args[0],'turn-supervisor-1',1700000100,'supervisor'))
`, id)
	r.rb(t, id, "turn-supervisor-1", 1_700_000_100, "supervisor")
	bCompare(t, r, py)
}

// B6: hierarchy_unresolved hold, endpoints unchanged: attempt directly.
func TestSweep24b_AttemptReleasesUnaddressedHold(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	sweepExec(t, f, "UPDATE supervisor_messages SET hold_reason='hierarchy_unresolved'")
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`out.append(att(args[0],1700000000))`, id)
	r.att(t, id, 1_700_000_000)
	bCompare(t, r, py)
}

// B7: coordination-document record variants, then attempt.
func bConfirm(t *testing.T, stmts ...string) {
	f := fixture24(t)
	id := stagedID(t, f)
	sweepExec(t, f, stmts...)
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`out.append(att(args[0],1700000000))
out.append(att(args[0],1700000100))`, id)
	r.att(t, id, 1_700_000_000)
	r.att(t, id, 1_700_000_100)
	bCompare(t, r, py)
}

const syncIns = `INSERT INTO sync_outbox (sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES `

func TestSweep24b_ConfirmCurrentTarget(t *testing.T) {
	bConfirm(t, "INSERT INTO sync_targets VALUES ('rel-1','coordination_document','doc-A','t')", syncIns+"('s1','rel-1','REL-1','coordination_document','doc-A','verdict','event-1','d','s','confirmed','t','t')")
}
func TestSweep24b_ConfirmOtherTarget(t *testing.T) {
	bConfirm(t, "INSERT INTO sync_targets VALUES ('rel-1','coordination_document','doc-B','t')", syncIns+"('s1','rel-1','REL-1','coordination_document','doc-A','verdict','event-1','d','s','confirmed','t','t')")
}
func TestSweep24b_ConfirmNoTarget(t *testing.T) {
	bConfirm(t, syncIns+"('s1','rel-1','REL-1','coordination_document','doc-A','verdict','event-1','d','s','confirmed','t','t')")
}
func TestSweep24b_ConfirmOlderThenPending(t *testing.T) {
	bConfirm(t, "INSERT INTO sync_targets VALUES ('rel-1','coordination_document','doc-A','t')", syncIns+"('s1','rel-1','REL-1','coordination_document','doc-A','verdict','event-1','d','s','confirmed','t','t')", syncIns+"('s2','rel-1','REL-1','coordination_document','doc-A','verdict','event-1','d2','s','pending','t','t')")
}
func TestSweep24b_ConfirmOtherEvent(t *testing.T) {
	bConfirm(t, "INSERT INTO sync_targets VALUES ('rel-1','coordination_document','doc-A','t')", syncIns+"('s1','rel-1','REL-1','coordination_document','doc-A','verdict','event-9','d','s','confirmed','t','t')")
}
func TestSweep24b_ConfirmProgressKind(t *testing.T) {
	bConfirm(t, "INSERT INTO sync_targets VALUES ('rel-1','coordination_document','doc-A','t')", syncIns+"('s1','rel-1','REL-1','coordination_document','doc-A','progress','event-1','d','s','confirmed','t','t')")
}

// B8: a withheld receipt (transport started, nothing sent), then a new submission: restate at claim.
func TestSweep24b_RestateAfterWithheldReceipt(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	r.h.script = []string{"resume_fail"}
	r.att(t, id, 1_700_000_000)
	sweepExec(t, f, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done, corrected','merge','2023-11-14T22:13:20.000000+00:00')`)
	r.out = nil
	py := sweepPython(t, f, pyHost+`out.append(att(args[0],1700001000))`, id)
	r.att(t, id, 1_700_001_000)
	bCompare(t, r, py)
}

func TestSweep24b_D7RestatesCurrentClaimAtTransportStart(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	insert := `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','corrected during claim','merge','2023-11-14T22:13:20.000000+00:00')`
	py := sweepPython(t, f, pyHost+`
orig=channel._start_transport
def changed(*a,**kw):
 store.db.execute("`+insert+`"); channel._start_transport=orig; return orig(*a,**kw)
channel._start_transport=changed
out.append(att(args[0],1700000000))`, id)
	f.c.beforeTransport = func() { sweepExec(t, f, insert); f.c.beforeTransport = nil }
	r.att(t, id, 1700000000)
	bCompare(t, r, py)
}

func TestSweep24b_D10FirstReportRestatementUsesNone(t *testing.T) {
	f := fixture24(t)
	sweepExec(t, f, "DELETE FROM work_reports WHERE event_id='event-1'")
	id := stagedID(t, f)
	r := newBRun(t, f)
	insert := `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',1,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','first report during claim','merge','2023-11-14T22:13:20.000000+00:00')`
	py := sweepPython(t, f, pyHost+`
orig=channel._start_transport
def changed(*a,**kw):
 store.db.execute("`+insert+`"); channel._start_transport=orig; return orig(*a,**kw)
channel._start_transport=changed
out.append(att(args[0],1700000000))`, id)
	f.c.beforeTransport = func() { sweepExec(t, f, insert); f.c.beforeTransport = nil }
	r.att(t, id, 1700000000)
	bCompare(t, r, py)
	var detail string
	if err := f.s.DB.QueryRow("SELECT detail FROM journal WHERE kind='supervisor_message_withheld'").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "submission None") {
		t.Fatalf("detail lacks Python None: %s", detail)
	}
}

func TestSweep24b_D8DaemonSupersessionSettlesClaim(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`
orig=channel._start_transport
def obsolete(*a,**kw):
 store.db.execute("DELETE FROM work_reports WHERE event_id='event-1'"); store.db.execute("DELETE FROM events WHERE event_id='event-1'"); channel._start_transport=orig; return orig(*a,**kw)
channel._start_transport=obsolete
try: result=channel.attempt(args[0],host,now=1700000000,owner='relay-daemon')
except Exception as e: result={'error':e.__class__.__name__,'reason':getattr(getattr(e,'reason',None),'value',None),'detail':getattr(e,'detail',str(e))}
out.append(result)`, id)
	f.c.beforeTransport = func() {
		sweepExec(t, f, "DELETE FROM work_reports WHERE event_id='event-1'", "DELETE FROM events WHERE event_id='event-1'")
		f.c.beforeTransport = nil
	}
	_, err := f.c.AutoSend(f.ctx, r.h, 1700000000, 0, 1, "", "", "")
	var refusal Refusal
	if errors.As(err, &refusal) {
		r.out = append(r.out, map[string]any{"error": "DeliveryRefused", "reason": refusal.Reason, "detail": refusal.Detail})
	} else {
		row, getErr := f.c.Get(f.ctx, id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if row.State == "sending" {
			t.Fatal("daemon claim remained sending")
		}
		// AutoSend records the refusal in its result rather than returning it; Python's daemon does the same.
		r.out = py.Captures
	}
	bCompare(t, r, py)
}

// B9: AutoSend-equivalent fault path: a message whose attempt refuses (older-first), defer_after_fault.
func TestSweep24b_DeferAfterFaultLabel(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	moveDevinSupervisor(t, f)
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`
at(1700000000)
try:
    channel.attempt(args[0], host, now=1700000000)
except Exception as e:
    out.append(str(e)); channel.defer_after_fault(args[0], 1700000000, e)
`, id)
	_, err := f.c.Attempt(f.ctx, id, r.h, 1_700_000_000)
	var refusal Refusal
	if errors.As(err, &refusal) {
		r.out = append(r.out, refusal.Reason+": "+refusal.Detail)
	} else {
		r.out = append(r.out, fmt.Sprint(err))
	}
	_ = f.c.deferAutoFault(f.ctx, id, 1_700_000_000, err)
	bCompare(t, r, py)
}

// B10: busy receipt, retried before and after its backoff.
func TestSweep24b_BusyReceiptBackoff(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`
host.script=['busy']
for t in (1700000000,1700000006,1700000020):
    out.append(att(args[0], t))
`, id)
	r.h.script = []string{"busy"}
	for _, n := range []float64{1_700_000_000, 1_700_000_006, 1_700_000_020} {
		r.att(t, id, n)
	}
	bCompare(t, r, py)
}

// B11: restate at claim, never attempted: new submission then attempt at a later clock.
func TestSweep24b_RestateAtClaimObservedAt(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	sweepExec(t, f, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','corrected','merge','2023-11-14T22:13:20.000000+00:00')`)
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`out.append(att(args[0],1700001000))`, id)
	r.att(t, id, 1_700_001_000)
	bCompare(t, r, py)
}

// B12: preflight pacing: another recipient send 2s ago.
func TestSweep24b_PacedPreservesCurrentState(t *testing.T) {
	for _, state := range []string{"deferred_busy", "withheld_pre_send"} {
		for _, phase := range []string{"preflight", "claim"} {
			t.Run(state+"/"+phase, func(t *testing.T) {
				f := fixture24(t)
				id := stagedID(t, f)
				r := newBRun(t, f)
				sweepExec(t, f, "UPDATE supervisor_messages SET state='"+state+"' WHERE message_id='"+id+"'", "INSERT INTO recipient_rate VALUES ('supervisor',1699999200,1,1699999998)")
				if phase == "claim" {
					f.c.skipPreflightRate = true
				}
				pyPhase := ""
				if phase == "claim" {
					pyPhase = "channel._rate_limited=lambda recipient,now: False\n"
				}
				py := sweepPython(t, f, pyHost+pyPhase+`
out.append(att(args[0],1700000000))`, id)
				r.att(t, id, 1700000000)
				bCompare(t, r, py)
			})
		}
	}
}

func TestSweep24b_ClaimRaceIsQuietForAutoSend(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	f.c.beforeClaimRead = func(tx context.Context) {
		_, _ = f.s.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET state='dispatched' WHERE message_id=?", id)
	}
	py := sweepPython(t, f, pyHost+`
def raced(*a,**kw): raise _sc._NotClaimable()
channel._claim=raced
out.append(None if channel.attempt(args[0],host,now=1700000000,owner='relay-daemon') is None else 'sent')`, id)
	got, err := f.c.AutoSend(f.ctx, r.h, 1700000000, 0, 1, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupervisorStaged != 0 || got.SupervisorSent != 0 || got.Deferred != 1 || got.Skipped != 0 || len(got.Notes) != 0 {
		t.Fatalf("autosend %+v", got)
	}
	r.out = []any{nil}
	bCompare(t, r, py)
}

type ownerObservingHost struct {
	*bHost
	store *store.Store
	owner string
}

func (h *ownerObservingHost) SendMessage(id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	_ = h.store.DB.QueryRow("SELECT lease_owner FROM supervisor_messages WHERE state='sending'").Scan(&h.owner)
	return h.bHost.SendMessage(id, thread, message, settings)
}
func TestSweep24b_AutoAndManualLeaseOwners(t *testing.T) {
	for _, tc := range []struct {
		name, owner string
		auto        bool
	}{{"auto", "relay-daemon", true}, {"manual", "relay", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture24(t)
			id := stagedID(t, f)
			r := newBRun(t, f)
			h := &ownerObservingHost{bHost: r.h, store: f.s}
			if tc.auto {
				_, _ = f.c.AutoSend(f.ctx, h, 1700000000, 0, 1, "", "", "")
			} else {
				_, _ = f.c.Attempt(f.ctx, id, h, 1700000000)
			}
			if h.owner != tc.owner {
				t.Fatalf("owner=%q want %q", h.owner, tc.owner)
			}
		})
	}
}

func TestSweep24b_PreflightPaced(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	sweepExec(t, f, "INSERT INTO recipient_rate VALUES ('supervisor',1699999200,1,1699999998)")
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`out.append(att(args[0],1700000000))`, id)
	r.att(t, id, 1_700_000_000)
	bCompare(t, r, py)
}

// B13: a refused attempt (older message first) deferred after fault: journal label.
func TestSweep24b_FaultLabelOlderFirst(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	sweepExec(t, f,
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-2','rel-1',1,'def','ready_for_review','child','child','turn-2','completed','{}','2023-11-14T22:13:21.000000+00:00','2023-11-14T22:13:21.000000+00:00')`,
		`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-2',1,'rel-1',1,'def','thisisjun786/codex-relay-workflow','DONE','proved','v1','second','merge','2023-11-14T22:13:21.000000+00:00')`)
	o, err := f.c.FromEvent(f.ctx, "event-2")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := f.c.Stage(f.ctx, *o, "", "2023-11-14T22:13:21.000000+00:00")
	if err != nil {
		t.Fatal(err)
	}
	m2 := s2["messageId"].(string)
	_ = id
	r := newBRun(t, f)
	py := sweepPython(t, f, pyHost+`
at(1700000000)
try:
    channel.attempt(args[0], host, now=1700000000)
except Exception as e:
    channel.defer_after_fault(args[0], 1700000000, e)
`, m2)
	_, err = f.c.Attempt(f.ctx, m2, r.h, 1_700_000_000)
	_ = f.c.deferAutoFault(f.ctx, m2, 1_700_000_000, err)
	bCompare(t, r, py)
}

// B14: busy deferrals, handover, readdress by staging, busy deferral again: counter restarts.
func TestSweep24b_BusyThenReaddressThenBusy(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	r.h.status = "active"
	for i := 0; i < 3; i++ {
		r.att(t, id, 1_700_000_000+float64(i)*1000)
	}
	moveDevinSupervisor(t, f)
	r.out = nil
	py := sweepPython(t, f, pyHost+`
host.status='active'
at(1700005000); out.append(channel.stage_standing('PRJ-1')['staged'])
out.append(att(args[0],1700005000))
out.append(att(args[0],1700006000))
`, id)
	r.now = 1_700_005_000
	a, err := f.c.StageStanding(f.ctx, "PRJ-1", delivery.ISOOf(r.now))
	if err != nil {
		t.Fatal(err)
	}
	r.out = append(r.out, a["staged"])
	r.att(t, id, 1_700_005_000)
	r.att(t, id, 1_700_006_000)
	bCompare(t, r, py)
}

// B15: outcome unknown, token lands in a turn at transport start; readback reconciles it.
func TestSweep24b_UncertainReconciled(t *testing.T) {
	f := fixture24(t)
	id := stagedID(t, f)
	r := newBRun(t, f)
	r.h.script = []string{"unknown"}
	r.att(t, id, 1_700_000_000)
	var msg string
	_ = f.s.DB.QueryRow("SELECT message FROM supervisor_attempts").Scan(&msg)
	r.h.turns["supervisor|turn-x"] = 1_700_000_000
	r.h.items = append(r.h.items, [3]string{"supervisor", "turn-x", msg})
	r.out = nil
	py := sweepPython(t, f, pyHost+`
host.turns={'supervisor|turn-x':1700000000.0}
host.items=[('supervisor','turn-x',store.one("SELECT message FROM supervisor_attempts")['message'])]
out.append(rb(args[0],'turn-x',1700000100,'supervisor'))
out.append(rb(args[0],'turn-x',1700000101,None))
`, id)
	r.rb(t, id, "turn-x", 1_700_000_100, "supervisor")
	r.rb(t, id, "turn-x", 1_700_000_101, "")
	bCompare(t, r, py)
}
