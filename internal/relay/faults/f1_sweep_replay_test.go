package faults

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func f1ReplayStores(t *testing.T) (context.Context, string, string) {
	t.Helper()
	home, e := os.MkdirTemp("/dev/shm", "f1-stores-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	for k, v := range map[string]string{"HOME": home, "XDG_STATE_HOME": home + "/xs", "XDG_CONFIG_HOME": home + "/xc", "CODEX_HOME": home + "/ch"} {
		t.Setenv(k, v)
	}
	ctx := context.WithValue(context.Background(), f1InputsKey{}, f1Inputs{clock: &testClock{now: 100000}, entropy: bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7})})
	gd, pd := home+"/go", home+"/py"
	f1SeedBoth(t, ctx, gd, pd, nil)
	// Both implementations start from identical metadata as well as domain rows.
	seed, e := os.ReadFile(gd + "/relay.sqlite3")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(pd+"/relay.sqlite3", seed, 0600); e != nil {
		t.Fatal(e)
	}
	return ctx, gd, pd
}
func f1SeedBoth(t *testing.T, ctx context.Context, gd, pd string, sql []string) {
	t.Helper()
	for _, dir := range []string{gd, pd} {
		s, e := store.Open(ctx, dir+"/relay.sqlite3", "")
		if e != nil {
			t.Fatal(e)
		}
		for _, query := range sql {
			if _, e = s.Q(ctx).ExecContext(ctx, query); e != nil {
				_ = s.Close()
				t.Fatalf("%s: %v", query, e)
			}
		}
		if e = s.Close(); e != nil {
			t.Fatal(e)
		}
	}
}

const f1Relationship = "INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES('rel','ISSUE','active','parent','host','child','host',1,'[]','[]','stamp','stamp')"
const f1Delivery = "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,created_at,updated_at) VALUES('event','rel','completion','parent','thread','pending',3,'stamp','stamp')"

func TestF1_FLT_18_21_22_SweepSourcesWholeCLI(t *testing.T) {
	for _, source := range []string{"delivery", "sync", "observation", "refusal"} {
		t.Run(source, func(t *testing.T) {
			ctx, gd, pd := f1ReplayStores(t)
			seed := []string{f1Relationship, "INSERT INTO relationship_scope VALUES('rel','CRW','stamp')"}
			var clear []string
			switch source {
			case "delivery":
				seed = append(seed, f1Delivery)
				for i := 1; i <= 3; i++ {
					seed = append(seed, fmt.Sprintf("INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES('request%d','event',%d,'completion','settled','withheld_pre_send','stamp')", i, i))
				}
				clear = []string{"UPDATE deliveries SET state='dispatched',hold_reason=NULL"}
			case "sync":
				seed = append(seed, "INSERT INTO sync_outbox(sync_id,relationship_id,issue_key,target,target_ref,subject_kind,identity_digest,summary,state,attempts,last_error,created_at,updated_at) VALUES('sync','rel','ISSUE','issue','ISSUE','event','digest','summary','failed',6,'lost','stamp','stamp')")
				clear = []string{"UPDATE sync_outbox SET state='confirmed'"}
			case "observation":
				seed = append(seed, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at) VALUES('rel',1,'dispatch','bound','turn','stamp')", "INSERT INTO poll_observations VALUES('rel',1,'turn',NULL,NULL,'stamp','failed')")
				clear = []string{"UPDATE poll_observations SET last_polled_at='stamp',last_error=NULL"}
			case "refusal":
				seed = append(seed, f1Delivery, "INSERT INTO journal(at,kind,subject,detail) VALUES('stamp','delivery_withheld','event','{\"reason\":\"settings_unavailable\",\"detail\":\"not readable\"}')")
				clear = []string{"UPDATE deliveries SET state='dispatched'"}
			}
			f1SeedBoth(t, ctx, gd, pd, seed)
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
			if source == "delivery" {
				f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE deliveries SET hold_reason='attempt_cap',attempt_count=6"})
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
			}
			f1SeedBoth(t, ctx, gd, pd, clear)
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
		})
	}
}
func TestF1_FLT_18_21_InFlightWholeCLI(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	f1SeedBoth(t, ctx, gd, pd, []string{f1Relationship, f1Delivery, "INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES('request','event',1,'completion','in_flight','held_uncertain','stamp')"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE attempts SET internal_state='settled'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
	f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE attempts SET internal_state='in_flight'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
}

func TestF1_FLT_21_SourceBeyondPageWholeCLI(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	seed := []string{f1Relationship}
	for i := 0; i < 34; i++ {
		seed = append(seed, fmt.Sprintf("INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,hold_reason,created_at,updated_at) VALUES('event%03d','rel','completion','parent%03d','thread','withheld_pre_send','host_lost_turn','stamp','stamp')", i, i))
	}
	f1SeedBoth(t, ctx, gd, pd, seed)
	for i := 0; i < 4; i++ {
		f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
	}
}

func TestF1_FLT_32_ReadingsContinuationWholeCLI(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	readings := []any{}
	for i := 0; i < 3; i++ {
		readings = append(readings, map[string]any{"schema": "reporting-observation/1", "relationshipId": fmt.Sprintf("rel-%d", i), "selectors": map[string]any{"turn": "turn-1"}, "reportingState": "unreported"})
	}
	raw, e := json.Marshal(readings)
	if e != nil {
		t.Fatal(e)
	}
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep", "--readings", string(raw), "--readings-after", "2"})
	s, e := store.Open(ctx, gd+"/relay.sqlite3", "")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	rows, e := s.All(ctx, "SELECT signature FROM fault_ledger WHERE fault_class='report_omitted'")
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 1 || text(rows[0], "signature") != `{"relationship":"rel-2","turn":"turn-1"}` {
		t.Fatalf("wrong continuation: %v", rows)
	}
}
func TestF1SweepBoundsAndScopeWholeCLI(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	reading := `[{"schema":"reporting-observation/1","relationshipId":"rel","selectors":{"turn":"turn"},"reportingState":"unreported"}]`
	for _, args := range [][]string{
		{"fault-sweep", "--readings-after", "1001"},
		{"fault-sweep", "--readings", "null"},
		{"fault-sweep", "--product", "bad:product", "--readings", reading},
	} {
		f1ReplayCLI(t, ctx, gd, pd, args)
	}
	f1SeedBoth(t, ctx, gd, pd, []string{f1Delivery, "UPDATE deliveries SET hold_reason='attempt_cap'"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep", "--project", "P"})
}

func TestF1SweepReadingsWholeCLI(t *testing.T) {
	for _, state := range []string{"unreported", "reported", "unmeasured", "in_progress", "unmanaged", "unexpected"} {
		t.Run(state, func(t *testing.T) {
			ctx, gd, pd := f1ReplayStores(t)
			reading := func(state string) string {
				raw, e := json.Marshal([]any{map[string]any{"schema": "reporting-observation/1", "relationshipId": "rel", "selectors": map[string]any{"turn": "turn"}, "reportingState": state, "reason": "unavailable"}})
				if e != nil {
					t.Fatal(e)
				}
				return string(raw)
			}
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep", "--readings", reading("unmeasured")})
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep", "--readings", reading(state)})
		})
	}
}
