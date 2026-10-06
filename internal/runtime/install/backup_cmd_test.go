package install_test

// backup_cmd_test.go is the operator command of CRW-862: `crw install backup-state --to <dir>` takes the same
// stopped byte copy outside an install, refuses while the relay service runs, and holds the store's write gate
// exclusively while it copies. The service reading is faked, as the packet requires (the "service running" case is a
// faked reading): no daemon is started and no live relay state is touched.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// backupStateHost is a host with a relay state directory holding a whole store (a database, its write gate and its
// ownership mirror), as a stopped relay leaves one.
func backupStateHost(t *testing.T) *host {
	t.Helper()
	h := newHost(t)
	testsupport.Create(t, filepath.Join(h.relayState, "relay.sqlite3"), "", "go")
	write(t, filepath.Join(h.relayState, "ledger.log"), "line one\n")
	return h
}

// The command takes the same artifact outside an install: the copy, its manifest and the integrity gate, with the
// service stopped and the write gate held exclusively.
//
// sequential: replaces the service-reading seam.
func TestTheOperatorCommandTakesTheArtifactOutsideAnInstall(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	defer restore()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.OK || at(result, "applied") != true || at(result, "stateBackup", "made") != true || at(result, "stateBackup", "restoreCandidate") != true {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if at(result, "stateBackup", "integrityCheck") != "ok" {
		t.Fatalf("the artifact records its integrity: %s", golden.Canon(at(result, "stateBackup")))
	}
	// the copy and its manifest are there, and the manifest records the gate
	var manifest struct {
		IntegrityCheck   string `json:"integrityCheck"`
		RestoreCandidate bool   `json:"restoreCandidate"`
	}
	if err := json.Unmarshal(mustRead(t, dest+install.ManifestSuffix), &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.RestoreCandidate || manifest.IntegrityCheck != "ok" {
		t.Fatalf("manifest: %+v", manifest)
	}
}

// The command refuses while the relay service reads as running: it copies nothing and writes nothing.
//
// sequential: replaces the service-reading seam.
func TestTheOperatorCommandRefusesWhileTheServiceRuns(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Running, true, "the service answered and reports itself running")
	})
	defer restore()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.Refused || at(result, "applied") != false || !strings.Contains(text(at(result, "refused")), "running") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	nothingAt(t, dest)
	nothingAt(t, dest+install.ManifestSuffix)
}

// A service reading that could not be taken refuses rather than assuming the service is stopped.
//
// sequential: replaces the service-reading seam.
func TestTheOperatorCommandRefusesAnUnreadableServiceReading(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell("UNREADABLE", false, "the service could not be asked")
	})
	defer restore()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.Refused || at(result, "applied") != false || !strings.Contains(text(at(result, "refused")), "could not be established") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	nothingAt(t, dest)
}

// The command holds the store's write gate exclusively: while a relay writer holds it, the command refuses instead of
// copying a store a writer may be reaching.
//
// sequential: takes the real write gate.
func TestTheOperatorCommandRefusesWhileTheWriteGateIsHeld(t *testing.T) {
	h := backupStateHost(t)
	restore := install.ReplaceServiceReading(func(context.Context, install.Options) install.Object {
		return install.ServiceCell(scope.Stopped, true, "the service answered and reports itself not running")
	})
	defer restore()
	held, err := ownership.Lock(filepath.Join(h.relayState, "write-gate.lock"), true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	dest := filepath.Join(h.home, "operator-backup")
	result, code := install.BackupState(context.Background(), h.options(), dest)
	if code != install.Refused || at(result, "applied") != false || !strings.Contains(text(at(result, "refused")), "write gate") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	nothingAt(t, dest)
}

// The command's own flag and its own help surface, and the frozen install usage line does not name it.
//
// sequential: t.Setenv("PATH") is process-wide.
func TestTheOperatorCommandSurface(t *testing.T) {
	fakeBin := t.TempDir()
	writeExecutable(t, filepath.Join(fakeBin, "codex"), []byte("#!/bin/sh\necho codex-cli 0.154.0\n"), 0o755)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := backupStateHost(t)
	common := []string{"--codex-home", h.codex, "--record", h.record, "--socket", h.fake.SocketPath, "--state", h.relayState}
	run := func(args ...string) (string, string, int) {
		var stdout, stderr strings.Builder
		code := install.Main(context.Background(), append(args, common...), h.env, &stdout, &stderr)
		return stdout.String(), stderr.String(), code
	}
	// help
	if stdout, _, code := run("backup-state", "--help"); code != install.OK || !strings.Contains(stdout, "crw install backup-state --to <dir>") {
		t.Fatalf("help: exit %d\n%s", code, stdout)
	}
	// --to is required
	if _, stderr, code := run("backup-state"); code != install.Usage || !strings.Contains(stderr, "--to") {
		t.Fatalf("missing --to: exit %d %s", code, stderr)
	}
	// the frozen usage line still names only the commands it named before this one arrived
	var stdout, stderr strings.Builder
	if code := install.Main(context.Background(), []string{"help"}, nil, &stdout, &stderr); code != install.OK {
		t.Fatalf("install help: exit %d", code)
	}
	want := "usage: crw install {install,update,rollback,remove,status,register-mcp,hook,register-service} ...\n"
	if stdout.String() != want {
		t.Fatalf("install usage = %q, want %q", stdout.String(), want)
	}
	// the invalid-choice list names it
	if _, stderr, code := run("unpack"); code != install.Usage || !strings.Contains(stderr, "backup-state") {
		t.Fatalf("invalid choice: exit %d %s", code, stderr)
	}
}
