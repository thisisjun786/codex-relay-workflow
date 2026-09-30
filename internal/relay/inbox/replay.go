package inbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// ReplayLock serializes replayers from reading an entry until after its unlink and directory
// sync (cutover.md Inbox step 6). It is never unlinked or replaced.
const ReplayLock = ".replay.lock"

// isEntryName reports whether a directory entry name is an inbox entry's: the decision-25
// grammar, shared with ownership.PendingInbox so that takeover commit counts exactly the
// names a drain reads and retires. Every other name, like a '.'-name, is not an entry.
func isEntryName(name string) bool {
	return ownership.IsInboxEntry(name)
}

// Error is an exception the fence's replay raises, as the host detail it prints:
// f"{type(error).__name__}: {error}".
type Error struct{ Detail string }

func (e *Error) Error() string { return e.Detail }

func valueError(format string, args ...any) error {
	return &Error{Detail: "ValueError: " + fmt.Sprintf(format, args...)}
}

// Apply runs one queued command's existing handler on the drainer's own store, inside the
// composing transaction ctx carries (never through a second store.Open, which would wait on
// the connection this transaction holds). argv is the command's own arguments. A completed
// answer is code 0, a domain refusal 2 and a usage refusal 4, each with the object the
// handler's stdout would hold. Anything else is err, and the entry is kept: a Retained(err)
// failure writes no marker and the drain goes on, any other fails the drain.
type Apply func(ctx context.Context, st *store.Store, command string, argv []string) (answer any, code int, err error)

// Retained reports whether a handler failure keeps its entry for a later replay without a
// marker and without stopping the drain (inbox.py retained): an ownership refusal raised after
// the entry's transaction opened, or a receipt refused because the host could not confirm the
// turn (HostUnavailable).
func Retained(err error) bool {
	var refused *ownership.Refused
	if errors.As(err, &refused) {
		return true
	}
	var receipt *store.RefusedError
	var host interface{ PythonExceptionKind() string }
	return errors.As(err, &receipt) && errors.As(err, &host) && host.PythonExceptionKind() == "HostUnavailable"
}

// unlink is the entry removal after commit; a seam for the crash-after-commit tests.
var unlink = os.Remove

// Drain replays S/takeover-inbox through apply (inbox.py replay), in name order, under the
// replay lock (waited for at most ownership.LockWait: expiry is LockWaitExpired, a retryable
// host error that changed nothing). Each entry's handler result and its schema_meta marker
// commit in one transaction on st; only after that commit is the entry, the very inode read,
// unlinked and the directory synced. A missing inbox is nothing to replay. The first entry that
// fails (an invalid entry, a storage or host failure) stops the drain with its error and is
// kept, so the writable command or the daemon recovery that drains fails closed.
func Drain(ctx context.Context, st *store.Store, state string, apply Apply) error {
	directory := Directory(state)
	if _, err := os.Stat(directory); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return nil // Path.exists() is False
		}
		return &Error{Detail: store.PythonOSError(err)}
	}
	lock, err := lockReplay(ctx, filepath.Join(directory, ReplayLock))
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	names, err := list(directory)
	if err != nil {
		return &Error{Detail: store.PythonOSError(err)}
	}
	for _, name := range names {
		if err = replay(ctx, st, directory, name, apply); err != nil {
			return err
		}
	}
	return nil
}

// Check reads S/takeover-inbox as Drain reads it and applies nothing: the replay lock opened as
// the replay opens it, without taking it, then every name the replay reads, read (inbox.py
// read_entry) and validated (_replay_entry's checks) as the replay does. Its error is the one
// the drain would stop with first, so the takeover transfer refuses at Step 4, before ownership
// moves, on an inbox the candidate's recovery could only fail closed on (cutover.md Step 4). A
// missing inbox is nothing to check.
func Check(state string) error {
	directory := Directory(state)
	if _, err := os.Stat(directory); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return nil
		}
		return &Error{Detail: store.PythonOSError(err)}
	}
	if err := checkReplayLock(directory); err != nil {
		return err
	}
	names, err := list(directory)
	if err != nil {
		return &Error{Detail: store.PythonOSError(err)}
	}
	for _, name := range names {
		raw, _, ok, err := readEntry(filepath.Join(directory, name), name)
		if err != nil {
			return err
		}
		if !ok {
			continue // retired since the listing, as the replay skips it
		}
		if _, _, err = validate(name, raw); err != nil {
			return err
		}
	}
	return nil
}

