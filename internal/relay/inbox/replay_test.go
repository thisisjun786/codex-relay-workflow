package inbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// goStore is a fresh store this runtime owns, in its own owner-only state directory.
func goStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	state := stateDir(t)
	st, err := store.Open(t.Context(), filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, state
}

func meta(t *testing.T, st *store.Store, key string) (string, bool) {
	t.Helper()
	var value string
	err := st.DB.QueryRowContext(t.Context(), "SELECT value FROM schema_meta WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return value, true
}

func metaPrefix(t *testing.T, st *store.Store, prefix string) []string {
	t.Helper()
	rows, err := st.DB.QueryContext(t.Context(), "SELECT key FROM schema_meta WHERE key LIKE ? ORDER BY key", prefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	return keys
}

func publish(t *testing.T, state string, entry Entry) {
	t.Helper()
	if err := Enqueue(state, entry); err != nil {
		t.Fatal(err)
	}
}

// recorder is an Apply that writes one domain row per application (schema_meta
// 'test:applied:<command>:<n>'), so a rollback, a duplicate or a missing application shows.
type recorder struct {
	calls  []string
	answer func(call int, argv []string) (any, int, error)
}

func (r *recorder) apply(ctx context.Context, st *store.Store, command string, argv []string) (any, int, error) {
	r.calls = append(r.calls, command+" "+strings.Join(argv, " "))
	call := len(r.calls)
	if _, err := st.Q(ctx).ExecContext(ctx, "INSERT INTO schema_meta VALUES (?, ?)", "test:applied:"+command+":"+strings.Join(argv, " ")+":"+string(rune('0'+call)), "row"); err != nil {
		return nil, 0, err
	}
	if r.answer != nil {
		return r.answer(call, argv)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "text", Value: "雪"}}, 0, nil
}

// A queued entry is applied once: its handler's result and the canonical marker commit in one
// transaction, then the entry is unlinked. A second drain finds nothing.
func TestDrain_applies_once_and_commits_the_marker_with_the_result(t *testing.T) {
	st, state := goStore(t)
	entry := mustEnvelope(t, "ack", cases["ack"]...)
	publish(t, state, entry)
	r := &recorder{}
	if err := Drain(t.Context(), st, state, r.apply); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ack --event=event-1 --ack-turn=parent-turn --ack-proof=proof-1"}; !slices.Equal(r.calls, want) {
		t.Fatalf("%q", r.calls)
	}
	marker, ok := meta(t, st, "inbox:ack.event-1")
	if want := `{"answer":{"ok":true,"text":"雪"},"exit":0,"payloadDigest":"` + entry.Digest + `"}`; !ok || marker != want {
		t.Fatalf("marker %q", marker)
	}
	if got := metaPrefix(t, st, "test:applied:"); len(got) != 1 {
		t.Fatalf("%v", got)
	}
	if got := names(t, Directory(state)); !slices.Equal(got, []string{ReplayLock}) {
		t.Fatalf("the applied entry was not retired: %v", got)
	}
	if err := Drain(t.Context(), st, state, r.apply); err != nil || len(r.calls) != 1 {
		t.Fatal(err, r.calls)
	}
}

