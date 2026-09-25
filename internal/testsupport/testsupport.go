// Package testsupport holds the Go form of the relay test fixtures that more than three Python
// test files share (packages/codex-session-relay/tests/support.py): the registry identities,
// the task-settings record a creation result reports, a clock tests move by hand, and a
// hermetic temporary tree.
//
// The Store, Registry and delivery wiring of RelayTestCase and DeliveryTestCase are not here:
// they need packages that do not exist yet, and they arrive with those packages.
package testsupport

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Fixture identities match the registry on purpose: the child thread is the registered child
// task id and the turn is the bound dispatch turn.
const (
	Parent       = "01parent-task"
	Child        = "01child-task"
	Issue        = "REL-1"
	Host         = "host-a"
	DispatchTurn = "turn-dispatch-1"
)

// TaskSettings is the execution-settings record a creation result reports for cwd, shaped like
// the real receipt. An override replaces a field in place and a new key is appended, as Python's
// dict.update does, so the record serializes in the same key order.
func TaskSettings(cwd string, overrides ...contract.Field) contract.OrderedObject {
	settings := contract.OrderedObject{
		{Key: "sandbox", Value: contract.OrderedObject{
			{Key: "type", Value: "workspaceWrite"},
			{Key: "writableRoots", Value: []any{}},
			{Key: "networkAccess", Value: false},
			{Key: "excludeTmpdirEnvVar", Value: false},
			{Key: "excludeSlashTmp", Value: false},
		}},
		{Key: "approvalPolicy", Value: "never"},
		{Key: "cwd", Value: cwd},
		{Key: "runtimeWorkspaceRoots", Value: []any{cwd}},
		{Key: "model", Value: "anthropic/claude-opus-5"},
		{Key: "reasoningEffort", Value: "xhigh"},
		{Key: "environments", Value: []any{contract.OrderedObject{
			{Key: "environmentId", Value: "local"},
			{Key: "cwd", Value: cwd},
			{Key: "runtimeWorkspaceRoots", Value: []any{cwd}},
		}}},
	}
	for _, override := range overrides {
		settings = set(settings, override)
	}
	return settings
}

func set(object contract.OrderedObject, field contract.Field) contract.OrderedObject {
	for i, existing := range object {
		if existing.Key == field.Key {
			updated := append(contract.OrderedObject(nil), object...)
			updated[i] = field
			return updated
		}
	}
	return append(object, field)
}

// FakeClock is a clock tests move by hand, so no test sleeps.
type FakeClock struct {
	now time.Time
}

// NewFakeClock starts at the Python FakeClock's default instant, 1_700_000_000 seconds.
func NewFakeClock() *FakeClock {
	return &FakeClock{now: time.Unix(1_700_000_000, 0).UTC()}
}

// Now is the clock's current instant.
func (c *FakeClock) Now() time.Time { return c.now }

// Advance moves the clock forward and returns the new instant.
func (c *FakeClock) Advance(d time.Duration) time.Time {
	c.now = c.now.Add(d)
	return c.now
}

// ISO formats the instant as Python's isoformat(timespec="microseconds") on a UTC datetime.
func (c *FakeClock) ISO() string {
	return c.now.UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// Tree is one test's own temporary tree.
type Tree struct {
	t *testing.T
	// Tmp is the tree's root; everything below is removed when the test ends.
	Tmp string
	// Root is the work directory artifacts are written under.
	Root string
	// StatePath is where the test's relay database lives: Tmp/state/relay.sqlite3.
	StatePath string
	Clock     *FakeClock
}

// NewTree builds the tree and points XDG_STATE_HOME inside it, so neither the test nor a relay
// process it spawns reads the host record this machine actually holds.
func NewTree(t *testing.T) *Tree {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "work")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("testsupport: work dir: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))
	return &Tree{
		t:         t,
		Tmp:       tmp,
		Root:      root,
		StatePath: filepath.Join(tmp, "state", "relay.sqlite3"),
		Clock:     NewFakeClock(),
	}
}

// Artifact writes text to name under Root, creating parent directories, and returns its path.
func (tr *Tree) Artifact(name, text string) string {
	tr.t.Helper()
	path := filepath.Join(tr.Root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tr.t.Fatalf("testsupport: artifact dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		tr.t.Fatalf("testsupport: artifact: %v", err)
	}
	return path
}
