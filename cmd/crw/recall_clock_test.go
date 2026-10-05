package main

import (
	"context"
	"database/sql/driver"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The recall mode reads the wall clock unless a build links an instant into recallTestClock
// (cmd/crw/recall_clock.go): the cxc domain of internal/contracttest links the recorder's clock so
// the nine clock-dependent corpus fixtures replay (contract/notes/cxc/CRW-567.json).

func TestRecallNowReadsTheWallClockWithoutASeam(t *testing.T) {
	if recallTestClock != "" {
		t.Fatalf("recallTestClock = %q, want empty: only a test build links it", recallTestClock)
	}
	before := time.Now()
	got := recallNow()
	if delta := got.Sub(before); delta < -time.Second || delta > time.Second {
		t.Fatalf("recallNow() = %s, want within one second of %s", got.UTC().Format(time.RFC3339Nano), before.UTC().Format(time.RFC3339Nano))
	}
}

func TestRecallNowUsesTheLinkedClock(t *testing.T) {
	previous := recallTestClock
	recallTestClock = "1767225600000"
	t.Cleanup(func() { recallTestClock = previous })
	want := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	got := recallNow()
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("recallNow() = %s (%s), want %s (UTC)", got, got.Location(), want)
	}
}

func TestRecallNowPanicsOnAMalformedSeam(t *testing.T) {
	previous := recallTestClock
	recallTestClock = "not milliseconds"
	t.Cleanup(func() { recallTestClock = previous })
	defer func() {
		reason := recover()
		if reason == nil {
			t.Fatal("recallNow() did not panic on a malformed recallTestClock")
		}
		if !strings.Contains(fmt.Sprint(reason), "not milliseconds") {
			t.Fatalf("panic %v does not name the value", reason)
		}
	}()
	_ = recallNow()
}

// TestRecallMemoryStatusUsesTheWallClockInAReleaseBuild runs the release-shaped binary (no linked
// seam): a job that finished 9000 seconds before the run reads "2h ago". A build that froze the
// clock at the recorder's 2026-01-01 would treat the finished_at as being in the future and clamp
// the label to "0m ago" (internal/recall/memorystatus.go memoryAgeLabel).
func TestRecallMemoryStatusUsesTheWallClockInAReleaseBuild(t *testing.T) {
	crw := testsupport.CRW(t)
	home := t.TempDir()
	for _, key := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(key, home)
	}
	db, err := (&sqlite.Driver{}).Open(filepath.Join(home, "memories_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.(driver.ExecerContext).ExecContext(context.Background(), fmt.Sprintf(
		"CREATE TABLE jobs(kind,status,retry_remaining,last_error,finished_at); INSERT INTO jobs VALUES ('stage1','done',3,NULL,%d)",
		time.Now().Unix()-9000), nil)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("seed memories_1.sqlite: %v %v", err, closeErr)
	}
	output, err := exec.Command(crw, "recall", "memory", "status").CombinedOutput()
	if err != nil {
		t.Fatalf("crw recall memory status: %v\n%s", err, output)
	}
	if want := "  last success: 2h ago\n"; !strings.Contains(string(output), want) {
		t.Fatalf("output does not contain %q:\n%s", want, output)
	}
}
