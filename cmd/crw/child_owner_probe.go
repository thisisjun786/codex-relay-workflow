//go:build childownerprobe

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/childcleanup"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// A crw built with -tags childownerprobe also answers `crw child-owner-probe <state>` (testsupport.ChildOwner): it
// stands in for the relay daemon of a fixture's state, so a fixture's child report goes where the daemon records it
// (CRW-1289). It listens on <state>/control.sock and runs each child command it is sent on that state, as the daemon
// does (dispatch.ExecuteOwnedChild), in the composition of this program, and answers the command's exit code and its
// stdout and stderr bytes. It ticks nothing and borrows no store, so the store sees only the command's own writes. It
// prints "ready" once it listens and stops when its standard input closes. Release builds leave it out.
func init() {
	if len(os.Args) != 3 || os.Args[1] != "child-owner-probe" {
		return
	}
	adapter.Register(childcleanup.ConfigureSubscriptions)
	os.Exit(childOwnerProbe(os.Args[2]))
}

func childOwnerProbe(state string) int {
	address, release, err := hook.ControlAddress(filepath.Join(state, "control.sock"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer release()
	listener, err := net.Listen("unix", address)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); _ = listener.Close() }()
	fmt.Println("ready")
	for {
		conn, err := listener.Accept()
		if err != nil {
			return 0
		}
		var request struct {
			Program string
			Argv    [][]byte // base64 on the wire
		}
		if json.NewDecoder(conn).Decode(&request) == nil {
			argv := make([]string, len(request.Argv))
			for index, argument := range request.Argv {
				argv[index] = string(argument)
			}
			var stdout, stderr bytes.Buffer
			code := dispatch.ExecuteOwnedChild(dispatch.OwnedChildState(context.Background(), state), request.Program, argv, &stdout, &stderr)
			encode := base64.StdEncoding.EncodeToString
			_ = json.NewEncoder(conn).Encode(map[string]any{"exitCode": code, "stdout": encode(stdout.Bytes()), "stderr": encode(stderr.Bytes())})
		}
		_ = conn.Close()
	}
}