// Crash after the commit and before the unlink: the entry is still there and the next drain
// resolves it through the marker, never through the handler. Crash before the commit (the
// transaction fails after the handler wrote): nothing is committed, the entry stays and the
// drain fails closed, applying nothing after it.
func TestDrain_crash_after_commit_replays_the_marker_and_crash_before_commit_keeps_the_entry(t *testing.T) {
	st, state := goStore(t)
	first := mustEnvelope(t, "ack", cases["ack"]...)
	second := mustEnvelope(t, "ack", "--event", "event-2", "--ack-turn", "t", "--ack-proof", "p")
	publish(t, state, first)
	publish(t, state, second)
	fail := true
	r := &recorder{answer: func(call int, argv []string) (any, int, error) {
		if fail && call == 1 {
			return nil, 0, &Error{Detail: "OSError: [Errno 5] Input/output error"}
		}
		return contract.OrderedObject{}, 0, nil
	}}
	err := Drain(t.Context(), st, state, r.apply)
	if err == nil || err.Error() != "OSError: [Errno 5] Input/output error" {
		t.Fatalf("crash before commit: %v", err)
	}
	if _, ok := meta(t, st, "inbox:ack.event-1"); ok || len(metaPrefix(t, st, "test:applied:")) != 0 || len(r.calls) != 1 {
		t.Fatalf("a failed transaction committed: %v", r.calls)
	}
	if got := names(t, Directory(state)); !slices.Contains(got, first.ID) || !slices.Contains(got, second.ID) {
		t.Fatalf("%v", got)
	}
	fail = false
	defer func(original func(string) error) { unlink = original }(unlink)
	unlink = func(path string) error { return errors.New("crash after commit") }
	if err = Drain(t.Context(), st, state, r.apply); err == nil || !strings.Contains(err.Error(), "crash after commit") {
		t.Fatalf("crash after commit: %v", err)
	}
	if _, ok := meta(t, st, "inbox:ack.event-1"); !ok || len(r.calls) != 2 {
		t.Fatal("the commit did not happen", r.calls)
	}
	unlink = os.Remove
	if err = Drain(t.Context(), st, state, r.apply); err != nil {
		t.Fatal(err)
	}
	// event-1 resolved by its marker; only event-2 ran its handler.
	if len(r.calls) != 3 || !strings.Contains(r.calls[2], "event-2") {
		t.Fatalf("the committed entry ran its handler again: %q", r.calls)
	}
	if got := metaPrefix(t, st, "test:applied:"); len(got) != 2 {
		t.Fatalf("%v", got)
	}
	if got := names(t, Directory(state)); !slices.Equal(got, []string{ReplayLock}) {
		t.Fatalf("%v", got)
	}
}

// Only a successful application deduplicates: a domain refusal (exit 2) or a usage refusal
// (exit 4) rolls its handler's writes back and stores its answer, and a byte-identical retry is
// judged again and replaces the marker.
func TestDrain_only_a_successful_application_deduplicates(t *testing.T) {
	st, state := goStore(t)
	entry := mustEnvelope(t, "ack", cases["ack"]...)
	refusal := contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: "unknown_event"}, {Key: "detail", Value: "no such event"}}
	codes := []int{2, 4, 0}
	r := &recorder{answer: func(call int, argv []string) (any, int, error) {
		if codes[call-1] == 4 {
			return contract.OrderedObject{{Key: "error", Value: "usage"}, {Key: "detail", Value: "needs --socket"}}, 4, nil
		}
		if codes[call-1] == 2 {
			return refusal, 2, nil
		}
		return contract.OrderedObject{{Key: "ok", Value: true}}, 0, nil
	}}
	for i, want := range []string{
		`{"answer":{"detail":"no such event","error":"refused","reason":"unknown_event"},"exit":2,"payloadDigest":"` + entry.Digest + `"}`,
		`{"answer":{"detail":"needs --socket","error":"usage"},"exit":4,"payloadDigest":"` + entry.Digest + `"}`,
		`{"answer":{"ok":true},"exit":0,"payloadDigest":"` + entry.Digest + `"}`,
	} {
		publish(t, state, entry)
		if err := Drain(t.Context(), st, state, r.apply); err != nil {
			t.Fatal(err)
		}
		if marker, _ := meta(t, st, "inbox:"+entry.ID); marker != want || len(r.calls) != i+1 {
			t.Fatalf("round %d: %q %v", i, marker, r.calls)
		}
		applied := metaPrefix(t, st, "test:applied:")
		if codes[i] != 0 && len(applied) != 0 || codes[i] == 0 && len(applied) != 1 {
			t.Fatalf("round %d: refused handler writes survived: %v", i, applied)
		}
		if slices.Contains(names(t, Directory(state)), entry.ID) {
			t.Fatalf("round %d: the judged entry was not retired", i)
		}
	}
	publish(t, state, entry)
	if err := Drain(t.Context(), st, state, r.apply); err != nil || len(r.calls) != 3 {
		t.Fatalf("a successful application ran again: %v %v", err, r.calls)
	}
}