// checkReplayLock is lockReplay's open without its side effects: the lock file opened for
// reading and writing without following a link, or, where there is none yet, the inbox
// directory writable and searchable, as creating it needs. It fails as that open would (a
// symbolic link, a directory, a file or directory this user may not write), with the drain's
// wording, and it neither creates nor takes the lock.
func checkReplayLock(directory string) error {
	path := filepath.Join(directory, ReplayLock)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err == nil {
		_ = unix.Close(fd)
		return nil
	}
	if errors.Is(err, unix.ENOENT) {
		err = unix.Access(directory, unix.W_OK|unix.X_OK)
	}
	if err != nil {
		return &Error{Detail: store.PythonOSError(&os.PathError{Op: "open", Path: path, Err: err})}
	}
	return nil
}

// lockReplay opens (creating 0600, never following a link) and takes the replay lock EX within
// the fence's bound.
func lockReplay(ctx context.Context, path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, &Error{Detail: store.PythonOSError(&os.PathError{Op: "open", Path: path, Err: err})}
	}
	lock := os.NewFile(uintptr(fd), path)
	deadline := time.Now().Add(ownership.LockWait)
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = lock.Close()
			return nil, &Error{Detail: store.PythonOSError(err)}
		}
		if !time.Now().Before(deadline) {
			_ = lock.Close()
			return nil, &ownership.LockWaitExpired{What: "the takeover inbox replay lock", Bound: ownership.LockWait}
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// list is the directory's entry names, sorted as sorted(directory.iterdir()) sorts them.
func list(directory string) ([]string, error) {
	f, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	return slices.DeleteFunc(names, func(name string) bool { return !isEntryName(name) }), nil
}

// inode identifies the file an entry's bytes were read from.
type inode struct{ dev, ino uint64 }

// readEntry is inbox.read_entry: the entry's bytes and inode, never following a link and never
// waiting on a FIFO; ok is false when the entry was already retired.
func readEntry(path, name string) (raw []byte, id inode, ok bool, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil, inode{}, false, nil
		case errors.Is(err, unix.ELOOP):
			return nil, inode{}, false, valueError("invalid takeover inbox entry: %s: symbolic link", name)
		}
		return nil, inode{}, false, &Error{Detail: store.PythonOSError(&os.PathError{Op: "open", Path: path, Err: err})}
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err != nil {
		return nil, inode{}, false, &Error{Detail: store.PythonOSError(err)}
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, inode{}, false, valueError("invalid takeover inbox entry: %s: not a regular file", name)
	}
	raw, err = io.ReadAll(file)
	if err != nil {
		return nil, inode{}, false, &Error{Detail: store.PythonOSError(err)}
	}
	return raw, inode{uint64(info.Dev), uint64(info.Ino)}, true, nil
}

// replay applies one entry: validated, then judged against its marker, in one transaction
// (inbox.py _replay_entry).
func replay(ctx context.Context, st *store.Store, directory, name string, apply Apply) error {
	path := filepath.Join(directory, name)
	raw, read, ok, err := readEntry(path, name)
	if err != nil || !ok {
		return err // not ok: another admitted owner process committed and retired it
	}
	entry, argv, err := validate(name, raw)
	if err != nil {
		return err
	}
	key := "inbox:" + name
	kept := false
	var failed error // the body's own failure, reported without the transaction's wrapping
	err = st.Compose(ctx, func(ctx context.Context, conn *sql.Conn) error {
		failed = replayed(ctx, st, conn, key, name, entry, argv, apply, &kept)
		return failed
	})
	if failed != nil {
		return failed
	}
	if err != nil || kept {
		return err
	}
	// The commit above precedes both unlink and directory sync. A crash here leaves a file whose
	// next replay reads the marker and calls no handler. Only the bytes just applied are
	// retired, never a newer entry a sender linked at the same name since.
	var current unix.Stat_t
	if err = unix.Lstat(path, &current); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return &Error{Detail: store.PythonOSError(&os.PathError{Op: "lstat", Path: path, Err: err})}
	}
	if (inode{uint64(current.Dev), uint64(current.Ino)}) != read {
		return nil
	}
	if err = unlink(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &Error{Detail: store.PythonOSError(err)}
	}
	if err = syncDirectory(directory); err != nil {
		return &Error{Detail: store.PythonOSError(err)}
	}
	return nil
}

