package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// These tests hold managed-start to one rule: a request the registry or the settings record is going to
// refuse is refused before the host is asked to create anything, with the answer it always had, and the
// span from the last project-scope ask to the creation is one step per project. The fake host's call record
// is the evidence: a refused request must leave no CreateThread call behind.

// rewrite changes the run's request before it is sent.
func (x *scopeRun) rewrite(change func(request map[string]any)) {
	x.t.Helper()
	var request map[string]any
	if err := json.Unmarshal(x.raw, &request); err != nil {
		x.t.Fatal(err)
	}
	change(request)
	raw, err := json.Marshal(request)
	if err != nil {
		x.t.Fatal(err)
	}
	x.raw = raw
}

// recordParentSettings writes the parent's authorized settings as an earlier registration would have: the
// request's own parent settings, with changes applied.
func (x *scopeRun) recordParentSettings(change func(settings map[string]any)) {
	x.t.Helper()
	var request map[string]any
	if err := json.Unmarshal(x.raw, &request); err != nil {
		x.t.Fatal(err)
	}
	parent := request["parent"].(map[string]any)
	settings := map[string]any{}
	for k, v := range parent["settings"].(map[string]any) {
		settings[k] = v
	}
	settings["citedRole"] = "parent"
	change(settings)
	if _, err := EnsureSettings(x.ctx, x.store, parent["taskId"].(string), settings, "earlier_registration", x.start.now()); err != nil {
		x.t.Fatal(err)
	}
}

func withoutProject(request map[string]any) { delete(request, "projectKey") }

// answer is what a refused run reported: a refusal with its reason and detail, or a plain error.
func answer(err error) (reason, detail string) {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason, refused.Detail
	}
	return "", err.Error()
}

