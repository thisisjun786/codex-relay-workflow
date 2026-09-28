package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func Test28_AckValidationLivePython(t *testing.T) {
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	host := fakehost.Start(t)
	valid := "0123456789abcdef0123456789abcdef"
	type inputs struct{ name, event, turn string }
	cases := []inputs{}
	for i, event := range []string{valid, valid + "\n", valid + "\n\n", valid + "\r\n", valid + "\r", valid + "\u2028", valid[:31], valid + "0", strings.ToUpper(valid), "", "nope"} {
		cases = append(cases, inputs{fmt.Sprintf("event%d", i), event, "turn-9"})
	}
	for i, turn := range []string{"", " ", "\x1c", "\x1d", "\x1e", "\x1f", "\x1c\x1d\x1e\x1f", "\u0085", "\u00a0", "\u2028\u2029", "\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a", "\u1680\u202f\u205f\u3000", "\u200b", "\ufeff", "\x1cturn-9\x1f"} {
		cases = append(cases, inputs{fmt.Sprintf("turn%d", i), valid, turn})
	}
	code := func(err error) int {
		if err == nil {
			return 0
		}
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode()
		}
		t.Fatal(err)
		return -1
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, command := range []string{"ack", "ack-proof"} {
				args := []string{command, "--event", tc.event}
				if command == "ack" {
					args = append(args, "--ack-turn", tc.turn, "--ack-proof", delivery.AckProof(tc.event, tc.turn))
				} else {
					args = append(args, "--turn", tc.turn)
				}
				for _, program := range []string{suiteBinary, suiteAlias} {
					dir := filepath.Join(root, tc.name, command, filepath.Base(program))
					argv := append([]string{"--state", filepath.Join(dir, "go"), "--socket", host.SocketPath}, args...)
					goArgs := argv
					if program == suiteBinary {
						goArgs = append([]string{"relay"}, argv...)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					goOut, goErr := exec.CommandContext(ctx, program, goArgs...).CombinedOutput()
					pyArgv := append([]string{}, argv...)
					pyArgv[1] = filepath.Join(dir, "python")
					py := exec.CommandContext(ctx, "uv", append([]string{"run", "--no-sync", "python", "-m", "codex_session_relay.cli"}, pyArgv...)...)
					py.Dir = repo
					pyOut, pyErr := py.CombinedOutput()
					cancel()
					if code(goErr) != code(pyErr) || !bytes.Equal(goOut, pyOut) {
						t.Fatalf("%s %q %q: Go exit %d %s\nPython exit %d %s", command, tc.event, tc.turn, code(goErr), goOut, code(pyErr), pyOut)
					}
				}
			}
		})
	}
	if len(host.Requests()) != 0 {
		t.Fatalf("invalid or absent events must be resolved locally: %+v", host.Requests())
	}
}
