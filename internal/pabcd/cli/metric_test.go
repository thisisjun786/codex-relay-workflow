package cli

// These expectations were recorded by Node v24 from the CXC v0.2.40 build's dist/metric-cli.js, not from Go.
// The fixture in testdata/metric/oracle.json is written only by testdata/metric/record-oracle.mjs.

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// metricCliOracle is one recorded answer of testdata/metric/oracle.json.
type metricCliOracle struct {
	ID    string            `json:"id"`
	Argv  []string          `json:"argv"`
	Stdin string            `json:"stdin"`
	Given map[string]string `json:"given"`
	Want  struct {
		Code   int    `json:"code"`
		Output string `json:"output"`
	} `json:"want"`
	Files map[string]string `json:"files"`
}

// metricCliTimestamp is Date.prototype.toISOString, which both the ledger rows and the kind file carry.
var metricCliTimestamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z`)

// metricCliNames applies the settled CRW names to a recorded answer: the cli table's metric row, R11's
// loop-skill name, R25's product name and R26's state directory.
func metricCliNames(s string) string {
	return strings.NewReplacer("cxc metric", "crw pabcd metric", "cxc-loop", "crw-loop", "CodexClaw", "CRW", ".codexclaw", ".crw").Replace(s)
}

// metricCliLeft is what the run left under the state directory, by path relative to cwd, timestamps
// placeholdered. An absent directory leaves no files.
func metricCliLeft(t *testing.T, cwd string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(filepath.Join(cwd, crwdir.DirName), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = metricCliTimestamp.ReplaceAllString(string(raw), "<TS>")
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return files
}

func metricCliOracleCases(t *testing.T) []metricCliOracle {
	t.Helper()
	raw, err := os.ReadFile("testdata/metric/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []metricCliOracle `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("the recorded oracle holds no cases")
	}
	return fixture.Cases
}

// TestMetricCLIMatchesTheRecordedOracle replays every recorded answer: the exit status, the output text and
// the files the run left, in a temporary workspace only.
func TestMetricCLIMatchesTheRecordedOracle(t *testing.T) {
	for _, c := range metricCliOracleCases(t) {
		t.Run(c.ID, func(t *testing.T) {
			cwd := t.TempDir()
			for rel, body := range c.Given {
				cliPut(t, filepath.Join(cwd, filepath.FromSlash(metricCliNames(rel))), body)
			}
			got, err := RunMetricCLI(c.Argv, cwd, c.Stdin)
			if err != nil {
				t.Fatalf("argv %q: %v", c.Argv, err)
			}
			output, want := metricCliTimestamp.ReplaceAllString(got.Output, "<TS>"), metricCliNames(c.Want.Output)
			if got.Code != c.Want.Code || output != want {
				t.Errorf("argv %q\ngot  (%d) %s\nwant (%d) %s", c.Argv, got.Code, output, c.Want.Code, want)
			}
			left := map[string]string{}
			for rel, body := range c.Files {
				left[metricCliNames(rel)] = metricCliNames(metricCliTimestamp.ReplaceAllString(body, "<TS>"))
			}
			if have := metricCliLeft(t, cwd); !reflect.DeepEqual(have, left) {
				t.Errorf("argv %q wrote %v, want %v", c.Argv, have, left)
			}
		})
	}
}

