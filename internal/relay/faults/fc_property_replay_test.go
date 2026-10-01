package faults

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The FC properties of test_fault_contract.py are owned by the tests that compare Go's complete
// replies, CLI bytes and fault_* rows with goldens that began as Python's answers (the composite
// tests that re-ran them under one more name went in wave R1):
//   - FC-1, 3, 4, 5, 23, 25, 26, 29, 31, 32 (publication fencing, issue ownership, adoption,
//     extension kinds, budgets, target changes, readback): TestF1_FLT_25_26_27_LifecycleWholeCLI
//     and TestF2WholeOutput;
//   - FC-2, 6-12, 14, 16-18, 24, 34, 38 (sweeps): the TestF1_FLT_18_21_22 / FLT_18_21 / FLT_21 /
//     FLT_32 sweep tests, TestF1SweepBoundsAndScopeWholeCLI, TestF1SweepReadingsWholeCLI,
//     Test22_OvertakenPresenceWholePythonPage and Test22_SettingsHoldWholePythonObservation;
//   - FC-13, 19, 22, 27, 35, 36 (records): Test22_FLT_1_IdentityWholeOutput,
//     Test22_FLT_2_3_4_14_RecordWholeOutput and Test22_FLT_5_6_7_8_19_23_24_LifecycleWholeOutput;
//   - FC-15, 20, 21, 37 (budget, attention, notifications): the TestD policy, attention,
//     notification-lifecycle, relationship-eligibility and notifications-page tests and
//     Test22_FN_7_BudgetAndRefundWholeOutput;
//   - FC-28, 30, 33 (relink): TestDRelinkRepointsBoundedWritesAgainstPython and
//     TestDRelinkOutstandingWriteSelectionAgainstPython.

