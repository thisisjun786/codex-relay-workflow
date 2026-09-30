package delivery

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The Python answers these tests compare with are recorded (internal/testsupport/pyoracle). Many
// of them carry values derived from absolute fixture paths: an artifact's declared path is part
// of the manifest revision hash, which is part of the event id, which is part of the request id
// and every row that names it. A recorded answer can only be replayed against a Go run that
// declares the same paths, so a tree a test shares with a Python run is a fixed directory under
// parityRoot rather than a fresh temporary one. The directory is named by a digest of the test's
// name, locked (flock) for as long as the test uses it so two test processes never share one, and
// recorded as "<tree>".
const parityRoot = "/tmp/crw-delivery-parity"

var (
	parityMu    sync.Mutex
	parityCount = map[string]int{}
	// parityHeld are the trees locked for the whole process (a capture module's root).
	parityHeld = map[string]string{}
)

// parityTree is the fixed tree for the calling test, empty and locked until the test ends. A test
// that asks twice gets a second tree.
func parityTree(t *testing.T) string {
	t.Helper()
	parityMu.Lock()
	n := parityCount[t.Name()]
	parityCount[t.Name()] = n + 1
	parityMu.Unlock()
	key := t.Name()
	if n > 0 {
		key = fmt.Sprintf("%s#%d", key, n)
	}
	dir, release, err := lockParityDir(key)
	mustDo(t, err)
	t.Cleanup(release)
	return dir
}

// processParityTree is the fixed tree for key, locked until the test process exits (TestMain
// releases it). A capture module's Python run writes every test's tree under one such root.
func processParityTree(t testing.TB, key string) string {
	t.Helper()
	parityMu.Lock()
	defer parityMu.Unlock()
	if dir, ok := parityHeld[key]; ok {
		return dir
	}
	dir, release, err := lockParityDir(key)
	if err != nil {
		t.Fatal(err)
	}
	parityHeld[key] = dir
	parityReleases = append(parityReleases, release)
	return dir
}

var parityReleases []func()

// releaseParityTrees removes and unlocks every process-lifetime tree.
func releaseParityTrees() {
	for _, release := range parityReleases {
		release()
	}
}

func lockParityDir(key string) (string, func(), error) {
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(parityRoot, hex.EncodeToString(sum[:])[:12])
	if err := os.MkdirAll(parityRoot, 0o755); err != nil {
		return "", nil, err
	}
	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return "", nil, err
	}
	release := func() {
		_ = testsupport.RemoveTempTree(dir)
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
	if err := testsupport.RemoveTempTree(dir); err != nil {
		release()
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		release()
		return "", nil, err
	}
	return dir, release, nil
}

// pythonOutput runs cmd and returns its stdout, or an error carrying its stderr.
func pythonOutput(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%w\n%s", err, e.Stderr)
		}
		return nil, err
	}
	return out, nil
}

// pythonCombined runs cmd and returns stdout and stderr together, or an error carrying them.
func pythonCombined(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, out)
	}
	return out, nil
}

// pyAnswer is pyoracle.Answer with the repository root substituted as well.
func pyAnswer(t *testing.T, key string, capture func() ([]byte, error), opts ...pyoracle.Option) []byte {
	t.Helper()
	opts = append(opts, pyoracle.Substitute(repoRoot(t), "<repo>"))
	return pyoracle.Answer(t, key, capture, opts...)
}

// namedTB files an answer several tests share under one recording name.
type namedTB struct {
	testing.TB
	name string
}

func (n namedTB) Name() string { return n.name }

// recordedStore is the SQLite store a Python run left at path, as recorded. When Python runs, the
// store is neutralized in place (neutralStore: its random id, wall-clock stamp and device and inode
// numbers become constants, and VACUUM lays it out the same way every time) and recorded
// gzip-compressed; otherwise the recording is written at path.
func recordedStore(t testing.TB, key, path string) {
	t.Helper()
	compressed := pyoracle.Answer(t, key, func() ([]byte, error) {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return nil, err
		}
		err = neutralStore(db)
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, err := writer.Write(content); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	})
	if pyoracle.Live() {
		return
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// inDirectory runs fn in dir and returns to the working directory it left. pyoracle files a
// recording relative to the package directory, so a test that must work elsewhere moves there only
// around its own calls rather than for the whole test (t.Chdir).
func inDirectory(t *testing.T, dir string, fn func()) {
	t.Helper()
	wd, err := os.Getwd()
	mustDo(t, err)
	mustDo(t, os.Chdir(dir))
	defer func() { mustDo(t, os.Chdir(wd)) }()
	fn()
}
