package store

import (
	"fmt"
	"testing"
)

// The two tables the region grades add (CRW-409) carry their constraints in the table, so a writer bug cannot store a grade the scheduler would not read: a mechanical grade names a rule
// of the closed set and no other grade names one, the grade and the rule belong to a declared region, a conflict observation names each conflicted file once, and a file belongs to an observation.
func TestDAGZoneGradeTablesRefuseBadRows(t *testing.T) {
	db := zoneOpenedDB(t)
	region := func(path string) string {
		return fmt.Sprintf("INSERT INTO dag_node_regions VALUES ('plan-1','node-a',1,'owner/repo','%s','file','','edit',0,'parent','2026-10-03T00:00:00Z')", path)
	}
	grade := func(path, grade, rule string) string {
		return fmt.Sprintf("INSERT INTO dag_node_region_grades VALUES ('plan-1','node-a',1,'owner/repo','%s','file','','%s','%s')", path, grade, rule)
	}
	zoneMustExec(t, db, "INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-03T00:00:00Z')",
		region("a.go"), region("b.go"), region("c.go"), region("d.go"), region("e.go"), region("f.go"), region("g.go"))
	zoneMustExec(t, db, grade("a.go", "mechanical", "union"), grade("b.go", "mechanical", "renumber"), grade("c.go", "mechanical", "regenerate:make generate"),
		grade("d.go", "local", ""), grade("e.go", "exclusive", ""), grade("f.go", "independent", ""))
	zoneRefuses(t, db, "a second grade for one region", grade("a.go", "local", ""))
	zoneRefuses(t, db, "a grade for a region nobody declared", grade("z.go", "local", ""))
	zoneRefuses(t, db, "a mechanical grade with no rule", grade("g.go", "mechanical", ""))
	zoneRefuses(t, db, "a mechanical grade with a rule outside the set", grade("g.go", "mechanical", "merge"))
	zoneRefuses(t, db, "a regenerate rule with no command", grade("g.go", "mechanical", "regenerate:"))
	zoneRefuses(t, db, "a rule on a local grade", grade("g.go", "local", "union"))
	zoneRefuses(t, db, "a rule on an exclusive grade", grade("g.go", "exclusive", "union"))
	zoneRefuses(t, db, "a rule on an independent grade", grade("g.go", "independent", "renumber"))
	zoneRefuses(t, db, "a grade outside the vocabulary", grade("g.go", "shared", ""))

	observation := fmt.Sprintf("INSERT INTO dag_conflict_observations VALUES ('obs-1','plan-1','node-a','node-b','/checkout','%s','%s','%s',2,'git merge-tree --write-tree','task-a','2026-10-03T00:00:00Z')",
		"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222", "3333333333333333333333333333333333333333")
	file := func(observation, repository, path string) string {
		return fmt.Sprintf("INSERT INTO dag_conflict_observation_files VALUES ('%s','%s','%s')", observation, repository, path)
	}
	zoneMustExec(t, db, observation, file("obs-1", "/checkout", "a.go"), file("obs-1", "/checkout", "b.go"), file("obs-1", "owner/repo", "a.go"))
	zoneRefuses(t, db, "a file named twice for one observation and repository", file("obs-1", "/checkout", "a.go"))
	zoneRefuses(t, db, "a file of an observation that does not exist", file("obs-none", "/checkout", "a.go"))
	zoneRefuses(t, db, "an empty path", file("obs-1", "/checkout", ""))
	zoneRefuses(t, db, "an empty repository", file("obs-1", "", "c.go"))
}