func TestPrecreate_RefusalsComeBeforeTheHostIsAsked(t *testing.T) {
	t.Parallel()
	const separator = " must not contain '|', which is the field separator"
	const conflict = "'parent' already has execution settings that differ from this record; ensure_only does not overwrite them"
	differentModel := func(settings map[string]any) { settings["model"] = "gpt-other" }
	hostSeparator := func(r map[string]any) {
		r["parent"].(map[string]any)["hostId"] = "ho|st"
		r["child"].(map[string]any)["hostId"] = "ho|st"
	}
	cases := []struct {
		name   string
		bound  bool
		change func(map[string]any)
		record func(*scopeRun)
		reason string
		detail string
		// state is the request's state afterwards: a refusal at the first scope ask leaves the reservation
		// unarmed; the asks directly before the effect come after the reservation is armed.
		state string
	}{
		{name: "issue key holding the separator", bound: true, state: "create_armed",
			change: func(r map[string]any) { r["issueKey"] = "REL|X" }, detail: "issue_key" + separator},
		{name: "parent task id holding the separator", state: "create_armed",
			change: func(r map[string]any) {
				withoutProject(r)
				r["parent"].(map[string]any)["taskId"] = "par|ent"
				r["allowedRecipients"] = []any{"par|ent"}
			}, detail: "parent_task_id" + separator},
		// Registration names the first field it finds, parent before issue.
		{name: "parent task id and issue key both holding the separator", state: "create_armed",
			change: func(r map[string]any) {
				withoutProject(r)
				r["issueKey"] = "REL|X"
				r["parent"].(map[string]any)["taskId"] = "par|ent"
				r["allowedRecipients"] = []any{"par|ent"}
			}, detail: "parent_task_id" + separator},
		{name: "host id holding the separator under a project", bound: true, state: "create_armed",
			change: hostSeparator, reason: "unregistered_scope", detail: "the child host id" + separator},
		{name: "parent settings that differ from the recorded ones", bound: true, state: "create_armed",
			record: func(x *scopeRun) { x.recordParentSettings(differentModel) },
			reason: "relationship_conflict", detail: conflict},
		// Each refusal keeps its own answer, and the checks that already ran before creation keep their
		// place. A request refused for two reasons the request alone decides gets the one registration
		// would have given first: the project scope, then the separator in the identity, then the
		// project's host, then the recorded settings.
		{name: "separator beats differing settings", bound: true, state: "create_armed",
			change: func(r map[string]any) { r["issueKey"] = "REL|X" },
			record: func(x *scopeRun) { x.recordParentSettings(differentModel) },
			detail: "issue_key" + separator},
		{name: "unbound project beats separator and differing settings", state: "reserved",
			change: func(r map[string]any) { r["issueKey"] = "REL|X" },
			record: func(x *scopeRun) { x.recordParentSettings(differentModel) },
			reason: "unregistered_scope", detail: "project 'P-SCOPE' has no registered parent, so an issue cannot be attached to it yet"},
		{name: "host separator beats differing settings", bound: true, state: "create_armed",
			change: hostSeparator,
			record: func(x *scopeRun) { x.recordParentSettings(differentModel) },
			reason: "unregistered_scope", detail: "the child host id" + separator},
		// Registration used to answer duplicate_scope_owner for the rival owner of the issue, a refusal that
		// names the created child and so cannot be asked before it exists; the settings refusal is decided
		// first now. This is the one answer that changes, for a request that is refused anyway.
		{name: "differing settings are asked before a rival child binding that needs the child", bound: true, state: "create_armed",
			record: func(x *scopeRun) {
				x.recordParentSettings(differentModel)
				if _, err := x.reg.BindScopeAs(x.ctx, "child", "REL-MANAGED", registry.Endpoint{TaskID: "rival", HostID: "host"}, "active"); err != nil {
					x.t.Fatal(err)
				}
			},
			reason: "relationship_conflict", detail: conflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			x := newScopeRun(t)
			if c.bound {
				x.bind("parent")
			}
			if c.change != nil {
				x.rewrite(c.change)
			}
			if c.record != nil {
				c.record(x)
			}
			_, err := x.start.Run(x.ctx, x.raw)
			if err == nil {
				t.Fatalf("the request was admitted; host calls %v", x.host.calls)
			}
			if reason, detail := answer(err); reason != c.reason || detail != c.detail {
				t.Fatalf("answer %q %q, want %q %q", reason, detail, c.reason, c.detail)
			}
			if len(x.host.calls) != 0 {
				t.Fatalf("a refused request reached the host: %v", x.host.calls)
			}
			if row := x.requestRow(); row.State != c.state || x.count("relationships") != 0 {
				t.Fatalf("refused request left %s with %d relationships, want %s with none", row.State, x.count("relationships"), c.state)
			}
		})
	}
}

// A request refused for its parent settings is armed and keeps its request id: once the recorded settings
// are the request's, the same request creates its one child.
func TestPrecreate_SettingsRefusalRetryCreatesTheChildOnce(t *testing.T) {
	t.Parallel()
	x := newScopeRun(t)
	x.bind("parent")
	x.recordParentSettings(func(settings map[string]any) { settings["model"] = "gpt-other" })
	if _, err := x.start.Run(x.ctx, x.raw); err == nil || len(x.host.calls) != 0 {
		t.Fatalf("first run: %v %v", err, x.host.calls)
	}
	if _, err := x.store.DB.ExecContext(x.ctx, "DELETE FROM authorized_settings WHERE task_id = 'parent'"); err != nil {
		t.Fatal(err)
	}
	x.recordParentSettings(func(map[string]any) {})
	got := x.run()
	if got["state"] != "admitted" || x.host.count("CreateThread") != 1 {
		t.Fatalf("retry: %v %v", got, x.host.calls)
	}
}