// FC-39: scope conflicts and moves compare complete command outputs and rows.
func Test22_FC_39_ScopeConflictWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	// A pre-restriction product collides with a modern product's project key.
	f1Seed(t, ctx, gd, []string{legacyScopeFault,
		"INSERT INTO fault_targets VALUES('a:b','legacy-team','stamp')"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", scopeConflictObservation("b", "new")})
	f1ReplayCLI(t, ctx, gd, []string{"fault-target", "--product", "a", "--project", "b", "--team", "team", "--project-ref", "p"})
	// Both automatic observation rescope and explicit move must refuse the same key.
	answer := f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", scopeConflictObservation("safe", "first")})
	f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", scopeConflictObservation("b", "again")})
	f1ReplayCLI(t, ctx, gd, []string{"fault-move", "--fault", answer["faultId"].(string), "--scope", `{"projectKey":"b"}`})
}

const legacyScopeFault = `INSERT INTO fault_ledger(fault_id,product,fault_class,component,severity,signature,scope,scope_key,state,cycle,occurrence_count,reopen_count,first_seen_at,last_seen_at,updated_at) VALUES('legacyfault','a:b','report_omitted','reporting','broken','{"relationship":"x"}','{}','a:b','open',1,1,0,'stamp','stamp','stamp')`

func scopeConflictObservation(project, occurrence string) string {
	return fmt.Sprintf(`{"schema":"fault-observation/1","product":"a","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"rel","turn":"turn"},"occurrenceKey":%q,"scope":{"projectKey":%q}}`, occurrence, project)
}

func Test22_FC_11_BusyIsWaitingWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	seed := []string{f1Relationship}
	for _, event := range []string{"busy", "cap", "mixed"} {
		hold := "NULL"
		if event == "cap" {
			hold = "'busy_cap'"
		}
		seed = append(seed, fmt.Sprintf("INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,hold_reason,created_at,updated_at) VALUES('%s','rel','completion','parent','parent','deferred_busy',3,%s,'stamp','stamp')", event, hold))
		for i := 1; i <= 3; i++ {
			state := "deferred_busy"
			if event == "mixed" && i == 1 {
				state = "withheld_pre_send"
			}
			seed = append(seed, fmt.Sprintf("INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES('%s-%d','%s',%d,'completion','settled','%s','stamp')", event, i, event, i, state))
		}
	}
	f1Seed(t, ctx, gd, seed)
	f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"crw","faultClass":"delivery_stalled","severity":"degraded","signature":{"recipient":"parent","attemptState":"deferred_busy"},"occurrenceKey":"attempt:busy-1"}`})
	f1ReplayCLI(t, ctx, gd, []string{"fault-sweep"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-show"})
}

func Test22_FC_16_CappedProductDoesNotHideAnotherWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	for _, product := range []string{"crw", "other"} {
		f1ReplayCLI(t, ctx, gd, []string{"fault-target", "--product", product, "--project", "P", "--team", "team-" + product, "--project-ref", "project-" + product})
	}
	pubs := []string{}
	for i := 0; i < 8; i++ {
		observation := fmt.Sprintf(`{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"rel-%d","turn":"turn"},"occurrenceKey":"one","scope":{"projectKey":"P"}}`, i)
		answer := f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", observation})
		pubs = append(pubs, answer["publication"].(map[string]any)["publicationId"].(string))
	}
	for _, pub := range pubs[:5] {
		f1ReplayCLI(t, ctx, gd, []string{"fault-claim", "--publication", pub, "--owner", "writer"})
	}
	f1ReplayCLI(t, ctx, gd, []string{"fault-observe", "--observation", `{"schema":"fault-observation/1","product":"other","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"rel","turn":"turn"},"occurrenceKey":"one","scope":{"projectKey":"P"}}`})
	f1ReplayCLI(t, ctx, gd, []string{"fault-next", "--limit", "3"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-attention"})
}

func Test22_FC_22_ProspectivePolicyWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	replayObservation(t, ctx, gd, "delivery_stalled", Degraded, "s1")
	f1ReplayCLI(t, ctx, gd, []string{"fault-policy", "--product", "crw", "--fault-class", "delivery_stalled", "--severity", "degraded", "--threshold", "1", "--reason", "one stall is enough here"})
	replayObservation(t, ctx, gd, "delivery_stalled", Degraded, "s2")
	f1ReplayCLI(t, ctx, gd, []string{"fault-policy", "--product", "crw", "--fault-class", "report_omitted", "--severity", "broken", "--threshold", "3", "--reason", "quiet"})
}

func Test22_FC_23_WriterTakeoverWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	f1ReplayCLI(t, ctx, gd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
	answer := replayObservation(t, ctx, gd, "report_omitted", Broken, "one")
	pub := answer["publication"].(map[string]any)["publicationId"].(string)
	claim := f1ReplayCLI(t, ctx, gd, []string{"fault-claim", "--publication", pub, "--owner", "writer-A"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-fail", "--publication", pub, "--claim-token", claim["claimToken"].(string), "--error", "network down"})
	// The clock is fixed on both sides; expire only the backoff, not the writer history.
	f1Seed(t, ctx, gd, []string{"UPDATE fault_publications SET next_attempt_at=99999"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-claim", "--publication", pub, "--owner", "writer-B"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-claim", "--publication", pub, "--owner", "writer-B", "--takeover"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-show", "--publication", pub})
}

func Test22_FC_36_UnloadedKindStoredLimitWholeOutput(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	// set_limit accepts extension names even without importing their implementation.
	f1ReplayCLI(t, ctx, gd, []string{"fault-limit", "--product", "crw", "--kind", "external_kind_limit", "--max-count", "3", "--window", "3600"})
	f1ReplayCLI(t, ctx, gd, []string{"fault-limit", "--product", "crw"})
}

func Test22_FC_9_ManagedReadingsWholeOutput(t *testing.T) {
	goldenParent(t)
	for _, variant := range []string{"unreported", "error", "none", "unnamed", "past_generation", "paged", "real_unreported", "real_absent", "real_reported", "real_unwitnessed", "real_bad_stop", "real_ready"} {
		t.Run(variant, func(t *testing.T) {
			ctx, gd := f1ReplayStores(t)
			seed := []string{f1Relationship,
				`INSERT INTO managed_start_requests(request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,child_task_id,standby_turn_id,relationship_id,execution_generation,receipt_status,created_at,updated_at) VALUES('req-1','ISSUE','fp','v1','/w','/m','sock','create-1','dispatch-1','attached',3,'child','standby-1','rel',1,'accepted','stamp','stamp')`,
				`INSERT INTO assignment_settlements VALUES('rel','child','turn-9','completed','stamp')`,
				`INSERT INTO assignment_settlements VALUES('rel','child','turn-9','failed','stamp')`,
				`INSERT INTO assignment_settlements VALUES('rel','child','standby-1','completed','stamp')`,
				`INSERT INTO assignment_settlements VALUES('rel','child','turn-10','completed','stamp')`}
			if variant == "past_generation" {
				seed = append(seed, "UPDATE relationships SET execution_generation=2")
			}
			if variant == "paged" {
				for i := 11; i < 19; i++ {
					seed = append(seed, fmt.Sprintf("INSERT INTO assignment_settlements VALUES('rel','child','turn-%d','completed','stamp')", i))
				}
			}
			f1Seed(t, ctx, gd, seed)
			real := strings.HasPrefix(variant, "real_")
			if real {
				root, work := filepath.Dir(gd)+"/markers", filepath.Dir(gd)+"/workspace"
				f1Seed(t, ctx, gd, []string{
					fmt.Sprintf("UPDATE managed_start_requests SET marker_root='%s',workspace='%s'", root, work),
					fmt.Sprintf("UPDATE relationships SET child_cwd='%s'", work),
					"DELETE FROM assignment_settlements WHERE turn_id='turn-10' OR terminal_status='failed'",
					"INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at) VALUES('rel',1,'dispatch-1','bound','turn-9','stamp')"})
				workspaceHash, dispatchHash := sha256.Sum256([]byte(work)), sha256.Sum256([]byte("dispatch-1"))
				dir := filepath.Join(root, fmt.Sprintf("%x", workspaceHash), fmt.Sprintf("%x", dispatchHash))
				if variant != "real_absent" {
					fcMarker(t, dir, "intent.json", map[string]any{"dispatchRequestIdHash": fmt.Sprintf("%x", dispatchHash), "workspace": work, "dbPath": gd + "/relay.sqlite3", "issueKey": "ISSUE"})
					fcMarker(t, dir, "claims/child/claim.json", map[string]any{"sessionId": "child", "dispatchRequestId": "dispatch-1"})
					fcMarker(t, dir, "bound.json", map[string]any{"sessionId": "child", "taskId": "child"})
					fcMarker(t, dir, "relationship.json", map[string]any{"relationshipId": "rel", "executionGeneration": 1})
					if variant != "real_unwitnessed" {
						turn := "turn-9"
						if variant == "real_bad_stop" {
							turn = "foreign"
						}
						fcMarker(t, dir, "hook/child/turn-9/1.json", map[string]any{"sessionId": "child", "turnId": turn, "observation": "undeclared_turn_end", "decisionState": "unresolved_handoff", "at": "1970-01-02T03:46:40+00:00"})
					}
					if variant == "real_reported" {
						fcMarker(t, dir, "dispositions/child/turn-9.json", map[string]any{"sessionId": "child", "turnId": "turn-9", "outcome": "failed", "at": "1970-01-02T03:46:40+00:00"})
					}
					if variant == "real_ready" {
						fcMarker(t, dir, "dispositions/child/turn-9.json", map[string]any{"sessionId": "child", "turnId": "turn-9", "outcome": "ready_for_review", "at": "1970-01-02T03:46:40+00:00"})
						fcMarker(t, work, "artifact.json", map[string]any{"ready": true})
						entries, e := store.BuildManifest([]string{work + "/artifact.json"}, []string{work})
						if e != nil {
							t.Fatal(e)
						}
						revision, e := store.ManifestRevision(entries)
						if e != nil {
							t.Fatal(e)
						}
						payload, e := json.Marshal(map[string]any{"revisionHash": revision, "manifest": entries})
						if e != nil {
							t.Fatal(e)
						}
						f1Seed(t, ctx, gd, []string{fmt.Sprintf(`UPDATE relationships SET artifact_roots='["%s"]'`, work), fmt.Sprintf(`INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES('receipt','rel',1,'%s','ready_for_review','child','child','turn-9','completed','%s','stamp','stamp')`, revision, strings.ReplaceAll(string(payload), "'", "''"))})
					}
				}
			}
			s := fcOpen(t, ctx, gd)
			calls := []any{}
			observer := fcObserver(func(_ context.Context, request ManagedReadingRequest) (any, error) {
				calls = append(calls, request)
				if variant == "error" && request.Turn == "turn-9" {
					return nil, fmt.Errorf("OSError: marker unreadable")
				}
				if variant == "none" {
					return nil, nil
				}
				answer := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "selectors": map[string]any{"turn": request.Turn, "session": request.Session}}
				if variant != "unnamed" {
					answer["relationshipId"] = "rel"
				}
				return answer, nil
			})
			clock := &testClock{now: 100000}
			sw := &Sweeper{Store: s, HostRecordPath: testHostRecordPath(), Now: clock.ISO, MaxAttempts: 6, Installation: Installation{Package: "codex-session-relay", Version: "test", Location: "test"}, Selection: "the-selection", ManagedObserver: observer}
			if real {
				sw.Selection = store.StateSelection{Path: gd}
				sw.ManagedObserver = nil
			}
			l := &Ledger{Store: s, Clock: clock}
			replies := []any{}
			for i := 0; i < 2; i++ {
				batch, err := sw.Sweep(ctx, "crw")
				if err != nil {
					t.Fatal(err)
				}
				reply, err := sw.RecordAll(ctx, l, batch)
				if err != nil {
					t.Fatal(err)
				}
				replies = append(replies, map[string]any{"read": reply.Read, "recorded": reply.Recorded, "queued": reply.Queued, "gaps": reply.Gaps, "results": reply.Results})
			}
			fcComparePath(t, ctx, s, gd, "managed", variant, replies, calls)
		})
	}
}

func fcMarker(t *testing.T, dir, name string, value any) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

type fcObserver func(context.Context, ManagedReadingRequest) (any, error)

func (f fcObserver) Observe(ctx context.Context, r ManagedReadingRequest) (any, error) {
	return f(ctx, r)
}

func Test22_FC_26_PreIssueSavepointWholeOutput(t *testing.T) {
	goldenParent(t)
	for _, variant := range []string{"cancel", "write", "hold", "invalid", "accept"} {
		t.Run(variant, func(t *testing.T) {
			ctx, gd := f1ReplayStores(t)
			f1ReplayCLI(t, ctx, gd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
			observed := replayObservation(t, ctx, gd, "report_omitted", Notice, "pre-issue")
			id := observed["faultId"].(string)
			s := fcOpen(t, ctx, gd)
			l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
			calls := []any{}
			if err := RegisterKind("project_create_check", kindPolicy{Creates: true, Target: "team", Evidence: "block", PreIssue: func(hook map[string]any) any {
				db := hook["db"].(PreIssueDB)
				hookctx := hook["context"].(context.Context)
				publication := hook["publication"].(map[string]any)
				var count int
				if err := db.QueryRowContext(hookctx, "SELECT COUNT(*) FROM fault_publications WHERE publication_id=?", publication["publication_id"]).Scan(&count); err != nil {
					return err
				}
				calls = append(calls, map[string]any{"publication": publication, "fault": hook["fault"], "now": hook["now"], "count": count})
				switch variant {
				case "write":
					_, err := db.ExecContext(hookctx, "UPDATE fault_ledger SET detail='written'")
					return err
				case "cancel":
					return map[string]any{"cancel": "a suitable project was bound after the claim"}
				case "hold":
					return map[string]any{"hold": "still measuring", "seconds": 17}
				case "invalid":
					return "unexpected"
				}
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { delete(kinds, "project_create_check") })
			queued, err := cQueue(ctx, l, map[string]string{"--fault": id, "--kind": "project_create_check", "--trigger": "need", "--payload": `{"name":"Ops"}`})
			if err != nil {
				t.Fatal(err)
			}
			pub := queued.(map[string]any)["publicationId"].(string)
			claim, err := f1Claim(ctx, l, map[string]string{"--publication": pub, "--owner": "writer-A"})
			if err != nil {
				t.Fatal(err)
			}
			token := claim.(map[string]any)["claimToken"].(string)
			replies := []any{queued, claim}
			reply, err := f1Operation(ctx, l, map[string]string{"--publication": pub, "--claim-token": token})
			if err != nil {
				reason, detail, _ := strings.Cut(strings.TrimPrefix(err.Error(), "transaction body: "), ": ")
				reply = map[string]any{"error": "FaultRefused", "reason": reason, "detail": detail}
			}
			replies = append(replies, reply)
			if variant == "accept" && err == nil {
				complete, e := f1Complete(ctx, l, map[string]string{"--publication": pub, "--claim-token": token, "--readback": reply.(map[string]any)["block"].(string), "--external-ref": "PROJECT-7"})
				if e != nil {
					t.Fatal(e)
				}
				replies = append(replies, complete)
			}
			fcComparePath(t, ctx, s, gd, "pre_issue", variant, replies, calls)
		})
	}
}

// fcComparePath compares the replies, hook calls and fault, journal and supervisor tables one
// path through the store in gd produced with the golden.
func fcComparePath(t *testing.T, ctx context.Context, s *store.Store, gd, action, variant string, replies, calls []any) {
	t.Helper()
	tables := testsupport.TableRows(t, s.DB, "name LIKE 'fault_%' OR name='journal' OR name LIKE 'supervisor_%'")
	gotRaw, err := json.Marshal(map[string]any{"replies": replies, "calls": calls, "tables": tables})
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err = json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatal(err)
	}
	checkGoldenEvidence(t, "replies, calls and tables: "+action+" "+variant, []string{action, variant}, runPathsOf(t, filepath.Dir(gd)), got)
}

func fcOpen(t *testing.T, ctx context.Context, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(ctx, dir+"/relay.sqlite3", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
