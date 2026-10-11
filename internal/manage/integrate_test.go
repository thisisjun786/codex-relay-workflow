package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// integrateTestRun drives crw manage integrate through Run with a configuration file whose
// integrate section is section (a JSON object body), and returns the status and both streams.
func integrateTestRun(t *testing.T, section string, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	doc := `{"manage": {"integrate": ` + section + `}}`
	if err := os.WriteFile(config, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", config)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), append([]string{"integrate"}, args...), strings.NewReader(""), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func integrateTestSection(dir string) string {
	data, _ := json.Marshal(map[string]any{
		"checkout": filepath.Join(dir, "checkout"), "queue_dir": filepath.Join(dir, "queue"), "lock_file": filepath.Join(dir, "lock"),
		"runs_dir": filepath.Join(dir, "runs"), "work_root": filepath.Join(dir, "work"),
		"gate_command": []string{"gate"}, "verify_command": []string{"verify"}, "record_version_command": []string{"record"},
	})
	return string(data)
}

func TestIntegrateIsRegisteredAndPrintsItsUsage(t *testing.T) {
	var stdout bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, strings.NewReader(""), &stdout, &bytes.Buffer{}); code != 0 || !strings.Contains(stdout.String(), "integrate\t") {
		t.Fatalf("help = %d\n%s", code, stdout.String())
	}
	// A usage request is answered even when the configuration cannot be used.
	code, out, _ := integrateTestRun(t, `"not an object"`, "-h")
	if code != 0 || !strings.HasPrefix(out, "usage: crw manage integrate") {
		t.Fatalf("-h = %d %q", code, out)
	}
}

func TestIntegrateRefusesWhatItCannotUse(t *testing.T) {
	dir := t.TempDir()
	good := integrateTestSection(dir)
	for name, tc := range map[string]struct {
		section string
		args    []string
		want    string
	}{
		"no host paths":                   {`{}`, nil, "checkout is required"},
		"a section that is not an object": {`[1]`, nil, "integrate section"},
		"unknown flag":                    {good, []string{"--force"}, `unexpected argument "--force"`},
		"watch and dry run":               {good, []string{"--watch", "--dry-run"}, "cannot be combined"},
		"gate without dry run":            {good, []string{"--with-gate"}, "belongs to --dry-run"},
		"interval without watch":          {good, []string{"--interval", "5s"}, "belongs to --watch"},
		"short interval":                  {good, []string{"--watch", "--interval", "10ms"}, "at least one second"},
		"interval missing":                {good, []string{"--watch", "--interval"}, "expected one argument"},
	} {
		code, out, errOut := integrateTestRun(t, tc.section, tc.args...)
		if code != usageExit || out != "" || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: code %d stdout %q stderr %q", name, code, out, errOut)
		}
	}
}

func TestIntegrateOnePassOfAnEmptyQueueIsIdleAndMakesNoLock(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := integrateTestRun(t, integrateTestSection(dir))
	if code != 0 || errOut != "" {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	var rep struct {
		Result string `json:"result"`
		Exit   int    `json:"exit"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Result != "idle" || rep.Exit != 0 {
		t.Fatalf("report %q: %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lock")); err == nil {
		t.Fatal("an idle pass made the lock file")
	}
}

func TestIntegrateStopFileEndsWatchWithStatusZero(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue", "stop"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := integrateTestRun(t, integrateTestSection(dir), "--watch", "--interval", "1s")
	if code != 0 || !strings.Contains(out, `"result":"stopped"`) {
		t.Fatalf("code %d\n%s", code, out)
	}
}

func TestIntegrateDryRunReadsTheQueueAndMakesNothing(t *testing.T) {
	dir := t.TempDir()
	pending := filepath.Join(dir, "queue", "pending")
	if err := os.MkdirAll(pending, 0o755); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("ab", 20)
	if err := os.WriteFile(filepath.Join(pending, "7"), []byte(head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := integrateTestRun(t, integrateTestSection(dir), "--dry-run")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	var rep struct {
		Lock    string `json:"lock"`
		Entries []struct {
			Name   string `json:"name"`
			Head   string `json:"head"`
			HeadOK bool   `json:"head_ok"`
		} `json:"entries"`
		Plan []string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Lock != "absent" || len(rep.Entries) != 1 || rep.Entries[0].Head != head || !rep.Entries[0].HeadOK || len(rep.Plan) != 1 || rep.Plan[0] != "crw-7="+head {
		t.Fatalf("report %s: %v", out, err)
	}
	for _, d := range []string{"runs", "work", "lock", "queue/done", "queue/blocked", "queue/runner.log"} {
		if _, err := os.Stat(filepath.Join(dir, d)); err == nil {
			t.Errorf("the dry run made %s", d)
		}
	}
}
