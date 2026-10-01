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
)

// clockReading is a clock reading, which differs between the two runs of one registration.
var clockReading = regexp.MustCompile(`"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+\+00:00"`)

// An argument that is not UTF-8 (an argv byte such as 0xff, which Python holds surrogate-escaped)
// is refused where the fence first encodes it: hashing it into an identity, or binding it into
// SQLite as TEXT, which raises UnicodeEncodeError and cli.main answers as a host error (exit 3). The
// Go CLI refuses at the same point in the same words, so it never writes TEXT Python's sqlite3
// cannot decode and never looks such a string up where Python raises; a reader Python lets the
// error escape from raises it too. An argument Python stores inside JSON is written as json.dumps
// writes it ("\udcff"), and a refusal that echoes it is its repr(). The Go CLI, as the built
// binary with every command family it carries, answers each argv in a store of its own, the bytes
// its golden holds (the fence's answers, at first). An artifact root, which the registration
// stores inside JSON, is stored as the TEXT Python's sqlite3 wrote and read back.
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
	argvs = append(argvs, register("--artifact-root", "a\xff"))
	var goArgvs [][]string
	for _, argv := range argvs {
		goArgvs = append(goArgvs, append([]string{"--state", filepath.Join(root, "go")}, argv...))
	}
	key := batchKey(t, goArgvs...)
	var answers []answer
	for i, argv := range goArgvs {
		if i == len(goArgvs)-1 {
			// Printed back through Go's reader, which decodes the stored escape as U+FFFD
			// (docs/port/known-defects.md): the stored TEXT is compared below instead.
			if err := exec.Command(relay, argv...).Run(); err != nil {
				t.Fatalf("registering an artifact root that is not UTF-8: %v", err)
			}
			break
		}
		command := exec.Command(relay, argv...)
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
		answers = append(answers, got)
	}
	expectAnswers(t, key, answers, goArgvs...)
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
	execute := func(path, statement string) error {
		database, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			return err
		}
		_, err = database.Exec(statement)
		return errors.Join(err, database.Close())
	}
	garbage := bytes.Repeat([]byte("not a database \x00\xff"), 256)
	prepare := func(state, kind string) error {
		if err := os.MkdirAll(state, 0o700); err != nil {
			return err
		}
		database := filepath.Join(state, "relay.sqlite3")
		switch kind {
		case "tables":
			return execute(database, "create table t(x)")
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
			return execute(database, "drop table store_challenge")
		}
		return nil
	}
	for _, flag := range []string{"--issue", "--expect-nonce"} {
		for _, kind := range []string{"tables", "garbage", "dropped"} {
			state := filepath.Join(root, flag, kind, "go")
			if err := prepare(state, kind); err != nil {
				t.Fatal(err)
			}
			argv := append([]string{"--state", state}, "--json", "doctor", flag, "x\xffy")
			command := exec.Command(relay, argv...)
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
			lookup := doctorLookup(t, stdout.String(), state)
			expectJSON(t, flag+" over "+kind, map[string]any{"code": code, "lookup": lookup}, argv...)
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
