package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// The properties of StableLauncherRemovalTest (test_plugin_wiring.py), which retired with
// plugin_transition.py: removal takes only the file CRW placed, proves the marker again under
// the lock the placement takes, and never touches the settings.

type launcherHome struct {
	codex, launcher, settings string
	settingsBytes             []byte
}

// newLauncherHome is a Codex home holding a launcher CRW placed (legacyLauncher, standing in for
// the one the package shipped until todo 43), the way the placement left it, beside a settings
// document the removal must not touch.
func newLauncherHome(t *testing.T) launcherHome {
	t.Helper()
	h := launcherHome{codex: t.TempDir()}
	h.launcher = filepath.Join(h.codex, install.LauncherName)
	h.settings = filepath.Join(h.codex, install.SettingsName)
	write(t, h.launcher, legacyLauncher)
	h.settingsBytes = []byte(`{"configVersion": 1, "owner": "plugin"}`)
	write(t, h.settings, string(h.settingsBytes))
	return h
}

func (h launcherHome) settingsUntouched(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(h.settings)
	if err != nil || string(got) != string(h.settingsBytes) {
		t.Fatalf("the settings changed: %q %v", got, err)
	}
	if _, err := os.Lstat(h.launcher + record.LockSuffix); !os.IsNotExist(err) {
		t.Fatalf("the launcher's lock was left behind: %v", err)
	}
}

func outcomeOf(t *testing.T, answer install.Object) (string, string) {
	t.Helper()
	return record.Get(answer, "outcome").(string), record.Get(answer, "detail").(string)
}

func TestRemoveLauncherTakesTheFileItPlacedAndNothingElse(t *testing.T) {
	h := newLauncherHome(t)
	if outcome, _ := outcomeOf(t, install.RemoveLauncher(context.Background(), h.codex, false)); outcome != install.LauncherWould {
		t.Fatalf("dry run: %s", outcome)
	}
	if _, err := os.Stat(h.launcher); err != nil {
		t.Fatalf("a dry run removed the launcher: %v", err)
	}
	answer := install.RemoveLauncher(context.Background(), h.codex, true)
	if outcome, detail := outcomeOf(t, answer); outcome != install.LauncherRemoved {
		t.Fatalf("apply: %s %s", outcome, detail)
	}
	if record.Get(answer, "settingsPresent") != true {
		t.Fatalf("the answer does not report the settings the host still holds: %v", answer)
	}
	if _, err := os.Lstat(h.launcher); !os.IsNotExist(err) {
		t.Fatalf("the launcher is still there: %v", err)
	}
	h.settingsUntouched(t)
}

func TestRemoveLauncherOnAnAbsentFileIsANoOp(t *testing.T) {
	codex := t.TempDir()
	answer := install.RemoveLauncher(context.Background(), codex, true)
	if outcome, _ := outcomeOf(t, answer); outcome != install.LauncherAbsent {
		t.Fatalf("absent: %s", outcome)
	}
	if record.Get(answer, "wrote") != false {
		t.Fatalf("absent reported a write: %v", answer)
	}
	if entries, _ := os.ReadDir(codex); len(entries) != 0 {
		t.Fatalf("an absent launcher left %d entries behind", len(entries))
	}
}

func TestRemoveLauncherRefusesAFileWithoutTheMarker(t *testing.T) {
	h := newLauncherHome(t)
	write(t, h.launcher, "not ours\n")
	if outcome, detail := outcomeOf(t, install.RemoveLauncher(context.Background(), h.codex, true)); outcome != install.LauncherRefused || !strings.Contains(detail, install.LauncherMarker) {
		t.Fatalf("foreign file: %s %s", outcome, detail)
	}
	if got, _ := os.ReadFile(h.launcher); string(got) != "not ours\n" {
		t.Fatalf("the foreign file changed: %q", got)
	}
	h.settingsUntouched(t)
}

func TestRemoveLauncherNeverFollowsALinkOrADirectory(t *testing.T) {
	h := newLauncherHome(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.py")
	shipped, _ := os.ReadFile(h.launcher)
	write(t, elsewhere, string(shipped))
	if err := os.Remove(h.launcher); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, h.launcher); err != nil {
		t.Fatal(err)
	}
	if outcome, detail := outcomeOf(t, install.RemoveLauncher(context.Background(), h.codex, true)); outcome != install.LauncherRefused || !strings.Contains(detail, "symlink") {
		t.Fatalf("symlink: %s %s", outcome, detail)
	}
	if target, err := os.Readlink(h.launcher); err != nil || target != elsewhere {
		t.Fatalf("the link changed: %q %v", target, err)
	}
	if got, _ := os.ReadFile(elsewhere); string(got) != string(shipped) {
		t.Fatal("the link's target changed")
	}
	if err := os.Remove(h.launcher); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	if outcome, detail := outcomeOf(t, install.RemoveLauncher(context.Background(), h.codex, true)); outcome != install.LauncherRefused || !strings.Contains(detail, "directory") {
		t.Fatalf("directory: %s %s", outcome, detail)
	}
	if info, err := os.Lstat(h.launcher); err != nil || !info.IsDir() {
		t.Fatalf("the directory changed: %v", err)
	}
	h.settingsUntouched(t)
}

