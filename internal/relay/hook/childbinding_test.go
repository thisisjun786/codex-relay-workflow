package hook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestChildBinding (CRW-1300): the relay child push guard's binding read. A home without the completion hook's settings, or a
// selected store that does not exist, has no relay child; the store is the settings' dbPath, else relay.sqlite3 in the relay's
// state selection; the newest relationship of the child gives the parent's working directory (none when it is NULL); settings or a
// store that cannot be read are an error.
func TestChildBinding(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	home, state := filepath.Join(dir, "codex"), filepath.Join(dir, "state")
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_SESSION_RELAY_STATE", state)
	settings := func(db string) {
		raw := `{"configVersion": 1, "event": "Stop", "relayExecutable": "/nonexistent/relay", "markerRoot": "` + dir + `/markers", ` + db +
			`"mode": "observe", "timeoutSeconds": 5, "journalRoot": "` + dir + `/journal", "journalPolicy": "every_invocation", "installedBy": "CRW-37", "isolationAssertedBy": null}`
		writeTest(t, filepath.Join(home, ConfigName), []byte(raw))
	}
	check := func(what, session string, child bool, parent, problem string) {
		t.Helper()
		c, p, err := ChildBinding(ctx, session)
		if c != child || p != parent || (err == nil) != (problem == "") || err != nil && !strings.Contains(err.Error(), problem) {
			t.Errorf("%s, %s: child %v, parent %q, error %v", what, session, c, p, err)
		}
	}
	check("no settings", "kid", false, "", "")
	settings(`"dbPath": "` + filepath.Join(dir, "absent", "relay.sqlite3") + `", `)
	check("a store that does not exist", "kid", false, "", "")
	db := filepath.Join(state, "relay.sqlite3")
	s, err := fixtureStore(ctx, db, filepath.Join(dir, "app.sock"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ id, child, parent, at string }{{"rel-1", "kid", "/old", "2026-10-12T10:00:00Z"}, {"rel-2", "kid", "/new", "2026-10-12T11:00:00Z"}, {"rel-3", "orphan", "", "2026-10-12T10:00:00Z"}} {
		if _, err := s.DB.Exec("INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,'ISS-1','active','par','host',NULLIF(?,''),?,'host',NULL,1,'[]','[]',?,?)", r.id, r.parent, r.child, r.at, r.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	settings("")
	check("the state selection's store", "kid", true, "/new", "")
	check("the state selection's store", "orphan", true, "", "")
	check("the state selection's store", "par", false, "", "")
	settings(`"dbPath": "` + db + `", `)
	check("dbPath", "kid", true, "/new", "")
	bad := filepath.Join(dir, "bad.sqlite3")
	writeTest(t, bad, []byte("this is not a database"))
	settings(`"dbPath": "` + bad + `", `)
	check("a store that cannot be read", "kid", false, "", "database")
	writeTest(t, filepath.Join(home, ConfigName), []byte("{"))
	check("settings that cannot be read", "kid", false, "", ":")
	if err := os.Remove(filepath.Join(home, ConfigName)); err != nil {
		t.Fatal(err)
	}
	check("no settings again", "kid", false, "", "")
}
