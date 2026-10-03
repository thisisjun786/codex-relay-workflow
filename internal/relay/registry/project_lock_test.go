package registry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/projectlock"
)

// A writer of a project's parent binding takes the project's lock exclusively, so it waits for a managed
// start that holds it shared and a start waits for the writer. Each case holds the lock shared the way a
// start inside its span does and shrinks the wait bound: the writer must give up with the lock's own
// error and leave the store as it was (it reached the lock and took nothing), and once the holder lets go
// the same call must succeed and change the binding it is meant to change.

func shrinkLockWait(t *testing.T) {
	t.Helper()
	saved := ownership.LockWait
	ownership.LockWait = 150 * time.Millisecond
	t.Cleanup(func() { ownership.LockWait = saved })
}

// storeState is every row a binding writer can touch, as one comparable text.
func storeState(t *testing.T, r *Registry) string {
	t.Helper()
	var out bytes.Buffer
	for _, table := range []string{"scope_bindings", "scope_links", "linkage_conflicts", "journal"} {
		rows, err := r.Store.All(ctx(), "SELECT * FROM "+table+" ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		out.WriteString(table)
		for _, row := range rows {
			out.WriteString("|")
			out.WriteString(rowText(row))
		}
		out.WriteString("\n")
	}
	return out.String()
}

func rowText(row store.Row) string {
	var out bytes.Buffer
	for _, column := range row {
		out.WriteString(column.Name)
		out.WriteString("=")
		out.WriteString(fmt.Sprint(column.Value))
		out.WriteString(",")
	}
	return out.String()
}

func TestProjectParentWriters_WaitForTheProjectLock(t *testing.T) {
	// Serial: assigns ownership.LockWait, which every other running test would read, and bounds waits by the wall clock.
	supervisor := Endpoint{"supervisor", host, ns("/supervisor"), ns("cxc-supervisor")}
	other := Endpoint{"other-parent", host, ns("/other"), ns("cxc-other")}
	own := Endpoint{parent, host, ns("/parent"), ns("cxc-" + parent)}
	cases := []struct {
		name    string
		project string
		setup   func(*testing.T, *Registry)
		write   func(*Registry) error
		changed func(*testing.T, *Registry) bool
	}{
		{name: "BindScope inserts a parent", project: "P1",
			write:   func(r *Registry) error { _, err := r.BindScope(ctx(), roleParent, "P1", own); return err },
			changed: func(t *testing.T, r *Registry) bool { return liveParent(t, r, "P1") == parent }},
		{name: "BindScopeAs inserts a parent", project: "P2",
			write:   func(r *Registry) error { _, err := r.BindScopeAs(ctx(), roleParent, "P2", own, Active); return err },
			changed: func(t *testing.T, r *Registry) bool { return liveParent(t, r, "P2") == parent }},
		{name: "BindScopeAs reactivates a cancelled parent", project: "P3",
			setup: func(t *testing.T, r *Registry) {
				if _, err := r.BindScopeAs(ctx(), roleParent, "P3", own, "cancelled"); err != nil {
					t.Fatal(err)
				}
				if liveParent(t, r, "P3") != "" {
					t.Fatal("a cancelled parent is live")
				}
			},
			write:   func(r *Registry) error { _, err := r.BindScopeAs(ctx(), roleParent, "P3", own, Active); return err },
			changed: func(t *testing.T, r *Registry) bool { return liveParent(t, r, "P3") == parent }},
		{name: "Handover moves the parent", project: "P4",
			setup: func(t *testing.T, r *Registry) { bindProject(t, r, "P4", parent) },
			write: func(r *Registry) error {
				_, err := r.Handover(ctx(), roleParent, "P4", parent, other, nil, "evidence", "test")
				return err
			},
			changed: func(t *testing.T, r *Registry) bool { return liveParent(t, r, "P4") == "other-parent" }},
		{name: "RegisterSupervision binds the project's parent", project: "P5",
			write: func(r *Registry) error {
				_, err := r.RegisterSupervision(ctx(), "INIT-1", "P5", supervisor, own, linkExec)
				return err
			},
			changed: func(t *testing.T, r *Registry) bool { return liveParent(t, r, "P5") == parent }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRegistry(t)
			if c.setup != nil {
				c.setup(t, r)
			}
			held, err := projectlock.Shared(ctx(), r.Store.Path, c.project)
			if err != nil {
				t.Fatal(err)
			}
			released := false
			t.Cleanup(func() {
				if !released {
					_ = held()
				}
			})
			shrinkLockWait(t)
			before := storeState(t, r)
			var expired *ownership.LockWaitExpired
			if err := c.write(r); !errors.As(err, &expired) {
				t.Fatalf("the writer did not wait for a start inside the span: %v", err)
			}
			if after := storeState(t, r); after != before {
				t.Fatalf("a writer that gave up changed the store:\nbefore %s\nafter  %s", before, after)
			}
			if c.changed(t, r) {
				t.Fatal("the binding changed although the writer gave up")
			}
			if err := held(); err != nil {
				t.Fatal(err)
			}
			released = true
			if err := c.write(r); err != nil {
				t.Fatalf("the writer failed once the start had left the span: %v", err)
			}
			if !c.changed(t, r) {
				t.Fatal("the writer did not change the binding it is for")
			}
		})
	}
}

