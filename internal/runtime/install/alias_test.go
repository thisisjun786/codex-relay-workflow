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
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// A runtime is known by identity, not by spelling. The same directory is mounted twice (bwrap
// binds <scratch>/a onto <scratch>/b); the host installs and updates through /b, so the record,
// the pointer and every registration spell /b, and remove is asked through /a. Resolving
// spellings follows no bind mount, so every check compared /a against /b and passed, and the
// selected runtime was deleted. Now the selected runtime, one a registration names and one a
// process runs out of are each refused, and a runtime nothing uses is removed with its /b-spelled
// entries and outgoing dropped. The test reruns itself inside bwrap; it is skipped where bwrap
// cannot bind.
func TestRemoveKnowsARuntimeByIdentityNotSpelling(t *testing.T) {
	a, b := os.Getenv("CRW_ALIAS_A"), os.Getenv("CRW_ALIAS_B")
	if a == "" {
		bwrap, err := exec.LookPath("bwrap")
		if err != nil {
			t.Skip("no bwrap to mount one directory twice")
		}
		scratch := t.TempDir()
		a, b = filepath.Join(scratch, "a"), filepath.Join(scratch, "b")
		for _, dir := range []string{a, b} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := exec.Command(bwrap, "--dev-bind", "/", "/", "--bind", a, b, "true").CombinedOutput(); err != nil {
			t.Skipf("bwrap cannot bind here: %v %s", err, out)
		}
		cmd := exec.Command(bwrap, "--dev-bind", "/", "/", "--bind", a, b, os.Args[0], "-test.run=^TestRemoveKnowsARuntimeByIdentityNotSpelling$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "CRW_ALIAS_A="+a, "CRW_ALIAS_B="+b, "TMPDIR="+a, testsupport.CRWBinaryEnv+"="+testsupport.CRW(t))
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "the aliased host was read by identity") {
			t.Fatalf("inside bwrap: %v\n%s", err, out)
		}
		return
	}
	alias := func(path string) string { return strings.Replace(path, a, b, 1) }
	h := newHost(t)
	if !strings.HasPrefix(h.dest, a) {
		t.Fatalf("the host is not under %s: %s", a, h.dest)
	}
	throughB := h.options()
	throughB.Dest = alias(h.dest)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, updated := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	for _, step := range []struct {
		command string
		from    string
	}{{"install", first}, {"update", second}} {
		if result, code := install.Install(context.Background(), throughB, step.command, install.Source{From: step.from}); code != install.OK {
			t.Fatalf("%s through /b: exit %d\n%s", step.command, code, golden.Canon(result))
		}
	}
	if at(h.hostRecord(t), "selected", "codex-session-relay") != filepath.Join(alias(updated), "bin") {
		t.Fatalf("the record does not spell /b: %s", golden.Canon(at(h.hostRecord(t), "selected")))
	}

	refused, code := install.Remove(context.Background(), h.options(), updated)
	if code != install.Refused || !strings.Contains(text(at(refused, "refused")), "selects") {
		t.Fatalf("the selected runtime named through /a: exit %d\n%s", code, golden.Canon(refused))
	}
	config := filepath.Join(h.codex, "config.toml")
	write(t, config, "[mcp_servers.bridge]\ncommand = \""+filepath.Join(alias(old), "bin", "codex-thread-bridge")+"\"\n")
	if refused, code := install.Remove(context.Background(), h.options(), old); code != install.Refused || len(golden.List(at(refused, "registrations"))) == 0 {
		t.Fatalf("a runtime a /b registration names, asked through /a: exit %d\n%s", code, golden.Canon(refused))
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	source, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	sleeper := filepath.Join(old, "bin", "sleep")
	if err := os.WriteFile(sleeper, []byte(readFile(t, source)), 0o755); err != nil {
		t.Fatal(err)
	}
	process := exec.Command(alias(sleeper), "30")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	if refused, code := install.Remove(context.Background(), h.realProcesses(), old); code != install.Refused || len(golden.List(at(refused, "processes"))) != 1 {
		t.Fatalf("a runtime a process runs out of through /b, asked through /a: exit %d\n%s", code, golden.Canon(refused))
	}
	_ = process.Process.Kill()
	_ = process.Wait()

	removed, code := install.Remove(context.Background(), h.options(), old)
	if code != install.OK || !listed(golden.List(at(removed, "droppedInstallEntries")), alias(old)) || at(removed, "clearedOutgoing") != true {
		t.Fatalf("a runtime nothing uses: exit %d\n%s", code, golden.Canon(removed))
	}
	if listed(h.installsOf(t), alias(old)) {
		t.Fatal("its /b-spelled install entries are still listed")
	}
	t.Log("the aliased host was read by identity")
}