// A host id holding the separator is refused only under a project, as registration refuses it; without one
// registration never looked, and the request is still admitted. So is a request whose parent settings are
// the recorded ones.
func TestPrecreate_AdmitsWhatRegistrationAdmits(t *testing.T) {
	t.Parallel()
	t.Run("host separator without a project", func(t *testing.T) {
		t.Parallel()
		x := newScopeRun(t)
		x.rewrite(func(r map[string]any) {
			withoutProject(r)
			r["parent"].(map[string]any)["hostId"] = "ho|st"
			r["child"].(map[string]any)["hostId"] = "ho|st"
		})
		got := x.run()
		if got["state"] != "admitted" || x.host.count("CreateThread") != 1 {
			t.Fatalf("%v %v", got, x.host.calls)
		}
	})
	t.Run("parent settings equal to the recorded ones", func(t *testing.T) {
		t.Parallel()
		x := newScopeRun(t)
		x.bind("parent")
		x.recordParentSettings(func(map[string]any) {})
		got := x.run()
		if got["state"] != "admitted" || x.host.count("CreateThread") != 1 {
			t.Fatalf("%v %v", got, x.host.calls)
		}
	})
}

// ledgerHook is the fake host with a callback on each RequireLedger, the adapter call the engine makes
// last before it asks the host to create the thread.
type ledgerHook struct {
	*callRecord
	onLedger func()
}

func (h *ledgerHook) RequireLedger(ctx context.Context, expected map[string]any) error {
	if h.onLedger != nil {
		h.onLedger()
	}
	return h.callRecord.RequireLedger(ctx, expected)
}

// The project's parent is unbound after the engine has armed the request and looked at the host, in the
// gap before the effect. The scope ask is the last thing before CreateThread, so it sees the removal and
// nothing is created. The request stays armed and a retry with its own id creates the one child.
func TestPrecreate_BindingRemovedJustBeforeTheEffectCreatesNothing(t *testing.T) {
	t.Parallel()
	x := newScopeRun(t)
	x.bind("parent")
	host := &ledgerHook{callRecord: x.host}
	x.start.Adapter = host
	creating, removed := false, false
	x.host.onGetOperation = func(id string) {
		if strings.HasPrefix(id, "managed-create-") {
			creating = true
		}
	}
	host.onLedger = func() {
		if creating && !removed {
			removed = true
			x.unbind()
		}
	}
	_, err := x.start.Run(x.ctx, x.raw)
	if !removed {
		t.Fatal("the binding was never removed, so the race was not exercised")
	}
	reasonIs(t, err, "unregistered_scope")
	if x.host.count("CreateThread") != 0 || x.requestRow().State != "create_armed" {
		t.Fatalf("armed request: %v %s", x.host.calls, x.requestRow().State)
	}
	x.bind("parent")
	got := x.run()
	if got["state"] != "admitted" || x.host.count("CreateThread") != 1 {
		t.Fatalf("retry after rebinding: %v %v", got, x.host.calls)
	}
}

// protocolLockPath is where the managed-start protocol keeps a project's lock: a sidecar beside the store's
// real directory. A writer of the project's parent binding takes it exclusively, a managed start takes it
// shared from its last scope ask until its child is registered. The tests spell the path out so they hold
// the engine to the protocol and not to its own helper.
func protocolLockPath(storePath, project string) string {
	sum := sha256.Sum256([]byte(project))
	dir, err := filepath.EvalSymlinks(filepath.Dir(storePath))
	if err != nil {
		panic(err)
	}
	return filepath.Join(dir, "managed-start-project-"+hex.EncodeToString(sum[:])+".lock")
}

// holdProject takes the project's lock exclusively without waiting, as a writer of the binding would. It
// reports false when a managed start is inside the span, which is what the lock exists to tell a writer.
func holdProject(t *testing.T, storePath, project string) (release func(), held bool) {
	t.Helper()
	fd, err := unix.Open(protocolLockPath(storePath, project), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, false
		}
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }) }
	t.Cleanup(release)
	return release, true
}

type startOutcome struct {
	got map[string]any
	err error
}

