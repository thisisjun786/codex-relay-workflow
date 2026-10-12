package audit_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// The audit of an acceptance reads the reviewer's thread back from a fake App Server host (thread/turns/list with itemsView full,
// thread/items/list for the turn the list returns in summary) and compares it with the journal of a run that really made the
// calls: the executor that wrote the head and the reviewer whose verdict the acceptance rests on ran on the same backend as in
// production, over the fake host.

const (
	acceptedHead   = "0123456789abcdef0123456789abcdef01234567"
	criteriaDigest = "sha256:criteria-of-node-a"
)

type auditLab struct {
	t       *testing.T
	host    *fakehost.Server
	threads *fakehost.Threads
	client  *appserver.Client
	events  []engine.Event
	review  string // the call id of the reviewer
	// reply is what the reviewer says.
	reply string
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newAuditLab runs a build call of node A and a review call of the head on the fake host, and keeps the journal.
func newAuditLab(t *testing.T, packetHead, packetCriteria string) *auditLab {
	t.Helper()
	l := &auditLab{t: t, reply: `{"verdict":"pass"}`}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")

	l.host = fakehost.Start(t)
	l.threads = l.host.Threads()
	l.threads.OnTurn(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if strings.Contains(r.Text, "ROLE: reviewer") {
			return fakehost.TurnPlan{Message: l.reply}
		}
		return fakehost.TurnPlan{Message: "built"}
	})
	registry := agent.NewThreads()
	l.client = appserver.New(l.host.SocketPath, appserver.DefaultBounds, agent.ClientOption(registry))
	t.Cleanup(func() { _ = l.client.Close() })
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: filepath.Join(root, "state"), RunID: "run-audit", Base: func(string) (string, error) { return base, nil }})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := agent.New(agent.Config{Client: l.client, Allocator: alloc, Threads: registry, PollEvery: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	journal := engine.NewMemoryJournal()
	packet := "ROLE: reviewer. Review the head.\\n\\n"
	if packetHead != "" {
		packet += "HEAD: " + packetHead + "\\n"
	}
	if packetCriteria != "" {
		packet += "CRITERIA DIGEST: " + packetCriteria + "\\n"
	}
	prog, err := script.Load(script.Source{Name: "audit.js", Text: []byte(`export const meta = { name: 'audit' }
const S = { type: 'object', properties: { verdict: { type: 'string' } }, required: ['verdict'], additionalProperties: false }
const b = await agent.call('ROLE: executor. build', { agentType: 'executor', node: 'A', label: 'build:A' })
const r = await agent.call('` + packet + `', { agentType: 'reviewer', node: 'A', head: '` + base + `', schema: S, label: 'review:A:r1' })
return { build: b.callId, review: r.callId, status: [b.status, r.status] }
`)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := engine.Run(ctx, prog, engine.Config{Backend: backend, Policy: engine.FakePolicy(), Journal: journal, RunID: "run-audit"})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	var out struct {
		Review string   `json:"review"`
		Status []string `json:"status"`
	}
	if err := json.Unmarshal(res.Value, &out); err != nil || !slices.Equal(out.Status, []string{"completed", "completed"}) {
		t.Fatalf("%s %v", res.Value, err)
	}
	l.review = out.Review
	l.events = journal.Events()
	// the rows of the run's operations that name the acceptance: the node's release, the verified review of the head and its acceptance
	op := func(name string, data map[string]any) {
		l.events = append(l.events, engine.Event{Seq: len(l.events) + 1, RunID: "run-audit", Kind: engine.KindCall, CallID: "c_" + name, Op: "crw." + name, Event: engine.EvCompleted, Data: data})
	}
	op("release", map[string]any{"node": "A", "milestone": "M1", "released": true})
	op("review", map[string]any{"node": "A", "head": acceptedHead, "criteria_digest": criteriaDigest, "state": "verdict", "verdict": "verified", "call": out.Review})
	op("accept", map[string]any{"node": "A", "head": acceptedHead, "acceptance_id": "acc-1", "plan": "plan-1"})
	return l
}

func (l *auditLab) audit(reader audit.Reader, events []engine.Event) audit.Result {
	l.t.Helper()
	subjects := audit.Subjects(events)
	if len(subjects) != 1 {
		l.t.Fatalf("%d subjects: %+v", len(subjects), subjects)
	}
	return audit.Audit(context.Background(), reader, events, subjects[0])
}

func (l *auditLab) reader() audit.Reader { return audit.BusReader{Bus: l.client.Bus()} }

func check(r audit.Result, name string) audit.Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return audit.Check{}
}

// withData copies the events and sets a member of the data of the rows the function picks.
func withData(events []engine.Event, pick func(engine.Event) bool, key string, value any) []engine.Event {
	out := slices.Clone(events)
	for i, ev := range out {
		if pick(ev) {
			d := map[string]any{}
			for k, v := range ev.Data {
				d[k] = v
			}
			d[key] = value
			out[i].Data = d
		}
	}
	return out
}

// The acceptance of a head whose reviewer thread the host lists, whose turn completed, whose last message has the digest the
// journal recorded, whose packet names the head and the criteria digest of the acceptance, and whose executor's pair was observed:
// ok, with every check ok.
func TestAuditOk(t *testing.T) {
	l := newAuditLab(t, acceptedHead, criteriaDigest)
	r := l.audit(l.reader(), l.events)
	if r.State != audit.OK || r.Reason != "" || r.AcceptanceID != "acc-1" || r.Node != "A" || r.Milestone != "M1" || r.Thread == "" || r.Turn == "" {
		t.Fatalf("%+v", r)
	}
	names := []string{}
	for _, c := range r.Checks {
		if c.Result != audit.OK {
			t.Errorf("check %+v", c)
		}
		names = append(names, c.Name)
	}
	for _, want := range []string{audit.CheckRecorded, audit.CheckExecutor, audit.CheckHost, audit.CheckStatus, audit.CheckDigest, audit.CheckHead, audit.CheckCriteria} {
		if !slices.Contains(names, want) {
			t.Errorf("no %s check among %v", want, names)
		}
	}
	// the row of the audit reads back as a pass for the acceptance
	data := audit.RowData("M1", []audit.Result{r})
	events := append(slices.Clone(l.events), engine.Event{Seq: 99, RunID: "run-audit", Kind: engine.KindCall, CallID: "c_audit", Op: audit.Op, Event: engine.EvCompleted, Data: data})
	if passed := audit.Passed(events); len(passed) != 1 || passed["acc-1"].Thread != r.Thread {
		t.Errorf("passed %+v", passed)
	}
	if sum := audit.Summarize([]audit.Result{r}); sum.OK != 1 || sum.Mismatch != 0 || sum.Unobserved != 0 {
		t.Errorf("summary %+v", sum)
	}
}

// The digest of the last agentMessage on the host is not the digest the journal recorded: the record does not describe what the
// reviewer said. A journal that lost the digest, the turn id or the end of the call does not describe it either.
func TestAuditDigestMismatch(t *testing.T) {
	l := newAuditLab(t, acceptedHead, criteriaDigest)
	reviewDone := func(ev engine.Event) bool {
		return ev.CallID == l.review && ev.Op == "agent" && ev.Event == engine.EvCompleted
	}
	r := l.audit(l.reader(), withData(l.events, reviewDone, "result_digest", strings.Repeat("0", 64)))
	if r.State != audit.Mismatch || check(r, audit.CheckDigest).Result != audit.Mismatch || !strings.Contains(r.Reason, "result_digest") {
		t.Fatalf("%+v", r)
	}
	// the host says something else than the journal recorded
	l.reply = `{"verdict":"fail"}`
	l2 := newAuditLab(t, acceptedHead, criteriaDigest)
	l2.reply = `{"verdict":"pass"}`
	other := withData(l2.events, func(ev engine.Event) bool {
		return ev.CallID == l2.review && ev.Op == "agent" && ev.Event == engine.EvCompleted
	},
		"result_digest", digestOfJSON(`{"verdict":"fail"}`))
	if r := l2.audit(l2.reader(), other); r.State != audit.Mismatch || check(r, audit.CheckDigest).Result != audit.Mismatch {
		t.Errorf("a digest of another answer: %+v", r)
	}
	// the record itself is incomplete
	for name, events := range map[string][]engine.Event{
		"no digest":                 withData(l.events, reviewDone, "result_digest", ""),
		"the call did not complete": dropRows(l.events, func(ev engine.Event) bool { return ev.CallID == l.review && ev.Event == engine.EvCompleted }),
		"no turn id":                dropRows(l.events, func(ev engine.Event) bool { return ev.CallID == l.review && ev.Event == engine.EvTurnStarted }),
	} {
		if r := l.audit(l.reader(), events); r.State != audit.Mismatch || check(r, audit.CheckRecorded).Result != audit.Mismatch {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

func digestOfJSON(text string) string {
	var v any
	_ = json.Unmarshal([]byte(text), &v)
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func dropRows(events []engine.Event, drop func(engine.Event) bool) []engine.Event {
	var out []engine.Event
	for _, ev := range events {
		if !drop(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// The packet the reviewer was sent names another head than the acceptance stands on, or another criteria digest, or neither.
func TestAuditHeadOrCriteriaMismatch(t *testing.T) {
	for name, c := range map[string]struct {
		packetHead, packetCriteria string
		check                      string
	}{
		"another head":          {"fedcba9876543210fedcba9876543210fedcba98", criteriaDigest, audit.CheckHead},
		"another criteria":      {acceptedHead, "sha256:another-set-of-criteria", audit.CheckCriteria},
		"a packet with no head": {"", criteriaDigest, audit.CheckHead},
		"no criteria digest":    {acceptedHead, "", audit.CheckCriteria},
	} {
		t.Run(name, func(t *testing.T) {
			l := newAuditLab(t, c.packetHead, c.packetCriteria)
			r := l.audit(l.reader(), l.events)
			if r.State != audit.Mismatch || check(r, c.check).Result != audit.Mismatch || !strings.HasPrefix(r.Reason, c.check) {
				t.Fatalf("%+v", r)
			}
			// the rest of the record is fine: only the thing that differs is a mismatch
			if check(r, audit.CheckDigest).Result != audit.OK {
				t.Errorf("digest check: %+v", check(r, audit.CheckDigest))
			}
		})
	}
	// the acceptance of the other head is not the one the thread was read for
	l := newAuditLab(t, acceptedHead, criteriaDigest)
	moved := withData(l.events, func(ev engine.Event) bool { return ev.Op == "crw.accept" || ev.Op == "crw.review" }, "head", "ffffffffffffffffffffffffffffffffffffffff")
	if r := l.audit(l.reader(), moved); r.State != audit.Mismatch || check(r, audit.CheckHead).Result != audit.Mismatch {
		t.Errorf("an acceptance of another head: %+v", r)
	}
}

// What the audit could not look at is unobserved, and unobserved is not a pass: no host reader, a host that does not list the
// turn, a turn whose items the host does not return, a host that fails, and an executor whose pair was never read back. None of
// them is a mismatch, none is ok, and none is in the acceptances that passed.
func TestAuditUnobservedIsNotPass(t *testing.T) {
	l := newAuditLab(t, acceptedHead, criteriaDigest)
	thread := l.audit(l.reader(), l.events).Thread
	var readerErr audit.Reader = failingReader{err: fmt.Errorf("the host is gone")}
	cases := map[string]struct {
		reader audit.Reader
		events []engine.Event
		check  string
	}{
		"no reader":      {nil, l.events, audit.CheckHost},
		"the host fails": {readerErr, l.events, audit.CheckHost},
		"an executor with no pair": {l.reader(), dropRows(l.events, func(ev engine.Event) bool {
			return ev.Op == "agent" && ev.Event == engine.EvThreadStarted && ev.CallID != l.review
		}), audit.CheckExecutor},
	}
	for name, c := range cases {
		r := audit.Audit(context.Background(), c.reader, c.events, audit.Subjects(c.events)[0])
		if r.State != audit.Unobserved || check(r, c.check).Result != audit.Unobserved || r.State == audit.OK {
			t.Errorf("%s: %+v", name, r)
		}
		events := append(slices.Clone(c.events), engine.Event{Seq: 99, RunID: "run-audit", Kind: engine.KindCall, CallID: "c_audit", Op: audit.Op, Event: engine.EvCompleted, Data: audit.RowData("M1", []audit.Result{r})})
		if passed := audit.Passed(events); len(passed) != 0 {
			t.Errorf("%s: an unobserved acceptance is in the passed ones: %+v", name, passed)
		}
	}
	// the host answers for the thread but does not return the items of its turns
	l.threads.ReadAs(thread, 0, fakehost.TurnPlan{SummaryOnly: true, ItemsListError: map[string]any{"code": -32600, "message": "items are not available"}})
	r := l.audit(l.reader(), l.events)
	if r.State == audit.OK {
		t.Fatalf("a turn whose items could not be read is a pass: %+v", r)
	}
	// a thread the host does not have (it was archived and forgotten): not found, not a pass
	gone := withData(l.events, func(ev engine.Event) bool {
		return ev.CallID == l.review && ev.Op == "agent" && ev.Event == engine.EvThreadStarted
	}, "thread_id", "no-such-thread")
	if r := l.audit(l.reader(), gone); r.State != audit.Unobserved && r.State != audit.Mismatch || r.State == audit.OK {
		t.Errorf("an unknown thread: %+v", r)
	}
	// a mismatch in the record still wins over what could not be looked at
	mixed := withData(l.events, func(ev engine.Event) bool {
		return ev.CallID == l.review && ev.Op == "agent" && ev.Event == engine.EvCompleted
	}, "result_digest", "")
	if r := audit.Audit(context.Background(), nil, mixed, audit.Subjects(mixed)[0]); r.State != audit.Mismatch {
		t.Errorf("a record with no digest and no host: %+v", r)
	}
}

type failingReader struct{ err error }

func (f failingReader) ReadTurns(context.Context, string, []string) ([]appserver.TurnReading, error) {
	return nil, f.err
}

// Subjects are the newest acceptance of each node and head, with the review it rests on; an acceptance with no verified review
// behind it is a subject the audit reports as a mismatch.
func TestAuditSubjectsAndAcceptanceWithoutReview(t *testing.T) {
	l := newAuditLab(t, acceptedHead, criteriaDigest)
	events := append(slices.Clone(l.events),
		engine.Event{Seq: 90, RunID: "run-audit", Kind: engine.KindCall, CallID: "c_acc2", Op: "crw.accept", Event: engine.EvCompleted, Data: map[string]any{"node": "A", "head": acceptedHead, "acceptance_id": "acc-2", "plan": "plan-1"}},
		engine.Event{Seq: 91, RunID: "run-audit", Kind: engine.KindCall, CallID: "c_acc3", Op: "crw.accept", Event: engine.EvCompleted, Data: map[string]any{"node": "B", "head": "abc", "acceptance_id": "acc-3", "plan": "plan-1"}})
	subjects := audit.Subjects(events)
	if len(subjects) != 2 || subjects[0].AcceptanceID != "acc-2" || subjects[0].ReviewerCall != l.review || subjects[1].Node != "B" || subjects[1].ReviewerCall != "" {
		t.Fatalf("%+v", subjects)
	}
	r := audit.Audit(context.Background(), l.reader(), events, subjects[1])
	if r.State != audit.Mismatch || check(r, audit.CheckRecorded).Result != audit.Mismatch {
		t.Errorf("an acceptance with no review: %+v", r)
	}
}