func TestRemoveLauncherProvesTheMarkerAgainUnderTheLock(t *testing.T) {
	h := newLauncherHome(t)
	// The file is replaced after the first reading and before the lock is taken: the window
	// the second proof exists for.
	defer install.ReplaceLauncherLock(func(ctx context.Context, target string, timeout time.Duration) (*record.Locked, error) {
		write(t, h.launcher, "replaced by someone else\n")
		return record.LockContext(ctx, target, timeout)
	})()
	if outcome, detail := outcomeOf(t, install.RemoveLauncher(context.Background(), h.codex, true)); outcome != install.LauncherRefused || !strings.Contains(detail, "replaced while this ran") {
		t.Fatalf("marker changed under the lock: %s %s", outcome, detail)
	}
	if got, _ := os.ReadFile(h.launcher); string(got) != "replaced by someone else\n" {
		t.Fatalf("the replacement was removed or changed: %q", got)
	}
	h.settingsUntouched(t)
}

func TestRemoveLauncherWaitsOnTheLockThePlacementTakes(t *testing.T) {
	// completion.place_launcher writes under hostrecord.Locked(<launcher>), the O_EXCL
	// <launcher>.crw-lock that record.Lock takes; a removal meeting it removes nothing.
	h := newLauncherHome(t)
	saved := record.LockTimeout
	record.LockTimeout = 200 * time.Millisecond
	defer func() { record.LockTimeout = saved }()
	held, err := record.Lock(h.launcher, 0)
	if err != nil {
		t.Fatal(err)
	}
	outcome, detail := outcomeOf(t, install.RemoveLauncher(context.Background(), h.codex, true))
	held.Release()
	if outcome != install.LauncherBusy || !strings.Contains(detail, h.launcher+record.LockSuffix) {
		t.Fatalf("under a held lock: %s %s", outcome, detail)
	}
	if _, err := os.Stat(h.launcher); err != nil {
		t.Fatalf("the launcher was removed under another run's lock: %v", err)
	}
	if outcome, _ := outcomeOf(t, install.RemoveLauncher(context.Background(), h.codex, true)); outcome != install.LauncherRemoved {
		t.Fatalf("after the lock was released: %s", outcome)
	}
	h.settingsUntouched(t)
}

// The lock wait ends with the caller's context, as every crw install path's does (todo 38): a
// removal interrupted while another run holds the launcher's lock removes nothing and leaves that
// run's lock alone, and one interrupted after the lock is taken does not unlink either.
func TestRemoveLauncherInterruptedRemovesNothing(t *testing.T) {
	h := newLauncherHome(t)
	held, err := record.Lock(h.launcher, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	outcome, detail := outcomeOf(t, install.RemoveLauncher(ctx, h.codex, true))
	if outcome != install.LauncherInterrupted || !strings.Contains(detail, "interrupted") || time.Since(started) > record.LockTimeout/2 {
		t.Fatalf("interrupted while waiting: %s %s after %s", outcome, detail, time.Since(started))
	}
	if _, err := os.Stat(h.launcher); err != nil {
		t.Fatalf("an interrupted removal removed the launcher: %v", err)
	}
	if _, err := os.Stat(held.Path); err != nil {
		t.Fatalf("an interrupted removal took another run's lock: %v", err)
	}
	held.Release()

	inside, stop := context.WithCancel(context.Background())
	defer install.ReplaceLauncherLock(func(ctx context.Context, target string, timeout time.Duration) (*record.Locked, error) {
		lock, err := record.LockContext(ctx, target, timeout)
		stop()
		return lock, err
	})()
	if outcome, detail := outcomeOf(t, install.RemoveLauncher(inside, h.codex, true)); outcome != install.LauncherInterrupted {
		t.Fatalf("interrupted under the lock: %s %s", outcome, detail)
	}
	if _, err := os.Stat(h.launcher); err != nil {
		t.Fatalf("a removal interrupted under the lock removed the launcher: %v", err)
	}
	h.settingsUntouched(t)
}
