package state

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// CRW-1005: the CXC Node original keeps a lone UTF-16 surrogate escape (\ud800) in a stored string when it rewrites the state,
// and the port used to write it back as U+FFFD, a silent change of stored text. The decision (D1) is refusal: a state file that
// holds an unpaired surrogate escape anywhere is not rewritten, and the file stays byte for byte as it was. A valid surrogate
// pair is not lossy and still writes.
func TestWriteStateRefusesToRewriteAFileHoldingALoneSurrogate(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"slug", `{"phase":"P","sessionId":"s","slug":"\ud800"}`},
		{"supersededBy", `{"phase":"P","sessionId":"s","supersededBy":"\udfff"}`},
		{"receiptClaimed", `{"phase":"P","sessionId":"s","unverifiedSubagents":[{"agentId":"a0","turnId":"t","agentType":"executor","attempts":3,"receiptClaimed":"x\ud800","recordedAt":"2026-01-01T00:00:00Z","resolvable":true}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			if err := makeSessionsDir(cwd); err != nil {
				t.Fatal(err)
			}
			path := StatePath(cwd, "s")
			raw := []byte(tc.body)
			if err := os.WriteFile(path, raw, 0o666); err != nil {
				t.Fatal(err)
			}
			s, _ := ReadStateStrict(cwd, "s")
			if err := WriteState(cwd, s); err == nil {
				t.Fatalf("WriteState rewrote a file holding a lone surrogate; the file now reads %q", fileText(t, path))
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
				t.Fatalf("a refused rewrite changed the file: got %q, want %q", got, raw)
			}
		})
	}
}

func TestWriteStateStillWritesAFileWhoseSurrogatePairIsWhole(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	path := StatePath(cwd, "s")
	if err := os.WriteFile(path, []byte(`{"phase":"P","sessionId":"s","slug":"\ud83d\ude00"}`), 0o666); err != nil {
		t.Fatal(err)
	}
	s, _ := ReadStateStrict(cwd, "s")
	if err := WriteState(cwd, s); err != nil {
		t.Fatalf("WriteState refused a file whose surrogate pair is whole: %v", err)
	}
	back, _ := ReadStateStrict(cwd, "s")
	if back.Slug != "\U0001F600" {
		t.Fatalf("the whole pair did not survive the rewrite: slug %q", back.Slug)
	}
}

// The check reads the file it guards. A file that cannot be read cannot be shown clean, so the write fails before any temp file
// is staged, and the bytes on disk stay as they were; only a file that does not exist is a new write.
func TestWriteStateRefusesWhenTheExistingFileCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	path := StatePath(cwd, "s")
	raw := []byte(`{"phase":"P","sessionId":"s","slug":"\ud800"}`)
	if err := os.WriteFile(path, raw, 0o666); err != nil {
		t.Fatal(err)
	}
	s, _ := ReadStateStrict(cwd, "s")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o666) })
	if err := WriteState(cwd, s); err == nil {
		t.Fatal("WriteState wrote over a file it could not read")
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
		t.Fatalf("a refused rewrite changed the file: got %q, want %q", got, raw)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("a refused rewrite left %d entries beside the state file, want 1", len(entries))
	}
}

func TestWriteStateCreatesAFileThatDoesNotExist(t *testing.T) {
	cwd := t.TempDir()
	if err := WriteState(cwd, State{Phase: "P", SessionID: "s", Slug: "x"}); err != nil {
		t.Fatalf("WriteState refused a new file: %v", err)
	}
	if back, _ := ReadStateStrict(cwd, "s"); back.Slug != "x" {
		t.Fatalf("the new file did not read back: slug %q", back.Slug)
	}
}

// The check opens the destination before staging anything. A special file there must not make that open or read block: a FIFO
// with no writer never returns from a blocking read, so the write would neither publish nor refuse, and a caller inside
// WithSessionLock would keep the lock for good. The target is refused at once with the bytes (here: the node) left as they were.
func TestWriteStateRefusesASpecialFileWithoutBlocking(t *testing.T) {
	for _, mode := range []string{"fifo", "symlink-to-fifo"} {
		for _, locked := range []bool{false, true} {
			name := mode
			if locked {
				name += "-under-lock"
			}
			t.Run(name, func(t *testing.T) {
				cwd := t.TempDir()
				if err := makeSessionsDir(cwd); err != nil {
					t.Fatal(err)
				}
				path := StatePath(cwd, "s")
				fifo := path
				if mode == "symlink-to-fifo" {
					fifo = filepath.Join(cwd, "target.fifo")
				}
				if err := syscall.Mkfifo(fifo, 0o666); err != nil {
					t.Skipf("no FIFOs here: %v", err)
				}
				if fifo != path {
					if err := os.Symlink(fifo, path); err != nil {
						t.Fatal(err)
					}
				}
				// Releases a write that is blocked on the FIFO, so a failing run does not leave a goroutine behind.
				t.Cleanup(func() {
					if f, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
						_ = f.Close()
					}
				})
				write := func() error { return WriteState(cwd, State{Phase: "P", SessionID: "s"}) }
				if locked {
					outer := write
					write = func() error { return WithSessionLock(cwd, "s", outer) }
				}
				done := make(chan error, 1)
				go func() { done <- write() }()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("WriteState wrote over a special file")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("WriteState blocked on a FIFO with no writer")
				}
				if fi, err := os.Lstat(path); err != nil || (mode == "fifo" && fi.Mode()&os.ModeNamedPipe == 0) || (mode != "fifo" && fi.Mode()&os.ModeSymlink == 0) {
					t.Fatalf("the refused write replaced the target: %v, %v", fi, err)
				}
				if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
					t.Fatalf("a refused write left %d entries in the sessions directory, want 1", len(entries))
				}
				if locked {
					// the lock was released: a second holder gets in at once
					if err := WithSessionLock(cwd, "s", func() error { return nil }); err != nil {
						t.Fatalf("the session lock was not released after the refused write: %v", err)
					}
				}
			})
		}
	}
}

// The refusal happens before the special file is opened at all. A writer blocked in open(2) on a FIFO returns as soon as any
// reader opens it, so a writer that is still blocked after WriteState has refused shows that the FIFO (or the target of a link
// to it) was classified without being opened; opening a device that a link points at has side effects of its own.
func TestWriteStateRefusesASpecialFileWithoutOpeningIt(t *testing.T) {
	for _, mode := range []string{"fifo", "symlink-to-fifo"} {
		t.Run(mode, func(t *testing.T) {
			cwd := t.TempDir()
			if err := makeSessionsDir(cwd); err != nil {
				t.Fatal(err)
			}
			path := StatePath(cwd, "s")
			fifo := path
			if mode == "symlink-to-fifo" {
				fifo = filepath.Join(cwd, "target.fifo")
			}
			if err := syscall.Mkfifo(fifo, 0o666); err != nil {
				t.Skipf("no FIFOs here: %v", err)
			}
			if fifo != path {
				if err := os.Symlink(fifo, path); err != nil {
					t.Fatal(err)
				}
			}
			opened := make(chan struct{})
			go func() {
				if f, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil { // blocks until a reader opens the FIFO
					_ = f.Close()
				}
				close(opened)
			}()
			time.Sleep(100 * time.Millisecond) // let the writer reach its open
			release := func() {
				if f, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
					_ = f.Close()
				}
			}
			t.Cleanup(func() { release(); <-opened })
			if err := WriteState(cwd, State{Phase: "P", SessionID: "s"}); err == nil {
				t.Fatal("WriteState wrote over a special file")
			}
			select {
			case <-opened:
				t.Fatal("WriteState opened the special file before refusing it")
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

// CRW-1065: the CXC original reads a file that is not JSON as an unreadable default and its writer rewrites it with the default
// state, which publishes over the stored bytes. The port refuses such a rewrite when the bytes hold a lone surrogate escape. The
// refusal must come before any temp file is staged or any byte is written, so the file stays byte for byte as it was, and the
// sessions directory keeps only that file. The inputs are the non-JSON shapes a lone escape can sit in.
func TestWriteStateRefusalLeavesANonJSONFileByteForByte(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"plain-text", "not json \\ud800"},
		{"truncated-object", `{"phase": "P", "slug": "\ud800"`},
		{"trailing-garbage", `{"phase": "P", "slug": "\ud800"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			if err := makeSessionsDir(cwd); err != nil {
				t.Fatal(err)
			}
			path := StatePath(cwd, "s")
			raw := []byte(tc.body)
			if err := os.WriteFile(path, raw, 0o666); err != nil {
				t.Fatal(err)
			}
			s, unreadable := ReadStateStrict(cwd, "s")
			if !unreadable {
				t.Fatal("the non-JSON file read as readable; the case does not exercise the unreadable default")
			}
			if err := WriteState(cwd, s); err == nil {
				t.Fatalf("WriteState rewrote a non-JSON file holding a lone surrogate; the file now reads %q", fileText(t, path))
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, raw) {
				t.Fatalf("a refused rewrite changed the file: got %q, want %q", got, raw)
			}
			if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
				t.Fatalf("a refused rewrite left %d entries beside the state file, want 1", len(entries))
			}
		})
	}
}