func (x *scopeRun) runAsync(raw []byte) <-chan startOutcome {
	done := make(chan startOutcome, 1)
	go func() {
		result, err := x.start.Run(x.ctx, raw)
		got := map[string]any{}
		for _, f := range result {
			got[f.Key] = f.Value
		}
		done <- startOutcome{got, err}
	}()
	return done
}

func awaitStart(t *testing.T, name string, done <-chan startOutcome) startOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not finish", name)
		return startOutcome{}
	}
}

func admitted(t *testing.T, name string, out startOutcome) {
	t.Helper()
	if out.err != nil || out.got["state"] != "admitted" {
		t.Fatalf("%s: %v %v", name, out.got, out.err)
	}
}

func bindProject(t *testing.T, x *scopeRun, project, parent string) {
	t.Helper()
	if _, err := x.reg.BindScopeAs(x.ctx, "parent", project, registry.Endpoint{TaskID: parent, HostID: "host"}, "active"); err != nil {
		t.Fatal(err)
	}
}

// atCreation signals, once, that the start with this creation request id has reached its creation step:
// the fake host is asked for the creation receipt just before the engine takes the lock.
func atCreation(host *managedFake, creation string) <-chan struct{} {
	reached := make(chan struct{})
	var once sync.Once
	host.onGetOperation = func(id string) {
		if id == creation {
			once.Do(func() { close(reached) })
		}
	}
	return reached
}

// A start waits for a writer that holds the project's lock, and asks about the binding only afterwards. The
// writer removes the binding while it holds the lock; the start then finds it gone and creates nothing. This
// is the race the lock closes: the binding that disappears between the scope check and the creation can
// only do so while a writer holds the lock, and a start does not check or create until it has the lock.
func TestPrecreate_StartWaitsForAWriterOfTheBinding(t *testing.T) {
	create, _ := OperationIDs("managed-1")
	for _, removes := range []bool{true, false} {
		name := "writer keeps the binding"
		if removes {
			name = "writer removes the binding"
		}
		t.Run(name, func(t *testing.T) {
			x := newScopeRun(t)
			x.bind("parent")
			release, held := holdProject(t, x.store.Path, scopeProject)
			if !held {
				t.Fatal("the project's lock was taken by someone else")
			}
			reached := atCreation(x.host.managedFake, create)
			created := make(chan struct{})
			x.host.onCreate = func() { close(created) }
			done := x.runAsync(x.raw)
			recv(t, reached, "the start to reach its creation step")
			select {
			case <-created:
				t.Fatal("the start asked the host to create while a writer held the project's lock")
			case <-time.After(300 * time.Millisecond):
			}
			if removes {
				x.unbind()
			}
			release()
			out := awaitStart(t, "start", done)
			if !removes {
				admitted(t, "start after the writer", out)
				return
			}
			reasonIs(t, out.err, "unregistered_scope")
			if x.host.count("CreateThread") != 0 {
				t.Fatalf("a binding that was gone created a thread: %v", x.host.calls)
			}
			x.bind("parent")
			admitted(t, "retry after rebinding", awaitStart(t, "retry", x.runAsync(x.raw)))
			if x.host.count("CreateThread") != 1 {
				t.Fatalf("retry: %v", x.host.calls)
			}
		})
	}
}

// spanHost is the fake host for starts that overlap. Calls into the shared fake are serialised, and a
// CreateThread can be parked until the test lets it go, which holds that start inside the span while the
// test starts another or tries to take the lock.
type spanHost struct {
	mu       sync.Mutex
	fake     *managedFake
	settings map[string]map[string]any // child settings by creation request id
	parked   map[string]chan struct{}  // creation request id -> closed to let the creation through
	entered  chan string               // creation request ids, as each CreateThread begins
	created  map[string]int
	threads  []any // the threads the host has created, as its thread list shows them
	// afterCreate runs as CreateThread is about to return its receipt, outside the host's lock.
	afterCreate func()
}

