package cli_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1486/S4 (finding 9). supervisor-list reads one recipient's queue of supervisor messages
// through the store API, with counts, so a backlog is read without opening the store by hand.

type listStore struct {
	t     *testing.T
	state string
	path  string
}

func newListStore(t *testing.T) *listStore {
	t.Helper()
	state := ownerOnlyState(t, tempHome(t), "supervisor-list")
	path := filepath.Join(state, "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	return &listStore{t: t, state: state, path: path}
}

// run runs one relay command line against the store and returns its exit code and its answer.
func (s *listStore) run(argv ...string) (int, map[string]any) {
	s.t.Helper()
	got := goCLI(s.t, append([]string{"--state", s.state}, argv...)...)
	if got.stdout == "" {
		return got.code, nil
	}
	return got.code, object(s.t, got.stdout)
}

func (s *listStore) must(argv ...string) map[string]any {
	s.t.Helper()
	code, answer := s.run(argv...)
	if code != 0 {
		s.t.Fatalf("%v: exit %d %v", argv, code, answer)
	}
	return answer
}

func (s *listStore) open() *store.Store {
	s.t.Helper()
	db, err := store.Open(context.Background(), s.path, "")
	if err != nil {
		s.t.Fatal(err)
	}
	return db
}

// seed stages one message to recipient as the channel's staging writes it, then sets its state.
func (s *listStore) seed(recipient, id, obligationKind, kind, project, state, stagedAt string) {
	s.t.Helper()
	db := s.open()
	defer db.Close()
	ctx := context.Background()
	if _, err := db.StageSupervisorMessage(ctx, store.SupervisorMessagesRow{MessageID: id, ObligationID: "obl-" + id, ObligationKind: obligationKind,
		RelationshipID: "rel-" + project, ProjectKey: sql.NullString{String: project, Valid: true}, Purpose: "report", Kind: kind,
		SenderTaskID: "01child", RecipientTaskID: recipient, Subject: "subject-" + id, Packet: "{}", State: "queued", StagedAt: stagedAt, UpdatedAt: stagedAt}); err != nil {
		s.t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "UPDATE supervisor_messages SET state=? WHERE message_id=?", state, id); err != nil {
		s.t.Fatal(err)
	}
}

// digest is the store's bytes: the database and its write-ahead log, when there is one.
func (s *listStore) digest() [32]byte {
	s.t.Helper()
	var all []byte
	for _, path := range []string{s.path, s.path + "-wal"} {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			s.t.Fatal(err)
		}
		all = append(all, data...)
	}
	return sha256.Sum256(all)
}

