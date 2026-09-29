package faults

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// This bridge replaces ONLY faultnotice.NoticeDeliverer and compose/stage/
// park_notice with Go; the Go deliverer reads its own NoticeFacts. The real Python DaemonChannelCase, self.tick(),
// supervisor channel, fake host, transport fences and original test assertions
// run unchanged on both sides. Todo 24 owns that channel, todo 29 the loop.
// Both executions use the same disposable pathname, clock and entropy inputs.
// Every tick compares complete fault/supervisor rows, journal, lifecycle, sends
// (including notice bytes), and the full tick report. No output is normalized.
//
// Under the fence one runtime writes a store at a time (docs/port/decisions.md 14
// and 30, cutover.md "When the other runtime owns the record"), so in the hybrid
// run the fixture's store changes hands at every step of either runtime, as it
// would between two hosts' processes: the runtime giving control away closes its
// connection (Python's Store.close ends its write-gate admission), Go hands the
// store over (testsupport.HandOver) and the runtime taking control opens it as a
// process of that runtime opens a store it owns. A callback that only reads runs
// while Go keeps the store, through the read-only Store a Python reader of a
// Go-owned store uses. The oracle run is untouched Python on a store Python owns.
const noticePython = `
import dataclasses, json, os, shutil, sys, traceback
from unittest import mock
from codex_session_relay import faultnotice, faults, supervisorchannel, lifecycle
from codex_session_relay.errors import DeliveryRefused, RefusalReason
from codex_session_relay.store import Store
from tests import test_fault_notices as tests, support

base, case_name, method = sys.argv[1:]
faults.secrets.token_hex = lambda size=None: bytes(range(32 if size is None else size)).hex()
case = None
hybrid = False
snapshots = []
# What Python holds of the fixture's store: 'writer', 'reader' or None (closed).
held = None
# The requests Go serves by writing the store, and the callbacks Python serves only reading it.
GO_WRITES = ('tick', 'settle', 'stage', 'park')
READS = ('resolve', 'evidence', 'return')

def suspend():
    """Python stops using the store, so Go may take it over."""
    global held
    if held is None:
        return
    if case.store.db.in_transaction:
        raise RuntimeError('the store would change hands inside a Python transaction')
    case.store.close()
    case.store._admission = None
    held = None

def resume(read_only=False):
    """Python opens the store again: its own as a writer, or Go's as a reader."""
    global held
    want = 'reader' if read_only else 'writer'
    if held == want:
        return
    suspend()
    hook = case.store.fault_hook
    Store.__init__(case.store, case.store.path, read_only=read_only)
    case.store.fault_hook = hook
    held = want

def send(value):
    print(json.dumps(value), flush=True)

def rpc(op, **values):
    moves = op in GO_WRITES
    if moves:
        suspend()
    try:
        send(dict(op=op, **values))
        return answer_of(op)
    finally:
        if moves:
            resume()

def answer_of(op):
    while True:
        answer = json.loads(sys.stdin.readline())
        if answer.get('op') == 'channel':
            action, args = answer['action'], answer['args']
            try:
                resume(read_only=action in READS)
                channel = active.channel
                if action == 'resolve': value = channel.resolve(args['anchor'])
                elif action == 'stage': value = channel.stage_notice(args['notice'])
                elif action == 'attempt': value = channel.attempt(args['message'], case.adapter, now=args['now'], owner=args['owner'])
                elif action == 'recover': value = dict(channel.recover(args['message'], now=args['now']))
                elif action == 'measure':
                    lifecycle.record(case.store, case.clock, lifecycle.observe(case.adapter, args['task'], require_evidence=channel.require_lifecycle_evidence))
                    value = None
                elif action == 'evidence': value = channel._command_line('fault-show', '--fault', args['fault'])
                elif action == 'return': value = active._return(**args)
                else: raise ValueError(action)
                reply = {'value': value}
            except Exception as error:
                reason = getattr(error, 'reason', None)
                reply = {'error': getattr(reason, 'value', type(error).__name__), 'detail': getattr(error, 'detail', str(error))}
            suspend()
            send(reply)
            continue
        if 'error' in answer:
            import builtins
            kind = getattr(builtins, answer['error'], RuntimeError)
            if answer['error'] in RefusalReason._value2member_map_:
                refused = faults.FaultRefused if op == 'settle' else DeliveryRefused
                raise refused(RefusalReason(answer['error']), answer.get('detail', ''))
            raise kind(answer.get('detail', ''))
        return answer.get('value')

class GoDeliverer:
    def __init__(self, ledger, channel, *, owner):
        self.ledger, self.channel, self.owner = ledger, channel, owner
        self.first = True
    def tick(self, adapter, *, now=None, limit):
        global active
        active = self
        answer = rpc('tick', path=str(case.store.path), now=now, limit=limit,
                     owner=self.owner, reset=self.first,
                     cap=case.daemon.policy.max_supervisor_sends_per_tick)
        self.first = False
        return answer
    def _return(self, **args):
        # Only the crash injection uses this callback; Go performs settlement.
        return None

def stage(channel, notice):
    global active
    if method == 'test_a_real_send_is_never_settled_as_not_sent':
        active = type('ChannelContext', (), {'channel': channel})()
        rpc('open', path=str(case.store.path), now=case.clock.now())
    return rpc('stage', notice=notice)

def compose(channel, notice, *, resolution, observed_at=None):
    return rpc('compose', notice=notice, resolution=resolution,
               stamp=observed_at or case.clock.iso(),
               evidence=channel._command_line('fault-show', '--fault', notice['faultId']))

def park(channel, message_id, reason):
    return rpc('park', message=message_id, reason=reason)

def snapshot(report):
    names = [row['name'] for row in case.store.all("SELECT name FROM sqlite_master WHERE type='table' AND (name LIKE 'fault_%' OR name LIKE 'supervisor_%' OR name IN ('journal','recipient_lifecycle')) ORDER BY name")]
    return {'report': dataclasses.asdict(report) if report is not None else None, 'tables': {name: [dict(row) for row in case.store.all('SELECT * FROM '+name+' ORDER BY rowid')] for name in names}, 'sends': case.adapter.sends}

# Wrap the real tick, not a synthetic CLI transition or a mock of the deliverer.
original_tick = tests.NoticeCase.tick

def tick(self, **kw):
    global case, active
    case = self
    if hybrid:
        active = type('ChannelContext', (), {'channel': self.channel})()
        rpc('open', path=str(self.store.path), now=self.clock.now()+kw.get('advance', 0))
    report = original_tick(self, **kw)
    # JSON roundtrip copies mutable send lists before the next tick changes them.
    snapshots.append(json.loads(json.dumps(snapshot(report))))
    if hybrid:
        rpc('compare', oracle=captures[0][len(snapshots)-1], actual=snapshots[-1])
    return report

tests.NoticeCase.tick = tick

# This scenario calls settlement directly, after a real channel send, not tick().
# Capture the complete refusal/reply and touched tables BEFORE its Python asserts.
def settlement(original, command):
    def settle(self, notification, **kw):
        try:
            if hybrid:
                rpc('open', path=str(case.store.path), now=case.clock.now())
                answer = rpc('settle', command=command, notification=notification, args=kw)
            else:
                answer = original(self, notification, **kw)
            failure = None
        except faults.FaultRefused as error:
            failure = error
            answer = {'error': 'refused', 'reason': error.reason.value, 'detail': error.detail}
        captured = json.loads(json.dumps({'reply': answer, **snapshot(None)}))
        snapshots.append(captured)
        if hybrid:
            rpc('compare', oracle=captures[0][len(snapshots)-1], actual=captured)
        if failure is not None:
            raise failure
        return answer
    return settle

if method == 'test_a_real_send_is_never_settled_as_not_sent':
    faults.FaultLedger.fail_notification = settlement(faults.FaultLedger.fail_notification, 'fault-notification-fail')
    faults.FaultLedger.ack_notification = settlement(faults.FaultLedger.ack_notification, 'fault-notification-ack')
try:
    captures = []
    for hybrid in (False, True):
        snapshots = []
        shutil.rmtree(base, ignore_errors=True)
        def tempdir(*args, **kw):
            os.makedirs(base)
            return base
        case = getattr(tests, case_name)(method)
        with mock.patch.object(support.tempfile, 'mkdtemp', tempdir):
            case.setUp()
        held = 'writer'
        try:
            if hybrid:
                with mock.patch.object(faultnotice, 'NoticeDeliverer', GoDeliverer), \
                     mock.patch.object(supervisorchannel.SupervisorChannel, 'stage_notice', stage), \
                     mock.patch.object(supervisorchannel.SupervisorChannel, 'compose_notice', compose), \
                     mock.patch.object(supervisorchannel.SupervisorChannel, 'park_notice', park):
                    getattr(case, method)()
            else:
                getattr(case, method)()
            captures.append(snapshots)
        finally:
            case.doCleanups()
    send({'op':'done', 'oracle':captures[0], 'go':captures[1]})
except BaseException:
    send({'op':'failed', 'trace':traceback.format_exc()})
`

