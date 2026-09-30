package contracttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
)

// statusDocument is the `status` run with `document: true`: the settings document the
// installer writes, built by the Go install's own builder, printed as the scenario's stdout.
func statusDocument(s Scenario) (map[string]any, error) {
	home, err := os.MkdirTemp("", "crw-hc-doc-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	text := func(key, fallback string) string {
		if value, ok := s.Run[key].(string); ok {
			return value
		}
		return fallback
	}
	settings := install.HookSettings{
		Relay: text("relay", "/opt/relay"), MarkerRoot: text("marker_root", "/markers"), Socket: text("socket", ""),
		Mode: "observe", Timeout: 5, CodexHome: home, Destination: filepath.Join(home, "runtime"),
	}
	document, err := settings.Document(nil)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := contract.Emit(&out, document); err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return nil, err
	}
	return map[string]any{"exit": float64(0), "stdout": out.String(), "stderr": "", "stdout_json": parsed, "status": map[string]any{}, "files": map[string]any{}, "observed": map[string]any{}}, nil
}

// agreementVolatile are the journal record fields that differ between two runs of the same
// Stop for reasons other than the answer: where the settings were read from, which journal
// slot the row took, and the clocks. contract/runner/files.py:agreement excludes exactly these.
var agreementVolatile = []string{"configuration", "journalledAs", "at", "elapsedMs", "guardElapsedMs", "identityScanMs"}

// runAgreement drives `crw hook` once under the checkout settings document (no owner, the
// Python-era checkout hook's shape) and once under the plugin-owned document (the packaged
// adapter's shape), each in its own home with its own guard peer answering what given.relay
// says, and requires both to return the same answer and journal the same record apart from
// agreementVolatile. It observes what contract/runner/files.py:agreement returns: the answer
// printed (null when nothing was), the checkout run's records and both guard calls.
func runAgreement(t *testing.T, s Scenario) (map[string]any, error) {
	type side struct {
		label    string
		kind     RunKind
		returned any
		records  []any
		call     any
	}
	sides := []*side{{label: "checkout", kind: "hook"}, {label: "packaged", kind: "stop"}}
	for _, one := range sides {
		run := map[string]any{"kind": string(one.kind)}
		if stdin, present := s.Run["stdin"]; present {
			run["stdin"] = stdin
		}
		actual, err := runHook(t, Scenario{ID: s.ID + "/" + one.label, Domain: s.Domain, Path: s.Path, Kind: one.kind, Given: s.Given, Run: run})
		if err != nil {
			return nil, fmt.Errorf("%s adapter: %w", one.label, err)
		}
		if exit := number(actual["exit"]); exit != 0 {
			return nil, fmt.Errorf("%s adapter exited %v: %v", one.label, exit, actual["stderr"])
		}
		if stdout, _ := actual["stdout"].(string); stdout != "" {
			one.returned = stdout
		}
		one.records, _ = actual["rows"].([]any)
		one.call = actual["call"]
	}
	left, right := sides[0], sides[1]
	if !reflect.DeepEqual(left.returned, right.returned) {
		return nil, fmt.Errorf("checkout and packaged adapters disagree: returned %q and %q", left.returned, right.returned)
	}
	if l, r := withoutVolatile(left.records), withoutVolatile(right.records); !reflect.DeepEqual(l, r) {
		lj, _ := json.MarshalIndent(l, "", " ")
		rj, _ := json.MarshalIndent(r, "", " ")
		return nil, fmt.Errorf("checkout and packaged adapters disagree on the records\ncheckout %s\npackaged %s", lj, rj)
	}
	return map[string]any{"exit": float64(0), "returned": left.returned, "records": left.records,
		"calls": map[string]any{"checkout": left.call, "packaged": right.call}}, nil
}

func withoutVolatile(records []any) []any {
	out := make([]any, len(records))
	for i, record := range records {
		row, ok := record.(map[string]any)
		if !ok {
			out[i] = record
			continue
		}
		kept := maps.Clone(row)
		for _, key := range agreementVolatile {
			delete(kept, key)
		}
		out[i] = kept
	}
	return out
}
