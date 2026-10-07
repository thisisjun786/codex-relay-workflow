package doctor

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The retrust subcommand reads the wall clock unless a build links an instant into retrustTestClock
// (cli.go): the cxc domain of internal/contracttest links the recorder's epoch so a fixture that
// records a clock-minted backup name can replay (contract/notes/cxc/CRW-936.json). These three cases
// are the recall seam's (cmd/crw/recall_clock_test.go) for this one.
func TestRetrustNowReadsTheWallClockWithoutASeam(t *testing.T) {
	if retrustTestClock != "" {
		t.Fatalf("retrustTestClock = %q, want empty: only a test build links it", retrustTestClock)
	}
	before := time.Now()
	got := retrustNow()
	if delta := got.Sub(before); delta < -time.Second || delta > time.Second {
		t.Fatalf("retrustNow() = %s, want within one second of %s", got.UTC().Format(time.RFC3339Nano), before.UTC().Format(time.RFC3339Nano))
	}
}

func TestRetrustNowUsesTheLinkedClock(t *testing.T) {
	previous := retrustTestClock
	retrustTestClock = "1767225600000"
	t.Cleanup(func() { retrustTestClock = previous })
	want := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	got := retrustNow()
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("retrustNow() = %s (%s), want %s (UTC)", got, got.Location(), want)
	}
}

func TestRetrustNowPanicsOnAMalformedSeam(t *testing.T) {
	previous := retrustTestClock
	retrustTestClock = "not milliseconds"
	t.Cleanup(func() { retrustTestClock = previous })
	defer func() {
		if recover() == nil {
			t.Fatal("retrustNow() did not panic on a malformed retrustTestClock")
		}
	}()
	_ = retrustNow()
}

// CRW-936's cases for the two report gaps the CRW-844 evaluation found. On dev a save that lands
// just after the publication is reported as retrust's own content (the displaced-differs branch
// returns before reading the target again), a refusal before the plan prints nothing at all, and a
// refusal with a plan never names the backup it did not create.
//
// HOME, CODEX_HOME and CRW_HOME all point into the temporary root the casFixture builds
// (hooktrust_retrust_cas_test.go), and TestHookTrustRetrust_real_home_is_untouched compares the real
// ~/.codex and ~/.crw listings before and after the package's tests run.

// A save C that lands between the last check and the exchange, and a save D that lands right after
// the publication, are two different writes. The report must name D as what config.toml holds and C
// as what the displaced path holds, and must never tell the operator config.toml holds retrust's
// content: on dev it does exactly that (the displaced-differs branch returns before the re-read).
func TestHookTrustRetrustReportsAConflictWithALaterSave(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	beforeTheExchange := "model = \"saved-before-the-exchange\"\n"
	afterThePublication := "model = \"saved-after-the-publication\"\n"

	result, _, err := hookTrustRetrustWith(f.home, f.plugin, f.key, true, okRunner, f.env(), f.now(), &hookTrustRetrustSeams{
		publish: func(target string, expected, next []byte, backupPath string) ([]byte, error) {
			// The exchange ran: C was displaced into the backup, and D landed at config.toml right
			// after the publication.
			if werr := os.WriteFile(backupPath, []byte(beforeTheExchange), 0o644); werr != nil {
				t.Fatal(werr)
			}
			if werr := os.WriteFile(target, []byte(afterThePublication), 0o644); werr != nil {
				t.Fatal(werr)
			}
			return []byte(beforeTheExchange), nil
		},
	})
	if err == nil || !result.Conflict || !result.LateWrite {
		t.Fatalf("the overlap was not reported as a conflict with a later save: result=%+v err=%v", result, err)
	}
	if got := f.read(f.config()); got != afterThePublication {
		t.Fatalf("config.toml holds %q, want the save that landed after the publication", got)
	}
	if got := f.read(f.backupName()); got != beforeTheExchange {
		t.Fatalf("the displaced path holds %q, want the save made in between", got)
	}

	var stdout bytes.Buffer
	hookTrustRetrustReport(&stdout, result)
	report := stdout.String()
	if strings.Contains(report, "holds retrust's config") {
		t.Fatalf("the report claims config.toml holds retrust's content:\n%s", report)
	}
	for _, want := range []string{f.config(), f.backupName(), "not retrust's config", "saved in between"} {
		if !strings.Contains(report, want) {
			t.Fatalf("the report does not carry %q:\n%s", want, report)
		}
	}
}