type noticeBridge struct {
	t      *testing.T
	ctx    context.Context
	input  *bufio.Scanner
	output io.Writer
	// ledger is the one Ledger the deliverer keeps across ticks; its Store is the connection
	// Go holds while it writes the fixture's store, and nil while Python has it.
	ledger    *Ledger
	deliverer *NoticeDeliverer
	path      string
	held      bool
}

// noticeReads are the channel callbacks Python answers by only reading the store; Go keeps
// the store (and any transaction it has open) through them. Every other callback writes it.
var noticeReads = map[string]bool{"resolve": true, "evidence": true, "return": true}

// acquire has Go write the fixture's store: Python closed it before it asked, and a
// completed takeover gives it to Go (testsupport.HandOver), which then opens it as its own.
func (b *noticeBridge) acquire() error {
	if b.held {
		return nil
	}
	testsupport.HandOver(b.t, b.path, "go")
	s, err := store.Open(b.ctx, b.path, "")
	if err != nil {
		return err
	}
	b.ledger.Store, b.held = s, true
	return nil
}

// release has Go stop writing the fixture's store before Python runs again: Go's connection
// closes and a completed takeover gives the store back to Python.
func (b *noticeBridge) release() {
	b.t.Helper()
	if !b.held {
		return
	}
	if err := b.ledger.Store.Close(); err != nil {
		b.t.Fatal(err)
	}
	b.ledger.Store, b.held = nil, false
	testsupport.HandOver(b.t, b.path, "python")
}