func newSpanHost(fake *managedFake) *spanHost {
	return &spanHost{fake: fake, settings: map[string]map[string]any{}, parked: map[string]chan struct{}{}, entered: make(chan string, 16), created: map[string]int{}}
}

func (h *spanHost) createdCount(id string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.created[id]
}

func (h *spanHost) RequireLedger(ctx context.Context, expected map[string]any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fake.RequireLedger(ctx, expected)
}

func (h *spanHost) LedgerIdentityRecord(ctx context.Context) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fake.LedgerIdentityRecord(ctx)
}

func (h *spanHost) GetOperation(ctx context.Context, id string) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fake.GetOperation(ctx, id)
}

func (h *spanHost) CreateThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	receipt, err := h.createThread(ctx, in)
	if err == nil && h.afterCreate != nil {
		h.afterCreate()
	}
	return receipt, err
}

func (h *spanHost) createThread(ctx context.Context, in CreateThreadRequest) (map[string]any, error) {
	h.mu.Lock()
	h.created[in.RequestID]++
	h.mu.Unlock()
	h.entered <- in.RequestID
	if gate, ok := h.parked[in.RequestID]; ok {
		<-gate
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fake.settings = h.settings[in.RequestID]
	receipt, err := h.fake.CreateThread(ctx, in)
	if err != nil {
		return nil, err
	}
	// Every child gets a thread of its own, as the host gives.
	thread := "child-" + strings.TrimPrefix(in.RequestID, "managed-create-")[:12]
	receipt["threadId"] = thread
	receipt["creation"].(map[string]any)["thread"].(map[string]any)["id"] = thread
	h.threads = append(h.threads, map[string]any{"id": thread})
	return receipt, nil
}

// SendMessage runs the engine's guard, which calls back into this host, so it cannot hold the lock.
func (h *spanHost) SendMessage(ctx context.Context, in SendRequest) (map[string]any, error) {
	return h.fake.SendMessage(ctx, in)
}

func (h *spanHost) ReadTurn(ctx context.Context, task, turn string) (*Turn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fake.ReadTurn(ctx, task, turn)
}

func (h *spanHost) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if method == "thread/list" {
		if params["archived"] == true {
			return map[string]any{"data": []any{}}, nil
		}
		return map[string]any{"data": append([]any(nil), h.threads...)}, nil
	}
	return h.fake.HostCall(ctx, method, params)
}

func (h *spanHost) Lifecycle(ctx context.Context, task, workspace string) (bool, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fake.Lifecycle(ctx, task, workspace)
}

// startFor is a request of its own, with its own workspaces and child settings, for the run's store and
// host. It names the same parent settings as the run's request: parents that agree are what a parent that
// starts several issues of one project sends.
func (x *scopeRun) startFor(host *spanHost, id, issue, parent, project string) (raw []byte, creation string) {
	x.t.Helper()
	var base, request map[string]any
	if err := json.Unmarshal(x.raw, &base); err != nil {
		x.t.Fatal(err)
	}
	if err := json.Unmarshal(requestFixture(x.t), &request); err != nil {
		x.t.Fatal(err)
	}
	request["requestId"], request["issueKey"] = id, issue
	request["criteriaSource"], request["scopeRef"] = "issue:"+issue, "issue:"+issue
	request["parent"].(map[string]any)["taskId"] = parent
	request["parent"].(map[string]any)["settings"] = base["parent"].(map[string]any)["settings"]
	request["allowedRecipients"] = []any{parent}
	request["projectKey"] = project
	raw, err := json.Marshal(request)
	if err != nil {
		x.t.Fatal(err)
	}
	creation, _ = OperationIDs(id)
	host.settings[creation] = request["child"].(map[string]any)["settings"].(map[string]any)
	return raw, creation
}