// A retired ID reused with different bytes is a terminal conflict: never applied, the inbox:
// marker unchanged, inbox-conflict:<id>:<16 hex> recorded with the fence's exact value, the
// entry retired, and the entries after it applied.
func TestDrain_reused_retired_id_is_a_terminal_conflict(t *testing.T) {
	st, state := goStore(t)
	first := mustEnvelope(t, "ack", cases["ack"]...)
	publish(t, state, first)
	r := &recorder{}
	if err := Drain(t.Context(), st, state, r.apply); err != nil {
		t.Fatal(err)
	}
	retired, _ := meta(t, st, "inbox:"+first.ID)
	second := mustEnvelope(t, "ack", append(append([]string{}, cases["ack"]...), "--reject", "stale_generation")...)
	later := mustEnvelope(t, "ack", "--event", "event-2", "--ack-turn", "t", "--ack-proof", "p")
	if second.ID != first.ID || second.Digest == first.Digest {
		t.Fatal(second)
	}
	publish(t, state, second)
	publish(t, state, later)
	for range 2 {
		if err := Drain(t.Context(), st, state, r.apply); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.calls) != 2 || !strings.Contains(r.calls[1], "event-2") {
		t.Fatalf("%q", r.calls)
	}
	key := "inbox-conflict:" + first.ID + ":" + second.Digest[7:23]
	value, ok := meta(t, st, key)
	if want := `{"answer":{"detail":"committed inbox payload differs","error":"refused","reason":"inbox_conflict"},"exit":2,"payloadDigest":"` + second.Digest + `"}`; !ok || value != want {
		t.Fatalf("%s = %q", key, value)
	}
	if now, _ := meta(t, st, "inbox:"+first.ID); now != retired {
		t.Fatal("the retired marker changed")
	}
	if got := names(t, Directory(state)); !slices.Equal(got, []string{ReplayLock}) {
		t.Fatalf("%v", got)
	}
}

// Ownership refusals raised inside the handler and receipts refused because the host could not
// confirm the turn keep their entry, write no marker, and never stop the drain; a later drain
// applies the entry once.
func TestDrain_retained_failures_keep_the_entry_without_blocking(t *testing.T) {
	for name, failure := range map[string]error{
		"ownership": &ownership.Refused{Detail: "takeover record unreadable"},
		"host":      store.RefusedBecause("unassigned_turn", "the host could not confirm turn 'u': listing not exhausted", hostUnavailable{}),
	} {
		t.Run(name, func(t *testing.T) {
			st, state := goStore(t)
			first := mustEnvelope(t, "ack", cases["ack"]...)
			later := mustEnvelope(t, "ack", "--event", "event-2", "--ack-turn", "t", "--ack-proof", "p")
			publish(t, state, first)
			publish(t, state, later)
			r := &recorder{answer: func(call int, argv []string) (any, int, error) {
				if call == 1 {
					return nil, 0, failure
				}
				return contract.OrderedObject{}, 0, nil
			}}
			if err := Drain(t.Context(), st, state, r.apply); err != nil {
				t.Fatal(err)
			}
			if _, ok := meta(t, st, "inbox:"+first.ID); ok {
				t.Fatal("a retained failure wrote a marker")
			}
			if _, ok := meta(t, st, "inbox:"+later.ID); !ok {
				t.Fatal("the retained entry blocked the one after it")
			}
			if got := names(t, Directory(state)); !slices.Equal(got, []string{ReplayLock, first.ID}) {
				t.Fatalf("%v", got)
			}
			if got := metaPrefix(t, st, "test:applied:"); len(got) != 1 {
				t.Fatalf("the retained handler's writes survived: %v", got)
			}
			if err := Drain(t.Context(), st, state, r.apply); err != nil || len(r.calls) != 3 {
				t.Fatal(err, r.calls)
			}
			if _, ok := meta(t, st, "inbox:"+first.ID); !ok || len(names(t, Directory(state))) != 1 {
				t.Fatal("the retained entry was not applied later")
			}
		})
	}
}

