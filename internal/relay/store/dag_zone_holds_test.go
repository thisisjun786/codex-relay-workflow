package store

import (
	"fmt"
	"testing"
)

// The table CRW-431 adds carries its constraints in the table, so a writer bug cannot store a hold the scheduler would misread: a row belongs to a declared region, a region has one row, and the statement is a
// yes or a no.
func TestDAGZoneHoldsTableRefusesBadRows(t *testing.T) {
	db := zoneOpenedDB(t)
	region := func(path string) string {
		return fmt.Sprintf("INSERT INTO dag_node_regions VALUES ('plan-1','node-a',1,'owner/repo','%s','file','','edit',0,'parent','2026-10-03T00:00:00Z')", path)
	}
	hold := func(path string, stated any) string {
		return fmt.Sprintf("INSERT INTO dag_node_region_holds VALUES ('plan-1','node-a',1,'owner/repo','%s','file','',%v)", path, stated)
	}
	zoneMustExec(t, db, "INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-03T00:00:00Z')", region("a.go"), region("b.go"), region("c.go"))
	zoneMustExec(t, db, hold("a.go", 0), hold("b.go", 1))
	zoneRefuses(t, db, "a second row for one region", hold("a.go", 1))
	zoneRefuses(t, db, "a row for a region nobody declared", hold("z.go", 0))
	zoneRefuses(t, db, "a statement that is neither yes nor no", hold("c.go", 2))
	zoneRefuses(t, db, "no statement", hold("c.go", "NULL"))
}
