package cli_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1194 (R9): `crw relay journal-tail` reads the journal table in seq order after a cursor. The
// tests build a Go-owned store, write journal rows through a writer of their own and read them
// back through the command.

// tailStore is a Go-owned store under a temporary home and a writer on it, for the rows a test
// appends while the command reads.
type tailStore struct {
	t     *testing.T
	state string
	db    *store.Store
}

func newTailStore(t *testing.T) *tailStore {
	t.Helper()
	home := tempHome(t)
	state := ownerOnlyState(t, home, "tail")
	path := filepath.Join(state, "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	db, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &tailStore{t: t, state: state, db: db}
}

// add appends one journal row and returns its seq.
func (s *tailStore) add(kind, subject, detail string) int64 {
	s.t.Helper()
	var subjectArg, detailArg any
	if subject != "" {
		subjectArg = subject
	}
	if detail != "" {
		detailArg = detail
	}
	result, err := s.db.DB.ExecContext(context.Background(), "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", "2026-10-11T00:00:00.000000+00:00", kind, subjectArg, detailArg)
	if err != nil {
		s.t.Fatal(err)
	}
	seq, err := result.LastInsertId()
	if err != nil {
		s.t.Fatal(err)
	}
	return seq
}

// tail runs journal-tail with args and decodes its answer.
func (s *tailStore) tail(args ...string) (int, map[string]any) {
	s.t.Helper()
	got := goCLI(s.t, append([]string{"--state", s.state, "journal-tail"}, args...)...)
	return got.code, object(s.t, got.stdout)
}

// seqs are the seq of each entry of an answer, in the answer's order.
func seqs(t *testing.T, answer map[string]any) []int64 {
	t.Helper()
	out := []int64{}
	entries, _ := answer["entries"].([]any)
	for _, e := range entries {
		out = append(out, int64(e.(map[string]any)["seq"].(float64)))
	}
	return out
}

func TestJournalTailAfterCursor(t *testing.T) {
	s := newTailStore(t)
	var added []int64
	for i := 0; i < 5; i++ {
		added = append(added, s.add("tail_probe", "subject-"+strconv.Itoa(i), `{"n":`+strconv.Itoa(i)+`}`))
	}
	code, all := s.tail("--after", "0", "--limit", "1000")
	if code != 0 || all["schema"] != "crw-journal-tail/1" || all["ok"] != true {
		t.Fatalf("the full read: exit %d %v", code, all)
	}
	got := seqs(t, all)
	if len(got) < 5 || !reflect.DeepEqual(got[len(got)-5:], added) {
		t.Fatalf("the entries are not in seq order: %v, wanted the tail %v", got, added)
	}
	// the cursor is the last seq returned, and reading after it returns nothing more
	if all["cursor"] != float64(got[len(got)-1]) || all["after"] != float64(0) || all["more"] != false {
		t.Fatalf("cursor/after/more of the full read: %v", all)
	}
	code, rest := s.tail("--after", strconv.FormatInt(added[1], 10))
	if code != 0 || !reflect.DeepEqual(seqs(t, rest), added[2:]) {
		t.Fatalf("after the second probe: exit %d %v", code, rest)
	}
	entry := rest["entries"].([]any)[0].(map[string]any)
	if entry["kind"] != "tail_probe" || entry["subject"] != "subject-2" || entry["detail"] != `{"n":2}` || entry["at"] != "2026-10-11T00:00:00.000000+00:00" {
		t.Fatalf("an entry reads as the row: %v", entry)
	}
	// a read past the head returns no entry and hands the cursor back
	code, none := s.tail("--after", strconv.FormatInt(added[4], 10))
	if code != 0 || len(seqs(t, none)) != 0 || none["cursor"] != float64(added[4]) || none["more"] != false {
		t.Fatalf("a read at the head: exit %d %v", code, none)
	}
	// a page that stops at --limit says more follows, and its cursor continues it
	code, page := s.tail("--after", strconv.FormatInt(added[0], 10), "--limit", "2")
	if code != 0 || !reflect.DeepEqual(seqs(t, page), added[1:3]) || page["more"] != true || page["cursor"] != float64(added[2]) {
		t.Fatalf("a limited page: exit %d %v", code, page)
	}
	// a NULL subject and detail read as null
	null := s.add("tail_null", "", "")
	_, one := s.tail("--after", strconv.FormatInt(null-1, 10))
	if e := one["entries"].([]any)[0].(map[string]any); e["subject"] != nil || e["detail"] != nil {
		t.Fatalf("NULL columns: %v", e)
	}
	// the usage refusals
	for _, args := range [][]string{{"--after", "-1"}, {"--after", "0", "--limit", "0"}, {"--after", "0", "--limit", "1001"}, {"--after", "0", "--wait", "-1"}, {"--after", "0", "--wait", "3601"}} {
		if got := goCLI(t, append([]string{"--state", s.state, "journal-tail"}, args...)...); got.code != 4 {
			t.Errorf("%v: exit %d, wanted the usage error 4: %s", args, got.code, got.stdout)
		}
	}
}

func TestJournalTailKindsFilter(t *testing.T) {
	s := newTailStore(t)
	a := s.add("kind_a", "", "")
	b := s.add("kind_b", "", "")
	c := s.add("kind_c", "", "")
	a2 := s.add("kind_a", "", "")
	code, only := s.tail("--after", strconv.FormatInt(a-1, 10), "--kinds", "kind_a")
	if code != 0 || !reflect.DeepEqual(seqs(t, only), []int64{a, a2}) || !reflect.DeepEqual(only["kinds"], []any{"kind_a"}) {
		t.Fatalf("kind_a only: exit %d %v", code, only)
	}
	_, two := s.tail("--after", strconv.FormatInt(a-1, 10), "--kinds", "kind_c, kind_b")
	if !reflect.DeepEqual(seqs(t, two), []int64{b, c}) {
		t.Fatalf("two kinds come back in seq order: %v", two)
	}
	// a cursor within a filtered read is the last entry returned, whatever the rows skipped
	_, page := s.tail("--after", strconv.FormatInt(a-1, 10), "--kinds", "kind_a", "--limit", "1")
	if page["cursor"] != float64(a) || page["more"] != true {
		t.Fatalf("a filtered page: %v", page)
	}
	_, nothing := s.tail("--after", "0", "--kinds", "kind_never")
	if len(seqs(t, nothing)) != 0 || nothing["cursor"] != float64(0) {
		t.Fatalf("a kind nothing journals: %v", nothing)
	}
	_, unfiltered := s.tail("--after", strconv.FormatInt(a-1, 10))
	if unfiltered["kinds"] != nil {
		t.Fatalf("no --kinds reads as null kinds: %v", unfiltered["kinds"])
	}
	for _, kinds := range []string{"", "a,,b", ",", "kind_a,"} {
		if got := goCLI(t, "--state", s.state, "journal-tail", "--after", "0", "--kinds", kinds); got.code != 4 {
			t.Errorf("--kinds %q: exit %d, wanted the usage error 4: %s", kinds, got.code, got.stdout)
		}
	}
}

func TestJournalTailWaitReturnsOnNewRow(t *testing.T) {
	s := newTailStore(t)
	head := s.add("tail_head", "", "")
	defer cli.SetJournalTailPoll(20 * time.Millisecond)()

	// without --wait, nothing new returns at once
	if code, none := s.tail("--after", strconv.FormatInt(head, 10)); code != 0 || len(seqs(t, none)) != 0 {
		t.Fatalf("no wait: exit %d %v", code, none)
	}
	// with --wait, a row written meanwhile ends the wait
	var wg sync.WaitGroup
	var added int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(150 * time.Millisecond)
		added = s.add("tail_late", "", "")
	}()
	started := time.Now()
	code, got := s.tail("--after", strconv.FormatInt(head, 10), "--wait", "30")
	wg.Wait()
	if code != 0 || !reflect.DeepEqual(seqs(t, got), []int64{added}) || got["cursor"] != float64(added) {
		t.Fatalf("waited for a new row: exit %d %v", code, got)
	}
	if took := time.Since(started); took >= 29*time.Second {
		t.Fatalf("the wait ran to its bound although the row arrived: %v", took)
	}
	// a row of another kind does not end a filtered wait; the wait runs to its bound and answers empty
	started = time.Now()
	other := make(chan struct{})
	go func() { defer close(other); time.Sleep(50 * time.Millisecond); s.add("tail_other", "", "") }()
	code, empty := s.tail("--after", strconv.FormatInt(added, 10), "--kinds", "tail_wanted", "--wait", "1")
	<-other
	if code != 0 || len(seqs(t, empty)) != 0 || time.Since(started) < 900*time.Millisecond {
		t.Fatalf("a filtered wait with no matching row: exit %d %v after %v", code, empty, time.Since(started))
	}
}

