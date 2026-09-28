package routing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

type projectReader struct{ args Object }

func (r projectReader) Attached(context.Context, string, sql.NullString, sql.NullString) ([]string, error) {
	if e := text(r.args["readError"]); e != "" {
		return nil, errors.New(e)
	}
	out := []string{}
	for _, v := range list(r.args["attached"]) {
		out = append(out, text(v))
	}
	return out, nil
}
func (r projectReader) Outstanding(context.Context, string, sql.NullString) ([]string, error) {
	if e := text(r.args["readError"]); e != "" {
		return nil, errors.New(e)
	}
	out := []string{}
	for _, v := range list(r.args["outstanding"]) {
		out = append(out, text(v))
	}
	return out, nil
}
func (r projectReader) Owners(context.Context, string, string) ([]contract.OrderedObject, error) {
	if e := text(r.args["ownersError"]); e != "" {
		return nil, errors.New(e)
	}
	out := []contract.OrderedObject{}
	for _, v := range list(r.args["owners"]) {
		out = append(out, contract.OrderedObject{{Key: "taskId", Value: object(v)["taskId"]}})
	}
	return out, nil
}
func projectCompletionReplay(t *testing.T, id string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	cmd := exec.Command("uv", "run", "--no-sync", "--no-project", "python3", filepath.Join(filepath.Dir(file), "testdata/project_completion_capture.py"), id, filepath.Join(filepath.Dir(file), "testdata/properties.md"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("Python %v %s", err, stderr.String())
	}
	var records []struct {
		Args Object
		Wire string
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("no scenarios")
	}
	for _, record := range records {
		var reader registry.ProjectReader
		if record.Args["reader"] == true {
			reader = projectReader{record.Args}
		}
		answer := registry.ProjectStateReading(context.Background(), text(record.Args["project"]), reader, func(context.Context, string) (contract.OrderedObject, error) {
			if e := text(record.Args["stateError"]); e != "" {
				return nil, errors.New(e)
			}
			return contract.OrderedObject{{Key: "state", Value: record.Args["fixed"]}}, nil
		})
		got := evidence.Dumps(answer, false, false, true)
		if got != record.Wire {
			t.Fatalf("project reading bytes differ\nPython: %s\nGo:     %s", record.Wire, got)
		}
	}
}
func Test23_PC_1_NoAnswer(t *testing.T)     { projectCompletionReplay(t, "PC-1") }
func Test23_PC_2_EveryChild(t *testing.T)   { projectCompletionReplay(t, "PC-2") }
func Test23_PC_3_ReadFailures(t *testing.T) { projectCompletionReplay(t, "PC-3") }
func Test23_PC_4_ReachableConsumer(t *testing.T) {
	projectCompletionReplay(t, "PC-4")
	if !cli.Registered("linkage-completion") {
		t.Fatal("linkage-completion not registered")
	}
}
