package managed

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// markerRootRun is one managed start over a temporary store and a fake host: the marker root the request
// names, and the root the daemon judges Stops under.
type markerRootRun struct {
	ctx   context.Context
	dir   string
	store *store.Store
	host  *managedFake
	raw   []byte
}

func newMarkerRootRun(t *testing.T) *markerRootRun {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: pyjson.Map(pyjson.Map(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	return &markerRootRun{ctx: ctx, dir: dir, store: s, host: host, raw: raw}
}

func (r *markerRootRun) start(marker, owner string) *Start {
	return &Start{StandbyWait: testStandbyWait, StandbyPoll: testStandbyPoll, Store: r.store, Adapter: r.host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(r.dir, "socket"), MarkerRoot: marker, OwnerMarkerRoot: owner, StateSelector: r.dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
}

func (r *markerRootRun) mkdir(t *testing.T, elem ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{r.dir}, elem...)...)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// CRW-1309: a marker root other than the one the daemon judges Stops under is refused at the start, naming both
// paths, with no reservation, no thread and no dispatch.
func TestCRW1309_MarkerRootOtherThanTheDaemonsIsRefusedAtStart(t *testing.T) {
	t.Parallel()
	run := newMarkerRootRun(t)
	owner := run.mkdir(t, "owner", "markers")
	other := run.mkdir(t, "parent", "f01-k1-a1", "markers")
	_, err := run.start(other, owner).Run(run.ctx, run.raw)
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a marker root the daemon does not judge under was not refused: %v", err)
	}
	if refused.Reason != "marker_root_mismatch" {
		t.Fatalf("reason %q", refused.Reason)
	}
	if !strings.Contains(refused.Detail, other) || !strings.Contains(refused.Detail, owner) {
		t.Fatalf("the reason names neither both paths: %q", refused.Detail)
	}
	if !strings.Contains(refused.Detail, "control.sock evaluates Stops only under this owner's marker root "+owner) {
		t.Fatalf("the reason does not word the owner's root as the hook side does: %q", refused.Detail)
	}
	if run.host.created != 0 || run.host.sent != 0 || len(run.host.hostCalls) != 0 {
		t.Fatalf("a refused start reached the host: created %d sent %d calls %v", run.host.created, run.host.sent, run.host.hostCalls)
	}
	if _, err := run.store.ManagedStartRequest(run.ctx, "managed-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a refused start left a reservation: %v", err)
	}
	var journaled int64
	if err := run.store.Querier(run.ctx).QueryRowContext(run.ctx, "SELECT count(*) FROM journal WHERE kind='managed_start_observed'").Scan(&journaled); err != nil || journaled != 0 {
		t.Fatalf("a refused start journaled a receipt: %d %v", journaled, err)
	}
}

// CRW-1309: the same root, spelled another way, starts as before: dot segments, a symlink to it, and a root
// that does not exist yet under a symlinked parent.
func TestCRW1309_MarkerRootNamingTheDaemonsRootStarts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, run *markerRootRun) (marker, owner string)
	}{
		{"identical", func(t *testing.T, run *markerRootRun) (string, string) {
			owner := run.mkdir(t, "owner", "markers")
			return owner, owner
		}},
		{"dot segments", func(t *testing.T, run *markerRootRun) (string, string) {
			owner := run.mkdir(t, "owner", "markers")
			run.mkdir(t, "elsewhere")
			return filepath.Join(run.dir, "elsewhere", "..", "owner", ".", "markers"), owner
		}},
		{"symlink to the root", func(t *testing.T, run *markerRootRun) (string, string) {
			owner := run.mkdir(t, "owner", "markers")
			link := filepath.Join(run.dir, "link")
			if err := os.Symlink(owner, link); err != nil {
				t.Fatal(err)
			}
			return link, owner
		}},
		{"owner spelled through a symlink", func(t *testing.T, run *markerRootRun) (string, string) {
			real := run.mkdir(t, "real", "markers")
			link := filepath.Join(run.dir, "home")
			if err := os.Symlink(filepath.Dir(real), link); err != nil {
				t.Fatal(err)
			}
			return real, filepath.Join(link, "markers")
		}},
		{"root not created yet under a symlinked parent", func(t *testing.T, run *markerRootRun) (string, string) {
			real := run.mkdir(t, "real")
			link := filepath.Join(run.dir, "home")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(real, "markers"), filepath.Join(link, "markers")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := newMarkerRootRun(t)
			marker, owner := tc.build(t, run)
			result, err := run.start(marker, owner).Run(run.ctx, run.raw)
			if err != nil {
				t.Fatal(err)
			}
			if result.Get("state") != "admitted" || run.host.created != 1 || run.host.sent != 1 {
				t.Fatalf("a matching root did not start: %v created %d sent %d", result.Get("state"), run.host.created, run.host.sent)
			}
		})
	}
}

// CRW-1309: the same requestId again keeps working through the reservation comparison, whichever root the
// daemon judges under now, and a replay with another root is still the reservation's conflict.
func TestCRW1309_ReplayOfAStartedRequestIsNotRefusedByTheRootCheck(t *testing.T) {
	t.Parallel()
	run := newMarkerRootRun(t)
	owner := run.mkdir(t, "owner", "markers")
	started := run.mkdir(t, "started", "markers")
	// Started before the daemon's root was asked for: nothing to compare against.
	first, err := run.start(started, "").Run(run.ctx, run.raw)
	if err != nil || first.Get("state") != "admitted" {
		t.Fatalf("first start: %v %v", first.Get("state"), err)
	}
	replay, err := run.start(started, owner).Run(run.ctx, run.raw)
	if err != nil {
		t.Fatalf("a replay of a started request was refused by the root check: %v", err)
	}
	if replay.Get("state") != "admitted" || run.host.created != 1 || run.host.sent != 1 {
		t.Fatalf("replay: %v created %d sent %d", replay.Get("state"), run.host.created, run.host.sent)
	}
	// Another root under the same requestId stays the reservation's relationship_conflict.
	_, err = run.start(owner, owner).Run(run.ctx, run.raw)
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "relationship_conflict" {
		t.Fatalf("a replay naming another root: %v", err)
	}
}