// TestRunMetricCLIRecordShowKindIngest ports metrics.test.ts:128-146.
func TestRunMetricCLIRecordShowKindIngest(t *testing.T) {
	cwd := t.TempDir()
	record, err := RunMetricCLI([]string{"record", "--session", "cli", "--name", "score", "--value", "4", "--json"}, cwd, "")
	if err != nil || record.Code != 0 {
		t.Fatalf("record: code %d, %v", record.Code, err)
	}
	show, err := RunMetricCLI([]string{"show", "--session", "cli", "--json"}, cwd, "")
	if err != nil || show.Code != 0 {
		t.Fatalf("show: code %d, %v", show.Code, err)
	}
	var shown struct {
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal([]byte(show.Output), &shown); err != nil || len(shown.Records) != 1 {
		t.Fatalf("show output %q: %d records, %v", show.Output, len(shown.Records), err)
	}
	kind, err := RunMetricCLI([]string{"kind", "--session", "cli", "satisfy", "--json"}, cwd, "")
	if err != nil || kind.Code != 0 {
		t.Fatalf("kind: code %d, %v", kind.Code, err)
	}
	var kindPayload struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(kind.Output), &kindPayload); err != nil || kindPayload.Kind != "satisfy" {
		t.Fatalf("kind output %q: %v", kind.Output, err)
	}
	ingest, err := RunMetricCLI([]string{"ingest", "--session", "cli"}, cwd, "METRIC score=5\n")
	if err != nil || ingest.Code != 0 || !strings.Contains(ingest.Output, "recorded 1") {
		t.Fatalf("ingest: code %d, output %q, %v", ingest.Code, ingest.Output, err)
	}
}

// TestMetricHelpVerbs ports help-verbs.test.ts:112-118: each help token prints the usage with exit 0 and
// never demands --session, and reading help writes nothing.
func TestMetricHelpVerbs(t *testing.T) {
	for _, token := range []string{"help", "--help", "-h"} {
		cwd := t.TempDir()
		got, err := RunMetricCLI([]string{token}, cwd, "")
		if err != nil || got.Code != 0 {
			t.Fatalf("%s: code %d, %v", token, got.Code, err)
		}
		if !strings.Contains(got.Output, "Usage:") || strings.Contains(got.Output, "--session <id> is required") {
			t.Fatalf("%s output: %q", token, got.Output)
		}
		if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
			t.Fatalf("%s wrote into the workspace: %v, %v", token, entries, err)
		}
	}
}

// TestMetricCLIGoErrors locks the paths where the oracle throws out of the CLI and the library returns an
// error instead: nothing is printed, the caller sees the error, and no file is replaced (the data-loss fix
// of the metrics port).
func TestMetricCLIGoErrors(t *testing.T) {
	t.Run("kind file of another session", func(t *testing.T) {
		cwd := t.TempDir()
		body := "{\n  \"sessionId\": \"other\",\n  \"kind\": \"maximize\",\n  \"updatedAt\": \"2026-01-01T00:00:00.000Z\"\n}"
		cliPut(t, filepath.Join(cwd, ".crw/objective-kind/s.json"), body)
		got, err := RunMetricCLI([]string{"kind", "--session", "s", "satisfy"}, cwd, "")
		if err == nil || got.Output != "" || got.Code != 0 {
			t.Fatalf("kind: %+v, %v", got, err)
		}
		if raw, err := os.ReadFile(filepath.Join(cwd, ".crw/objective-kind/s.json")); err != nil || string(raw) != body {
			t.Fatalf("the other session's kind file changed: %q, %v", raw, err)
		}
	})
	t.Run("state directory is a file", func(t *testing.T) {
		cwd := t.TempDir()
		cliPut(t, filepath.Join(cwd, ".crw"), "retained")
		got, err := RunMetricCLI([]string{"record", "--session", "s", "--name", "n", "--value", "1"}, cwd, "")
		if err == nil || got.Output != "" {
			t.Fatalf("record: %+v, %v", got, err)
		}
		if raw, err := os.ReadFile(filepath.Join(cwd, ".crw")); err != nil || string(raw) != "retained" {
			t.Fatalf("the file was overwritten: %q, %v", raw, err)
		}
	})
	t.Run("ledger is a directory", func(t *testing.T) {
		cwd := t.TempDir()
		if err := os.MkdirAll(filepath.Join(cwd, ".crw/metrics.jsonl"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := RunMetricCLI([]string{"ingest", "--session", "s"}, cwd, "METRIC score=1\n")
		if err == nil || got.Output != "" {
			t.Fatalf("ingest: %+v, %v", got, err)
		}
	})
}
