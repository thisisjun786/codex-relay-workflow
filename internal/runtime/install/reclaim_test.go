package install_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// unsettle leaves dir's claim as an install that exits 3 leaves it: STAGING, nobody holding it.
func unsettle(t *testing.T, dir string) {
	t.Helper()
	write(t, staging.ClaimPath(dir), string(record.Encode(staging.NewPayload(staging.Staging, "CRW-158", "1"))))
}

// The reproduced sequence: an install that exits 3 (promoted and in service, its claim never
// settled), an update that replaces it, and a reinstall of the first archive. The update settles
// the claim of the in-service runtime it replaces, as a rollback settles the one it leaves, so
// the reinstall reads a finished runtime and keeps it, and a rollback returns to it.
func TestAReinstallNeverReclaimsARuntimeThatWasInService(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	unsettle(t, old)
	updated := h.mustInstall(t, "update", second)
	if claimState(t, old) != staging.Complete || at(updated, "leftClaim", "settled") != true {
		t.Fatalf("the update left the replaced runtime's claim %v\n%s", claimState(t, old), golden.Canon(at(updated, "leftClaim")))
	}
	reinstall, code := install.Install(context.Background(), h.options(), "install", install.Source{From: first})
	if code != install.Refused || at(reinstall, "stagingDecision") != staging.Keep {
		t.Fatalf("the reinstall: exit %d\n%s", code, golden.Canon(reinstall))
	}
	if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil {
		t.Fatalf("the reinstall removed a runtime that was in service: %v", err)
	}
	if back, code := install.Rollback(context.Background(), h.options(), ""); code != install.OK || h.pointerTarget(t) != old {
		t.Fatalf("rollback to it: exit %d\n%s", code, golden.Canon(back))
	}
}

// Reclaim removes a staging its run abandoned only when nothing may still be using it, judged as
// remove judges it. A STAGING runtime the record's outgoing names was put in service by a
// promotion: its claim is settled COMPLETE and it is kept. One a live process runs out of, or a
// registration names or cannot be read, is kept as it is. Each is a refusal with nothing removed
// or built; once nothing uses it, the same reinstall reclaims and rebuilds it.
func TestReclaimKeepsAStagingThatMayStillBeInUse(t *testing.T) {
	h := newHost(t)
	first, second, third := archive(t, "0.9.0", ""), archive(t, "0.9.1", ""), archive(t, "0.9.2", "")
	old := runtimeDir(h, "0.9.0", first, t)
	reinstall := func() (record.Object, int) {
		return install.Install(context.Background(), h.options(), "update", install.Source{From: first})
	}
	kept := func(label string, result record.Object, code int, claim string) {
		t.Helper()
		if code != install.Refused || at(result, "stagingDecision") != staging.Reclaim || at(result, "applied") != false {
			t.Fatalf("%s: exit %d\n%s", label, code, golden.Canon(result))
		}
		if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil {
			t.Fatalf("%s: the staging was reclaimed: %v", label, err)
		}
		if claimState(t, old) != claim {
			t.Fatalf("%s: the claim says %v", label, claimState(t, old))
		}
	}
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)

	unsettle(t, old)
	result, code := reinstall()
	kept("the record's outgoing names it", result, code, staging.Complete)
	if !strings.Contains(text(at(result, "refused")), "outgoing") || at(result, "claim", "settled") != true {
		t.Fatalf("outgoing: %s", golden.Canon(result))
	}

	h.mustInstall(t, "update", third) // outgoing moves on to the second runtime
	unsettle(t, old)
	source, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	sleeper := filepath.Join(old, "bin", "sleep")
	if err := os.WriteFile(sleeper, []byte(readFile(t, source)), 0o755); err != nil {
		t.Fatal(err)
	}
	process := exec.Command(sleeper, "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	result, code = reinstall()
	kept("a live process", result, code, staging.Staging)
	if len(golden.List(at(result, "processes"))) != 1 {
		t.Fatalf("processes: %s", golden.Canon(result))
	}
	_ = process.Process.Kill()
	_ = process.Wait()

	config := filepath.Join(h.codex, "config.toml")
	write(t, config, "[mcp_servers.bridge]\ncommand = \""+filepath.Join(old, "bin", "codex-thread-bridge")+"\"\n")
	result, code = reinstall()
	kept("a registration", result, code, staging.Staging)
	if len(golden.List(at(result, "registrations"))) == 0 {
		t.Fatalf("registrations: %s", golden.Canon(result))
	}
	write(t, config, "[mcp_servers.bridge\n")
	result, code = reinstall()
	kept("an unreadable registration", result, code, staging.Staging)
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}

	h.mustInstall(t, "update", first)
	if _, err := os.Stat(sleeper); !os.IsNotExist(err) || h.pointerTarget(t) != old {
		t.Fatal("once nothing uses it the abandoned staging is reclaimed and rebuilt")
	}
}
