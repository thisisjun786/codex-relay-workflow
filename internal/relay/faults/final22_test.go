package faults

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func Test22_FaultTargetRelinkWholeOutput(t *testing.T) {
	for _, outstanding := range []bool{false, true} {
		t.Run(fmt.Sprintf("outstanding_%t", outstanding), func(t *testing.T) {
			ctx, gd, pd := f1ReplayStores(t)
			invoke := func(args ...string) map[string]any { return f1ReplayCLI(t, ctx, gd, pd, args) }
			target := func(project string) {
				args := []string{"fault-target", "--product", "crw", "--project", "P", "--team", "team"}
				if project != "" {
					args = append(args, "--project-ref", project)
				}
				invoke(args...)
			}
			target("P0")
			a := replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "target-relink")
			pub := a["publication"].(map[string]any)["publicationId"].(string)
			claim := invoke("fault-claim", "--publication", pub, "--owner", "writer")
			token := claim["claimToken"].(string)
			op := invoke("fault-operation", "--publication", pub, "--claim-token", token)
			invoke("fault-complete", "--publication", pub, "--claim-token", token, "--readback", op["block"].(string), "--external-ref", "ISSUE-1", "--project-ref", "P0")
			target("P1")
			if outstanding {
				// A pre-existing issued write may still land while an unsent P1
				// write exists. Cancellation must precede the outstanding check.
				f1SeedBoth(t, ctx, gd, pd, []string{
					`INSERT INTO fault_publications(publication_id,fault_id,kind,trigger_key,cycle,external_ref,summary,identity_digest,state,attempts,created_at,updated_at) SELECT 'old-issued',fault_id,kind,'old-issued',cycle,external_ref,summary,identity_digest,'issued',1,created_at,updated_at FROM fault_publications WHERE kind='update_record'`,
					`INSERT INTO fault_publication_payloads(publication_id,payload,updated_at,target_mode) VALUES('old-issued','{"op":"set_project","value":"P-old"}','stamp','none')`,
				})
			}
			target("P2")
			target("")
			target("P5")
			if outstanding {
				f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_publications SET state='cancelled' WHERE publication_id='old-issued'"})
				invoke("fault-relink")
			}
			// Returning to the observed project cancels the unsent P5 write.
			target("P0")
		})
	}
}

func Test22_FaultTargetUnchangedWholeOutput(t *testing.T) {
	for _, mode := range []string{"legacy", "team", "team+project"} {
		t.Run(mode, func(t *testing.T) {
			ctx, gd, pd := f1ReplayStores(t)
			target := func(team, project string) map[string]any {
				return f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--project", "P", "--team", team, "--project-ref", project})
			}
			target("old-team", "P0")
			replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "bounded")
			if mode != "legacy" {
				f1SeedBoth(t, ctx, gd, pd, []string{"UPDATE fault_publications SET kind='unloaded_extension'", "UPDATE fault_publication_payloads SET target_mode='" + mode + "'"})
			}
			f1SeedBoth(t, ctx, gd, pd, []string{
				`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<101) INSERT INTO fault_publications(publication_id,fault_id,kind,trigger_key,cycle,tracker_ref,summary,identity_digest,state,attempts,created_at,updated_at) SELECT 'batch-'||i,fault_id,kind,'batch-'||i,cycle,tracker_ref,summary,identity_digest,'pending',0,created_at,updated_at FROM n CROSS JOIN fault_publications WHERE trigger_key='open'`,
				`INSERT INTO fault_publication_payloads(publication_id,project_ref,updated_at,target_mode) SELECT p.publication_id,x.project_ref,x.updated_at,x.target_mode FROM fault_publications p CROSS JOIN fault_publication_payloads x WHERE p.publication_id LIKE 'batch-%'`,
			})
			target("new-team", "P2")
			// Fail even same-value writes during the unchanged call, not only
			// changes visible in the complete table snapshots. Store startup
			// maintains schema metadata independently of this command.
			s := fcOpen(t, ctx, gd)
			names, err := s.All(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND (name LIKE 'fault_%' OR name='journal')")
			if err != nil {
				t.Fatal(err)
			}
			var guards, drops []string
			for _, r := range names {
				for _, verb := range []string{"INSERT", "UPDATE", "DELETE"} {
					name := "no_write_" + text(r, "name") + "_" + verb
					guards = append(guards, "CREATE TRIGGER "+name+" BEFORE "+verb+" ON "+text(r, "name")+" BEGIN SELECT RAISE(ABORT,'unchanged target wrote'); END")
					drops = append(drops, "DROP TRIGGER "+name)
				}
			}
			f1SeedBoth(t, ctx, gd, pd, guards)
			a := target("new-team", "P2")
			if a["backfilled"] != float64(0) || a["backfillPending"] != float64(2) {
				t.Fatalf("bounded repeat: %v", a)
			}
			f1SeedBoth(t, ctx, gd, pd, drops)
			f1ReplayCLI(t, ctx, gd, pd, []string{"fault-relink"})
		})
	}
}

