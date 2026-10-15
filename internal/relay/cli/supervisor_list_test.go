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
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1486/S4 (finding 9). supervisor-list reads one recipient's queue of supervisor messages
// through the store API, with counts, so a backlog is read without opening the store by hand.
// CRW-1500 (U8). supervisor-acknowledge parks a recipient's informational notifications (or named
// messages) under the hold rule's acknowledge entry: the existing superseded_by_report hold with one
// supervisor_message_superseded journal row naming supervisor-acknowledge, so a backlog of old
// notices stops opening one turn each and the daemon's staging passes leave the park parked; a
// decision, a fault notice and a message an attempt may have sent are never parked.

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

// seed stages one message to recipient as the channel's staging writes it, then sets its state. An
// empty project stages a message with no project (a NULL project_key).
func (s *listStore) seed(recipient, id, obligationKind, kind, project, state, stagedAt string) {
	s.t.Helper()
	db := s.open()
	defer db.Close()
	ctx := context.Background()
	if _, err := db.StageSupervisorMessage(ctx, store.SupervisorMessagesRow{MessageID: id, ObligationID: "obl-" + id, ObligationKind: obligationKind,
		RelationshipID: "rel-" + project, ProjectKey: sql.NullString{String: project, Valid: project != ""}, Purpose: "report", Kind: kind,
		SenderTaskID: "01child", RecipientTaskID: recipient, Subject: "subject-" + id, Packet: "{}", State: "queued", StagedAt: stagedAt, UpdatedAt: stagedAt}); err != nil {
		s.t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, "UPDATE supervisor_messages SET state=? WHERE message_id=?", state, id); err != nil {
		s.t.Fatal(err)
	}
}

func (s *listStore) message(id string) store.SupervisorMessagesRow {
	s.t.Helper()
	db := s.open()
	defer db.Close()
	row, err := db.SupervisorMessage(context.Background(), id)
	if err != nil {
		s.t.Fatalf("message %s: %v", id, err)
	}
	return row
}

func (s *listStore) journal(kind, subject string) []store.JournalEntry {
	s.t.Helper()
	db := s.open()
	defer db.Close()
	entries, err := db.Journal(context.Background(), kind, subject)
	if err != nil {
		s.t.Fatal(err)
	}
	return entries
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

func stringsOf(t *testing.T, value any) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%v is not a list", value)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.(string))
	}
	return out
}

