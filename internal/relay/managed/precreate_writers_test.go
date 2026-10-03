package managed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// These tests run the registry's real writers of a project's parent binding against managed starts that are
// parked inside the span the project lock covers. The writer is the one the CLI runs for linkage-handover
// and linkage-bind, not a test double: the lock is only worth anything if the writers take it.

// parentOf is the task bound as the live parent of project, as the store says now.
func (x *scopeRun) parentOf(project string) string {
	x.t.Helper()
	var task string
	err := x.store.DB.QueryRowContext(x.ctx, "SELECT task_id FROM scope_bindings WHERE scope_kind = 'project' AND scope_key = ? AND role = 'parent'"+
		" AND status IN ('active','paused') AND superseded_by IS NULL", project).Scan(&task)
	if err != nil {
		return ""
	}
	return task
}

func (x *scopeRun) handover(project, from, to string) error {
	_, err := x.reg.Handover(x.ctx, "parent", project, from, registry.Endpoint{TaskID: to, HostID: "host"}, nil, "evidence", "test")
	return err
}

// writerDone runs a writer in a goroutine and reports its answer on the channel.
func writerDone(write func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- write() }()
	return done
}

// stillRunning fails the test when the writer returned inside the window, which is longer than a writer
// that was not held up needs to return; the matching positive check, the writer returning once the start
// has left the span, is the caller's.
func stillRunning(t *testing.T, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s returned (%v) while a start was inside the span", what, err)
	case <-time.After(300 * time.Millisecond):
	}
}

// reachedTheLock runs the writer once with the wait bound shrunk, while the start is parked: a writer that
// gives up with the lock's own error has reached the lock and waited for it, which silence alone does not
// show on a loaded host. It runs before the unbounded writer of the test starts, so the bound is never
// read and written at once.
func reachedTheLock(t *testing.T, what string, write func() error) {
	t.Helper()
	saved := ownership.LockWait
	ownership.LockWait = 150 * time.Millisecond
	err := write()
	ownership.LockWait = saved
	var expired *ownership.LockWaitExpired
	if !errors.As(err, &expired) {
		t.Fatalf("%s did not wait for the project lock while a start was inside the span: %v", what, err)
	}
}

// parked is a start of project, request id and issue, held inside its CreateThread until release is called.
func parkedStart(t *testing.T, x *scopeRun, host *spanHost, id, issue, parent, project string) (done <-chan startOutcome, release func()) {
	t.Helper()
	raw, create := x.startFor(host, id, issue, parent, project)
	letGo := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(letGo) }) }
	t.Cleanup(release)
	host.parked[create] = letGo
	done = x.runAsync(raw)
	if got := recv(t, host.entered, "the creation to begin"); got != create {
		t.Fatalf("the creation was %s, want %s", got, create)
	}
	return done, release
}

// The control for the race below: with no start in the span the same handover moves the binding, so a
// handover that does not move it in the race was held back, not refused for some other reason.
func TestPrecreate_HandoverMovesTheBindingWhenNoStartIsInTheSpan(t *testing.T) {
	t.Parallel()
	x := newScopeRun(t)
	bindProject(t, x, scopeProject, "parent")
	if err := x.handover(scopeProject, "parent", "other-parent"); err != nil {
		t.Fatal(err)
	}
	if got := x.parentOf(scopeProject); got != "other-parent" {
		t.Fatalf("parent after the handover: %q", got)
	}
}

