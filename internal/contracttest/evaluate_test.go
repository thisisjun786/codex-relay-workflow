package contracttest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluate_passes_and_fails_each_operator_on_its_own_input(t *testing.T) {
	// Given: one observation shaped like a runner's output.
	actual := map[string]any{
		"exit": float64(2), "stdout": "ready\n", "stdout_json": nil,
		"rows": []any{"a", "b"}, "call": map[string]any{"argv": []any{"--state", "/s", "x"}},
		"json": `{"ok":true}`, "hex": "6869", "same": "a", "other": "a",
	}
	cases := []struct {
		check Check
		pass  bool
	}{
		{Check{Kind: "eq", Path: []any{"exit"}, Value: float64(2)}, true},
		{Check{Kind: "eq", Path: []any{"exit"}, Value: float64(0)}, false},
		{Check{Kind: "ne", Path: []any{"exit"}, Value: float64(2)}, false},
		{Check{Kind: "json_eq", Path: []any{"json"}, Value: map[string]any{"ok": true}}, true},
		{Check{Kind: "contains", Path: []any{"stdout"}, Value: "ready"}, true},
		{Check{Kind: "contains", Path: []any{"rows"}, Value: "c"}, false},
		{Check{Kind: "excludes", Path: []any{"rows"}, Value: "a"}, false},
		{Check{Kind: "truthy", Path: []any{"stdout_json"}}, false},
		{Check{Kind: "falsy", Path: []any{"stdout_json"}}, true},
		{Check{Kind: "length", Path: []any{"rows"}, Value: float64(2)}, true},
		{Check{Kind: "regex", Path: []any{"stdout"}, Value: "^ready"}, true},
		{Check{Kind: "lt", Path: []any{"exit"}, Value: float64(2)}, false},
		{Check{Kind: "gt", Path: []any{"exit"}, Value: float64(1)}, true},
		{Check{Kind: "after", Path: []any{"call", "argv"}, Flag: "--state", Value: "/s"}, true},
		{Check{Kind: "same", Path: []any{"same"}, Other: []any{"other"}}, true},
		{Check{Kind: "set_eq", Path: []any{"rows"}, Value: []any{"b", "a"}}, true},
		{Check{Kind: "set_eq", Path: []any{"rows"}, Value: []any{"a"}}, false},
		{Check{Kind: "subset", Path: []any{"rows"}, Value: []any{"a"}}, true},
		{Check{Kind: "bytes", Path: []any{"hex"}, Value: "6869"}, true},
		{Check{Kind: "eq", Path: []any{"rows", float64(-1)}, Value: "b"}, true},
	}
	for _, c := range cases {
		// When
		err := Evaluate("case", actual, []Check{c.check})
		// Then
		if (err == nil) != c.pass {
			t.Errorf("%+v: pass=%v, got %v", c.check, c.pass, err)
		}
	}
}

func TestLoad_names_the_fixture_that_is_not_json(t *testing.T) {
	// Given: a fixture file that does not parse.
	path := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// When
	_, err := parse("synthetic", path)
	// Then
	if !errors.Is(err, ErrFixture) || !strings.Contains(err.Error(), path) {
		t.Fatalf("want ErrFixture naming %s, got %v", path, err)
	}
}

func TestRoot_is_the_checkout_holding_the_contract_corpus(t *testing.T) {
	// When
	root, err := Root()
	// Then
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "contract", "fixtures")); err != nil {
		t.Fatalf("root %s has no contract/fixtures: %v", root, err)
	}
}