// TestSupervisorListCountsAQueue: three completions and one blocked notice staged to R come back as
// four rows in staging order with their counts; another recipient's row and a sent row are not in
// the default (unsent) list; the flags parse through argparse; the store is byte-unchanged.
func TestSupervisorListCountsAQueue(t *testing.T) {
	s := newListStore(t)
	s.seed("01R", "c1", "completion", "notification", "P1", "queued", "2026-10-13T12:50:01.000000+00:00")
	s.seed("01R", "c2", "completion", "notification", "P2", "deferred_busy", "2026-10-13T12:50:02.000000+00:00")
	s.seed("01R", "c3", "completion", "notification", "P1", "queued", "2026-10-13T12:50:03.000000+00:00")
	s.seed("01R", "b1", "blocked", "notification", "P2", "queued", "2026-10-13T12:50:04.000000+00:00")
	s.seed("01R", "sent", "completion", "notification", "P1", "dispatched", "2026-10-13T12:50:00.000000+00:00")
	s.seed("01OTHER", "o1", "completion", "notification", "P1", "queued", "2026-10-13T12:50:00.000000+00:00")
	before := s.digest()
	answer := s.must("supervisor-list", "--recipient", "01R")
	if s.digest() != before {
		t.Fatal("supervisor-list changed the store")
	}
	if answer["schema"] != "supervisor-list/1" || answer["recipient"] != "01R" || answer["state"] != "unsent" {
		t.Fatalf("answer %v", answer)
	}
	rows, _ := answer["rows"].([]any)
	var ids []string
	for _, row := range rows {
		one := row.(map[string]any)
		ids = append(ids, one["messageId"].(string))
		for _, key := range []string{"relationshipId", "projectKey", "kind", "obligationKind", "state", "holdReason", "stagedAt", "nextEligibleAt", "claimable"} {
			if _, ok := one[key]; !ok {
				t.Errorf("row %v has no %s", one, key)
			}
		}
	}
	if want := []string{"c1", "c2", "c3", "b1"}; !slices.Equal(ids, want) {
		t.Fatalf("rows %v, want %v", ids, want)
	}
	counts, _ := answer["counts"].(map[string]any)
	wantCounts := `{"byHold":{"none":4},"byObligationKind":{"blocked":1,"completion":3},"byProject":{"P1":2,"P2":2},"byState":{"deferred_busy":1,"queued":3},"total":4}`
	if got, _ := json.Marshal(counts); string(got) != wantCounts {
		t.Fatalf("counts %s, want %s", got, wantCounts)
	}
	// The filters: one project, every state, one hold.
	if got := s.must("supervisor-list", "--recipient", "01R", "--project", "P2"); len(got["rows"].([]any)) != 2 {
		t.Fatalf("--project P2 listed %v", got["rows"])
	}
	if got := s.must("supervisor-list", "--recipient", "01R", "--state", "all"); len(got["rows"].([]any)) != 5 {
		t.Fatalf("--state all listed %v", got["rows"])
	}
	if got := s.must("supervisor-list", "--recipient", "01R", "--hold", store.SupervisorHoldSuperseded); len(got["rows"].([]any)) != 0 {
		t.Fatalf("--hold %s listed %v", store.SupervisorHoldSuperseded, got["rows"])
	}
	if got := s.must("supervisor-list", "--recipient", "01R", "--hold", "none"); len(got["rows"].([]any)) != 4 {
		t.Fatalf("--hold none listed %v", got["rows"])
	}
	// argparse: --recipient is required and --state is one of its two choices.
	for _, argv := range [][]string{{"supervisor-list"}, {"supervisor-list", "--recipient", "01R", "--state", "settled"}} {
		if code, _ := s.run(argv...); code != 2 {
			t.Errorf("%v: exit %d, want the parser's refusal", argv, code)
		}
	}
	if s.digest() != before {
		t.Fatal("supervisor-list changed the store")
	}
}

// TestSupervisorListStaysOutOfDoctorLists: supervisor-list is a registered read-only relay command
// and appears in neither of the lists doctor reports (TestDoctorCommandListsUnchanged keeps the lists
// themselves). The bulk hold it once came with is not a relay command (it moved to CRW-1500).
func TestSupervisorListStaysOutOfDoctorLists(t *testing.T) {
	state := ownerOnlyState(t, tempHome(t), "doctor")
	got := goCLI(t, "--state", state, "doctor")
	if got.code != 0 {
		t.Fatalf("doctor: exit %d %s", got.code, got.stdout)
	}
	reach, _ := object(t, got.stdout)["actorReachability"].(map[string]any)
	if _, ok := dispatch.Lookup("supervisor-acknowledge"); ok {
		t.Error("supervisor-acknowledge is a registered relay command")
	}
	for _, name := range []string{"supervisor-list"} {
		command, ok := dispatch.Lookup(name)
		if !ok {
			t.Fatalf("%s is not a registered relay command", name)
		}
		if !command.ReadOnly {
			t.Errorf("%s read-only %v", name, command.ReadOnly)
		}
		for _, key := range []string{"offlineCommands", "hostRequiredCommands"} {
			items, _ := reach[key].([]any)
			for _, item := range items {
				if item == name {
					t.Errorf("%s is in doctor's %s", name, key)
				}
			}
		}
	}
}
