package contracttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RunKind is a scenario's run.kind: the runner family contract/README.md names.
type RunKind string

// CheckKind is an expect.checks operator from contract/README.md.
type CheckKind string

var checkKinds = map[CheckKind]bool{
	"eq": true, "ne": true, "json_eq": true, "contains": true, "excludes": true, "truthy": true,
	"falsy": true, "length": true, "regex": true, "lt": true, "gt": true, "after": true,
	"same": true, "set_eq": true, "subset": true, "bytes": true, "ordered_calls": true,
	"absent_effects": true, "exception_code": true, "timeout": true, "signal": true,
}

// Check is one expect.checks element. Path and Other address the observation: string keys
// index objects and integers index arrays (negative from the end, as the Python runner does).
type Check struct {
	Kind       CheckKind `json:"kind"`
	Path       []any     `json:"path"`
	Other      []any     `json:"other,omitempty"`
	Value      any       `json:"value"`
	Flag       string    `json:"flag,omitempty"`
	DecodeJSON bool      `json:"decode_json,omitempty"`
}

// Expect is a scenario's expect block. Exit is mandatory; StdoutJSON is compared only when
// the fixture carries the key, so HasStdoutJSON records its presence.
type Expect struct {
	Exit          int
	HasStdoutJSON bool
	StdoutJSON    any
	Files         map[string]bool
	Observe       []string
	Queries       map[string]string
	Checks        []Check
}

// Scenario is one parsed fixture. Given and Run keep the fixture's JSON for the runner of
// its kind; only the fields every runner shares are typed.
type Scenario struct {
	ID     string
	Domain string
	Path   string
	Kind   RunKind
	Given  map[string]any
	Run    map[string]any
	Expect Expect
}

// ErrFixture marks a fixture that does not parse as a scenario.
var ErrFixture = errors.New("contracttest: invalid fixture")

// Domains lists the directories under contract/fixtures, sorted.
func Domains() ([]string, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(root, "contract", "fixtures"))
	if err != nil {
		return nil, fmt.Errorf("contracttest: list domains: %w", err)
	}
	var domains []string
	for _, entry := range entries {
		if entry.IsDir() {
			domains = append(domains, entry.Name())
		}
	}
	return domains, nil
}

// Load parses every contract/fixtures/<domain>/*.json in place, sorted by scenario ID. The
// first fixture that does not parse fails the load with its path.
func Load(domain string) ([]Scenario, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(root, "contract", "fixtures", domain, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("contracttest: glob %s: %w", domain, err)
	}
	sort.Strings(paths)
	scenarios := make([]Scenario, 0, len(paths))
	for _, path := range paths {
		scenario, err := parse(domain, path)
		if err != nil {
			return nil, err
		}
		scenarios = append(scenarios, scenario)
	}
	return scenarios, nil
}

type rawScenario struct {
	Given  map[string]any             `json:"given"`
	Run    map[string]any             `json:"run"`
	Expect map[string]json.RawMessage `json:"expect"`
}

func parse(domain, path string) (Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Scenario{}, fmt.Errorf("contracttest: read %s: %w", path, err)
	}
	fail := func(format string, args ...any) (Scenario, error) {
		return Scenario{}, fmt.Errorf("%w: %s: %s", ErrFixture, path, fmt.Sprintf(format, args...))
	}
	var raw rawScenario
	if err := strictDecode(data, &raw); err != nil {
		return fail("%v", err)
	}
	kind, ok := raw.Run["kind"].(string)
	if !ok || kind == "" {
		return fail("run.kind is missing")
	}
	var expect Expect
	if _, ok := raw.Expect["exit"]; !ok {
		return fail("expect.exit is missing")
	}
	fields := []struct {
		key  string
		into any
	}{
		{"exit", &expect.Exit}, {"files", &expect.Files}, {"observe", &expect.Observe},
		{"queries", &expect.Queries}, {"checks", &expect.Checks},
	}
	for _, field := range fields {
		if value, present := raw.Expect[field.key]; present {
			if err := strictDecode(value, field.into); err != nil {
				return fail("expect.%s: %v", field.key, err)
			}
		}
	}
	if value, present := raw.Expect["stdout_json"]; present {
		expect.HasStdoutJSON = true
		if err := json.Unmarshal(value, &expect.StdoutJSON); err != nil {
			return fail("expect.stdout_json: %v", err)
		}
	}
	for i, check := range expect.Checks {
		if !checkKinds[check.Kind] {
			return fail("expect.checks[%d]: unknown kind %q", i, check.Kind)
		}
		if len(check.Path) == 0 {
			return fail("expect.checks[%d]: empty path", i)
		}
	}
	if raw.Given == nil {
		raw.Given = map[string]any{}
	}
	return Scenario{
		ID:     strings.TrimSuffix(filepath.Base(path), ".json"),
		Domain: domain,
		Path:   path,
		Kind:   RunKind(kind),
		Given:  raw.Given,
		Run:    raw.Run,
		Expect: expect,
	}, nil
}

func strictDecode(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}
