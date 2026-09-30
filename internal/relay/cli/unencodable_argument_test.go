package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// writes it ("\udcff"), and a refusal that echoes it is its repr(). Both runtimes answer each argv
// in a store of their own, the Go CLI as the built binary with every command family it carries;
// the printed answers must be the same bytes. An artifact root, which the registration stores
// inside JSON, is stored as the same TEXT, which Python's sqlite3 reads back from either store.
func TestAnArgumentThatIsNotUTF8IsRefusedWherePythonEncodesIt(t *testing.T) {
	_, relay := packageBinary(t)
	root := t.TempDir()
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
	stored := `import json, sqlite3, sys
print(json.dumps([sqlite3.connect(path).execute("SELECT artifact_roots FROM relationships").fetchall() for path in sys.argv[1:]]))`
	command := exec.Command(filepath.Join(repositoryRoot(t), ".venv", "bin", "python"), "-c", stored, filepath.Join(root, "python", "relay.sqlite3"), filepath.Join(root, "go", "relay.sqlite3"))
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
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("python reading both stores: %v\n%s", err, raw)
	}
	if want := `[[["[\"a\\udcff\"]"]], [["[\"a\\udcff\"]"]]]` + "\n"; string(raw) != want {
		t.Errorf("stored artifact roots, Python's store then Go's: %s want %s", raw, want)
	}
}
