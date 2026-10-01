package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// selectionFixture is what the selection scenario's Python left for the Go side: its calls, the
// stores it created (as dumps, by path under the base directory) and every other file it wrote
// there.
type selectionFixture struct {
	Rows     storePool                  `json:"rows"`
	Stores   map[string]json.RawMessage `json:"stores"`
	Files    map[string]string          `json:"files"`
	Captures []struct {
		Argv []string `json:"argv"`
	} `json:"captures"`
}

// selectionReplay runs the calls of the Python selection scenario (SR-15: a socket whose state is
// ambiguous between two stores) through the built binary in a directory holding what Python
// left, and checks each answer against the golden.
func selectionReplay(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	var fixture selectionFixture
	if e := json.Unmarshal(relocated(readFixture(t, "selection"), "<base>", base), &fixture); e != nil {
		t.Fatal(e)
	}
	if len(fixture.Captures) == 0 {
		t.Fatal("the fixture holds no call")
	}
	fixture.restore(t, base)
	// The state directory a socket would get is named by a digest of the socket's path.
	socketState := socketStateName(t, fixture.Captures[0].Argv)
	binary := builtBinary(t)
	for index, c := range fixture.Captures {
		run := exec.Command(binary, append([]string{"relay"}, c.Argv...)...)
		env := []string{}
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "CODEX_SESSION_RELAY_STATE=") && !strings.HasPrefix(v, "XDG_STATE_HOME=") {
				env = append(env, v)
			}
		}
		run.Env = append(env, "XDG_STATE_HOME="+base+"/state")
		var out, errout bytes.Buffer
		run.Stdout, run.Stderr = &out, &errout
		code := processCode(run.Run())
		checkProcess(t, fmt.Sprintf("%03d", index), code, out.String(), errout.String(), golden.Substitute(binary, "<crw>"), golden.Substitute(base, "<base>"), golden.Substitute(socketState, "<socket-state>"))
	}
}

// restore puts what Python left back under base, each store fenced for the owner it names.
func (a selectionFixture) restore(t *testing.T, base string) {
	t.Helper()
	for relative, content := range a.Files {
		path := filepath.Join(base, relative)
		if e := os.MkdirAll(filepath.Dir(path), 0o700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, []byte(content), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	for relative, dump := range a.Stores {
		path := filepath.Join(base, relative)
		restoreStore(t, a.Rows.dump(t, dump), path)
		testsupport.Rehome(t, path)
	}
}

// socketStateName is the directory name a socket's own state gets (store.py socket_hash): the
// first 16 hex digits of the SHA-256 of the socket path argv names.
func socketStateName(t *testing.T, argv []string) string {
	t.Helper()
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--socket" {
			sum := sha256.Sum256([]byte(argv[i+1]))
			return hex.EncodeToString(sum[:8])
		}
	}
	t.Fatalf("no --socket in %v", argv)
	return ""
}