// A refusal with a plan (the verification probe fails) must say which backup it did not create:
// on dev the report stops at "unchanged" and never names the planned backup path.
func TestHookTrustRetrustCLIRefusalWithAPlanNamesTheBackupItDidNotCreate(t *testing.T) {
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
	if _, err := os.Stat(f.backupName()); !os.IsNotExist(err) {
		t.Fatalf("the refused run left a backup: %v", err)
	}
	for _, want := range []string{
		"updated keys: " + f.entries[0].Key,
		"appended keys: " + f.entries[1].Key,
		"config.toml unchanged",
		"backup: " + f.backupName() + " (not created)",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the refusal report does not carry %q:\nstdout:\n%s", want, stdout)
		}
	}
}

// A refusal before the plan exists (another writer holds the lock) must print a report too: on dev
// the result is the zero value, the report returns at once, and stdout is empty.
func TestHookTrustRetrustCLIRefusalBeforeThePlanPrintsTheConfigAndNoPlan(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	original := f.read(f.config())

	held, err := crwdir.LockConfig(f.config(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	stdout, stderr, code := f.run(okRunner)
	if code != 1 || !strings.Contains(stderr, crwdir.ConfigLockBusy) {
		t.Fatalf("a held lock did not refuse: code=%d stderr=%q", code, stderr)
	}
	if got := f.read(f.config()); got != original {
		t.Fatalf("the refused run changed config.toml: %q", got)
	}
	for _, want := range []string{
		f.config(),
		"no plan: " + crwdir.ConfigLockBusy,
		"config.toml unchanged",
		"no backup",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("the pre-plan refusal report does not carry %q:\nstdout:\n%s", want, stdout)
		}
	}
}

// When the target cannot be read again after the exchange displaced something else, there is no
// evidence of what config.toml holds, so the report must say the re-read failed and assert nothing
// about the target. On dev the branch never reads the target at all and claims retrust's content.
func TestHookTrustRetrustReportsAFailedRecheckWithoutClaimingTheTarget(t *testing.T) {
	f := newCASFixture(t, "")
	f.write(f.config(), f.installed())
	beforeTheExchange := "model = \"saved-before-the-exchange\"\n"

	result, _, err := hookTrustRetrustWith(f.home, f.plugin, f.key, true, okRunner, f.env(), f.now(), &hookTrustRetrustSeams{
		publish: func(target string, expected, next []byte, backupPath string) ([]byte, error) {
			if werr := os.WriteFile(backupPath, []byte(beforeTheExchange), 0o644); werr != nil {
				t.Fatal(werr)
			}
			// The target cannot be read back: the re-read the report depends on fails.
			if werr := os.Remove(target); werr != nil {
				t.Fatal(werr)
			}
			if werr := os.Mkdir(target, 0o755); werr != nil {
				t.Fatal(werr)
			}
			return []byte(beforeTheExchange), nil
		},
	})
	if err == nil || !result.Conflict || !result.RecheckFailed {
		t.Fatalf("the failed re-read was not recorded: result=%+v err=%v", result, err)
	}
	if result.LateWrite {
		t.Fatalf("a failed re-read must not assert a later save: %+v", result)
	}
	if got := f.read(f.backupName()); got != beforeTheExchange {
		t.Fatalf("the displaced path holds %q, want the save made in between", got)
	}

	var stdout bytes.Buffer
	hookTrustRetrustReport(&stdout, result)
	report := stdout.String()
	if strings.Contains(report, "holds retrust's config") {
		t.Fatalf("the report claims config.toml holds retrust's content after a failed re-read:\n%s", report)
	}
	for _, want := range []string{f.config(), f.backupName(), "could not be read again", "saved in between"} {
		if !strings.Contains(report, want) {
			t.Fatalf("the report does not carry %q:\n%s", want, report)
		}
	}
}
