package store

import (
	"strings"
	"testing"
)

// CRW-965 review: a SQLite rowid table's TEXT PRIMARY KEY admits NULL, and the appended CHECK (<> '') lets it through.
// The three appended key columns of the commit path are guarded by a BEFORE INSERT trigger, as the merge train's id is
// (CRW-768 decision 9). A NULL key is refused by that trigger's own message, not by a generic constraint.
func TestDAGZoneRefusesANullKeyOfTheCommitPath(t *testing.T) {
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneRawDB(t, path)
	for _, insert := range []string{
		"INSERT INTO dag_acceptance_verifications (acceptance_id) VALUES (NULL)",
		"INSERT INTO dag_integration_batches (batch_id) VALUES (NULL)",
		"INSERT INTO dag_integration_stages (stage_id) VALUES (NULL)",
	} {
		err := zoneExec(t, db, insert)
		if err == nil || !strings.Contains(err.Error(), "is NULL") {
			t.Fatalf("%s: got %v; want the trigger's refusal of a NULL key", insert, err)
		}
	}
}