// The transaction a handler opens inside the drain's composition revalidates admission, as
// store.py transaction() does before it joins (test_ownership_or_host_failure_during_replay_
// retains_the_entry, ownership). An ownership change that handler's own transaction sees, here
// takeover.json unreadable for that one transaction, is the retained ownership refusal: the
// handler's writes roll back, no marker, the entry kept, and the entry after it still applied.
func TestDrain_a_handler_transaction_revalidates_admission(t *testing.T) {
	st, state := goStore(t)
	first := mustEnvelope(t, "ack", cases["ack"]...)
	later := mustEnvelope(t, "ack", "--event", "event-2", "--ack-turn", "t", "--ack-proof", "p")
	publish(t, state, first)
	publish(t, state, later)
	mirror := filepath.Join(state, "takeover.json")
	original, err := os.ReadFile(mirror)
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	apply := func(ctx context.Context, st *store.Store, command string, argv []string) (any, int, error) {
		answer, code, err := r.apply(ctx, st, command, argv)
		if err != nil || len(r.calls) != 1 {
			return answer, code, err
		}
		if err = os.WriteFile(mirror, []byte("{"), 0600); err != nil {
			return nil, 0, err
		}
		err = st.Transaction(ctx, func(context.Context, *sql.Conn) error { return nil })
		if restore := os.WriteFile(mirror, original, 0600); restore != nil {
			return nil, 0, restore
		}
		if err != nil {
			return nil, 0, err
		}
		return answer, code, nil
	}
	if err := Drain(t.Context(), st, state, apply); err != nil {
		t.Fatal(err)
	}
	if _, ok := meta(t, st, "inbox:"+first.ID); ok {
		t.Fatal("a handler whose transaction admission refused committed its marker")
	}
	if _, ok := meta(t, st, "inbox:"+later.ID); !ok {
		t.Fatal("the retained entry blocked the one after it")
	}
	if got := names(t, Directory(state)); !slices.Equal(got, []string{ReplayLock, first.ID}) {
		t.Fatalf("%v", got)
	}
	if got := metaPrefix(t, st, "test:applied:"); len(got) != 1 {
		t.Fatalf("the refused handler's writes survived: %v", got)
	}
}

type hostUnavailable struct{}

func (hostUnavailable) Error() string               { return "listing not exhausted" }
func (hostUnavailable) PythonExceptionKind() string { return "HostUnavailable" }

// A stale replayer never retires a newer entry at the same name: while this drain applies an
// entry, another owner applies and retires it and a sender publishes different bytes under the
// same ID. The drain unlinks only the inode it read, so the newer entry survives, and the next
// drain records it as the reused-ID conflict.
func TestDrain_unlinks_only_the_inode_it_read(t *testing.T) {
	st, state := goStore(t)
	first := mustEnvelope(t, "ack", cases["ack"]...)
	newer := mustEnvelope(t, "ack", append(append([]string{}, cases["ack"]...), "--reject", "stale_generation")...)
	publish(t, state, first)
	// The old inode stays allocated under a name outside the grammar, so the newer entry's
	// inode cannot be a reused number that only looks like the one read.
	if err := os.Link(filepath.Join(Directory(state), first.ID), filepath.Join(Directory(state), ".held")); err != nil {
		t.Fatal(err)
	}
	r := &recorder{answer: func(call int, argv []string) (any, int, error) {
		if call == 1 {
			if err := os.Remove(filepath.Join(Directory(state), first.ID)); err != nil {
				return nil, 0, err
			}
			if err := Enqueue(state, newer); err != nil {
				return nil, 0, err
			}
		}
		return contract.OrderedObject{}, 0, nil
	}}
	if err := Drain(t.Context(), st, state, r.apply); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(Directory(state), first.ID)); err != nil || !bytes.Equal(raw, newer.Raw) {
		t.Fatalf("the newer entry was retired: %v", err)
	}
	if err := Drain(t.Context(), st, state, r.apply); err != nil || len(r.calls) != 1 {
		t.Fatal(err, r.calls)
	}
	if _, ok := meta(t, st, "inbox-conflict:"+first.ID+":"+newer.Digest[7:23]); !ok {
		t.Fatal("no conflict recorded")
	}
}

