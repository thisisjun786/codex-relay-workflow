package faults

import (
	"fmt"
	"strings"
	"testing"
)

func Test22_FC_5_TargetOwnershipWholeOutput(t *testing.T) {
	for _, variant := range []string{"no_project", "retarget_team", "retarget_project", "ownerless", "foreign_product"} {
		t.Run(variant, func(t *testing.T) {
			ctx, gd, pd := f1ReplayStores(t)
			target := []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team"}
			if variant != "no_project" {
				target = append(target, "--project-ref", "project-P")
			}
			f1ReplayCLI(t, ctx, gd, pd, target)
			a := replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "target")
			pub := a["publication"].(map[string]any)["publicationId"].(string)
			if variant == "no_project" {
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-next"})
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-claim", "--publication", pub, "--owner", "writer"})
				return
			}
			claim := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-claim", "--publication", pub, "--owner", "writer"})
			switch variant {
			case "retarget_team":
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "new-team", "--project-ref", "project-P"})
			case "retarget_project":
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "new-project"})
			case "ownerless":
				f1SeedBoth(t, ctx, gd, pd, []string{"DELETE FROM fault_target_projects"})
			case "foreign_product":
				f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_target_projects SET product='other'"})
			}
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-operation", "--publication", pub, "--claim-token", claim["claimToken"].(string)})
		})
	}
}

func Test22_FC_14_WorkspaceIsolationWholeOutput(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(fmt.Sprintf("moved_%t", moved), func(t *testing.T) {
			ctx, gd, pd := f1ReplayStores(t)
			observe := func(workspace, key string) string {
				scope := map[string]any{"projectKey": "CRW"}
				if workspace != "" {
					scope["workspace"] = workspace
				}
				a := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-observe", "--observation", dumps(map[string]any{"schema": "fault-observation/1", "product": "crw", "faultClass": "delivery_stalled", "severity": "broken", "signature": map[string]any{"recipient": "p", "attemptState": nil}, "occurrenceKey": key, "scope": scope, "detail": "held", "evidence": []any{map[string]any{"kind": "row", "ref": "deliveries"}}}, false)})
				return a["faultId"].(string)
			}
			if !moved {
				observe("", "own:1")
				observe("ws-B", "other:1")
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep"})
				return
			}
			id := observe("unassigned", "own:1")
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-move", "--fault", id, "--scope", `{"projectKey":"CRW","workspace":"ws-B"}`})
			s := fcOpen(t, ctx, gd)
			l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
			sw := &Sweeper{Store: s, Workspace: "unassigned", Now: l.Clock.ISO}
			batch, err := sw.Sweep(ctx, "crw")
			if err != nil {
				t.Fatal(err)
			}
			r, err := sw.RecordAll(ctx, l, batch)
			if err != nil {
				t.Fatal(err)
			}
			fcComparePath(t, ctx, s, pd, "workspace", "", []any{map[string]any{"read": r.Read, "recorded": r.Recorded, "queued": r.Queued, "gaps": r.Gaps, "results": r.Results}}, []any{})
		})
	}
}

func Test22_FC_29_ClaimLeaseBudgetWholeOutput(t *testing.T) {
	for _, variant := range []string{"lapsed_claim", "lapsed_issue", "at_budget", "consume_boundary"} {
		t.Run(variant, func(t *testing.T) {
			ctx, gd, pd := f1ReplayStores(t)
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team", "--project-ref", "project-P"})
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-limit", "--product", "crw", "--kind", "open_record", "--max-count", "1", "--window", "3600"})
			a := replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "lease")
			pub := a["publication"].(map[string]any)["publicationId"].(string)
			if variant == "consume_boundary" {
				// A SQLite trigger spends the last unit between classification and
				// consumption, exercising the transactional budget guard itself.
				f1SeedBoth(t, ctx, gd, pd, []string{`CREATE TRIGGER spend_on_claim AFTER INSERT ON fault_publication_attempts BEGIN INSERT INTO fault_budget_uses(product,kind,ref,used_at,used_ts) VALUES('crw','open_record','trigger','stamp',100000); END`})
				s := fcOpen(t, ctx, gd)
				l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
				reply, err := f1Claim(ctx, l, map[string]string{"--publication": pub, "--owner": "writer"})
				if err != nil {
					reason, detail, _ := strings.Cut(strings.TrimPrefix(err.Error(), "transaction body: "), ": ")
					reply = map[string]any{"error": "FaultRefused", "reason": reason, "detail": detail}
				}
				fcComparePath(t, ctx, s, pd, "claim_budget", "", []any{reply}, []any{})
				return
			}
			claim := f1ReplayCLI(t, ctx, gd, pd, []string{"fault-claim", "--publication", pub, "--owner", "writer"})
			if variant == "at_budget" {
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-fail", "--publication", pub, "--claim-token", claim["claimToken"].(string), "--error", "before issue"})
				f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_publications SET next_attempt_at=0"})
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-claim", "--publication", pub, "--owner", "writer"})
				return
			}
			if variant == "lapsed_issue" {
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-operation", "--publication", pub, "--claim-token", claim["claimToken"].(string)})
			}
			f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_publications SET lease_until=99999"})
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-next"})
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-claim", "--publication", pub, "--owner", "writer"})
		})
	}
}

func Test22_FC_27_CreatesRequireBlockWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	err := RegisterKind("fields_create_test", kindPolicy{Creates: true, RequiresIssue: true, Evidence: "fields"})
	t.Cleanup(func() { delete(kinds, "fields_create_test") })
	var reply any
	if err != nil {
		reply = map[string]any{"error": "ValueError", "detail": err.Error()}
	}
	_, registered := kinds["fields_create_test"]
	spec := kinds["update_record"]
	fcComparePath(t, ctx, fcOpen(t, ctx, gd), pd, "fields_create", "", []any{reply, map[string]any{"registered": registered}, map[string]any{"creates": spec.Creates, "requires_issue": spec.RequiresIssue, "target": nilIfEmpty(spec.Target), "evidence": spec.Evidence}}, []any{})
}

func Test22_PruneAliasesAndJournalWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-prune", "--fault", "nosuch", "--keep", "1"})
	a := replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "one")
	id := a["faultId"].(string)
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "two")
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "three")
	f1SeedBoth(t, ctx, gd, pd, []string{"INSERT INTO fault_aliases VALUES('alias','" + id + "','stamp')"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-prune", "--fault", "alias", "--keep", "1"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-prune", "--fault", id, "--keep", "1"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-fix", "--fault", "alias", "--ref", "fix"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-reverify", "--fault", "alias", "--ref", "check", "--method", "suite", "--outcome", "passed"})
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-resolve", "--fault", "alias"})
}

func Test22_UnknownFaultDetailsWholeOutput(t *testing.T) {
	for _, args := range [][]string{
		{"fault-resolve", "--fault", "nosuch"},
		{"fault-fix", "--fault", "nosuch", "--ref", "fix"},
		{"fault-reverify", "--fault", "nosuch", "--method", "suite", "--ref", "check", "--outcome", "passed"},
	} {
		t.Run(args[0], func(t *testing.T) {
			ctx, gd, pd := f1ReplayStores(t)
			f1ReplayCLI(t, ctx, gd, pd, args)
		})
	}
}