func (b *noticeBridge) send(value any) {
	b.t.Helper()
	if err := json.NewEncoder(b.output).Encode(value); err != nil {
		b.t.Fatal(err)
	}
}
func (b *noticeBridge) read() map[string]any {
	b.t.Helper()
	if !b.input.Scan() {
		b.t.Fatalf("Python bridge ended: %v", b.input.Err())
	}
	var value map[string]any
	if err := json.Unmarshal(b.input.Bytes(), &value); err != nil {
		b.t.Fatalf("bridge %s: %v", b.input.Bytes(), err)
	}
	return value
}
func (b *noticeBridge) call(action string, args map[string]any) (any, error) {
	moved := b.held && !noticeReads[action]
	if moved {
		b.release()
	}
	b.send(map[string]any{"op": "channel", "action": action, "args": args})
	for {
		answer := b.read()
		if op, _ := answer["op"].(string); op != "" {
			b.respond(answer)
			continue
		}
		if moved {
			if err := b.acquire(); err != nil {
				return nil, err
			}
		}
		if kind, ok := answer["error"].(string); ok {
			return nil, &NoticeError{kind, noticeString(answer, "detail")}
		}
		return answer["value"], nil
	}
}
func (b *noticeBridge) Resolve(_ context.Context, anchor string) (map[string]any, error) {
	value, err := b.call("resolve", map[string]any{"anchor": anchor})
	if err != nil {
		return nil, err
	}
	return value.(map[string]any), nil
}
func (b *noticeBridge) StageNotice(_ context.Context, notice map[string]any) (map[string]any, error) {
	value, err := b.call("stage", map[string]any{"notice": notice})
	if err != nil {
		return nil, err
	}
	return value.(map[string]any), nil
}
func (b *noticeBridge) Attempt(_ context.Context, id string, now float64, owner string) error {
	_, err := b.call("attempt", map[string]any{"message": id, "now": now, "owner": owner})
	return err
}
func (b *noticeBridge) Recover(_ context.Context, id string, now float64) error {
	_, err := b.call("recover", map[string]any{"message": id, "now": now})
	return err
}
func (b *noticeBridge) Measure(_ context.Context, task string) error {
	_, err := b.call("measure", map[string]any{"task": task})
	return err
}

