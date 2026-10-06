package doctor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	hostenv "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// These are CRW-844's cases for the swap-then-verify publication: the lock, the pre-write
// verification, the conflict and late-save reports, and the sync warning. The interleavings that
// cannot be reached through the command line (a save between the last check and the exchange) are
// staged with the publication seam, whose field shape follows orchestrateCommitSeams; crwdir's own
// tests prove the real exchange keeps what it displaced.
//
// HOME, CODEX_HOME and CRW_HOME all point into the temporary root this fixture builds, and
// TestHookTrustRetrust_real_home_is_untouched (hooktrust_retrust_test.go) compares the real ~/.codex
// and ~/.crw listings before and after the package's tests run.

type casFixture struct {
	t       *testing.T
	root    string
	home    string
	plugin  string
	key     string
	entries []HookTrustEntry
}

const casKey = "crw@local"

func newCASFixture(t *testing.T, config string) *casFixture {
	t.Helper()
	root := t.TempDir()
	f := &casFixture{t: t, root: root, home: filepath.Join(root, "codex"), plugin: filepath.Join(root, "plugin"), key: casKey}
	for _, dir := range []string{f.home, filepath.Join(f.plugin, ".codex-plugin"), filepath.Join(f.plugin, "hooks")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.write(filepath.Join(f.plugin, ".codex-plugin", "plugin.json"), "{\"name\":\"crw\",\"hooks\":[\"./hooks/one.json\",\"./hooks/two.json\"]}"+"\n")
	f.write(filepath.Join(f.plugin, "hooks", "one.json"), "{\"hooks\":{\"Stop\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"echo one\"}]}]}}"+"\n")
	f.write(filepath.Join(f.plugin, "hooks", "two.json"), "{\"hooks\":{\"Stop\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"echo two\"}]}]}}"+"\n")
	if config != "" {
		f.write(f.config(), config)
	}
	entries, err := ListHookTrustEntries(f.plugin, f.key)
	if err != nil {
		t.Fatal(err)
	}
	f.entries = entries
	return f
}

func (f *casFixture) write(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *casFixture) read(path string) string {
	f.t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(raw)
}

func (f *casFixture) config() string { return filepath.Join(f.home, "config.toml") }

// installed is a config that enables the plugin and already trusts one hook, so the safety pin
// passes and the other hook is the item the plan appends.
func (f *casFixture) installed() string {
	return "model = \"gpt-5.5\"\n\n[plugins.\"crw@local\"]\nenabled = true\n\n[hooks.state.\"" +
		f.entries[0].Key + "\"]\ntrusted_hash = \"" + f.entries[0].Hash + "\"\n"
}

func (f *casFixture) backupName() string { return f.config() + ".bak-2026-01-01T00-00-00.000Z" }

func (f *casFixture) env() hostenv.LookupEnv {
	vars := map[string]string{"HOME": f.root, "CODEX_HOME": f.home, "CRW_HOME": filepath.Join(f.root, "crw")}
	return func(key string) (string, bool) { value, ok := vars[key]; return value, ok }
}

func (f *casFixture) now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// okRunner is the verification probe that succeeds.
func okRunner(string, []string, []string) HookTrustRetrustRun {
	zero := 0
	return HookTrustRetrustRun{Status: &zero}
}

// failingRunner is the probe that fails, the fake codex the issue names (exit 1).
func failingRunner(string, []string, []string) HookTrustRetrustRun {
	one := 1
	return HookTrustRetrustRun{Status: &one}
}

func (f *casFixture) run(runner HookTrustRetrustRunner, args ...string) (string, string, int) {
	f.t.Helper()
	var stdout, stderr bytes.Buffer
	code := HookTrustRetrustCLI(args, &stdout, &stderr, f.env(), runner, f.plugin, f.now())
	return stdout.String(), stderr.String(), code
}

// A verification failure must leave config.toml untouched and still print the planned items: the
// dev build published, rolled back and printed nothing.
func TestHookTrustRetrustCASVerificationFailureLeavesTheConfigAndReportsThePlan(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	original := f.read(f.config())

	stdout, stderr, code := f.run(failingRunner)
	if code != 1 {
		t.Fatalf("verification failure: code=%d stderr=%q", code, stderr)
	}
	if got := f.read(f.config()); got != original {
		t.Fatalf("the refused run changed config.toml:\n%s", got)
	}
	if _, err := os.Stat(f.backupName()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused run left a backup: %v", err)
	}
	for _, want := range []string{
		"updated keys: " + f.entries[0].Key,
		"appended keys: " + f.entries[1].Key,
		"config.toml unchanged",
		"pre-write verification failed",
	} {
		if !strings.Contains(stdout+stderr, want) {
			t.Fatalf("the report does not carry %q:\nstdout:\n%s\nstderr:\n%s", want, stdout, stderr)
		}
	}
}

// A CRW writer that holds the sidecar lock makes retrust refuse with the busy message, and nothing
// is written.
func TestHookTrustRetrustCASRefusesWhileAnotherWriterHoldsTheLock(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	original := f.read(f.config())

	held, err := crwdir.LockConfig(f.config(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	_, stderr, code := f.run(okRunner)
	if code != 1 || !strings.Contains(stderr, crwdir.ConfigLockBusy) {
		t.Fatalf("a held lock did not refuse: code=%d stderr=%q", code, stderr)
	}
	if got := f.read(f.config()); got != original {
		t.Fatalf("the refused run changed config.toml: %q", got)
	}
}

// A save between the last check and the exchange is the P0: the saved content stays in the backup,
// config.toml holds retrust's content, nothing is exchanged back, and both paths are reported.
func TestHookTrustRetrustCASReportsAConflictAndKeepsBothPaths(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	raced := "model = \"saved-by-another-process\"\n"
	result, _, err := hookTrustRetrustWith(f.home, f.plugin, f.key, true, okRunner, f.env(), f.now(), &hookTrustRetrustSeams{
		publish: func(target string, expected, next []byte, backupPath string) ([]byte, error) {
			// The exchange ran; what it displaced is not what retrust read.
			if werr := os.WriteFile(target, next, 0o644); werr != nil {
				t.Fatal(werr)
			}
			if werr := os.WriteFile(backupPath, []byte(raced), 0o644); werr != nil {
				t.Fatal(werr)
			}
			return []byte(raced), nil
		},
	})
	if err == nil || !result.Conflict {
		t.Fatalf("the conflict was not reported: result=%+v err=%v", result, err)
	}
	if !strings.Contains(err.Error(), f.backupName()) || !strings.Contains(err.Error(), f.config()) {
		t.Fatalf("the conflict does not name both paths: %v", err)
	}
	if got := f.read(f.backupName()); got != raced {
		t.Fatalf("the raced save is not in the backup: %q", got)
	}
	if got := f.read(f.config()); !strings.Contains(got, f.entries[1].Hash) {
		t.Fatalf("config.toml does not hold retrust's content: %q", got)
	}
}

// A save after the publication is reported and left in place: retrust does not overwrite it again.
func TestHookTrustRetrustCASReportsASaveAfterThePublication(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	original := f.read(f.config())
	later := "model = \"saved-after-publication\"\n"
	result, _, err := hookTrustRetrustWith(f.home, f.plugin, f.key, true, okRunner, f.env(), f.now(), &hookTrustRetrustSeams{
		publish: func(target string, expected, next []byte, backupPath string) ([]byte, error) {
			if werr := os.WriteFile(target, []byte(later), 0o644); werr != nil {
				t.Fatal(werr)
			}
			if werr := os.WriteFile(backupPath, expected, 0o644); werr != nil {
				t.Fatal(werr)
			}
			return expected, nil
		},
	})
	if err == nil || !result.LateWrite || !result.Published {
		t.Fatalf("the late save was not reported: result=%+v err=%v", result, err)
	}
	if got := f.read(f.config()); got != later {
		t.Fatalf("the late save was overwritten: %q", got)
	}
	if got := f.read(f.backupName()); got != original {
		t.Fatalf("the backup does not hold the displaced content: %q", got)
	}
}

// Only the directory sync failed, so the publication counts as done and the failure is a warning.
func TestHookTrustRetrustCASCountsASyncFailureAsWritten(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	original := f.read(f.config())
	injected := errors.New("injected directory sync failure")
	result, _, err := hookTrustRetrustWith(f.home, f.plugin, f.key, true, okRunner, f.env(), f.now(), &hookTrustRetrustSeams{
		publish: func(target string, expected, next []byte, backupPath string) ([]byte, error) {
			if werr := os.WriteFile(target, next, 0o644); werr != nil {
				t.Fatal(werr)
			}
			if werr := os.WriteFile(backupPath, expected, 0o644); werr != nil {
				t.Fatal(werr)
			}
			return expected, &crwdir.PublishedError{Err: injected}
		},
	})
	if err != nil {
		t.Fatalf("a sync-only failure was reported as a refusal: %v", err)
	}
	if !result.Published || result.Warning == "" {
		t.Fatalf("the sync failure was not carried as a warning: %+v", result)
	}
	if got := f.read(f.config()); !strings.Contains(got, f.entries[1].Hash) {
		t.Fatalf("the published content is not in place: %q", got)
	}
	if got := f.read(f.backupName()); got != original {
		t.Fatalf("the backup does not hold the displaced content: %q", got)
	}
}

// The happy path still reports the oracle's own lines and adds the plan and the file roles.
func TestHookTrustRetrustCASReportsThePlanOnSuccess(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())

	stdout, stderr, code := f.run(okRunner)
	if code != 0 || stderr != "" {
		t.Fatalf("success: code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"updated=1 appended=1\n",
		"backup: " + f.backupName() + "\n",
		"updated keys: " + f.entries[0].Key + "\n",
		"appended keys: " + f.entries[1].Key + "\n",
		f.config() + " holds the rewritten config; " + f.backupName() + " holds the content it displaced\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the success report does not carry %q:\n%s", want, stdout)
		}
	}
}

// A failure after the exchange that stopped the backup being filled must be reported as published
// with the file that really holds the displaced content, not as an unchanged config.
func TestHookTrustRetrustCASReportsAPostExchangeFailureAsPublished(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	original := f.read(f.config())
	kept := filepath.Join(f.root, "displaced.toml")
	result, _, err := hookTrustRetrustWith(f.home, f.plugin, f.key, true, okRunner, f.env(), f.now(), &hookTrustRetrustSeams{
		publish: func(target string, expected, next []byte, backupPath string) ([]byte, error) {
			if werr := os.WriteFile(target, next, 0o644); werr != nil {
				t.Fatal(werr)
			}
			if werr := os.WriteFile(kept, expected, 0o644); werr != nil {
				t.Fatal(werr)
			}
			return nil, &crwdir.PublishedError{Err: errors.New("injected move failure"), DisplacedAt: kept}
		},
	})
	if err != nil {
		t.Fatalf("a post-exchange failure was reported as a refusal: %v", err)
	}
	if !result.Published || result.Warning == "" || result.DisplacedAt != kept {
		t.Fatalf("the publication state is wrong: %+v", result)
	}
	if result.Conflict {
		t.Fatalf("a cooperative displaced file was reported as a conflict: %+v", result)
	}
	if got := f.read(f.config()); !strings.Contains(got, f.entries[1].Hash) {
		t.Fatalf("the published content is not in place: %q", got)
	}
	if got := f.read(kept); got != original {
		t.Fatalf("the displaced content is not where the report says: %q", got)
	}
}

// A hook that drifts between the pre-write verification and the final diagnosis must not be reported
// as a success: the published config is no longer trusted.
func TestHookTrustRetrustCASFailsWhenTheFinalDiagnosisDrifts(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	// The plan is verified good, the publication succeeds, and only then does the hook file change,
	// so the final diagnosis reads a hash the published config does not carry.
	result, verification, err := hookTrustRetrustWith(f.home, f.plugin, f.key, true, okRunner, f.env(), f.now(), &hookTrustRetrustSeams{
		publish: func(target string, expected, next []byte, backupPath string) ([]byte, error) {
			if werr := os.WriteFile(filepath.Join(f.plugin, "hooks", "two.json"), []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo changed"}]}]}}`+"\n"), 0o644); werr != nil {
				t.Fatal(werr)
			}
			return crwdir.PublishSwap(target, expected, next, backupPath)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "post-publication verification failed") {
		t.Fatalf("a drifted final diagnosis was reported as success: result=%+v verification=%+v err=%v", result, verification, err)
	}
	if !result.Published {
		t.Fatalf("the publication did happen and must be recorded: %+v", result)
	}
}

// A conflict that follows a post-exchange failure must name the file that really holds the displaced
// content (in the stderr error and in the stdout report alike), and must still carry the failure
// detail: an operator following the message must be sent to the right bytes.
func TestHookTrustRetrustCASConflictNamesTheDisplacedPathAndKeepsTheWarning(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	raced := "model = \"saved-by-another-process\"\n"
	kept := filepath.Join(f.root, "displaced.toml")
	result, _, err := hookTrustRetrustWith(f.home, f.plugin, f.key, true, okRunner, f.env(), f.now(), &hookTrustRetrustSeams{
		publish: func(target string, expected, next []byte, backupPath string) ([]byte, error) {
			// The exchange ran, the move failed, and what the exchange displaced is not what retrust
			// read: a conflict whose displaced content is at the reported path, not the backup path.
			if werr := os.WriteFile(target, next, 0o644); werr != nil {
				t.Fatal(werr)
			}
			if werr := os.WriteFile(kept, []byte(raced), 0o644); werr != nil {
				t.Fatal(werr)
			}
			return []byte(raced), &crwdir.PublishedError{Err: errors.New("injected move failure"), DisplacedAt: kept}
		},
	})
	if err == nil || !result.Conflict || result.DisplacedAt != kept {
		t.Fatalf("the conflict is wrong: result=%+v err=%v", result, err)
	}
	if !strings.Contains(err.Error(), kept) || strings.Contains(err.Error(), f.backupName()) {
		t.Fatalf("the conflict error does not name the displaced path: %v", err)
	}

	// The same through the command line: stdout names the displaced path and stderr carries both the
	// conflict and the warning detail.
	var stdout, stderr bytes.Buffer
	hookTrustRetrustReport(&stdout, result)
	hookTrustRetrustWarn(&stderr, result)
	if !strings.Contains(stdout.String(), kept) || strings.Contains(stdout.String(), f.backupName()) {
		t.Fatalf("the report does not name the displaced path: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "injected move failure") {
		t.Fatalf("the warning detail was dropped: %q", stderr.String())
	}
}
