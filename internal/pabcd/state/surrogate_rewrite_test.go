package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