// A start inside the span holds off a writer of the binding: the writer finds the lock taken, and gets it
// once the creation is done.
func TestPrecreate_StartInsideTheSpanHoldsOffAWriter(t *testing.T) {
	t.Parallel()
	x := newScopeRun(t)
	bindProject(t, x, scopeProject, "parent")
	host := newSpanHost(x.host.managedFake)
	x.start.Adapter = host
	raw, create := x.startFor(host, "managed-A", "REL-A", "parent", scopeProject)
	letGo := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(letGo) }) })
	host.parked[create] = letGo
	done := x.runAsync(raw)
	if id := recv(t, host.entered, "the creation to begin"); id != create {
		t.Fatalf("the creation was %s, want %s", id, create)
	}
	if release, held := holdProject(t, x.store.Path, scopeProject); held {
		release()
		t.Fatal("a writer took the project's lock while a start was inside the span")
	}
	once.Do(func() { close(letGo) })
	admitted(t, "start", awaitStart(t, "start", done))
	release, held := holdProject(t, x.store.Path, scopeProject)
	if !held {
		t.Fatal("the start kept the project's lock after its creation")
	}
	release()
}

// The lock is shared among starts, and per project: neither another issue of the same project nor a start
// of another project is held up by a start parked inside its CreateThread.
func TestPrecreate_StartsAreNotSerialised(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, project, parent string
	}{
		{"another issue of the same project", scopeProject, "parent"},
		{"another project", "P-B", "parent-B"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			x := newScopeRun(t)
			bindProject(t, x, scopeProject, "parent")
			if c.project != scopeProject {
				bindProject(t, x, c.project, c.parent)
			}
			host := newSpanHost(x.host.managedFake)
			x.start.Adapter = host
			rawA, createA := x.startFor(host, "managed-A", "REL-A", "parent", scopeProject)
			rawB, createB := x.startFor(host, "managed-B", "REL-B", c.parent, c.project)
			letGo := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(letGo) }) })
			host.parked[createA] = letGo
			doneA := x.runAsync(rawA)
			if id := recv(t, host.entered, "the first creation to begin"); id != createA {
				t.Fatalf("the first creation was %s, want A's", id)
			}
			// A stays parked inside CreateThread for the whole of B's start.
			admitted(t, "B", awaitStart(t, "B", x.runAsync(rawB)))
			if host.createdCount(createA) != 1 || host.createdCount(createB) != 1 {
				t.Fatalf("created A %d, B %d", host.createdCount(createA), host.createdCount(createB))
			}
			once.Do(func() { close(letGo) })
			admitted(t, "A", awaitStart(t, "A", doneA))
		})
	}
}

// recv is a bounded receive: a test that waits on the engine fails with what it waited for, not by hanging.
func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// A writer that holds the lock of one project holds up the starts of that project and of no other.
func TestPrecreate_AWriterOfOneProjectDoesNotHoldUpAnother(t *testing.T) {
	t.Parallel()
	x := newScopeRun(t)
	bindProject(t, x, scopeProject, "parent")
	bindProject(t, x, "P-B", "parent-B")
	host := newSpanHost(x.host.managedFake)
	x.start.Adapter = host
	rawA, createA := x.startFor(host, "managed-A", "REL-A", "parent", scopeProject)
	rawB, createB := x.startFor(host, "managed-B", "REL-B", "parent-B", "P-B")
	release, held := holdProject(t, x.store.Path, scopeProject)
	if !held {
		t.Fatal("the project's lock was taken by someone else")
	}
	reached := atCreation(x.host.managedFake, createA)
	doneA := x.runAsync(rawA)
	recv(t, reached, "A to reach its creation step")
	// A is waiting for the writer; B, of another project, goes through meanwhile.
	admitted(t, "B", awaitStart(t, "B", x.runAsync(rawB)))
	if host.createdCount(createA) != 0 || host.createdCount(createB) != 1 {
		t.Fatalf("created A %d, B %d", host.createdCount(createA), host.createdCount(createB))
	}
	release()
	admitted(t, "A", awaitStart(t, "A", doneA))
}