func TestJournalTailOpensReadOnly(t *testing.T) {
	s := newTailStore(t)
	s.add("tail_probe", "", "")
	path := filepath.Join(s.state, "relay.sqlite3")
	opened, err := cli.OpenJournalTailStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if !opened.ReadOnly() {
		t.Fatal("journal-tail's store is not the read-only store")
	}
	if _, err := opened.DB.ExecContext(context.Background(), "INSERT INTO journal (at, kind) VALUES ('x','y')"); err == nil {
		t.Fatal("a write through the journal-tail store succeeded")
	}
	// a state with no store answers store_absent and creates nothing
	empty := ownerOnlyState(t, tempHome(t), "none")
	got := goCLI(t, "--state", empty, "journal-tail", "--after", "0")
	if got.code != 2 || object(t, got.stdout)["reason"] != "store_absent" {
		t.Fatalf("an absent store: exit %d %s", got.code, got.stdout)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Fatalf("journal-tail created %d files in a state with no store", len(entries))
	}
	// the same read of a halted store still answers: no writable store is opened
	if err := os.WriteFile(filepath.Join(s.state, "corruption.json"), []byte(`{"detectedAt":"2026-10-06T13:44:00.000000+00:00","pid":1,"command":"crw relay","code":522,"message":"disk I/O error","site":"observation","sequence":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, halted := s.tail("--after", "0"); code != 0 || len(seqs(t, halted)) == 0 {
		t.Fatalf("a halted store: exit %d %v", code, halted)
	}
}

func TestJournalTailNoLongReadTx(t *testing.T) {
	s := newTailStore(t)
	head := s.add("tail_head", "", "")
	defer cli.SetJournalTailPoll(20 * time.Millisecond)()
	// the command waits for a kind that is written last, so the rows written meanwhile do not end its
	// wait and its bound is far past anything a loaded host needs
	done := make(chan map[string]any, 1)
	go func() {
		got := goCLI(t, "--state", s.state, "journal-tail", "--after", strconv.FormatInt(head, 10), "--kinds", "tail_wanted", "--wait", "120")
		var answer map[string]any
		_ = json.Unmarshal([]byte(got.stdout), &answer)
		done <- answer
	}()
	// while the command waits, a checkpoint that restarts the log is never blocked by a reader's
	// snapshot: a reader held across the sleeps would make it busy
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 5; i++ {
		s.add("tail_noise", "", "")
		var busy, logFrames, checkpointed int
		if err := s.db.DB.QueryRowContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		if busy != 0 {
			t.Fatalf("checkpoint %d was blocked (busy=%d): the waiting command holds a read transaction", i, busy)
		}
		time.Sleep(30 * time.Millisecond)
	}
	wanted := s.add("tail_wanted", "", "")
	if answer := <-done; answer == nil || !reflect.DeepEqual(seqs(t, answer), []int64{wanted}) {
		t.Fatalf("the wait did not return the row written while it ran: %v", answer)
	}
}

// journal-tail judges a state with no readable store as every read-only command does (the fence's
// classification of D, its ownership mirror and its write gate): a D link naming no file beside
// nothing else is an absent store, and a gate or a mirror beside no database is the partial store the
// fence refuses. Nothing is created or changed.
func TestJournalTailClassifiesAbsentAndPartialStores(t *testing.T) {
	home := tempHome(t)
	for _, c := range []struct {
		name     string
		files    []string
		dangling bool
		fifoGate bool
		reason   string
		detail   string
	}{
		{name: "dangling link", dangling: true, reason: "store_absent"},
		{name: "gate only", files: []string{"write-gate.lock"}, reason: "store_owned_by_other", detail: "partial store: write-gate.lock without a database"},
		{name: "gate beside a dangling link", files: []string{"write-gate.lock"}, dangling: true, reason: "store_owned_by_other", detail: "partial store: write-gate.lock without a database"},
		{name: "fifo gate", fifoGate: true, reason: "store_owned_by_other", detail: "partial store: write-gate.lock without a database"},
		{name: "mirror only", files: []string{"takeover.json"}, reason: "store_owned_by_other"},
	} {
		state := ownerOnlyState(t, home, "tail-"+strings.ReplaceAll(c.name, " ", "-"))
		for _, name := range c.files {
			if err := os.WriteFile(filepath.Join(state, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if c.fifoGate {
			if err := syscall.Mkfifo(filepath.Join(state, "write-gate.lock"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if c.dangling {
			if err := os.Symlink(filepath.Join(state, "nowhere.sqlite3"), filepath.Join(state, "relay.sqlite3")); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := os.ReadDir(state)
		got := goCLI(t, "--state", state, "journal-tail", "--after", "0")
		answer := object(t, got.stdout)
		if got.code != 2 || answer["reason"] != c.reason || c.detail != "" && answer["detail"] != c.detail {
			t.Errorf("%s: exit %d %s, wanted the refusal %s", c.name, got.code, got.stdout, c.reason)
		}
		// the fence reads every other read-only command's answer for this state alike
		status := goCLI(t, "--state", state, "status")
		if status.code != got.code || object(t, status.stdout)["reason"] != answer["reason"] {
			t.Errorf("%s: status answers exit %d %s, journal-tail exit %d %s", c.name, status.code, status.stdout, got.code, got.stdout)
		}
		if after, _ := os.ReadDir(state); len(after) != len(before) {
			t.Errorf("%s: journal-tail changed the state directory: %d names before, %d after", c.name, len(before), len(after))
		}
	}
}
