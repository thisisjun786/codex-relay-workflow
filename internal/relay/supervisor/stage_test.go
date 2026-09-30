package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type stageFixture struct {
	c     *Channel
	s     *store.Store
	ctx   context.Context
	root  string
	event string
	at    string
}

func fixture24(t *testing.T) *stageFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("HOME", root)
	s, err := store.Open(ctx, filepath.Join(root, "state", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, stmt := range []string{
		`INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES ('rel-1','REL-1','active','parent','host','child','host',1,'[]','[]','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`,
		`INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('event-1','rel-1',1,'abc123456789abcdef','ready_for_review','child','child','turn-1','completed','{}','2023-11-14T22:13:20.000000+00:00','2023-11-14T22:13:20.000000+00:00')`,
		`INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',1,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done','merge','2023-11-14T22:13:20.000000+00:00')`,
	} {
		if _, err = s.DB.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err = storeseed.RecordRelationshipScope(ctx, s, "rel-1", "PRJ-1", "t"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []store.ScopeBindingsRow{{BindingID: "b-child", Role: "child", ScopeKind: "issue", ScopeKey: "REL-1", TaskID: "child", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {BindingID: "b-parent", Role: "parent", ScopeKind: "project", ScopeKey: "PRJ-1", TaskID: "parent", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {BindingID: "b-supervisor", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "supervisor", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}} {
		if err = storeseed.InsertScopeBinding(ctx, s, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []store.ScopeLinksRow{{LinkID: "lnk-issue", LinkKind: "execution", UpperKind: "project", UpperKey: "PRJ-1", UpperTaskID: "parent", LowerKind: "issue", LowerKey: "REL-1", LowerTaskID: "child", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}, {LinkID: "lnk-project", LinkKind: "execution", UpperKind: "initiative", UpperKey: "INI-1", UpperTaskID: "supervisor", LowerKind: "project", LowerKey: "PRJ-1", LowerTaskID: "parent", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}} {
		if err = storeseed.InsertScopeLink(ctx, s, l); err != nil {
			t.Fatal(err)
		}
	}
	return &stageFixture{c: &Channel{Store: s, Linkage: StoreLinkage{s}, Program: "/usr/bin/codex-session-relay"}, s: s, ctx: ctx, root: root, event: "event-1", at: "2023-11-14T22:13:20.000000+00:00"}
}
func (f *stageFixture) obligation(t *testing.T) Obligation {
	t.Helper()
	o, err := f.c.FromEvent(f.ctx, f.event)
	if err != nil || o == nil {
		t.Fatalf("obligation %v: %v", o, err)
	}
	return *o
}
func (f *stageFixture) staged(t *testing.T) (Obligation, StageResult) {
	t.Helper()
	o := f.obligation(t)
	r, err := f.c.Stage(f.ctx, o, "", f.at)
	if err != nil {
		t.Fatal(err)
	}
	return o, r
}

func Test24_SCH_2_OneFactOneMessageAndStanding(t *testing.T) {
	t.Run("prior_report", func(t *testing.T) {
		supervisorMirror(t, "WhatMayBeStaged.test_staging_records_the_report_so_the_next_reading_converges", "event", func(c *Channel, s *store.Store) []any {
			ctx := context.Background()
			var event string
			if err := s.DB.QueryRowContext(ctx, "SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
				t.Fatal(err)
			}
			o, err := c.FromEvent(ctx, event)
			if err != nil || o == nil {
				t.Fatalf("obligation: %v", err)
			}
			_, err = c.Stage(ctx, *o, "", "2023-11-14T22:13:20.000000+00:00")
			if err != nil {
				t.Fatal(err)
			}
			prior, err := c.PriorReport(ctx, o.ID)
			if err != nil {
				t.Fatal(err)
			}
			can, _, err := c.Reportable(ctx, *o)
			if err != nil {
				t.Fatal(err)
			}
			return []any{prior != nil, prior["detail"].(map[string]any)["messageId"], can}
		})
	})
	t.Run("stage_standing", func(t *testing.T) {
		supervisorMirror(t, "WhatMayBeStaged.test_a_project_is_staged_in_one_call_and_what_is_refused_is_reported_beside_it", "event", func(c *Channel, _ *store.Store) []any {
			ctx := context.Background()
			answer, err := c.StageStanding(ctx, "PRJ-1", "2023-11-14T22:13:20.000000+00:00")
			if err != nil {
				t.Fatal(err)
			}
			staged := answer["staged"].([]any)
			again, err := c.StageStanding(ctx, "PRJ-1", "2023-11-14T22:13:20.000000+00:00")
			if err != nil {
				t.Fatal(err)
			}
			second := again["staged"].([]any)
			var count int
			if err := c.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM supervisor_messages").Scan(&count); err != nil {
				t.Fatal(err)
			}
			return []any{len(staged), answer["refused"], staged[0].(StageResult)["staged"], second[0].(StageResult)["staged"], count}
		})
	})
	supervisorMirror(t, "WhatMayBeStaged.test_one_fact_is_one_message_however_often_it_is_staged", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		var event string
		if err := s.DB.QueryRowContext(ctx, "SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
			t.Fatal(err)
		}
		o, err := c.FromEvent(ctx, event)
		if err != nil || o == nil {
			t.Fatalf("obligation: %v", err)
		}
		first, err := c.Stage(ctx, *o, "", "2023-11-14T22:13:20.000000+00:00")
		if err != nil {
			t.Fatal(err)
		}
		second, err := c.Stage(ctx, *o, "", "2023-11-14T22:13:20.000000+00:00")
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM supervisor_messages").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return []any{first["staged"], second["staged"], first["messageId"], count}
	})
	f := fixture24(t)
	o, first := f.staged(t)
	second, err := f.c.Stage(f.ctx, o, "", f.at)
	if err != nil {
		t.Fatal(err)
	}
	if first["staged"] != true || second["staged"] != false || first["messageId"] != second["messageId"] {
		t.Fatalf("first %v second %v", first, second)
	}
	var messages, reports int
	if err = f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM supervisor_messages").Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err = f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM journal WHERE kind='supervisor_report'").Scan(&reports); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || reports != 1 {
		t.Fatalf("messages=%d reports=%d", messages, reports)
	}
	prior, err := f.c.PriorReport(f.ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prior["detail"].(map[string]any)["messageId"] != first["messageId"] {
		t.Fatalf("prior %v", prior)
	}
	can, reason, err := f.c.Reportable(f.ctx, o)
	if err != nil || can || reason != "already_reported_under_this_obligation" {
		t.Fatalf("reportable %t reason %s err %v", can, reason, err)
	}
	stand, err := f.c.StageStanding(f.ctx, "PRJ-1", f.at)
	if err != nil {
		t.Fatal(err)
	}
	if len(stand["staged"].([]any)) != 1 || stand["staged"].([]any)[0].(StageResult)["staged"] != false || len(stand["refused"].([]any)) != 0 {
		t.Fatalf("standing %v", stand)
	}
}
func Test24_SCH_3_DurableStagedPacket(t *testing.T) {
	supervisorMirror(t, "WhatMayBeStaged.test_the_staged_row_outlives_the_process_that_wrote_it", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		var event string
		if err := s.DB.QueryRowContext(ctx, "SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
			t.Fatal(err)
		}
		o, err := c.FromEvent(ctx, event)
		if err != nil || o == nil {
			t.Fatalf("obligation: %v", err)
		}
		result, err := c.Stage(ctx, *o, "", "2023-11-14T22:13:20.000000+00:00")
		if err != nil {
			t.Fatal(err)
		}
		row, err := s.SupervisorMessage(ctx, result["messageId"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var packet Packet
		if err := json.Unmarshal([]byte(row.Packet), &packet); err != nil {
			t.Fatal(err)
		}
		return []any{true, row.State, packet["version"], packet["envelope"].(map[string]any)["direction"]}
	})
	f := fixture24(t)
	_, result := f.staged(t)
	other, err := store.Open(f.ctx, f.s.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	row, err := other.SupervisorMessage(f.ctx, result["messageId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var packet Packet
	if err = json.Unmarshal([]byte(row.Packet), &packet); err != nil {
		t.Fatal(err)
	}
	if row.State != "queued" || packet["version"] != packetVersion || packet["envelope"].(map[string]any)["direction"] != "parent_to_supervisor" {
		t.Fatalf("row %v packet %v", row, packet)
	}
}
func Test24_SCH_4_OrdinaryAlreadyReportedAndWrongRecipient(t *testing.T) {
	supervisorMirror(t, "WhatMayBeStaged.test_a_caller_naming_another_recipient_is_the_finding", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		var event string
		if err := s.DB.QueryRowContext(ctx, "SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
			t.Fatal(err)
		}
		o, err := c.FromEvent(ctx, event)
		if err != nil || o == nil {
			t.Fatalf("obligation: %v", err)
		}
		_, err = c.Stage(ctx, *o, "01someone-else", "2023-11-14T22:13:20.000000+00:00")
		var refusal Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("expected refusal: %v", err)
		}
		return []any{refusal.Reason, strings.Contains(refusal.Detail, "01supervisor-task")}
	})
	f := fixture24(t)
	if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE events SET outcome='failed' WHERE event_id=?", f.event); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.ExecContext(f.ctx, "DELETE FROM work_reports WHERE event_id=?", f.event); err != nil {
		t.Fatal(err)
	}
	o, err := f.c.FromEvent(f.ctx, f.event)
	if err != nil || o != nil {
		t.Fatalf("ordinary event: %v %v", o, err)
	}
	if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE events SET outcome='ready_for_review' WHERE event_id=?", f.event); err != nil {
		t.Fatal(err)
	}
	value := f.obligation(t)
	o = &value
	_, err = f.c.Stage(f.ctx, *o, "wrong", f.at)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "recipient_not_authorized" || !strings.Contains(refusal.Detail, "supervisor") {
		t.Fatalf("wrong recipient: %v", err)
	}
	_, err = f.s.DB.ExecContext(f.ctx, "INSERT INTO journal (at,kind,subject,detail) VALUES (?,'supervisor_report',?,'{}')", f.at, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.c.Stage(f.ctx, *o, "", f.at)
	if !errors.As(err, &refusal) || refusal.Reason != "not_claimable" || !strings.Contains(refusal.Detail, "already_reported_under_this_obligation") {
		t.Fatalf("prior report: %v", err)
	}
	var count int
	if err = f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM supervisor_messages").Scan(&count); err != nil || count != 0 {
		t.Fatalf("message count=%d err=%v", count, err)
	}
}
func Test24_SCH_5_PacketPurposeAndEvidence(t *testing.T) {
	supervisorMirror(t, "WhatTheMessageCarries.test_a_completion_names_the_issue_the_generation_and_where_to_read_it", "event", func(c *Channel, s *store.Store) []any {
		ctx := context.Background()
		var event string
		if err := s.DB.QueryRowContext(ctx, "SELECT event_id FROM events ORDER BY rowid DESC LIMIT 1").Scan(&event); err != nil {
			t.Fatal(err)
		}
		o, err := c.FromEvent(ctx, event)
		if err != nil || o == nil {
			t.Fatalf("obligation: %v", err)
		}
		r, err := c.Resolve(ctx, o.RelationID)
		if err != nil {
			t.Fatal(err)
		}
		p, err := c.Compose(ctx, *o, r, "2023-11-14T22:13:20.000000+00:00")
		if err != nil {
			t.Fatal(err)
		}
		evidence := p["evidence"].([]string)
		return []any{p["issue"], p["generation"], len(evidence) > 0, strings.Contains(evidence[0], "show --event"), p["envelope"].(map[string]any)["kind"]}
	})
	f := fixture24(t)
	o := f.obligation(t)
	r, err := f.c.Resolve(f.ctx, o.RelationID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.c.Compose(f.ctx, o, r, f.at)
	if err != nil {
		t.Fatal(err)
	}
	if p["issue"] != "REL-1" || p["generation"] != int64(1) || p["envelope"].(map[string]any)["kind"] != "notification" || !strings.Contains(p["evidence"].([]string)[0], "show --event event-1") {
		t.Fatalf("packet %v", p)
	}
	o.Kind = "decision_request"
	o.ID = hash32(o.Kind + "|" + o.RelationID + "|" + o.Subject)
	o.Detail = "which reading is agreed?"
	p, err = f.c.Compose(f.ctx, o, r, f.at)
	if err != nil {
		t.Fatal(err)
	}
	region := p["envelope"].(map[string]any)
	if region["kind"] != "decision" || region["answerOwedBy"] != "user" || region["decision"] != "which reading is agreed?" {
		t.Fatalf("decision %v", region)
	}
}
func Test24_SCH_9_HandoverRefusesStaleStaging(t *testing.T) {
	f := fixture24(t)
	o, _ := f.staged(t)
	if err := storeseed.ArchiveScopeBinding(f.ctx, f.s, "b-supervisor", "archived", "b-next", f.at); err != nil {
		t.Fatal(err)
	}
	b := store.ScopeBindingsRow{BindingID: "b-next", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INI-1", TaskID: "next", HostID: "host", Status: "active", Revision: 2, CreatedAt: "t2", UpdatedAt: "t2"}
	if err := storeseed.InsertScopeBinding(f.ctx, f.s, b); err != nil {
		t.Fatal(err)
	}
	_, err := f.c.Stage(f.ctx, o, "", f.at)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "relation_owner_drift" {
		t.Fatalf("stale hierarchy: %v", err)
	}
}
