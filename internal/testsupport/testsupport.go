// Package testsupport holds the Go form of the relay test fixtures that more than three Python
// test files share (packages/codex-session-relay/tests/support.py): the registry identities,
// the task-settings record a creation result reports, a clock tests move by hand, and a
// hermetic temporary tree.
//
// The Store, Registry and delivery wiring of RelayTestCase and DeliveryTestCase are not here:
// they need packages that do not exist yet, and they arrive with those packages.
package testsupport

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// RefuseLiveStateEnv is the variable under which the Go store refuses a database below a live
// relay state directory (store.ErrLiveState). The product never sets it and opens the live state;
// this package sets it to "1" in every test binary that links it, before any TestMain runs, so a
// test that forgets isolation is still refused, and every process such a test starts inherits it
// (decisions.md 46). A test that exercises the product's default clears it for itself.
const RefuseLiveStateEnv = "CRW_REFUSE_LIVE_STATE"

// IsolationRootEnv names the isolation root of a tree of test processes. The first test binary to
// isolate exports it, and every process it starts with its environment inherits it. A test binary
// that finds it naming an existing crw-relay-test-* directory makes its own root inside that
// directory instead of in TMPDIR (inheritedRoot), so a helper process of the test binary (a crash
// child, a lock holder) leaves nothing behind however it ends: the process that made the
// directory removes it with everything below. The variable keeps naming the first process's
// root, so a helper's helper nests there too. It is internal to the test binaries: set it by
// hand and the next run nests in whatever directory it names.
const IsolationRootEnv = "CRW_TEST_ISOLATION_ROOT"

// KeepRootEnv, when set to anything but "" or "0", keeps the isolation roots after the tests, so a
// failing run can be examined, and prints each kept root's path to stderr. It is for local
// debugging: nothing removes what it keeps (docs/CI.md has the command).
const KeepRootEnv = "CRW_TEST_KEEP_ROOT"

// rootPrefix starts the name of every isolation root made directly in TMPDIR.
const rootPrefix = "crw-relay-test-"

func init() {
	if testing.Testing() {
		if err := RefuseLiveState(); err != nil {
			panic("testsupport: " + err.Error())
		}
	}
}

// RefuseLiveState sets RefuseLiveStateEnv=1 in this process's environment.
func RefuseLiveState() error { return os.Setenv(RefuseLiveStateEnv, "1") }

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

// IsolateRelayState points every relay-owned home and state root at one temporary tree and
// keeps the live-state refusal (RefuseLiveStateEnv) in force for the process and its children.
// The caller invokes the returned cleanup when it is done; a TestMain uses Main, which does.
func IsolateRelayState() (func() error, error) {
	root, err := isolate()
	if err != nil {
		return nil, err
	}
	return func() error { return removeRoot(root) }, nil
}

// removeRoot removes an isolation root with everything below it, unless KeepRootEnv asks to keep it.
func removeRoot(root string) error {
	if keep := os.Getenv(KeepRootEnv); keep != "" && keep != "0" {
		fmt.Fprintf(os.Stderr, "testsupport: %s is set, keeping the isolation root %s\n", KeepRootEnv, root)
		return nil
	}
	return RemoveTempTree(root)
}

// isolate makes the isolation root and points the homes, the XDG directories and the relay's
// state, scope and marker roots below it. The Go toolchain keeps the module and build caches it
// had before (GOPATH, GOMODCACHE, GOCACHE), so a go command a test runs is not cold.
//
// The root is made in TMPDIR, unless the process was started by a test process that isolated
// (IsolationRootEnv): then it is made inside that process's root, which that process removes.
func isolate() (string, error) {
	toolchain, err := toolchainDirs()
	if err != nil {
		return "", err
	}
	top, nested := inheritedRoot()
	var root string
	if nested {
		root, err = os.MkdirTemp(top, "h-")
	} else {
		root, err = os.MkdirTemp("", rootPrefix)
		top = root
	}
	if err != nil {
		return "", err
	}
	env := map[string]string{
		IsolationRootEnv:                  top,
		"HOME":                            filepath.Join(root, "home"),
		"XDG_STATE_HOME":                  filepath.Join(root, "xdg-state"),
		"XDG_DATA_HOME":                   filepath.Join(root, "xdg-data"),
		"XDG_CONFIG_HOME":                 filepath.Join(root, "xdg-config"),
		"XDG_CACHE_HOME":                  filepath.Join(root, "xdg-cache"),
		"CODEX_HOME":                      filepath.Join(root, "codex-home"),
		"CODEX_SESSION_RELAY_STATE":       filepath.Join(root, "relay-state"),
		"CODEX_SESSION_RELAY_SCOPE_DIR":   filepath.Join(root, "scopes"),
		"CODEX_SESSION_RELAY_MARKER_ROOT": filepath.Join(root, "markers"),
	}
	maps.Copy(env, toolchain)
	for key, path := range env {
		if err := os.Setenv(key, path); err != nil {
			return "", errors.Join(err, RemoveTempTree(root))
		}
	}
	if err := RefuseLiveState(); err != nil {
		return "", errors.Join(err, RemoveTempTree(root))
	}
	return root, nil
}