// skipped is the acknowledgement's skipped rows as messageId -> reason.
func skipped(t *testing.T, answer map[string]any) map[string]string {
	t.Helper()
	items, ok := answer["skipped"].([]any)
	if !ok {
		t.Fatalf("answer %v has no skipped list", answer)
	}
	out := map[string]string{}
	for _, item := range items {
		one := item.(map[string]any)
		out[one["messageId"].(string)] = one["reason"].(string)
	}
	return out
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

// TestSupervisorListStaysOutOfDoctorLists: supervisor-list (read-only) and supervisor-acknowledge
// (a write) are registered relay commands, and neither appears in the lists doctor reports
// (TestDoctorCommandListsUnchanged keeps the lists themselves).
func TestSupervisorListStaysOutOfDoctorLists(t *testing.T) {
	state := ownerOnlyState(t, tempHome(t), "doctor")
	got := goCLI(t, "--state", state, "doctor")
	if got.code != 0 {
		t.Fatalf("doctor: exit %d %s", got.code, got.stdout)
	}
	reach, _ := object(t, got.stdout)["actorReachability"].(map[string]any)
	for _, name := range []string{"supervisor-list", "supervisor-acknowledge"} {
		command, ok := dispatch.Lookup(name)
		if !ok {
			t.Fatalf("%s is not a registered relay command", name)
		}
		if command.ReadOnly != (name == "supervisor-list") {
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

// TestSupervisorListProjectNoneIsTheNoneCount (CRW-1500): the byProject none count and the
// --project none list are the same set. One message with no project counts as none and is the one
// row --project none lists; with a message of a project literally named none added, the count and
// the list agree at two, and a real project's message is in neither.
func TestSupervisorListProjectNoneIsTheNoneCount(t *testing.T) {
	s := newListStore(t)
	s.seed("01R", "n1", "completion", "notification", "", "queued", "2026-10-13T12:50:01.000000+00:00")
	s.seed("01R", "p1", "completion", "notification", "P1", "queued", "2026-10-13T12:50:02.000000+00:00")
	noneCount := func(answer map[string]any) any {
		counts, _ := answer["counts"].(map[string]any)
		byProject, _ := counts["byProject"].(map[string]any)
		return byProject[store.SupervisorListProjectNone]
	}
	listedIDs := func(answer map[string]any) []string {
		var ids []string
		for _, row := range answer["rows"].([]any) {
			ids = append(ids, row.(map[string]any)["messageId"].(string))
		}
		return ids
	}
	before := s.digest()
	if got := noneCount(s.must("supervisor-list", "--recipient", "01R")); got != float64(1) {
		t.Fatalf("byProject.none %v, want 1", got)
	}
	listed := s.must("supervisor-list", "--recipient", "01R", "--project", "none")
	if ids := listedIDs(listed); !slices.Equal(ids, []string{"n1"}) {
		t.Fatalf("--project none listed %v, want [n1]", ids)
	}
	if listed["project"] != "none" || listed["rows"].([]any)[0].(map[string]any)["projectKey"] != nil {
		t.Fatalf("--project none answer %v", listed)
	}
	if s.digest() != before {
		t.Fatal("supervisor-list changed the store")
	}
	s.seed("01R", "n2", "completion", "notification", "none", "queued", "2026-10-13T12:50:03.000000+00:00")
	if got := noneCount(s.must("supervisor-list", "--recipient", "01R")); got != float64(2) {
		t.Fatalf("byProject.none %v with the none project, want 2", got)
	}
	listed = s.must("supervisor-list", "--recipient", "01R", "--project", "none")
	if ids := listedIDs(listed); !slices.Equal(ids, []string{"n1", "n2"}) {
		t.Fatalf("--project none listed %v with the none project, want [n1 n2]", ids)
	}
	if got := noneCount(listed); got != float64(len(listed["rows"].([]any))) {
		t.Fatalf("--project none counts none %v and lists %d", got, len(listed["rows"].([]any)))
	}
}

// listedRecipientSends is a host that accepts every send and keeps what it was sent.
type listedRecipientSends struct{ sends []string }

func (h *listedRecipientSends) ReadThread(context.Context, string) (delivery.ThreadFacts, error) {
	yes := true
	return delivery.ThreadFacts{RuntimeStatus: "idle", CanAcceptInput: &yes}, nil
}
func (h *listedRecipientSends) IsArchived(context.Context, string, any) (*bool, error) {
	no := false
	return &no, nil
}
func (h *listedRecipientSends) ReadGoalStatus(context.Context, string) (any, error) { return "", nil }
func (h *listedRecipientSends) ListTurnIDs(context.Context, string, int) ([]any, error) {
	return []any{"turn-1"}, nil
}
func (h *listedRecipientSends) ReadTurn(_ context.Context, _ string, id string) (*delivery.TurnInfo, error) {
	start := 1_700_000_001.0
	return &delivery.TurnInfo{TurnID: id, StartedAt: &start}, nil
}
func (h *listedRecipientSends) SendMessage(_ context.Context, id, _, message string, _ *delivery.TaskSettings) (delivery.Obj, error) {
	h.sends = append(h.sends, message)
	return delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "requestId", Value: id}, {Key: "turnId", Value: "turn-1"}}, nil
}
func (h *listedRecipientSends) GetOperation(context.Context, string) (delivery.Obj, error) {
	return nil, nil
}
func (h *listedRecipientSends) FindToken(_ context.Context, _ string, token string, _ int, _ bool) (delivery.TokenScan, error) {
	for _, text := range h.sends {
		if strings.Contains(text, token) {
			return delivery.TokenScan{Found: true, TurnID: "turn-1", Exhausted: true, Scanned: 1}, nil
		}
	}
	return delivery.TokenScan{Exhausted: true, Scanned: len(h.sends)}, nil
}
func (h *listedRecipientSends) FindDispatchedTurn(context.Context, string, string, float64) (delivery.TurnPresence, error) {
	return delivery.TurnPresence{}, nil
}
func (h *listedRecipientSends) FindTokenSince(context.Context, string, string, []string, int) (delivery.TokenScan, error) {
	return delivery.TokenScan{}, nil
}
func (h *listedRecipientSends) FindTokenInTurn(context.Context, string, string, string, int) (delivery.TokenScan, error) {
	return delivery.TokenScan{}, nil
}
func (h *listedRecipientSends) RecipientFingerprint(context.Context, string) (string, error) {
	return "", nil
}

// TestSupervisorAcknowledgeParksAndStaysParked (CRW-1500 U8): three lead reports, a lead decision
// request and a fault notice addressed to the management seat 01mgmtA. supervisor-acknowledge
// --all-informational parks the three reports under superseded_by_report, with one
// supervisor_message_superseded row each naming supervisor-acknowledge and the actor, and skips the
// decision (answer_owed) and the notice (fault_notice). Three daemon passes then open no turn for a
// parked report (the decision is the only message sent), and the parks are still parked, still read
// as the acknowledgement's, with no reopened row.
func TestSupervisorAcknowledgeParksAndStaysParked(t *testing.T) {
	s := newListStore(t)
	t.Setenv("CODEX_THREAD_ID", "")
	s.must("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", "01mgmtA", "--task", "01mgmtA", "--host", "host")
	s.must("linkage-bind", "--role", "supervisor", "--scope", "INI-1", "--task", "01lead", "--host", "host", "--creator", "01mgmtA")
	t.Setenv("CODEX_THREAD_ID", "01lead")
	var reports []string
	for _, id := range []string{"lrp-one", "lrp-two", "lrp-three"} {
		reports = append(reports, s.must("report-up", "--initiative", "INI-1", "--kind", "lead_report", "--id", id, "--text", "milestone "+id+" is done")["messageId"].(string))
	}
	decision := s.must("report-up", "--initiative", "INI-1", "--kind", "lead_decision_request", "--id", "lrp-ask", "--subject", "which gate goes first", "--text", "two gates are ready")["messageId"].(string)
	t.Setenv("CODEX_THREAD_ID", "")
	s.seed("01mgmtA", "fault-1", store.SupervisorNoticeObligationKind, "notification", "", "queued", "2026-10-13T12:50:01.000000+00:00")

	answer := s.must("supervisor-acknowledge", "--recipient", "01mgmtA", "--actor", "01mgmtA", "--all-informational")
	if got := stringsOf(t, answer["parked"]); !slices.Equal(got, reports) {
		t.Fatalf("parked %v, want the three reports %v", got, reports)
	}
	if got := skipped(t, answer); len(got) != 2 || got[decision] != supervisor.AcknowledgeAnswerOwed || got["fault-1"] != supervisor.AcknowledgeFaultNotice {
		t.Fatalf("skipped %v, want the decision answer_owed and the notice fault_notice", got)
	}
	if answer["schema"] != "supervisor-acknowledge/1" || answer["hold"] != store.SupervisorHoldSuperseded || answer["actor"] != "01mgmtA" || answer["recipient"] != "01mgmtA" {
		t.Fatalf("answer %v", answer)
	}
	for _, id := range reports {
		if row := s.message(id); row.HoldReason.String != store.SupervisorHoldSuperseded || row.State != "queued" {
			t.Fatalf("report %s is %s held %q", id, row.State, row.HoldReason.String)
		}
		entries := s.journal("supervisor_message_superseded", id)
		if len(entries) != 1 || !strings.Contains(entries[0].Detail, `"by": "`+supervisor.AcknowledgeCommand+`"`) || !strings.Contains(entries[0].Detail, `"01mgmtA"`) {
			t.Fatalf("report %s journaled %+v", id, entries)
		}
	}
	for _, id := range []string{decision, "fault-1"} {
		if row := s.message(id); row.HoldReason.Valid || row.State != "queued" {
			t.Fatalf("%s is %s held %q", id, row.State, row.HoldReason.String)
		}
	}

	db := s.open()
	channel := &supervisor.Channel{Store: db, Linkage: supervisor.StoreLinkage{Store: db}, Settings: &delivery.TaskSettings{}}
	host := &listedRecipientSends{}
	now := 1_900_000_000.0
	for range 3 {
		now += 60
		if _, err := channel.AutoSend(context.Background(), host, now, 10, 10, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for _, text := range host.sends {
		for _, id := range reports {
			if strings.Contains(text, id) {
				t.Fatalf("a pass opened a turn for the parked report %s: %v", id, host.sends)
			}
		}
	}
	if len(host.sends) != 1 || !strings.Contains(host.sends[0], decision) {
		t.Fatalf("the passes opened %d turn(s), want the decision's one: %v", len(host.sends), host.sends)
	}
	for _, id := range reports {
		if row := s.message(id); row.HoldReason.String != store.SupervisorHoldSuperseded || row.State != "queued" || row.AttemptCount != 0 {
			t.Fatalf("report %s after the passes: %+v", id, row)
		}
		if entries := s.journal("supervisor_message_reopened", id); len(entries) != 0 {
			t.Fatalf("report %s was reopened: %+v", id, entries)
		}
		if entries := s.journal("supervisor_message_superseded", id); len(entries) != 1 || !strings.Contains(entries[0].Detail, supervisor.AcknowledgeCommand) {
			t.Fatalf("report %s journaled %+v after the passes", id, entries)
		}
	}
}

// TestSupervisorAcknowledgeByIdAndUsage (CRW-1500 U8): a blocked notice named by --message is parked
// although --all-informational would skip it; a message of another recipient and an id the store
// does not hold are skipped with their reasons; naming a parked message again parks nothing. A
// message in sending, or with an attempt that may have gone, is skipped claimed. A line the command
// does not take writes nothing, an empty --project or --staged-before included (read as omitted,
// either would widen the bulk form).
func TestSupervisorAcknowledgeByIdAndUsage(t *testing.T) {
	s := newListStore(t)
	s.seed("01R", "b1", "blocked", "notification", "P1", "queued", "2026-10-13T12:50:01.000000+00:00")
	s.seed("01OTHER", "o1", "completion", "notification", "P1", "queued", "2026-10-13T12:50:02.000000+00:00")
	s.seed("01R", "sending", "completion", "notification", "P1", "sending", "2026-10-13T12:50:03.000000+00:00")
	s.seed("01R", "gone", "completion", "notification", "P1", "withheld_pre_send", "2026-10-13T12:50:04.000000+00:00")
	s.seed("01R", "early", "flow_completion", "notification", "P1", "queued", "2026-10-13T12:50:05.000000+00:00")
	s.seed("01R", "late", "completion", "notification", "P1", "queued", "2026-10-14T00:00:00.000000+00:00")
	s.seed("01R", "elsewhere", "completion", "notification", "P2", "queued", "2026-10-13T12:50:06.000000+00:00")
	db := s.open()
	if _, err := db.DB.ExecContext(context.Background(), "INSERT INTO supervisor_attempts(request_id,message_id,attempt_no,message,state,send_attempted,retry_safe,record,sent_at,observed_at) VALUES('rq-gone','gone',1,'m','held_uncertain','unknown',0,'{}','t','t')"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	before := s.digest()
	for _, argv := range [][]string{
		{"supervisor-acknowledge", "--recipient", "01R", "--actor", "01R"},
		{"supervisor-acknowledge", "--recipient", "01R", "--actor", "01R", "--all-informational", "--message", "early"},
		{"supervisor-acknowledge", "--recipient", "01R", "--actor", "01R", "--message", "early", "--project", "P1"},
		{"supervisor-acknowledge", "--recipient", "01R", "--actor", "01R", "--all-informational", "--staged-before", "yesterday"},
		{"supervisor-acknowledge", "--recipient", "01R", "--all-informational"},
		{"supervisor-acknowledge", "--actor", "01R", "--all-informational"},
		{"supervisor-acknowledge", "--recipient", "01R", "--actor", "01R", "--all-informational", "--staged-before", ""},
		{"supervisor-acknowledge", "--recipient", "01R", "--actor", "01R", "--all-informational", "--project", ""},
	} {
		if code, answer := s.run(argv...); code == 0 {
			t.Errorf("%v: exit 0 %v, want a usage refusal", argv, answer)
		}
	}
	if s.digest() != before {
		t.Fatal("a refused acknowledgement changed the store")
	}

	byID := s.must("supervisor-acknowledge", "--recipient", "01R", "--actor", "01operator", "--message", "b1", "--message", "o1", "--message", "absent")
	if got := stringsOf(t, byID["parked"]); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("parked %v, want b1", got)
	}
	if got := skipped(t, byID); got["o1"] != supervisor.AcknowledgeOtherRecipient || got["absent"] != supervisor.AcknowledgeNotFound || len(got) != 2 {
		t.Fatalf("skipped %v", got)
	}
	if row := s.message("b1"); row.HoldReason.String != store.SupervisorHoldSuperseded || row.State != "queued" {
		t.Fatalf("b1 is %s held %q", row.State, row.HoldReason.String)
	}
	if entries := s.journal("supervisor_message_superseded", "b1"); len(entries) != 1 || !strings.Contains(entries[0].Detail, `"01operator"`) {
		t.Fatalf("b1 journaled %+v", entries)
	}
	again := s.must("supervisor-acknowledge", "--recipient", "01R", "--actor", "01operator", "--message", "b1")
	if got := skipped(t, again); got["b1"] != supervisor.AcknowledgeHeld || len(stringsOf(t, again["parked"])) != 0 {
		t.Fatalf("the second acknowledgement answered %v", again)
	}
	if entries := s.journal("supervisor_message_superseded", "b1"); len(entries) != 1 {
		t.Fatalf("b1 journaled %d rows after the second acknowledgement", len(entries))
	}

	bulk := s.must("supervisor-acknowledge", "--recipient", "01R", "--actor", "01R", "--all-informational", "--project", "P1", "--staged-before", "2026-10-13T13:00:00Z")
	if got := stringsOf(t, bulk["parked"]); !slices.Equal(got, []string{"early"}) {
		t.Fatalf("parked %v, want only the early flow completion", got)
	}
	if got := skipped(t, bulk); got["sending"] != supervisor.AcknowledgeClaimed || got["gone"] != supervisor.AcknowledgeClaimed || got["b1"] != supervisor.AcknowledgeHeld || len(got) != 3 {
		t.Fatalf("skipped %v", got)
	}
	for _, id := range []string{"sending", "gone", "late", "elsewhere", "o1"} {
		if row := s.message(id); row.HoldReason.Valid {
			t.Fatalf("%s was parked: %+v", id, row)
		}
	}
	if row := s.message("sending"); row.State != "sending" {
		t.Fatalf("the sending message is %s", row.State)
	}
}