// liveParent is the task bound as the live parent of project, or "".
func liveParent(t *testing.T, r *Registry, project string) string {
	t.Helper()
	rows, err := r.Store.All(ctx(), "SELECT task_id FROM scope_bindings WHERE scope_kind = 'project' AND scope_key = ? AND role = 'parent'"+
		" AND status IN ('active','paused') AND superseded_by IS NULL", project)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return ""
	}
	return colString(rows[0], "task_id")
}

// Writers that do not bind a project's parent take no lock, and the lock of one project holds up nothing of
// another: with a project's lock held shared, a bind of the same text under the child or supervisor role,
// and a bind of another project's parent, return at once.
func TestProjectParentWriters_OnlyTheProjectParentWaits(t *testing.T) {
	// Serial: assigns ownership.LockWait, which every other running test would read, and bounds waits by the wall clock.
	r := newRegistry(t)
	held, err := projectlock.Shared(ctx(), r.Store.Path, "K")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	shrinkLockWait(t)
	for _, c := range []struct {
		name string
		bind func() error
	}{
		{"a child bound under an issue of the same key", func() error {
			_, err := r.BindScopeAs(ctx(), roleChild, "K", Endpoint{child, host, ns("/child"), ns("cxc-" + child)}, Active)
			return err
		}},
		{"a supervisor bound under an initiative of the same key", func() error {
			_, err := r.BindScopeAs(ctx(), "supervisor", "K", Endpoint{"supervisor", host, ns("/supervisor"), ns("cxc-supervisor")}, Active)
			return err
		}},
		{"the parent of another project", func() error {
			_, err := r.BindScopeAs(ctx(), roleParent, "K2", Endpoint{parent, host, ns("/parent"), ns("cxc-" + parent)}, Active)
			return err
		}},
	} {
		if err := c.bind(); err != nil {
			t.Fatalf("%s waited for the lock of K: %v", c.name, err)
		}
	}
}

// A store with no path has no place for the lock file: the writer refuses with a host error, and does not
// run unlocked or leave a sidecar in the working directory.
func TestProjectParentWriters_AStoreWithoutAPathIsRefused(t *testing.T) {
	t.Parallel()
	r := &Registry{Store: &store.Store{}, Now: func() string { return fakeISO }}
	_, err := r.BindScopeAs(ctx(), roleParent, "P1", Endpoint{parent, host, ns("/parent"), ns("cxc-" + parent)}, Active)
	if err == nil {
		t.Fatal("a writer of a store with no path ran unlocked")
	}
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		t.Fatalf("a missing path is a host failure, not a refusal: %v", err)
	}
}

// The writer's whole answer on the built command line when it waits out the bound: the host envelope, exit
// 3, the lock's own text; nothing changed. The reasons and exit codes of every other ending are untouched.
func TestProjectParentWriters_LockWaitExpiredIsTheHostEnvelope(t *testing.T) {
	// Serial: assigns ownership.LockWait, which every other running test would read, and bounds waits by the wall clock.
	state := filepath.Join(t.TempDir(), "state")
	run := func(argv ...string) (int, string) {
		var stdout, stderr bytes.Buffer
		code := dispatch.Execute(context.Background(), "codex-session-relay", append([]string{"--state", state}, argv...), &stdout, &stderr)
		return code, stdout.String()
	}
	bind := func(project string) (int, string) {
		return run("linkage-bind", "--role", "parent", "--scope", project, "--task", "parent-"+project, "--host", "host")
	}
	if code, out := bind("P0"); code != 0 {
		t.Fatalf("setup bind: exit %d %q", code, out)
	}
	held, err := projectlock.Shared(ctx(), filepath.Join(state, "relay.sqlite3"), "P1")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	shrinkLockWait(t)
	code, out := bind("P1")
	want := "{\n  \"error\": \"host\",\n  \"detail\": \"LockWaitExpired: the project binding lock was not acquired within 0.15s; retry\"\n}\n"
	if code != 3 || out != want {
		t.Fatalf("exit %d stdout %q, want exit 3 stdout %q", code, out, want)
	}
	// Another project's bind, and the same bind once the start has let go, answer as ever.
	if code, out := bind("P2"); code != 0 {
		t.Fatalf("another project: exit %d %q", code, out)
	}
	if err := held(); err != nil {
		t.Fatal(err)
	}
	if code, out := bind("P1"); code != 0 {
		t.Fatalf("after the start left: exit %d %q", code, out)
	}
}

// Two spellings of one store are one lock: a start holds the project's lock through the store's real path,
// and a writer that reached the same store through a link to the file waits for it.
func TestProjectParentWriters_ALinkedSpellingOfTheStoreIsTheSameLock(t *testing.T) {
	// Serial: assigns ownership.LockWait, which every other running test would read, and bounds waits by the wall clock.
	r := newRegistry(t)
	link := filepath.Join(t.TempDir(), "alias.sqlite3")
	if err := os.Symlink(r.Store.Path, link); err != nil {
		t.Fatal(err)
	}
	other, err := store.Open(ctx(), link, "")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	aliased := &Registry{Store: other, Now: r.Now, Policy: r.Policy}
	held, err := projectlock.Shared(ctx(), r.Store.Path, "P1")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	shrinkLockWait(t)
	var expired *ownership.LockWaitExpired
	_, err = aliased.BindScopeAs(ctx(), roleParent, "P1", Endpoint{parent, host, ns("/parent"), ns("cxc-" + parent)}, Active)
	if !errors.As(err, &expired) {
		t.Fatalf("a writer reaching the store through a link did not meet the start's lock: %v", err)
	}
}

// A store with no path has no place for the lock file: the writer refuses with a host error, and does not