// inheritedRoot is the isolation root a test process that started this one exported, when the
// variable names one that exists: an absolute path to a directory called crw-relay-test-*.
// Anything else (unset, relative, missing, not a directory, another name) is not a root to nest in.
func inheritedRoot() (string, bool) {
	root := os.Getenv(IsolationRootEnv)
	if !filepath.IsAbs(root) || !strings.HasPrefix(filepath.Base(root), rootPrefix) {
		return "", false
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return "", false
	}
	return root, true
}

// toolchainDirs are the Go toolchain's module and build cache directories as this process has
// them before isolation: each variable as set, else the default the go command derives from the
// home and the user cache directory.
func toolchainDirs() (map[string]string, error) {
	dirs := map[string]string{"GOPATH": os.Getenv("GOPATH"), "GOMODCACHE": os.Getenv("GOMODCACHE"), "GOCACHE": os.Getenv("GOCACHE")}
	if dirs["GOPATH"] == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dirs["GOPATH"] = filepath.Join(home, "go")
	}
	if dirs["GOMODCACHE"] == "" {
		dirs["GOMODCACHE"] = filepath.Join(filepath.SplitList(dirs["GOPATH"])[0], "pkg", "mod")
	}
	if dirs["GOCACHE"] == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		dirs["GOCACHE"] = filepath.Join(cache, "go-build")
	}
	return dirs, nil
}

// Setup extends Main for a package whose tests need more than the isolated relay state. It runs
// once the state is isolated and is given the isolation root, which Main removes after the tests;
// the cleanup it returns (nil for none) runs after the tests, before the root is removed.
type Setup func(root string) (cleanup func() error, err error)

// Main is a test package's TestMain: it isolates the relay state (IsolateRelayState), runs each
// setup in order, runs the tests, then runs the setups' cleanups (the last first), removes the
// isolation root and every binary this process built (CRW, CRWDevPath, BuildCRW), and exits with
// the tests' code, or 1 when isolation, a setup or a cleanup failed. A package whose test binary
// also runs as a helper process (a crash child, a lock holder) may start the helper before calling
// Main: the helper then inherits its parent's isolation and makes no root. A helper that does go
// through Main makes its root inside its parent's (IsolationRootEnv), which the parent removes, so
// a helper that ends by a signal or os.Exit before its own cleanup leaves nothing behind.
func Main(m *testing.M, setups ...Setup) {
	os.Exit(run(m, setups))
}

func run(m *testing.M, setups []Setup) (code int) {
	root, err := isolate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "testsupport: isolate the relay state:", err)
		return 1
	}
	cleanups := []func() error{removeBuilds, func() error { return removeRoot(root) }}
	defer func() {
		var errs []error
		for i := len(cleanups) - 1; i >= 0; i-- {
			errs = append(errs, cleanups[i]())
		}
		if err := errors.Join(errs...); err != nil {
			fmt.Fprintln(os.Stderr, "testsupport: clean up after the tests:", err)
			code = 1
		}
	}()
	for _, setup := range setups {
		cleanup, err := setup(root)
		if cleanup != nil {
			cleanups = append(cleanups, cleanup)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "testsupport: set up the tests:", err)
			return 1
		}
	}
	return m.Run()
}

// TempDirInRoot is a Setup that points TMPDIR at the isolation root, so the temporary directories
// the tests and the processes they start make (t.TempDir, os.MkdirTemp) lie in the tree Main
// removes.
func TempDirInRoot(root string) (func() error, error) { return nil, os.Setenv("TMPDIR", root) }

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
