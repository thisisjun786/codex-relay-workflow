package inbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// Directory is the inbox under a state directory S.
func Directory(state string) string { return filepath.Join(state, "takeover-inbox") }

// ConflictError is the refusal of the same operation ID with different bytes while the earlier
// entry still exists (RefusalReason.INBOX_CONFLICT): exit 2.
type ConflictError struct{}

func (*ConflictError) Error() string { return "the operation ID already names different inbox bytes" }

// writeRequest is the one injectable write boundary (inbox.py write_request): the whole entry,
// then fsync. Tests inject ENOSPC here.
var writeRequest = func(f *os.File, raw []byte) error {
	if _, err := f.Write(raw); err != nil {
		return err
	}
	return f.Sync()
}

// syncDirectory fsyncs a directory (inbox.py sync_directory); a seam for the tests that fail it.
var syncDirectory = func(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	err = unix.Fsync(fd)
	if e := unix.Close(fd); err == nil && e != nil {
		err = e
	}
	if err != nil {
		return &os.PathError{Op: "fsync", Path: path, Err: err}
	}
	return nil
}

// Enqueue publishes entry in S/takeover-inbox exactly as inbox.enqueue does: the directory
// (0700) and its parent are synced, the bytes are written and fsynced under a fresh O_EXCL 0600
// temporary name, linked to the operation ID without replacing anything, and the directory is
// synced before the answer. An existing final name with the same bytes is a retry, acknowledged
// only after this sender's own directory sync; different bytes are a ConflictError. The
// temporary name is always removed and the final name never is: once linked, another sender may
// already hold a durable acknowledgment for those bytes, and only the owner retires an entry.
func Enqueue(state string, entry Entry) (err error) {
	directory := Directory(state)
	if err = os.Mkdir(directory, 0700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if info, e := os.Stat(directory); e != nil || !info.IsDir() {
			return err // Path.mkdir(exist_ok=True) raises for an existing non-directory
		}
	}
	if err = syncDirectory(state); err != nil {
		return err
	}
	suffix := make([]byte, 16)
	if _, err = rand.Read(suffix); err != nil {
		return err
	}
	final := filepath.Join(directory, entry.ID)
	temporary := filepath.Join(directory, fmt.Sprintf(".tmp-%s-%d-%s", entry.ID, os.Getpid(), hex.EncodeToString(suffix)))
	defer func() {
		if e := os.Remove(temporary); e != nil && !errors.Is(e, os.ErrNotExist) && err == nil {
			err = e
		}
	}()
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = writeRequest(file, entry.Raw)
	if e := file.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Link(temporary, final); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, e := os.ReadFile(final)
		if e != nil {
			return e
		}
		if !bytes.Equal(existing, entry.Raw) {
			return &ConflictError{}
		}
	}
	return syncDirectory(directory)
}

// Queue answers a queueable refusal of command (argv: its own arguments) as cli.main does:
// the entry is built before any I/O, published, and the answer printed with its exit code.
func Queue(state, command string, argv []string) (contract.OrderedObject, int) {
	entry, err := Envelope(command, argv)
	if err != nil {
		var usage *UsageError
		if errors.As(err, &usage) {
			return contract.OrderedObject{{Key: "error", Value: "usage"}, {Key: "detail", Value: usage.Detail}}, contract.ExitUsage
		}
		return hostAnswer(err), contract.ExitHost
	}
	if err = Enqueue(state, entry); err != nil {
		var conflict *ConflictError
		if errors.As(err, &conflict) {
			return contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: "inbox_conflict"}, {Key: "detail", Value: conflict.Error()}}, contract.ExitRefused
		}
		return hostAnswer(err), contract.ExitHost
	}
	return contract.OrderedObject{{Key: "status", Value: "durably_queued"}, {Key: "operationId", Value: entry.ID}, {Key: "payloadDigest", Value: entry.Digest}, {Key: "detail", Value: "durably queued; not yet applied"}}, contract.ExitOk
}

// hostAnswer is the exit-3 answer of a publication that failed: no durable acknowledgment.
func hostAnswer(err error) contract.OrderedObject {
	return contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "reason", Value: "inbox_unavailable"}, {Key: "detail", Value: store.PythonOSError(err)}}
}

// QueueableRefusal reports whether err is an ownership refusal the fence answers by queueing
// (ownership.py OwnershipRefused.queueable): the store belongs to the other runtime, it is
// draining, or it is starting and this process is not the designated candidate.
func QueueableRefusal(err error) bool {
	var refused *ownership.Refused
	return errors.As(err, &refused) && refused.Queueable
}

// RefusedAtStart is cli.main's lock-free check_start for a queueable command: the fence runs it
// before the selection refusal, --kind-module and the writable open, so a store whose ownership
// refuses this writer queueably (another owner, draining, starting without the permit) queues
// the request whatever --socket or --kind-module says. Any other outcome, another refusal
// included, is left to the writable open, which judges the store again under its gate.
func RefusedAtStart(ctx context.Context, dbPath, socket string) bool {
	if r, err := ownership.ReadRecord(dbPath); err == nil && r.Owner == "go" && r.Phase == "active" {
		// Never queueable, so the preflight's database snapshot is not taken on every receipt:
		// a durable owner that disagrees with this mirror fails validation (not queueable), and
		// one that agrees admits an active Go writer.
		return false
	}
	canonical := ""
	if socket != "" {
		// A socket that cannot be canonicalized binds nothing; the writable open reports it.
		canonical, _ = store.CanonicalSocket(socket)
	}
	return QueueableRefusal(ownership.CheckStart(ctx, dbPath, canonical))
}
