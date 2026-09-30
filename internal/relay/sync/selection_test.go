package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func selectionReplay(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	// Python works in a directory of its own, and the Go side in one Python never touched.
	answer := pyoracle.AnswerInterned(t, "selection_capture.py", func() ([]byte, error) {
		base := t.TempDir()
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(root, "internal/relay/sync/testdata/selection_capture.py"), base)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, ".venv"), "UV_CACHE_DIR="+base+"/uv")
		raw, e := cmd.Output()
		if e != nil {
			return nil, e
		}
		answer, e := leftBehind(base, raw)
		if e != nil {
			return nil, e
		}
		// The state directory a socket would get is named by a digest of the socket's path.
		digest := socketStateName(t, raw)
		if !bytes.Contains(answer, []byte("/"+digest+"/")) {
			return nil, fmt.Errorf("python named no state directory %s for its socket", digest)
		}
		return relocated(relocated(answer, base, "<base>"), digest, "<socket-state>"), nil
	})
	answer, pool := openStores(t, relocated(answer, "<base>", base))
	answer = relocated(answer, "<socket-state>", socketStateName(t, answer))
	var recorded selectionAnswer
	if e = json.Unmarshal(answer, &recorded); e != nil {
		t.Fatal(e)
	}
	recorded.restore(t, base, pool)
	var captures []storeCapture
	if e = json.Unmarshal(recorded.Captures, &captures); e != nil {
		t.Fatal(e)
	}
	for _, c := range captures {
		binary := builtBinary(t)
		run := exec.Command(binary, append([]string{"relay"}, c.Argv...)...)
		c.Stdout = strings.ReplaceAll(c.Stdout, "'crw relay'", binary+" relay")
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
		if code != c.Exit || out.String() != c.Stdout || errout.String() != c.Stderr {
			t.Fatalf("ambiguous selection %v Python exit=%d Go=%d\nPython:%s\nGo:%s\nPython stderr:%s\nGo stderr:%s", c.Argv, c.Exit, code, c.Stdout, out.String(), c.Stderr, errout.String())
		}
	}
}

// selectionAnswer is what the selection scenario's Python left for the Go side: its captured
// calls, the stores it created (as dumps, by path under the base directory) and every other
// file it wrote there.
type selectionAnswer struct {
	Captures json.RawMessage            `json:"captures"`
	Stores   map[string]json.RawMessage `json:"stores"`
	Files    map[string]string          `json:"files"`
}

// leftBehind records Python's output with the stores and files it left under base; the uv cache
// is not Python's answer.
func leftBehind(base string, captures []byte) ([]byte, error) {
	answer := selectionAnswer{Captures: captures, Stores: map[string]json.RawMessage{}, Files: map[string]string{}}
	stores := newStoreRecorder()
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir() && relative == "uv":
			return filepath.SkipDir
		case entry.IsDir():
			return nil
		case entry.Name() == "relay.sqlite3":
			answer.Stores[relative], err = stores.dump(path)
			return err
		case strings.HasPrefix(relative, "state"+string(filepath.Separator)):
			// A store's WAL, mirror and gate are its own: the restore fences the copy afresh.
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(data) {
			return fmt.Errorf("%s is not text", path)
		}
		answer.Files[relative] = string(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stores.finish(answer, captures, "")
}

// restore puts what Python left back under base, each store fenced for the owner it names.
func (a selectionAnswer) restore(t *testing.T, base string, pool storePool) {
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
		restoreStore(t, pool.dump(t, dump), path)
		testsupport.Rehome(t, path)
	}
}

// socketStateName is the directory name a socket's own state gets (store.py socket_hash): the
// first 16 hex digits of the SHA-256 of the socket path the first captured call names.
func socketStateName(t *testing.T, answer []byte) string {
	t.Helper()
	var shape struct {
		Captures []struct{ Argv []string }
	}
	if e := json.Unmarshal(answer, &shape); e != nil || len(shape.Captures) == 0 {
		if e = json.Unmarshal(answer, &shape.Captures); e != nil || len(shape.Captures) == 0 {
			t.Fatalf("no captured call names a socket: %v", e)
		}
	}
	argv := shape.Captures[0].Argv
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--socket" {
			sum := sha256.Sum256([]byte(argv[i+1]))
			return hex.EncodeToString(sum[:8])
		}
	}
	t.Fatalf("no --socket in %v", argv)
	return ""
}