// A real handover of the project's parent does not change the binding while a start is parked between its
// last scope ask and CreateThread: it waits for the start, which then registers its child under the parent
// it asked about. Without the lock the handover completes at once and moves the binding under the start.
func TestPrecreate_RealHandoverWaitsForAStartInsideTheSpan(t *testing.T) {
	x := newScopeRun(t)
	bindProject(t, x, scopeProject, "parent")
	host := newSpanHost(x.host.managedFake)
	x.start.Adapter = host
	doneA, release := parkedStart(t, x, host, "managed-A", "REL-A", "parent", scopeProject)
	reachedTheLock(t, "the handover", func() error { return x.handover(scopeProject, "parent", "other-parent") })
	write := writerDone(func() error { return x.handover(scopeProject, "parent", "other-parent") })
	stillRunning(t, "the handover", write)
	if got := x.parentOf(scopeProject); got != "parent" {
		t.Fatalf("the binding changed to %q while a start was inside the span", got)
	}
	release()
	admitted(t, "start", awaitStart(t, "start", doneA))
	// The start registered a child under the project, so the handover, which acknowledged no unfinished
	// work, is refused when it finally runs: the binding it would have moved under the start stays.
	reasonIs(t, recv(t, write, "the handover to return"), "handover_unconfirmed")
	if got := x.parentOf(scopeProject); got != "parent" {
		t.Fatalf("parent after the refused handover: %q", got)
	}
	if x.count("relationships") != 1 {
		t.Fatalf("the start left %d relationships", x.count("relationships"))
	}
}

// The hold reaches the registration: a start parked after its CreateThread returned and before Register,
// at the clock call BindIdentity makes (the second after the creation; the first is inside the receipt's
// own transaction), still holds the writer off. Releasing the lock when CreateThread returns would let the
// handover move the binding here, and the registration would then be refused with the thread already made.
func TestPrecreate_RealHandoverWaitsForTheRegistration(t *testing.T) {
	x := newScopeRun(t)
	bindProject(t, x, scopeProject, "parent")
	host := newSpanHost(x.host.managedFake)
	x.start.Adapter = host
	var mu sync.Mutex
	created, afterCreation := false, 0
	host.afterCreate = func() { mu.Lock(); created = true; mu.Unlock() }
	atRegistration, letGo := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(letGo) }) })
	clock := x.start.Now
	x.start.Now = func() string {
		mu.Lock()
		if created {
			afterCreation++
			if afterCreation == 2 {
				mu.Unlock()
				close(atRegistration)
				<-letGo
				return clock()
			}
		}
		mu.Unlock()
		return clock()
	}
	raw, _ := x.startFor(host, "managed-A", "REL-A", "parent", scopeProject)
	doneA := x.runAsync(raw)
	recv(t, atRegistration, "the start to reach its registration")
	// The park holds no store lock: another write goes through while it lasts.
	ctx, cancel := context.WithTimeout(x.ctx, 5*time.Second)
	defer cancel()
	if _, err := x.store.DB.ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES('2026-09-26T00:00:00.000000+00:00','test','probe','{}')"); err != nil {
		t.Fatalf("the parked start holds the store: %v", err)
	}
	reachedTheLock(t, "the handover", func() error { return x.handover(scopeProject, "parent", "other-parent") })
	write := writerDone(func() error { return x.handover(scopeProject, "parent", "other-parent") })
	stillRunning(t, "the handover", write)
	if got := x.parentOf(scopeProject); got != "parent" {
		t.Fatalf("the binding changed to %q between the creation and the registration", got)
	}
	once.Do(func() { close(letGo) })
	admitted(t, "start", awaitStart(t, "start", doneA))
	reasonIs(t, recv(t, write, "the handover to return"), "handover_unconfirmed")
}

// A binding write that changes nothing takes the lock all the same: bind the parent it already has.
func TestPrecreate_RealBindWaitsForAStartInsideTheSpan(t *testing.T) {
	x := newScopeRun(t)
	bindProject(t, x, scopeProject, "parent")
	host := newSpanHost(x.host.managedFake)
	x.start.Adapter = host
	doneA, release := parkedStart(t, x, host, "managed-A", "REL-A", "parent", scopeProject)
	bind := func() error {
		_, err := x.reg.BindScopeAs(x.ctx, "parent", scopeProject, registry.Endpoint{TaskID: "parent", HostID: "host"}, registry.Active)
		return err
	}
	reachedTheLock(t, "the bind", bind)
	write := writerDone(bind)
	stillRunning(t, "the bind", write)
	release()
	admitted(t, "start", awaitStart(t, "start", doneA))
	if err := recv(t, write, "the bind to return"); err != nil {
		t.Fatal(err)
	}
}

