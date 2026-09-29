package inbox

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// stateDir is an owner-only S, as both runtimes create it.
func stateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

func mustEnvelope(t *testing.T, command string, argv ...string) Entry {
	t.Helper()
	entry, err := Envelope(command, argv)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// A new entry is published 0600 in a 0700 inbox under its operation ID with no temporary
// left; a byte-identical retry is acknowledged again; the same ID with different bytes is a
// conflict (exit 2) that leaves the published entry untouched. The answers are cli.main's.
func TestEnqueue_publishes_once_and_refuses_a_conflicting_retry(t *testing.T) {
	state := stateDir(t)
	answer, code := Queue(state, "ack", cases["ack"])
	want := contract.OrderedObject{{Key: "status", Value: "durably_queued"}, {Key: "operationId", Value: "ack.event-1"}, {Key: "payloadDigest", Value: "sha256:c40a0f56c0021fa3e0986a87085051a4f50fbd61531a98cd69b907366c06a110"}, {Key: "detail", Value: "durably queued; not yet applied"}}
	if code != 0 || !equalAnswer(answer, want) {
		t.Fatalf("%d %v", code, answer)
	}
	directory := Directory(state)
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
	final := filepath.Join(directory, "ack.event-1")
	info, err = os.Stat(final)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if raw, _ := os.ReadFile(final); !bytes.Equal(raw, golden(t, "ack")) {
		t.Fatalf("%s", raw)
	}
	if got := names(t, directory); len(got) != 1 {
		t.Fatalf("temporary left behind: %v", got)
	}
	if again, code := Queue(state, "ack", cases["ack"]); code != 0 || !equalAnswer(again, want) {
		t.Fatalf("retry: %d %v", code, again)
	}
	conflict, code := Queue(state, "ack", append(append([]string{}, cases["ack"]...), "--reject", "stale_generation"))
	if code != 2 || !equalAnswer(conflict, contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: "inbox_conflict"}, {Key: "detail", Value: "the operation ID already names different inbox bytes"}}) {
		t.Fatalf("conflict: %d %v", code, conflict)
	}
	if raw, _ := os.ReadFile(final); !bytes.Equal(raw, golden(t, "ack")) || len(names(t, directory)) != 1 {
		t.Fatal("the conflicting retry changed the inbox")
	}
}

// A failed write or file fsync (ENOSPC through the write seam) never receives a durable
// acknowledgment: exit 3 inbox_unavailable, no final entry and no temporary.
func TestEnqueue_disk_full_publishes_nothing(t *testing.T) {
	state := stateDir(t)
	defer func(original func(*os.File, []byte) error) { writeRequest = original }(writeRequest)
	writeRequest = func(f *os.File, raw []byte) error {
		if _, err := f.Write(raw[:len(raw)/2]); err != nil {
			return err
		}
		return &os.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC}
	}
	answer, code := Queue(state, "emit", cases["emit"])
	if code != 3 || answer[0].Value != "host" || answer[1].Value != "inbox_unavailable" || !strings.HasPrefix(answer[2].Value.(string), "OSError: [Errno 28] No space left on device") {
		t.Fatalf("%d %v", code, answer)
	}
	if got := names(t, Directory(state)); len(got) != 0 {
		t.Fatalf("published after a failed write: %v", got)
	}
}

// test_a_failed_directory_sync_never_removes_a_retry_another_sender_acknowledged: sender A links
// the entry, sender B (a byte-identical retry) finds it and is acknowledged after its own
// directory sync, then A's directory sync fails. A gets no acknowledgment, and the entry B was
// acknowledged for stays published.
func TestEnqueue_failed_directory_sync_never_removes_the_final_entry(t *testing.T) {
	state := stateDir(t)
	entry := mustEnvelope(t, "ack", cases["ack"]...)
	final := filepath.Join(Directory(state), entry.ID)
	original := syncDirectory
	defer func() { syncDirectory = original }()
	var retry error = errors.New("not retried")
	syncDirectory = func(path string) error {
		if _, err := os.Stat(final); path == Directory(state) && err == nil && retry != nil {
			syncDirectory = original
			retry = Enqueue(state, entry)
			return &os.PathError{Op: "fsync", Path: path, Err: syscall.EIO}
		}
		return original(path)
	}
	if err := Enqueue(state, entry); err == nil {
		t.Fatal("a failed directory sync was acknowledged")
	}
	if retry != nil {
		t.Fatalf("the retry was not acknowledged: %v", retry)
	}
	if raw, err := os.ReadFile(final); err != nil || !bytes.Equal(raw, entry.Raw) {
		t.Fatalf("the acknowledged entry was removed: %v", err)
	}
	if got := names(t, Directory(state)); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

// Overlength identifiers and argv that is not UTF-8 are usage rejections decided before any
// I/O: exit 4, and no inbox directory is even created.
func TestQueue_deterministic_rejects_are_usage_errors_without_io(t *testing.T) {
	state := stateDir(t)
	for _, argv := range [][]string{
		{"--event", strings.Repeat("x", 197), "--ack-turn", "t", "--ack-proof", "p"},
		{"--event", "event-\xff", "--ack-turn", "t", "--ack-proof", "p"},
	} {
		answer, code := Queue(state, "ack", argv)
		if code != 4 || len(answer) != 2 || answer[0].Value != "usage" {
			t.Fatalf("%q: %d %v", argv, code, answer)
		}
	}
	if _, err := os.Stat(Directory(state)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected request touched the inbox: %v", err)
	}
}

func equalAnswer(got, want contract.OrderedObject) bool {
	var a, b bytes.Buffer
	return contract.Emit(&a, got) == nil && contract.Emit(&b, want) == nil && a.String() == b.String()
}

// An inbox path that is not a directory is a failed publication (Path.mkdir(exist_ok=True)
// raises FileExistsError): exit 3, nothing published.
func TestQueue_an_inbox_path_that_is_not_a_directory_is_a_host_error(t *testing.T) {
	state := stateDir(t)
	if err := os.WriteFile(Directory(state), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	answer, code := Queue(state, "ack", cases["ack"])
	if code != 3 || answer[2].Value != "FileExistsError: [Errno 17] File exists: '"+Directory(state)+"'" {
		t.Fatalf("%d %v", code, answer)
	}
}