// replayed is one entry's transaction body (inbox.py _replay_entry inside composing()): the
// marker check, the handler under a savepoint and the marker, or the terminal conflict. kept is
// set for a retained failure, which writes no marker and keeps the entry.
func replayed(ctx context.Context, st *store.Store, conn *sql.Conn, key, name string, entry Entry, argv []string, apply Apply, kept *bool) error {
	previous, err := readMarker(ctx, conn, key)
	if err != nil {
		return err
	}
	if previous != nil {
		digest, present := previous["payloadDigest"]
		if !present {
			return &Error{Detail: "KeyError: 'payloadDigest'"}
		}
		if text, isString := digest.(string); !isString || text != entry.Digest {
			// A retired ID reused with different bytes: a terminal conflict, never applied,
			// never blocking the entries after it.
			conflict := canonical(map[string]any{"payloadDigest": entry.Digest, "exit": 2, "answer": map[string]any{"error": "refused", "reason": "inbox_conflict", "detail": "committed inbox payload differs"}})
			_, err = conn.ExecContext(ctx, "INSERT OR IGNORE INTO schema_meta VALUES (?,?)", "inbox-conflict:"+name+":"+entry.Digest[7:23], string(conflict))
			return err
		}
		exit, present := previous["exit"]
		if !present {
			return &Error{Detail: "KeyError: 'exit'"}
		}
		if n, isInt := integer(exit); isInt && n.Sign() == 0 {
			return nil // applied before a crash between commit and unlink: never run again
		}
	}
	// Only a successful application deduplicates; a refused one is judged again.
	if _, err = conn.ExecContext(ctx, "SAVEPOINT inbox_handler"); err != nil {
		return err
	}
	answer, code, failure := apply(ctx, st, entry.Command, argv)
	if failure == nil && code != 0 && code != contract.ExitRefused && code != contract.ExitUsage {
		failure = fmt.Errorf("queued %s answered exit %d", entry.Command, code)
	}
	if failure != nil && !Retained(failure) {
		return failure // the whole transaction rolls back; the entry stays
	}
	if failure != nil || code != 0 {
		if _, err = conn.ExecContext(ctx, "ROLLBACK TO inbox_handler"); err != nil {
			return err
		}
	}
	if _, err = conn.ExecContext(ctx, "RELEASE inbox_handler"); err != nil {
		return err
	}
	if failure != nil {
		*kept = true // retained: no marker, the entry stays for a later replay
		return nil
	}
	value, err := markerValue(entry.Digest, code, answer)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "INSERT OR REPLACE INTO schema_meta VALUES (?,?)", key, value)
	return err
}

// readMarker is json.loads of schema_meta['inbox:<id>']: nil when there is none (or it is
// JSON null, which inbox.py also reads as no marker).
func readMarker(ctx context.Context, conn *sql.Conn, key string) (map[string]any, error) {
	var value string
	err := conn.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decoded, err := decodeJSON([]byte(value))
	if err != nil || decoded == nil {
		return nil, err
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, &Error{Detail: "TypeError: the inbox marker " + key + " is not an object"}
	}
	return object, nil
}

// markerValue is the canonical marker {"payloadDigest", "exit", "answer"}, the answer being the
// handler's stdout object.
func markerValue(digest string, code int, answer any) (string, error) {
	var printed bytes.Buffer
	if err := contract.Emit(&printed, answer); err != nil {
		return "", err
	}
	value, err := decodeJSON(printed.Bytes())
	if err != nil {
		return "", err
	}
	return string(canonical(map[string]any{"payloadDigest": digest, "exit": code, "answer": value})), nil
}

// decodeJSON is json.loads for a document Go can read: objects as maps, numbers as their text.
func decodeJSON(raw []byte) (any, error) {
	if _, err := store.DecodeUTF8(raw); err != nil {
		return nil, &Error{Detail: "UnicodeDecodeError: " + err.Error()}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, jsonError(raw, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, jsonError(raw, errors.New("extra data"))
	}
	return value, nil
}

// jsonError is json.loads's failure in Python's words where Python fails the same way.
func jsonError(raw []byte, err error) error {
	text, e := store.DecodeUTF8(raw)
	if e != nil {
		return &Error{Detail: "UnicodeDecodeError: " + e.Error()}
	}
	if message := store.PythonJSONError(text); message != "" {
		return &Error{Detail: "JSONDecodeError: " + message}
	}
	return &Error{Detail: "ValueError: " + err.Error()}
}

// integer is a JSON number json.loads reads as an int (no fraction, no exponent).
func integer(value any) (*big.Int, bool) {
	number, ok := value.(json.Number)
	if !ok || strings.ContainsAny(string(number), ".eE") {
		return nil, false
	}
	return new(big.Int).SetString(string(number), 10)
}

// pythonCanonical is canonical(json.loads(raw)) for a decoded document: integers as Python
// ints, other numbers as Python floats.
func pythonCanonical(value any) any {
	switch v := value.(type) {
	case json.Number:
		if n, ok := integer(v); ok {
			return json.Number(n.String())
		}
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return v
		}
		return f
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = pythonCanonical(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = pythonCanonical(item)
		}
		return out
	}
	return value
}

