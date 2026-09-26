package contracttest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scenarioFrom parses a scenario written in the fixture grammar through the same parser Load uses.
func scenarioFrom(t *testing.T, text string) Scenario {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.json")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	scenario, err := parse("synthetic", path)
	if err != nil {
		t.Fatal(err)
	}
	return scenario
}

func TestCLIRunner_observes_exit_stderr_and_step_results_of_the_built_crw(t *testing.T) {
	// Given: a two-step scenario. The first step is an argparse refusal (exit 2, usage on
	// stderr); the second references the first step's output and is Python's usage exit (4).
	scenario := scenarioFrom(t, `{"run":{"kind":"cli","steps":[
		{"id":"first","argv":["store-challenge","--bogus"],"stdin":{"x":1}},
		{"id":"second","argv":["store-challenge","--actor",{"$step":"first","path":["stderr"]}]}]},
		"expect":{"exit":4,"checks":[
			{"kind":"eq","path":["steps","first","exit"],"value":2},
			{"kind":"contains","path":["steps","first","stderr"],"value":"usage: crw relay store-challenge"},
			{"kind":"eq","path":["steps","first","stdout_json"],"value":null},
			{"kind":"eq","path":["stdout_json","error"],"value":"usage"}]}}`)
	// When
	actual, err := runCLI(t, scenario)
	// Then
	if err != nil {
		t.Fatal(err)
	}
	if err := Assert(scenario, actual); err != nil {
		t.Fatal(err)
	}
}

func TestCLIRunner_fails_a_scenario_whose_exit_differs(t *testing.T) {
	// Given: an expectation the binary does not meet.
	scenario := scenarioFrom(t, `{"run":{"kind":"cli","argv":["store-challenge"]},"expect":{"exit":0}}`)
	// When
	actual, err := runCLI(t, scenario)
	if err != nil {
		t.Fatal(err)
	}
	// Then
	if err := Assert(scenario, actual); !errors.Is(err, errCheck) || !strings.Contains(err.Error(), scenario.ID) {
		t.Fatalf("want a check failure naming %s, got %v", scenario.ID, err)
	}
}

func TestCLIRunner_reads_sqlite_rows_and_file_observations(t *testing.T) {
	// Given: a standalone database from given.sql and a text file from given.files.
	scenario := scenarioFrom(t, `{"given":{"sql":"CREATE TABLE t (a TEXT, b INTEGER); INSERT INTO t VALUES ('x', 7);",
		"files":{"note.txt":"hi"}},
		"run":{"kind":"cli","argv":["store-challenge"]},
		"expect":{"exit":4,"files":{"note.txt":true,"absent":false},"observe":["note.txt","state"],
			"queries":{"rows":"SELECT a, b FROM t"},
			"checks":[{"kind":"eq","path":["sql","rows"],"value":[["x",7]]},
				{"kind":"bytes","path":["observed","note.txt","bytes"],"value":"6869"},
				{"kind":"eq","path":["observed","note.txt","mode"],"value":420},
				{"kind":"eq","path":["observed","state","entries"],"value":["relay.sqlite3"]}]}}`)
	// When
	actual, err := runCLI(t, scenario)
	// Then
	if err != nil {
		t.Fatal(err)
	}
	if err := Assert(scenario, actual); err != nil {
		t.Fatal(err)
	}
}

func TestCLIRunner_reports_an_unregistered_relay_command_as_not_ported(t *testing.T) {
	// Given: a command the Go relay does not register yet.
	scenario := scenarioFrom(t, `{"run":{"kind":"cli","argv":["no-such-command"]},"expect":{"exit":0}}`)
	// When
	_, err := runCLI(t, scenario)
	// Then: a counted skip, never a run against the usage path.
	if !errors.Is(err, ErrNotPorted) {
		t.Fatalf("want ErrNotPorted, got %v", err)
	}
}

func TestCLIRunner_seeds_given_sql_seed_through_the_real_relay_store(t *testing.T) {
	// Given: given.sql_seed on top of the relay schema, read back with a query.
	scenario := scenarioFrom(t, `{"given":{"sql_seed":"INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES ('r','p','t');"},`+
		`"run":{"kind":"cli","argv":["dispositions-show","--relationship","r"]},"expect":{"exit":0,"queries":{"seeded":"SELECT project_key FROM relationship_scope"}}}`)
	// When
	actual, err := runCLI(t, scenario)
	// Then: the seed landed in a relay store the command could read.
	if err != nil {
		t.Fatal(err)
	}
	seeded := actual["sql"].(map[string]any)["seeded"].([]any)
	if len(seeded) != 1 || actual["exit"] != float64(0) {
		t.Fatalf("seeded %v exit %v", seeded, actual["exit"])
	}
}
