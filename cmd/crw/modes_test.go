package main

import (
	"bufio"
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The modes the usage line and the invalid-choice message advertise are the table's listed rows,
// and pabcd is dispatched without being advertised until the switch lists it.
func TestModeTableDrivesUsageAndDispatch(t *testing.T) {
	var out, errOut strings.Builder
	if code := run(context.Background(), "crw", []string{"nope"}, &out, &errOut); code != parserExit ||
		!strings.Contains(errOut.String(), "usage: crw [-h] [--version] {relay,bridge,hook,skill,doctor,install,review,manage,gui,config,help,version} ...") ||
		!strings.Contains(errOut.String(), "(choose from 'relay', 'bridge', 'hook', 'skill', 'doctor', 'install', 'review', 'manage', 'gui', 'config', 'help', 'version')") {
		t.Fatalf("unknown mode: %d %q", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), "crw", []string{"config"}, &out, &errOut); code != parserExit || !strings.Contains(errOut.String(), "usage: crw config") {
		t.Errorf("crw config: %d %q", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), "crw", []string{"review"}, &out, &errOut); code != parserExit || !strings.Contains(errOut.String(), "crw review: error: the following arguments are required") {
		t.Errorf("crw review: %d %q", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := run(context.Background(), "crw", []string{"pabcd", "--help"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "usage: crw pabcd") || errOut.Len() != 0 {
		t.Errorf("crw pabcd --help: %d %q %q", code, out.String(), errOut.String())
	}
	errOut.Reset()
	if code := run(context.Background(), "crw", []string{"pabcd"}, &out, &errOut); code != parserExit || !strings.Contains(errOut.String(), "crw pabcd: error: the following arguments are required: verb") {
		t.Errorf("crw pabcd: %d %q", code, errOut.String())
	}
}

// crw hook <event> --leg <leg> reaches the harness through the mode table, and every other argument
// list reaches the Stop adapter as before (its own tests run the binary and read its journal).
func TestHookRoutesALegToTheHarnessAndTheRestToTheStopAdapter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	over := filepath.Join(home, "oversized.json")
	if err := os.WriteFile(over, []byte(strings.Repeat("x", 4*1024*1024+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin := os.Stdin
	t.Cleanup(func() { os.Stdin = stdin })
	for _, c := range []struct {
		args []string
		out  string
	}{
		{[]string{"hook", "stop", "--leg", "stop-checking-pabcd-continuation"}, `{"decision":"block","reason":"[crw] hook input exceeded 4194304 bytes; refusing to bypass policy enforcement"}` + "\n"},
		{[]string{"hook", "stop", "--leg", "no-such-leg"}, ""},
		{[]string{"hook", "stop"}, ""},
		{[]string{"hook", "--plugin-launch"}, ""},
		{[]string{"hook"}, ""},
	} {
		f, err := os.Open(over)
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin = f
		var out, errOut strings.Builder
		code := run(context.Background(), "crw", c.args, &out, &errOut)
		f.Close()
		if code != 0 || out.String() != c.out || errOut.Len() != 0 {
			t.Errorf("%v: %d %q %q, want 0 %q", c.args, code, out.String(), errOut.String(), c.out)
		}
	}
}

// A leg whose input stays open ends on the first interrupt, which cancels the run's context.
func TestAnInterruptEndsAHookLegWaitingForItsInput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	in, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	defer in.Close()
	stdin := os.Stdin
	os.Stdin = in
	t.Cleanup(func() { os.Stdin = stdin })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	got := make(chan int, 1)
	var out, errOut strings.Builder
	go func() {
		got <- run(ctx, "crw", []string{"hook", "stop", "--leg", "stop-checking-pabcd-continuation"}, &out, &errOut)
	}()
	select {
	case code := <-got:
		if code != 130 || out.Len() != 0 || errOut.Len() != 0 {
			t.Errorf("interrupted hook: %d %q %q", code, out.String(), errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an interrupted hook is still waiting for its input")
	}
}

// The gui mode serves until a signal, then ends cleanly on SIGINT and SIGTERM alike: the
// process exits 0 and its listener is closed. The signal reaches the run through the mode
// row's cancelOn, which is what makes SIGTERM behave as SIGINT does.
func TestGuiEndsOnASignal(t *testing.T) {
	crw := testsupport.CRW(t)
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command(crw, "gui", "--port", "0")
			cmd.Env = []string{
				"HOME=" + home,
				"CODEX_HOME=" + filepath.Join(home, "codex"),
				"CRW_HOME=" + filepath.Join(home, "crw"),
				"PATH=" + os.Getenv("PATH"),
				testsupport.RefuseLiveStateEnv + "=1",
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			lines := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				if scanner.Scan() {
					lines <- scanner.Text()
				}
				close(lines)
			}()
			var line string
			select {
			case got, ok := <-lines:
				if !ok {
					t.Fatalf("the gui printed no line (stderr %q)", stderr.String())
				}
				line = got
			case <-time.After(30 * time.Second):
				t.Fatal("the gui printed no line")
			}
			if !strings.HasPrefix(line, "crw gui: serving http://127.0.0.1:") || !strings.Contains(line, "/#token=") {
				t.Fatalf("the line is %q", line)
			}
			address := strings.TrimPrefix(strings.SplitN(line, "/#token=", 2)[0], "crw gui: serving http://")
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Get("http://" + address + "/api/health")
			if err != nil {
				t.Fatalf("the printed port does not serve: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("health: %d", resp.StatusCode)
			}
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the gui did not end cleanly: %v (stderr %q)", err, stderr.String())
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("the gui did not end after %v", signal)
			}
			if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
				conn.Close()
				t.Fatalf("the listener is still open on %s", address)
			}
		})
	}
}
