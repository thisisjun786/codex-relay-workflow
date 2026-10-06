//go:build dev

package cxcfuzz

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// shimTargets are the three targets whose oracle shim this issue owns. The echo, shellwrite and
// memorygate shims belong to other issues.
func shimTargets() []string { return []string{"pyjson", "state", "goalplan"} }

// A start-up handshake is one request with a null input and an empty root (CRW-854). Each of this
// issue's shims must answer it inertly, without building a path from the empty root, mirroring or
// reading a document, or writing one: a shim that ran the case would create .codexclaw/... in the
// worker's own working directory on every worker start, and the pyjson shim would spawn python3.
func TestShimsAnswerTheStartupHandshakeInertly(t *testing.T) {
	requireNode(t)
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range shimTargets() {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			shim := filepath.Join(root, "internal", "dev", "cxcfuzz", "testdata", name, "shim.mjs")
			cmd := exec.Command("node", shim)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "ORACLE_ROOT="+DefaultOracleRoot)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = io.Discard
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

			if _, err := io.WriteString(stdin, "{\"id\":1,\"input\":null,\"root\":\"\"}\n"); err != nil {
				t.Fatal(err)
			}
			_ = stdin.Close()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil {
				t.Fatalf("the shim did not answer the handshake: %v", err)
			}
			// The answer must be the inert one: the reply to request 1 carrying a null output. A shim
			// that ran the case still answers (an error envelope or a case answer) and may leave no file
			// behind, so checking only for a reply and an empty directory would miss it: the pyjson shim
			// would spawn python3 on every worker start and still pass.
			var reply struct {
				ID     int
				Output any
				Error  any
			}
			if err := json.Unmarshal([]byte(line), &reply); err != nil {
				t.Fatalf("the handshake answer %q is not JSON: %v", line, err)
			}
			if reply.ID != 1 || reply.Output != nil || reply.Error != nil {
				t.Fatalf("the handshake answer %q is not the inert reply {id:1, output:null}", line)
			}
			// The reply is discarded by the pool, so only its presence matters. Wait for the process
			// to exit, then prove the handshake wrote nothing under the worker's working directory.
			done := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, stdout); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the shim did not exit after its stdin closed")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			sort.Strings(names)
			if len(names) > 0 {
				t.Fatalf("the handshake wrote under the worker's working directory: %v", names)
			}
		})
	}
}
