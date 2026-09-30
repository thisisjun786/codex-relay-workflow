package cli_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// clockReading is a clock reading, which differs between the two runs of one registration.
var clockReading = regexp.MustCompile(`"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+\+00:00"`)

// An argument that is not UTF-8 (an argv byte such as 0xff, which Python holds surrogate-escaped)
// is refused where the fence first encodes it: hashing it into an identity, or binding it into
// SQLite as TEXT, which raises UnicodeEncodeError and cli.main answers as a host error (exit 3). The
// Go CLI refuses at the same point in the same words, so it never writes TEXT Python's sqlite3
// cannot decode and never looks such a string up where Python raises; a reader Python lets the
// error escape from raises it too. An argument Python stores inside JSON is written as json.dumps
// writes it ("\udcff"), and a refusal that echoes it is its repr(). Both runtimes answer each argv
// in a store of their own, the Go CLI as the built binary with every command family it carries;
// the printed answers must be the same bytes. An artifact root, which the registration stores
// inside JSON, is stored as the same TEXT, which Python's sqlite3 reads back from either store.
func TestAnArgumentThatIsNotUTF8IsRefusedWherePythonEncodesIt(t *testing.T) {
	_, relay := packageBinary(t)
	root := t.TempDir()
	// A relative workspace is refused at its position in the absolute path, which starts with
	// the working directory: one at a fixed path spells the same position on every run.
	t.Chdir(fixedTree(t, t.Name()))
	artifacts := filepath.Join(root, "artifacts")
	register := func(field, value string) []string {
		argv := []string{"--json", "register", "--parent-task", "p", "--parent-host", "h", "--child-task", "c", "--child-host", "h", "--issue", "T",
			"--artifact-root", artifacts, "--allowed-recipient", "p", "--dispatch-request-id", "d1"}
		for i := range argv {
			if argv[i] == field {
				argv[i+1] = value
			}
		}
		return argv
	}
	argvs := [][]string{
		// The relationship identity hashes parent|child|issue before anything is read.
		register("--issue", "T\xff"),
		register("--child-task", "c\xfe\xff"),
		// Looked up: sqlite3 refuses to bind it.
		{"--json", "relationship-status", "--relationship", "x\xffy", "--status", "paused", "--actor", "a"},
		{"--json", "show", "--event", "e\xff"},
		{"--json", "settings-show", "--task", "t\xff"},
		{"--json", "criteria-show", "--relationship", "r\xff"},
		{"--json", "assignment-find", "--issue", "i\xff"},
		{"--json", "supervisor-show", "--message", "m\xff"},
		{"--json", "generation-open", "--relationship", "r\xff", "--dispatch-request-id", "d"},
		{"--json", "fault-resolve", "--fault", "f\xff"},
		{"--json", "sync-retry", "--sync", "s\xff"},
		{"--json", "merge-turn-ready", "--turn", "t\xff", "--actor", "a", "--ready"},
		// doctor's read-only lookups let the error escape (read_only_rows and nonce_lookup
		// catch sqlite3.Error only), once a store the lines above opened holds their tables.
		{"--json", "doctor", "--issue", "x\xffy"},
		{"--json", "doctor", "--expect-nonce", "x\xffy"},
		// Linkage identities hash their fields first; its readers let the error escape.
		{"--json", "linkage-bind", "--role", "parent", "--scope", "s\xff", "--task", "t", "--host", "h"},
		{"--json", "linkage-handover", "--role", "supervisor", "--scope", "v", "--expect-task", "v", "--task", "x\xffy", "--host", "v", "--evidence", "v", "--actor", "v"},
		{"--json", "linkage-supervise", "--initiative", "x\xffy", "--project", "v", "--supervisor-task", "v", "--supervisor-host", "v", "--parent-task", "v", "--parent-host", "v"},
		{"--json", "linkage-peer", "--left-project", "x\xffy", "--left-task", "v", "--left-host", "v", "--right-project", "v", "--right-task", "v", "--right-host", "v"},
		{"--json", "linkage-down", "--scope-kind", "initiative", "--scope", "x\xffy"},
		{"--json", "linkage-up", "--task", "x\xffy"},
		{"--json", "linkage-counterpart", "--from-task", "x\xffy", "--to-task", "v"},
		{"--json", "linkage-completion", "--project", "x\xffy"},
		{"--json", "dispositions-show", "--project", "x\xffy"},
		// Coordination identities hash their fields before the request reads anything.
		{"--json", "merge-turn-request", "--repository", "v", "--base-ref", "x\xffy", "--project", "v", "--task", "v", "--host", "v", "--head", "v"},
		{"--json", "region-propose", "--repository", "v", "--revision", "v", "--path", "x\xffy", "--kind", "tree", "--left-project", "v", "--right-project", "v", "--peer-link", "v", "--task", "v", "--constraint", "v"},
		{"--json", "region-followup", "--agreement", "v", "--trigger", "x\xffy", "--acceptance", "v", "--recorded-by", "v"},
		// The marker identities hash the workspace path and the dispatch request id.
		{"--json", "intent-declare", "--workspace", "x\xffy", "--dispatch-request-id", "v", "--issue", "v"},
		{"--json", "intent-declare", "--workspace", "v", "--dispatch-request-id", "x\xffy", "--issue", "v"},
		{"--json", "intent-claim", "--workspace", "v", "--assignment", "v", "--session", "v", "--dispatch-request-id", "x\xffy"},
		// A managed-start request is encoded before it is parsed.
		{"--socket", filepath.Join(root, "app.sock"), "--json", "managed-start", "--request", "{\"a\": \"\xff\"}", "--marker-root", filepath.Join(root, "markers")},
		// Echoed in a refusal: repr() of the str.
		{"--json", "fault-queue", "--fault", "v", "--kind", "x\xffy", "--trigger", "v"},
		{"--json", "fault-policy", "--product", "x\xffy"},
	}
	var pythonArgvs [][]string
	for _, argv := range argvs {
		pythonArgvs = append(pythonArgvs, append([]string{"--state", filepath.Join(root, "python")}, argv...))
	}
	argvs = append(argvs, register("--artifact-root", "a\xff"))
	pythonArgvs = append(pythonArgvs, append([]string{"--state", filepath.Join(root, "python")}, argvs[len(argvs)-1]...))
	want := pythonCLI(t, pythonArgvs...)
	// What Python's sqlite3 read back from the store Python registered in (recorded; see
	// oracleRun). Go's store is read below, by Go, as TEXT.
	stored := `import json, sqlite3, sys
print(json.dumps([sqlite3.connect(path).execute("SELECT artifact_roots FROM relationships").fetchall() for path in sys.argv[1:]]))`
	pythonDatabase := filepath.Join(root, "python", "relay.sqlite3")
	pythonStored := oracleRun(t, "stored artifact roots", func() (run, error) {
		command := exec.Command(filepath.Join(repositoryRoot(t), ".venv", "bin", "python"), "-c", stored, pythonDatabase)
		command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		raw, err := command.CombinedOutput()
		if err != nil {
			return run{}, fmt.Errorf("python reading its store: %v\n%s", err, raw)
		}
		return run{stdout: string(raw)}, nil
	}, placeholders(pythonDatabase)...)
	for i, argv := range argvs {
		if i == len(argvs)-1 {
			// Printed back through Go's reader, which decodes the stored escape as U+FFFD
			// (docs/port/known-defects.md): the stored TEXT is compared below instead.
			if err := exec.Command(relay, append([]string{"--state", filepath.Join(root, "go")}, argv...)...).Run(); err != nil || want[i].code != 0 {
				t.Fatalf("registering an artifact root that is not UTF-8: go %v, python %d %s", err, want[i].code, want[i].stdout)
			}
			break
		}
		command := exec.Command(relay, append([]string{"--state", filepath.Join(root, "go")}, argv...)...)
		command.Env = os.Environ()
		var stdout bytes.Buffer
		command.Stdout = &stdout
		code := 0
		if err := command.Run(); err != nil {
			exit, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		got := answer{code, clockReading.ReplaceAllString(stdout.String(), `"<at>"`)}
		want[i].stdout = clockReading.ReplaceAllString(want[i].stdout, `"<at>"`)
		if got != want[i] {
			t.Errorf("%q:\ngo     %d %s\npython %d %s", argv, got.code, got.stdout, want[i].code, want[i].stdout)
		}
	}
	if want := `[[["[\"a\\udcff\"]"]]]` + "\n"; pythonStored.stdout != want {
		t.Errorf("stored artifact roots, Python's store: %s want %s", pythonStored.stdout, want)
	}
	database, err := sql.Open("sqlite", "file:"+filepath.Join(root, "go", "relay.sqlite3")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var kind string
	var text []byte
	if err = database.QueryRow("SELECT typeof(artifact_roots), CAST(artifact_roots AS BLOB) FROM relationships").Scan(&kind, &text); err != nil {
		t.Fatal(err)
	}
	if want := `["a\udcff"]`; kind != "text" || string(text) != want {
		t.Errorf("stored artifact roots, Go's store: %s %q want text %q", kind, text, want)
	}
}

// doctor prepares its read-only lookup before it binds the argument, as sqlite3's execute does:
// over a file that holds no relay tables, or that is not a database, the statement fails as that
// sqlite3.Error, which read_only_rows and nonce_lookup answer as a reading, before an argument
// that is not UTF-8 is bound. Only a store whose tables exist reaches the bind and raises
// (TestAnArgumentThatIsNotUTF8IsRefusedWherePythonEncodesIt).
func TestDoctorLookupsReadAStoreTheyCannotPrepareBeforeTheyBind(t *testing.T) {
	_, relay := packageBinary(t)
	root := t.TempDir()
	// Each runtime's store is prepared with its own sqlite3: Go's here, Python's in the capture
	// closure (see oracleRun), where the fence's answer is taken.
	execute := func(python bool, path, statement string) error {
		if python {
			command := exec.Command(filepath.Join(repositoryRoot(t), ".venv", "bin", "python"), "-c",
				"import sqlite3, sys; c = sqlite3.connect(sys.argv[1]); c.execute(sys.argv[2]); c.commit()", path, statement)
			if raw, err := command.CombinedOutput(); err != nil {
				return fmt.Errorf("%v\n%s", err, raw)
			}
			return nil
		}
		database, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			return err
		}
		_, err = database.Exec(statement)
		return errors.Join(err, database.Close())
	}
	garbage := bytes.Repeat([]byte("not a database \x00\xff"), 256)
	prepare := func(python bool, state, kind string) error {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return err
		}
		database := filepath.Join(state, "relay.sqlite3")
		switch kind {
		case "tables":
			return execute(python, database, "create table t(x)")
		case "garbage":
			return os.WriteFile(database, garbage, 0o600)
		case "dropped":
			// A relay store without the table the nonce lookup reads.
			registered := exec.Command(relay, "--state", state, "--json", "register", "--parent-task", "p", "--parent-host", "h",
				"--child-task", "c", "--child-host", "h", "--issue", "T", "--artifact-root", filepath.Join(root, "artifacts"),
				"--allowed-recipient", "p", "--dispatch-request-id", "d1")
			if raw, err := registered.CombinedOutput(); err != nil {
				return fmt.Errorf("%v\n%s", err, raw)
			}
			return execute(python, database, "drop table store_challenge")
		}
		return nil
	}
	for _, flag := range []string{"--issue", "--expect-nonce"} {
		for _, kind := range []string{"tables", "garbage", "dropped"} {
			states := map[string]string{"python": filepath.Join(root, flag, kind, "python"), "go": filepath.Join(root, flag, kind, "go")}
			if err := prepare(false, states["go"], kind); err != nil {
				t.Fatal(err)
			}
			argv := []string{"--json", "doctor", flag, "x\xffy"}
			pythonArgv := append([]string{"--state", states["python"]}, argv...)
			var recorded recordedRun
			pyoracle.JSON(t, flag+" over "+kind, &recorded, func() (any, error) {
				if err := prepare(true, states["python"], kind); err != nil {
					return nil, err
				}
				answers, err := livePythonCLI(pythonArgv)
				if err != nil {
					return nil, err
				}
				return recordedRun{Code: answers[0].code, Stdout: answers[0].stdout}, nil
			}, placeholders(pythonArgv...)...)
			want := answer{recorded.Code, recorded.Stdout}
			command := exec.Command(relay, append([]string{"--state", states["go"]}, argv...)...)
			var stdout bytes.Buffer
			command.Stdout = &stdout
			code := 0
			if err := command.Run(); err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if code != want.code || doctorLookup(t, stdout.String(), states["go"]) != doctorLookup(t, want.stdout, states["python"]) {
				t.Errorf("%s over %s:\ngo     %d %s\npython %d %s", flag, kind, code, stdout.String(), want.code, want.stdout)
			}
		}
	}
}

// doctorLookup is the part of a doctor answer the lookup decides: its issue or nonce reading, or
// the host error, with the state directory spelled <state>.
func doctorLookup(t *testing.T, printed, state string) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(printed), &document); err != nil {
		return printed
	}
	kept := map[string]any{}
	for _, key := range []string{"issue", "nonce", "error", "detail"} {
		if value, ok := document[key]; ok {
			kept[key] = value
		}
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), state, "<state>")
}