// validate checks a grammar-valid entry's bytes as inbox.py _replay_entry does before it
// replays them: canonical JSON of inboxVersion (the integer 1), a replayable command, an
// operationId equal to the file name, typed arguments of that command, and an entry that the
// typed arguments re-derive byte for byte (envelope of those values with defaults omitted, as
// inbox.py re-derives from the rebuilt Namespace, so an empty append list stays in it). It
// returns the entry and the command's own argv rebuilt from its arguments, which is how the
// handler parses them (an empty list passes no flag, which the handler reads as the None it
// treats alike). Any failure is storage corruption: the entry is kept and the drain fails
// closed.
func validate(name string, raw []byte) (Entry, []string, error) {
	decoded, err := decodeJSON(raw)
	if err != nil {
		return Entry{}, nil, err
	}
	request, isObject := decoded.(map[string]any)
	invalid := valueError("invalid takeover inbox entry: %s", name)
	if !isObject {
		return Entry{}, nil, invalid
	}
	version, isInt := integer(request["inboxVersion"])
	command, _ := request["command"].(string)
	_, replayable := stableKeys[command]
	id, _ := request["operationId"].(string)
	if !isInt || version.Cmp(big.NewInt(1)) != 0 || !replayable || id != name || !bytes.Equal([]byte(evidence.Dumps(pythonCanonical(decoded), true, true, false)), raw) {
		return Entry{}, nil, invalid
	}
	arguments, isObject := request["arguments"].(map[string]any)
	actions := map[string]argparse.Action{}
	var order []argparse.Action
	for _, action := range argparse.Specs[command].Actions {
		if action.Dest != "help" && len(action.Flags) > 0 {
			actions[action.Dest] = action
			order = append(order, action)
		}
	}
	if !isObject {
		return Entry{}, nil, valueError("invalid inbox arguments")
	}
	for dest := range arguments {
		if _, known := actions[dest]; !known {
			return Entry{}, nil, valueError("invalid inbox arguments")
		}
	}
	var argv []string
	typed := map[string]any{}
	for _, action := range order {
		value, present := arguments[action.Dest]
		if !present {
			if action.Required {
				return Entry{}, nil, valueError("missing inbox argument: %s", action.Dest)
			}
			continue
		}
		flag := action.Flags[len(action.Flags)-1]
		text := []string{}
		valid := false
		switch {
		case action.Kind == "_AppendAction":
			items, isList := value.([]any)
			valid = isList
			for _, item := range items {
				s, isString := item.(string)
				valid = valid && isString
				text = append(text, s)
			}
			typed[action.Dest] = text
		case action.Kind == "_StoreTrueAction" || action.Kind == "_StoreFalseAction":
			typed[action.Dest], valid = value.(bool)
		case action.Type == "int":
			var n *big.Int
			n, valid = integer(value)
			if valid {
				text = []string{n.String()}
			}
			typed[action.Dest] = n
		default:
			var s string
			s, valid = value.(string)
			text = []string{s}
			typed[action.Dest] = s
		}
		if valid && action.Choices != nil {
			s, isString := value.(string)
			valid = isString && slices.Contains(action.Choices, s)
		}
		if !valid {
			return Entry{}, nil, valueError("invalid inbox argument: %s", action.Dest)
		}
		switch action.Kind {
		case "_StoreTrueAction", "_StoreFalseAction":
			if value == (action.Kind == "_StoreTrueAction") {
				argv = append(argv, flag)
			}
		default:
			for _, item := range text {
				argv = append(argv, flag+"="+item)
			}
		}
	}
	for _, action := range order {
		if value, present := typed[action.Dest]; present && isDefault(command, action, value) {
			delete(typed, action.Dest) // Python's envelope omits a value equal to its default
		}
	}
	derived, err := envelope(command, typed)
	if err != nil {
		var usage *UsageError
		if errors.As(err, &usage) {
			return Entry{}, nil, valueError("%s", usage.Detail)
		}
		return Entry{}, nil, valueError("inbox payload digest or identifier disagrees")
	}
	if !bytes.Equal(derived.Raw, raw) {
		return Entry{}, nil, valueError("inbox payload digest or identifier disagrees")
	}
	return derived, argv, nil
}