// A request that already has its creation answer, or is registered, takes no lock: a retry is never held up
// by a writer, and answers as it did.
func TestPrecreate_ReplaysTakeNoLock(t *testing.T) {
	t.Parallel()
	t.Run("attached request", func(t *testing.T) {
		t.Parallel()
		x := newScopeRun(t)
		x.bind("parent")
		x.run()
		release, held := holdProject(t, x.store.Path, scopeProject)
		if !held {
			t.Fatal("the start kept the project's lock")
		}
		defer release()
		admitted(t, "replay", awaitStart(t, "replay", x.runAsync(x.raw)))
		if x.host.count("CreateThread") != 1 {
			t.Fatalf("replay created again: %v", x.host.calls)
		}
	})
	t.Run("request armed with a creation answer", func(t *testing.T) {
		t.Parallel()
		x := newScopeRun(t)
		x.bind("parent")
		x.host.creationEnvironmentChanged = true
		if first := x.run(); first["reason"] != "creation_settings_unverified" {
			t.Fatalf("first run: %v", first)
		}
		release, held := holdProject(t, x.store.Path, scopeProject)
		if !held {
			t.Fatal("the start kept the project's lock")
		}
		defer release()
		out := awaitStart(t, "retry", x.runAsync(x.raw))
		if out.err != nil || out.got["reason"] != "creation_settings_unverified" || x.host.count("CreateThread") != 1 {
			t.Fatalf("retry: %v %v %v", out.got, out.err, x.host.calls)
		}
	})
}

// The checks that already ran before creation keep their place: on a request that is armed and has no
// creation answer, a vanished binding still beats a failing ledger check, and a failing ledger check still
// beats the new refusals, which come last, directly before the effect.
func TestPrecreate_ChecksThatRanBeforeCreationKeepTheirPrecedence(t *testing.T) {
	t.Parallel()
	// armed returns a run whose request is armed with nothing created, and whose ledger check fails on the
	// creation path from now on.
	armed := func(t *testing.T) *scopeRun {
		x := newScopeRun(t)
		x.bind("parent")
		removed := false
		x.host.onGetOperation = func(id string) {
			if strings.HasPrefix(id, "managed-create-") && !removed {
				removed = true
				x.unbind()
			}
		}
		if _, err := x.start.Run(x.ctx, x.raw); err == nil || x.requestRow().State != "create_armed" {
			t.Fatalf("setup: %v %s", err, x.requestRow().State)
		}
		x.host.onGetOperation = nil
		host := &ledgerHook{callRecord: x.host}
		x.start.Adapter = host
		creating := false
		x.host.onGetOperation = func(id string) {
			if strings.HasPrefix(id, "managed-create-") {
				creating = true
			}
		}
		host.onLedger = func() {
			if creating {
				x.host.ledger = map[string]any{"realPath": "replaced", "device": 9, "inode": 9}
			}
		}
		return x
	}
	t.Run("vanished binding beats a failing ledger check", func(t *testing.T) {
		t.Parallel()
		x := armed(t)
		_, err := x.start.Run(x.ctx, x.raw)
		reasonIs(t, err, "unregistered_scope")
		if x.host.count("CreateThread") != 0 {
			t.Fatalf("created: %v", x.host.calls)
		}
	})
	t.Run("failing ledger check beats differing settings", func(t *testing.T) {
		t.Parallel()
		x := armed(t)
		x.bind("parent")
		x.recordParentSettings(func(settings map[string]any) { settings["model"] = "gpt-other" })
		_, err := x.start.Run(x.ctx, x.raw)
		if reason, detail := answer(err); reason != "relationship_conflict" || detail != "ledger replaced" {
			t.Fatalf("answer %q %q", reason, detail)
		}
		if x.host.count("CreateThread") != 0 {
			t.Fatalf("created: %v", x.host.calls)
		}
	})
}