// Names outside the entry grammar are ignored and never retired; a grammar-valid symbolic link
// or non-regular file is refused by name and never followed; a grammar-valid regular file whose
// bytes do not validate fails the drain closed and is kept.
func TestDrain_classifies_inbox_names_as_the_fence_does(t *testing.T) {
	t.Run("ignored", func(t *testing.T) {
		st, state := goStore(t)
		directory := Directory(state)
		if err := os.MkdirAll(filepath.Join(directory, "sub dir"), 0700); err != nil {
			t.Fatal(err)
		}
		ignored := map[string]string{"ack.event-1~": "editor backup", "a b": "x", strings.Repeat("x", 201): "x", ".tmp-x": "x", "%zz": "x"}
		for name, raw := range ignored {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
		}
		entry := mustEnvelope(t, "ack", cases["ack"]...)
		publish(t, state, entry)
		r := &recorder{}
		if err := Drain(t.Context(), st, state, r.apply); err != nil || len(r.calls) != 1 {
			t.Fatal(err, r.calls)
		}
		for name, raw := range ignored {
			if got, err := os.ReadFile(filepath.Join(directory, name)); err != nil || string(got) != raw {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
	for _, kind := range []string{"symlink", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			st, state := goStore(t)
			directory := Directory(state)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(directory, "ack.event-9")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(filepath.Join(state, "relay.sqlite3"), entry)
			case "directory":
				err = os.Mkdir(entry, 0700)
			default:
				err = unix.Mkfifo(entry, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			reason := map[string]string{"symlink": "symbolic link"}[kind]
			if reason == "" {
				reason = "not a regular file"
			}
			err = Drain(t.Context(), st, state, (&recorder{}).apply)
			if err == nil || err.Error() != "ValueError: invalid takeover inbox entry: ack.event-9: "+reason {
				t.Fatalf("%v", err)
			}
			if _, err = os.Lstat(entry); err != nil {
				t.Fatal("the refused entry was removed")
			}
		})
	}
	// Accepted though no producer writes it: an empty append list passes the fence's typed check
	// and stays in the envelope it re-derives ([] is not the default None), so the entry is
	// applied - the handler passed no --artifact, which it reads alike - and retired.
	t.Run("empty-append-list", func(t *testing.T) {
		st, state := goStore(t)
		directory := Directory(state)
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		const id = "emit.edb680737833a16cc18b813934d667cf"
		raw := `{"arguments":{"artifact":[],"generation":1,"outcome":"failed","relationship":"relationship-1","turn_id":"turn-1","turn_thread":"child-1"},"command":"emit","inboxVersion":1,"operationId":"` + id + `","payloadDigest":"sha256:edb680737833a16cc18b813934d667cff1b1d2d0ad5336e2dc847e03f55b024b"}`
		if err := os.WriteFile(filepath.Join(directory, id), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		r := &recorder{}
		if err := Drain(t.Context(), st, state, r.apply); err != nil {
			t.Fatal(err)
		}
		if want := []string{"emit --relationship=relationship-1 --generation=1 --outcome=failed --turn-thread=child-1 --turn-id=turn-1"}; !slices.Equal(r.calls, want) {
			t.Fatalf("%q", r.calls)
		}
		if marker, ok := meta(t, st, "inbox:"+id); !ok || !strings.Contains(marker, `"exit":0`) {
			t.Fatalf("marker %q", marker)
		}
		if got := names(t, directory); !slices.Equal(got, []string{ReplayLock}) {
			t.Fatalf("the applied entry was not retired: %v", got)
		}
	})
	invalid := map[string]struct{ raw, detail string }{}
	good := string(golden(t, "ack"))
	invalid["version-float"] = struct{ raw, detail string }{strings.Replace(good, `"inboxVersion":1`, `"inboxVersion":1.0`, 1), "ValueError: invalid takeover inbox entry: ack.event-1"}
	invalid["version-bool"] = struct{ raw, detail string }{strings.Replace(good, `"inboxVersion":1`, `"inboxVersion":true`, 1), "ValueError: invalid takeover inbox entry: ack.event-1"}
	invalid["not-canonical"] = struct{ raw, detail string }{good + "\n", "ValueError: invalid takeover inbox entry: ack.event-1"}
	invalid["escaped"] = struct{ raw, detail string }{strings.Replace(good, `"event-1"`, `"event`+string(rune(92))+`u002d1"`, 1), "ValueError: invalid takeover inbox entry: ack.event-1"}
	invalid["not-json"] = struct{ raw, detail string }{"{", "JSONDecodeError: Expecting property name enclosed in double quotes: line 1 column 2 (char 1)"}
	invalid["empty"] = struct{ raw, detail string }{"", "JSONDecodeError: Expecting value: line 1 column 1 (char 0)"}
	invalid["not-an-object"] = struct{ raw, detail string }{"[]", "ValueError: invalid takeover inbox entry: ack.event-1"}
	invalid["not-utf8"] = struct{ raw, detail string }{"\xff", "UnicodeDecodeError: 'utf-8' codec can't decode byte 0xff in position 0: invalid start byte"}
	invalid["digest"] = struct{ raw, detail string }{strings.Replace(good, "sha256:c40a", "sha256:c40b", 1), "ValueError: inbox payload digest or identifier disagrees"}
	invalid["extra-argument"] = struct{ raw, detail string }{strings.Replace(good, `{"ack_proof"`, `{"a":"b","ack_proof"`, 1), "ValueError: invalid inbox arguments"}
	invalid["missing-argument"] = struct{ raw, detail string }{strings.Replace(good, `"ack_turn":"parent-turn",`, "", 1), "ValueError: missing inbox argument: ack_turn"}
	invalid["mistyped-argument"] = struct{ raw, detail string }{strings.Replace(good, `"proof-1"`, `1`, 1), "ValueError: invalid inbox argument: ack_proof"}
	invalid["explicit-default"] = struct{ raw, detail string }{strings.Replace(string(golden(t, "emit")), `{"generation"`, `{"attempt":1,"generation"`, 1), "ValueError: inbox payload digest or identifier disagrees"}
	for name, tc := range invalid {
		t.Run(name, func(t *testing.T) {
			st, state := goStore(t)
			directory := Directory(state)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			id := "ack.event-1"
			if name == "explicit-default" {
				id = mustEnvelope(t, "emit", cases["emit"]...).ID
			}
			path := filepath.Join(directory, id)
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			r := &recorder{}
			if err := Drain(t.Context(), st, state, r.apply); err == nil || err.Error() != tc.detail || len(r.calls) != 0 {
				t.Fatalf("%v %v", err, r.calls)
			}
			if raw, err := os.ReadFile(path); err != nil || string(raw) != tc.raw {
				t.Fatal("the invalid entry was not kept as it was")
			}
		})
	}
}

// The replay lock bounds a second replayer's wait (cutover.md Lock order): when it expires the
// drain is a retryable host error that changed nothing.
func TestDrain_a_held_replay_lock_bounds_the_wait(t *testing.T) {
	st, state := goStore(t)
	entry := mustEnvelope(t, "ack", cases["ack"]...)
	publish(t, state, entry)
	holder, err := os.OpenFile(filepath.Join(Directory(state), ReplayLock), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err = unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func(original time.Duration) { ownership.LockWait = original }(ownership.LockWait)
	ownership.LockWait = 300 * time.Millisecond
	r := &recorder{}
	err = Drain(t.Context(), st, state, r.apply)
	if err == nil || err.Error() != "LockWaitExpired: the takeover inbox replay lock was not acquired within 0.3s; retry" || len(r.calls) != 0 {
		t.Fatalf("%v %v", err, r.calls)
	}
	if !slices.Contains(names(t, Directory(state)), entry.ID) {
		t.Fatal("the entry was touched")
	}
	info, err := os.Stat(filepath.Join(Directory(state), ReplayLock))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}

// No inbox is nothing to replay, and a drain creates nothing.
func TestDrain_without_an_inbox_creates_nothing(t *testing.T) {
	st, state := goStore(t)
	if err := Drain(t.Context(), st, state, (&recorder{}).apply); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Directory(state)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// An inbox this user cannot write fails the drain on its replay lock, before any entry is read,
// with Python's PermissionError text; nothing is applied and nothing is created.
func TestDrain_an_unwritable_inbox_fails_on_the_replay_lock(t *testing.T) {
	st, state := goStore(t)
	entry := mustEnvelope(t, "ack", cases["ack"]...)
	publish(t, state, entry)
	directory := Directory(state)
	if err := os.Chmod(directory, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(directory, 0700) }()
	r := &recorder{}
	err := Drain(t.Context(), st, state, r.apply)
	want := "PermissionError: [Errno 13] Permission denied: '" + filepath.Join(directory, ReplayLock) + "'"
	if err == nil || err.Error() != want || len(r.calls) != 0 {
		t.Fatalf("%v %v", err, r.calls)
	}
}

// Check stops where Drain stops on the replay lock (backlog line 40, review): the lock opened as
// lockReplay opens it, never following a link and for writing, or created where there is none.
// For each lock the drain cannot open, Check fails with the drain's own error before any entry is
// read, and it neither creates nor takes the lock; over a lock the drain opens, Check passes and
// the drain applies the entry. It stops too where the drain stops after that entry's commit: an
// inbox this user may not remove a name from fails the drain's unlink, and Check fails with the
// same error, while an inbox with no entry to retire refuses neither.
func TestCheck_stops_where_the_drain_stops_on_its_replay_lock(t *testing.T) {
	unwritable := func(t *testing.T, directory, lock string) {
		if err := os.WriteFile(lock, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0500); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, directory, lock string)
		refused bool
		applied int  // the entries the drain applies before it stops
		empty   bool // the inbox holds no entry
	}{
		{"a symbolic link", func(t *testing.T, directory, lock string) {
			if err := os.Symlink("elsewhere", lock); err != nil {
				t.Fatal(err)
			}
		}, true, 0, false},
		{"a symbolic link to a lock file", func(t *testing.T, directory, lock string) {
			if err := os.WriteFile(filepath.Join(directory, "saved~"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("saved~", lock); err != nil {
				t.Fatal(err)
			}
		}, true, 0, false},
		{"a directory", func(t *testing.T, directory, lock string) {
			if err := os.Mkdir(lock, 0700); err != nil {
				t.Fatal(err)
			}
		}, true, 0, false},
		{"a lock this user cannot write", func(t *testing.T, directory, lock string) {
			if err := os.WriteFile(lock, nil, 0400); err != nil {
				t.Fatal(err)
			}
		}, true, 0, false},
		{"no lock in an inbox this user cannot write", func(t *testing.T, directory, lock string) {
			if err := os.Chmod(directory, 0500); err != nil {
				t.Fatal(err)
			}
		}, true, 0, false},
		// The lock opens, the entry is applied and its marker committed, and its unlink fails.
		{"a lock file in an inbox this user cannot write", unwritable, true, 1, false},
		{"a lock file in an empty inbox this user cannot write", unwritable, false, 0, true},
		{"no lock yet", func(t *testing.T, directory, lock string) {}, false, 1, false},
		{"a lock file", func(t *testing.T, directory, lock string) {
			if err := os.WriteFile(lock, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}, false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root is refused no permission")
			}
			st, state := goStore(t)
			entry := mustEnvelope(t, "ack", cases["ack"]...)
			directory := Directory(state)
			if tc.empty {
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				publish(t, state, entry)
			}
			lock := filepath.Join(directory, ReplayLock)
			if err := os.Remove(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			tc.prepare(t, directory, lock)
			defer func() { _ = os.Chmod(directory, 0700) }()
			before := names(t, directory)
			checked := Check(state)
			if after := names(t, directory); !slices.Equal(after, before) {
				t.Fatalf("Check changed the inbox: %v, then %v", before, after)
			}
			r := &recorder{}
			drained := Drain(t.Context(), st, state, r.apply)
			if len(r.calls) != tc.applied {
				t.Fatalf("the drain applied %v (check %v, drain %v)", r.calls, checked, drained)
			}
			if !tc.refused {
				if checked != nil || drained != nil {
					t.Fatalf("check %v, drain %v", checked, drained)
				}
				return
			}
			if drained == nil {
				t.Fatalf("the drain passed where Check answered %v", checked)
			}
			if checked == nil || checked.Error() != drained.Error() {
				t.Fatalf("Check answered %v where the drain stopped with %v", checked, drained)
			}
		})
	}
}