func Test22_FC_5_SweepNoProjectWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	f1ReplayCLI(t, ctx, gd, pd, []string{"fault-target", "--product", "crw", "--team", "team"})
	s := fcOpen(t, ctx, gd)
	l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
	sw := &Sweeper{Store: s, Now: l.Clock.ISO, HostRecordPath: testHostRecordPath(), Installation: Installation{Package: "codex-session-relay", Version: RelayPackageVersion, Location: relayPackageLocation}}
	batch, err := sw.SweepReadings(ctx, "crw", "", []any{map[string]any{"schema": "reporting-observation/1", "relationshipId": "rel", "selectors": map[string]any{"turn": "turn"}, "reportingState": "unreported"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	r, err := sw.RecordAll(ctx, l, batch)
	if err != nil {
		t.Fatal(err)
	}
	fcComparePath(t, ctx, s, pd, "sweep_no_project", "", []any{map[string]any{"read": r.Read, "recorded": r.Recorded, "queued": r.Queued, "gaps": r.Gaps, "results": r.Results}}, []any{})
}

func Test22_PruneDirectAliasWholeOutput(t *testing.T) {
	ctx, gd, pd := f1ReplayStores(t)
	a := replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "one")
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "two")
	replayObservation(t, ctx, gd, pd, "report_omitted", Broken, "three")
	f1SeedBoth(t, ctx, gd, pd, []string{"INSERT INTO fault_aliases VALUES('alias','" + a["faultId"].(string) + "','stamp')"})
	s := fcOpen(t, ctx, gd)
	l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
	removed, err := l.Prune(ctx, "alias", 1)
	if err != nil {
		t.Fatal(err)
	}
	fcComparePath(t, ctx, s, pd, "prune_alias", "", []any{removed}, []any{})
}

func Test22_FaultTargetValidationWholeOutput(t *testing.T) {
	for _, field := range []string{"product", "team", "workspace", "project", "project-ref"} {
		for _, value := range []string{"", " ", "a:b"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				ctx, gd, pd := f1ReplayStores(t)
				args := []string{"fault-target", "--product", "crw", "--team", "team", "--" + field, value}
				f1ReplayCLI(t, ctx, gd, pd, args)
			})
		}
	}
}

func Test22_FC_17_ReadingsBoundaryWholeOutput(t *testing.T) {
	reading := func(turn, state string) any {
		return map[string]any{"schema": "reporting-observation/1", "relationshipId": "rel", "selectors": map[string]any{"turn": turn}, "reportingState": state}
	}
	for _, variant := range []string{"boundary", "ceiling_1001", "last_wins"} {
		t.Run(variant, func(t *testing.T) {
			ctx, gd, pd := f1ReplayStores(t)
			readings := []any{}
			count := 33
			if variant == "ceiling_1001" {
				count = 1001
			}
			for i := 0; i < count; i++ {
				readings = append(readings, reading(fmt.Sprintf("turn-%04d", i), "unreported"))
			}
			if variant == "last_wins" {
				// Reduction happens before paging: the first turn moves to the
				// end with its newer answer, not the obsolete omission.
				readings = append(readings, reading("turn-0000", "reported"))
			}
			path := filepath.Join(t.TempDir(), "readings.json")
			if err := os.WriteFile(path, []byte(dumps(readings, false)), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"fault-sweep", "--readings", "@" + path}
			f1ReplayCLI(t, ctx, gd, pd, args)
			if variant != "ceiling_1001" {
				f1ReplayCLI(t, ctx, gd, pd, append(args, "--readings-after", "32"))
				// Exact exhaustion must not advertise an extra page.
				f1ReplayCLI(t, ctx, gd, pd, []string{"fault-sweep", "--readings", dumps(readings[:32], false)})
			}
		})
	}
}