// Writers and starts of different projects do not wait for each other: with a start of A parked in its
// span and a handover of A waiting behind it, a start of B completes, and so does a handover of B.
func TestPrecreate_OtherProjectsNeitherWaitNorAreWaitedFor(t *testing.T) {
	x := newScopeRun(t)
	bindProject(t, x, "P-A", "parent-A")
	bindProject(t, x, "P-B", "parent-B")
	host := newSpanHost(x.host.managedFake)
	x.start.Adapter = host
	doneA, release := parkedStart(t, x, host, "managed-A", "REL-A", "parent-A", "P-A")
	reachedTheLock(t, "the handover of A", func() error { return x.handover("P-A", "parent-A", "other-A") })
	writeA := writerDone(func() error { return x.handover("P-A", "parent-A", "other-A") })
	stillRunning(t, "the handover of A", writeA)
	rawB, _ := x.startFor(host, "managed-B", "REL-B", "parent-B", "P-B")
	admitted(t, "start of B", awaitStart(t, "start of B", x.runAsync(rawB)))
	if err := x.handover("P-B", "parent-B", "other-B"); err == nil {
		t.Fatal("the handover of B went through although B's start registered a child")
	} else {
		reasonIs(t, err, "handover_unconfirmed")
	}
	release()
	admitted(t, "start of A", awaitStart(t, "start of A", doneA))
	reasonIs(t, recv(t, writeA, "the handover of A to return"), "handover_unconfirmed")
}

// The lock is let go on every way a start can end, so a writer never waits for one that is over.
func TestPrecreate_TheProjectLockIsFreeAfterEveryEnding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(*scopeRun)
		want  string // the answer's reason, or "" for an admitted start
	}{
		{name: "admitted", setup: func(x *scopeRun) { x.bind("parent") }},
		{name: "refused at the last scope ask", setup: func(x *scopeRun) {
			x.bind("parent")
			creating := false
			x.host.onGetOperation = func(id string) {
				if len(id) > 15 && id[:15] == "managed-create-" && !creating {
					creating = true
					x.unbind()
				}
			}
		}, want: "unregistered_scope"},
		{name: "refused by the request asks", setup: func(x *scopeRun) {
			x.bind("parent")
			x.recordParentSettings(func(settings map[string]any) { settings["model"] = "gpt-other" })
		}, want: "relationship_conflict"},
		{name: "creation settings unverified", setup: func(x *scopeRun) {
			x.bind("parent")
			x.host.creationEnvironmentChanged = true
		}, want: "creation_settings_unverified"},
		// The preflight readiness passes; the one in the creation step, with the lock already held, refuses.
		{name: "readiness refused once the lock is held", setup: func(x *scopeRun) {
			x.bind("parent")
			calls := 0
			x.start.Readiness = func(context.Context, map[string]any) (string, error) {
				calls++
				if calls > 1 {
					return "worker_policy_unconfigured", nil
				}
				return "", nil
			}
		}, want: "worker_policy_unconfigured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			x := newScopeRun(t)
			c.setup(x)
			result, err := x.start.Run(x.ctx, x.raw)
			got := ""
			if err != nil {
				got, _ = answer(err)
			} else {
				for _, f := range result {
					if f.Key == "reason" && f.Value != nil {
						got, _ = f.Value.(string)
					}
				}
			}
			if got != c.want {
				t.Fatalf("answer %q (%v), want %q", got, err, c.want)
			}
			release, held := holdProject(t, x.store.Path, scopeProject)
			if !held {
				t.Fatal("the project lock was still held after the start ended")
			}
			release()
		})
	}
}
