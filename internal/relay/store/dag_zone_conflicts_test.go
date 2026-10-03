package store

import (
	"fmt"
	"testing"
)

// The tables the conflict sweep adds (CRW-410) carry their constraints in the table, so a writer bug cannot store a row the reading would misread: a tip observation is one row per node, head, tip and base;
// drift names a node and a path once; a ledger row has a known trigger and a hook's trigger is unique per reference (a manual sweep repeats); a member is a pair (nodes in sorted order) or a tip, is
// observed with an observation id or unmeasured with a reason from the closed set, and belongs to a ledger row.
func TestDAGZoneConflictSweepTablesRefuseBadRows(t *testing.T) {
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, "INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-03T00:00:00Z')")
	sha := func(c string) string { return fmt.Sprintf("%040s", c) }
	tip := func(id, node, head, source string) string {
		return fmt.Sprintf("INSERT INTO dag_tip_conflict_observations VALUES ('%s','plan-1','%s','/checkout','%s','%s','owner/repo@dev','%s','%s',1,'git merge-tree --write-tree','task-a','2026-10-03T00:00:00Z')",
			id, node, head, source, sha("2"), sha("3"))
	}
	zoneMustExec(t, db, tip("dto-1", "node-a", sha("1"), "acceptance"))
	zoneRefuses(t, db, "the same node, head, tip and base twice", tip("dto-2", "node-a", sha("1"), "explicit"))
	zoneRefuses(t, db, "a head source outside the vocabulary", tip("dto-3", "node-a", sha("4"), "work_report"))
	zoneRefuses(t, db, "an observation of no plan", "INSERT INTO dag_tip_conflict_observations VALUES ('dto-4','plan-none','node-a','/checkout','"+sha("5")+"','explicit','x','"+sha("2")+"','"+sha("3")+"',0,'m','task-a','t')")
	zoneRefuses(t, db, "a negative conflict count", "INSERT INTO dag_tip_conflict_observations VALUES ('dto-5','plan-1','node-a','/checkout','"+sha("6")+"','explicit','x','"+sha("2")+"','"+sha("3")+"',-1,'m','task-a','t')")
	file := func(id, repository, path string) string {
		return fmt.Sprintf("INSERT INTO dag_tip_conflict_observation_files VALUES ('%s','%s','%s')", id, repository, path)
	}
	zoneMustExec(t, db, file("dto-1", "/checkout", "a.go"), file("dto-1", "owner/repo", "a.go"))
	zoneRefuses(t, db, "a file named twice", file("dto-1", "/checkout", "a.go"))
	zoneRefuses(t, db, "a file of an observation that does not exist", file("dto-none", "/checkout", "a.go"))
	zoneRefuses(t, db, "an empty path", file("dto-1", "/checkout", ""))

	drift := func(id, node, path string) string {
		return fmt.Sprintf("INSERT INTO dag_conflict_drift VALUES ('%s','%s','%s')", id, node, path)
	}
	zoneMustExec(t, db, drift("dco-1", "node-a", "x.go"), drift("dco-1", "node-b", "x.go"), drift("dto-1", "node-a", "x.go"))
	zoneRefuses(t, db, "drift named twice for a node and a path", drift("dco-1", "node-a", "x.go"))
	zoneRefuses(t, db, "drift of no node", drift("dco-1", "", "y.go"))
	zoneRefuses(t, db, "drift of no path", drift("dco-1", "node-a", ""))

	sweep := func(seq int, kind, node, ref string) string {
		return fmt.Sprintf("INSERT INTO dag_conflict_sweeps VALUES ('plan-1',%d,'%s','%s','%s','/checkout','task-a','2026-10-03T00:00:00Z')", seq, kind, node, ref)
	}
	zoneMustExec(t, db, sweep(1, "landing", "node-a", "dio-1"), sweep(2, "manual", "", ""), sweep(3, "manual", "", ""), sweep(4, "receipt", "node-a", "acc-1"), sweep(5, "landing", "node-b", "dio-2"))
	zoneRefuses(t, db, "a second sweep of one landing", sweep(6, "landing", "node-a", "dio-1"))
	zoneRefuses(t, db, "a second sweep of one acceptance", sweep(6, "receipt", "node-a", "acc-1"))
	zoneRefuses(t, db, "the same sequence twice", sweep(1, "manual", "", ""))
	zoneRefuses(t, db, "a trigger outside the vocabulary", sweep(6, "wake", "", ""))
	zoneRefuses(t, db, "a sweep of no plan", "INSERT INTO dag_conflict_sweeps VALUES ('plan-none',1,'manual','','','/checkout','task-a','t')")
	zoneRefuses(t, db, "a sweep with no checkout", "INSERT INTO dag_conflict_sweeps VALUES ('plan-1',7,'manual','','','','task-a','t')")

	member := func(seq, n int, kind, left, right, status, reason, obs string, conflicts int) string {
		return fmt.Sprintf("INSERT INTO dag_conflict_sweep_members VALUES ('plan-1',%d,%d,'%s','%s','%s','%s','%s','acceptance','child_checkout','%s','%s','%s',%d)", seq, n, kind, left, right, sha("a"), sha("b"), status, reason, obs, conflicts)
	}
	zoneMustExec(t, db,
		member(1, 1, "pair", "node-a", "node-b", "observed", "", "dco-1", 2),
		member(1, 2, "pair", "node-a", "node-c", "replayed", "", "dco-2", 0),
		member(1, 3, "tip", "node-a", "", "observed", "", "dto-1", 1),
		member(1, 4, "pair", "node-b", "node-c", "unmeasured", "commit_missing", "", 0),
		member(1, 5, "tip", "node-b", "", "unmeasured", "tip_unreadable", "", 0))
	zoneRefuses(t, db, "a member twice", member(1, 1, "pair", "node-a", "node-d", "observed", "", "dco-3", 0))
	zoneRefuses(t, db, "a member of no sweep", member(9, 1, "pair", "node-a", "node-d", "observed", "", "dco-3", 0))
	zoneRefuses(t, db, "a pair whose nodes are not in sorted order", member(2, 1, "pair", "node-b", "node-a", "observed", "", "dco-3", 0))
	zoneRefuses(t, db, "a pair with no second node", member(2, 1, "pair", "node-a", "", "observed", "", "dco-3", 0))
	zoneRefuses(t, db, "a tip with a second node", member(2, 1, "tip", "node-a", "node-b", "observed", "", "dto-3", 0))
	zoneRefuses(t, db, "an observed member with no observation", member(2, 1, "pair", "node-a", "node-b", "observed", "", "", 0))
	zoneRefuses(t, db, "an unmeasured member with an observation", member(2, 1, "pair", "node-a", "node-b", "unmeasured", "commit_missing", "dco-3", 0))
	zoneRefuses(t, db, "an unmeasured member with no reason", member(2, 1, "pair", "node-a", "node-b", "unmeasured", "", "", 0))
	zoneRefuses(t, db, "a measured member with a reason", member(2, 1, "pair", "node-a", "node-b", "observed", "commit_missing", "dco-3", 0))
	zoneRefuses(t, db, "a reason outside the closed set", member(2, 1, "pair", "node-a", "node-b", "unmeasured", "unreachable", "", 0))
	zoneRefuses(t, db, "a status outside the vocabulary", member(2, 1, "pair", "node-a", "node-b", "failed", "", "dco-3", 0))
	zoneRefuses(t, db, "a kind outside the vocabulary", member(2, 1, "single", "node-a", "", "observed", "", "dco-3", 0))
}
