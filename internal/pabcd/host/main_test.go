package host

import (
	"database/sql"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) { testsupport.Main(m) }

func envOf(vars map[string]string) LookupEnv {
	return func(key string) (string, bool) { value, set := vars[key]; return value, set }
}

func seed(t *testing.T, path, statement string, args ...any) { // creates the database and runs one statement
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}
