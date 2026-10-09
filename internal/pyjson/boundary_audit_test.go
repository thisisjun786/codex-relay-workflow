package pyjson_test

import (
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-860 (package audit J1-J4). Each row is a boundary input the audit named; the oracle is
// json.loads and json.dumps of CXC v0.2.40 (commit 3c1459ac).

// J1: a lone surrogate key is read as the surrogate it is, so two distinct lone surrogates
// are not a duplicate key; the same surrogate twice still is.
func TestCRW860_HookedErrorKeepsDistinctLoneSurrogateKeysDistinct(t *testing.T) {
	cases := []struct {
		doc       string
		duplicate bool
	}{
		{`{"\ud800":1,"\ud801":2}`, false},
		{`{"\ud800":1,"\ud800":2}`, true},
		{`{"😀":1,"😀":2}`, true},
		{`{"😀":1,"😁":2}`, false},
	}
	for _, c := range cases {
		message, duplicate, recursion := pyjson.HookedError(c.doc, 0)
		if recursion || message != "" || duplicate != c.duplicate {
			t.Errorf("HookedError(%s) = %q, duplicate %v, recursion %v; want duplicate %v",
				c.doc, message, duplicate, recursion, c.duplicate)
		}
	}
}

// J2: Normalize validates the spelling before it normalizes, so an integer spelling that
// json.loads refuses is refused by Encode, as it is without Normalize.
func TestCRW860_NormalizeRefusesAnInvalidNumberSpelling(t *testing.T) {
	for _, text := range []string{"+1", "01", "-", "1.", ".5", "1e"} {
		if _, err := pyjson.Encode(json.Number(text), pyjson.Options{Normalize: true}); err == nil {
			t.Errorf("Encode(json.Number(%q), Normalize) succeeded, want a refusal", text)
		}
		if _, err := pyjson.Encode(json.Number(text), pyjson.Options{}); err == nil {
			t.Errorf("Encode(json.Number(%q)) succeeded, want a refusal", text)
		}
	}
	// Controls: valid spellings are normalized as before.
	for text, want := range map[string]string{"-0": "0", "12": "12", "1.0": "1.0", "1e2": "100.0"} {
		got, err := pyjson.Encode(json.Number(text), pyjson.Options{Normalize: true})
		if err != nil || string(got) != want {
			t.Errorf("Encode(json.Number(%q), Normalize) = %q, %v; want %q", text, got, err, want)
		}
	}
	if got := pyjson.Dumps(json.Number("+1"), pyjson.Options{Normalize: true}); got != "+1" {
		t.Errorf("Dumps(json.Number(\"+1\"), Normalize) = %q, want the spelling as it is", got)
	}
}

// J3: the hook's recursion budget applies for budgets of one and two too. An object closing
// deeper than limit-2 refuses, so a lone object refuses at limit 1 and 2 and not at 3.
func TestCRW860_HookRecursionBudgetOfOneAndTwoIsEnforced(t *testing.T) {
	cases := []struct {
		doc   string
		limit int
		want  bool
	}{
		{"{}", 1, true},
		{"{}", 2, true},
		{"{}", 3, false},
		{"{}", 0, false},
		{"[]", 2, false},
		{`[{"a":{}}]`, 3, true},
	}
	for _, c := range cases {
		if got := pyjson.HookedRecursion(c.doc, c.limit); got != c.want {
			t.Errorf("HookedRecursion(%s, %d) = %v, want %v", c.doc, c.limit, got, c.want)
		}
	}
}

// J4: a value that holds itself is refused by Encode with an error, not by exhausting the
// stack; a lenient Dumps writes the back reference as null. A shared, acyclic value is not a
// cycle and is written in full.
func TestCRW860_EncodeRefusesACircularValue(t *testing.T) {
	self := map[string]any{}
	self["self"] = self
	if _, err := pyjson.Encode(self, pyjson.Options{}); err == nil {
		t.Errorf("Encode(self-referencing map) succeeded, want an error")
	}
	if got := pyjson.Dumps(self, pyjson.Options{}); got != `{"self": null}` {
		t.Errorf("Dumps(self-referencing map) = %q, want the back reference as null", got)
	}

	list := []any{nil}
	list[0] = list
	if _, err := pyjson.Encode(list, pyjson.Options{}); err == nil {
		t.Errorf("Encode(self-referencing slice) succeeded, want an error")
	}

	fields := pyjson.Object{{Key: "a"}}
	fields[0].Value = fields
	if _, err := pyjson.Encode(fields, pyjson.Options{}); err == nil {
		t.Errorf("Encode(self-referencing Object) succeeded, want an error")
	}

	shared := map[string]any{"x": 1}
	got, err := pyjson.Encode([]any{shared, shared}, pyjson.Options{})
	if err != nil || string(got) != `[{"x": 1}, {"x": 1}]` {
		t.Errorf("Encode(shared acyclic value) = %q, %v; want it written in full", got, err)
	}
}