// open names the fixture's store and the clock of Python's next request; Go takes the store
// only when a request writes it.
func (b *noticeBridge) open(path string, now float64) {
	if b.path != path {
		b.release()
		b.path = path
	}
	b.ledger.Clock = &testClock{now: now}
}
func (b *noticeBridge) respond(request map[string]any) {
	b.t.Helper()
	if b.held {
		// Python asks only while it has the store: never from a callback that reads.
		b.t.Fatalf("Python sent %v while Go holds the store", request["op"])
	}
	value, err := b.handle(request)
	// Python runs next, so the store is Python's again before it reads the answer.
	b.release()
	if err != nil {
		if e, ok := err.(*NoticeError); ok {
			b.send(map[string]any{"error": e.Kind, "detail": e.Detail})
		} else {
			b.send(map[string]any{"error": fmt.Sprintf("%T", err), "detail": err.Error()})
		}
	} else {
		b.send(map[string]any{"value": value})
	}
}
func (b *noticeBridge) handle(r map[string]any) (any, error) {
	switch r["op"] {
	case "compare":
		if diff := noticeDifference("tick", r["oracle"], r["actual"]); diff != "" {
			b.t.Fatal("whole-output comparison diff: " + diff)
		}
		return nil, nil
	case "open":
		b.open(noticeString(r, "path"), r["now"].(float64))
		return nil, nil
	case "tick":
		b.open(noticeString(r, "path"), r["now"].(float64))
		if err := b.acquire(); err != nil {
			return nil, err
		}
		if r["reset"] == true {
			b.deliverer = &NoticeDeliverer{Ledger: b.ledger, Channel: b, Owner: noticeString(r, "owner"), beforeReturn: func(context.Context) error { _, err := b.call("return", map[string]any{}); return err }}
		}
		cap := int(r["cap"].(float64))
		left := int(r["limit"].(float64))
		// Python's real report pass supplies spent attempts, including post-send
		// exceptions. The Go seam applies the same shared-cap subtraction.
		return b.deliverer.Tick(b.ctx, r["now"].(float64), NoticeRemaining(cap, cap-left))
	case "settle":
		if err := b.acquire(); err != nil {
			return nil, err
		}
		args := map[string]string{"--notification": noticeString(r, "notification")}
		for key, value := range r["args"].(map[string]any) {
			args["--"+key] = value.(string)
		}
		value, err := executeD(b.ctx, b.ledger, noticeString(r, "command"), args)
		if err != nil {
			reason, detail, _ := strings.Cut(strings.TrimPrefix(err.Error(), "transaction body: "), ": ")
			return nil, &NoticeError{reason, detail}
		}
		return value, nil
	case "stage":
		if err := b.acquire(); err != nil {
			return nil, err
		}
		notice := r["notice"].(map[string]any)
		evidence, err := b.call("evidence", map[string]any{"fault": notice["faultId"]})
		if err != nil {
			return nil, err
		}
		return b.ledger.StageNotice(b.ctx, notice, b.Resolve, evidence.(string))
	case "compose":
		return ComposeNotice(r["notice"].(map[string]any), r["resolution"].(map[string]any), noticeString(r, "stamp"), noticeString(r, "evidence"))
	case "park":
		if err := b.acquire(); err != nil {
			return nil, err
		}
		return nil, b.ledger.ParkNotice(b.ctx, noticeString(r, "message"), noticeString(r, "reason"))
	}
	return nil, fmt.Errorf("unknown bridge operation %v", r["op"])
}
func noticeReplay(t *testing.T, caseName, method string) {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp("/dev/shm", "notice-replay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	script := filepath.Join(home, "replay.py")
	if err = os.WriteFile(script, []byte(noticePython), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(root, ".venv/bin/python"), script, home+"/fixture", caseName, method)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONPATH="+root+"/packages/codex-session-relay", "TMPDIR=/dev/shm")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	b := &noticeBridge{t: t, ctx: context.WithValue(ctx, f1InputsKey{}, f1Inputs{entropy: bytes.NewReader(bytes.Repeat([]byte{0, 1, 2, 3, 4, 5, 6, 7}, 4096))}), input: bufio.NewScanner(stdout), output: stdin, ledger: &Ledger{}}
	b.input.Buffer(make([]byte, 4096), 64*1024*1024)
	defer func() {
		if b.held {
			_ = b.ledger.Store.Close()
		}
	}()
	for {
		request := b.read()
		switch request["op"] {
		case "failed":
			t.Fatalf("Python scenario %s.%s:\n%s\nstderr: %s", caseName, method, request["trace"], stderr.String())
		case "done":
			if !reflect.DeepEqual(request["oracle"], request["go"]) {
				oracle, _ := json.MarshalIndent(request["oracle"], "", "  ")
				got, _ := json.MarshalIndent(request["go"], "", "  ")
				// Persist the full mismatch outside the checkout, and print its first
				// differing line rather than silently projecting away any fields.
				dir := filepath.Join("/dev/shm", "notice-diff-"+strings.TrimPrefix(method, "test_"))
				_ = os.MkdirAll(dir, 0700)
				_ = os.WriteFile(dir+"/python.json", oracle, 0600)
				_ = os.WriteFile(dir+"/go.json", got, 0600)
				g, p := strings.Split(string(got), "\n"), strings.Split(string(oracle), "\n")
				for i := 0; i < min(len(g), len(p)); i++ {
					if g[i] != p[i] {
						t.Fatalf("whole-output mismatch line %d: Go %s Python %s; full captures %s", i+1, g[i], p[i], dir)
					}
				}
				t.Fatalf("whole-output length mismatch: %s", dir)
			}
			if err = cmd.Wait(); err != nil {
				t.Fatalf("Python %v %s", err, stderr.String())
			}
			return
		default:
			b.respond(request)
		}
	}
}

// Report the first differing field while preserving the complete comparison.
func noticeDifference(path string, want, got any) string {
	if reflect.DeepEqual(want, got) {
		return ""
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			break
		}
		keys := make([]string, 0, len(w))
		for key := range w {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if diff := noticeDifference(path+"."+key, w[key], g[key]); diff != "" {
				return diff
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			break
		}
		if len(w) != len(g) {
			return fmt.Sprintf("%s.length: Go %d Python %d", path, len(g), len(w))
		}
		for i := range w {
			if diff := noticeDifference(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); diff != "" {
				return diff
			}
		}
	}
	return fmt.Sprintf("%s: Go %v Python %v", path, got, want)
}
