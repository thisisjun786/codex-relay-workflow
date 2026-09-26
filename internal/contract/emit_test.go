package contract_test

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

func TestEmit_matchesPythonCLI_whenOrderedFields(t *testing.T) {
	// Given: a golden captured from the real Python ack-proof CLI.
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller location unavailable")
	}
	golden, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "contract", "golden", "ack-proof.json"))
	if err != nil {
		t.Fatal(err)
	}
	result := contract.Result{
		{Key: "eventId", Value: "1234567890abcdef1234567890abcdef"},
		{Key: "turnId", Value: "1234567890abcdef1234567890abcdef"},
		{Key: "ackProof", Value: "99a7ab965ebdcf0785ddf60ed1864469fe83c4ef6ba9c9de9325a7188a8859b0"},
	}
	var output bytes.Buffer

	// When
	err = contract.Emit(&output, result)

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), golden) {
		t.Fatalf("Python golden mismatch:\nwant %s\ngot %s", golden, output.Bytes())
	}
}

func TestEmit_matchesPythonJSON_whenFloatsIncludeBoundariesAndNonFinite(t *testing.T) {
	// Given: Python's json.dumps(..., indent=2) output for this exact float list.
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller location unavailable")
	}
	golden, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "contract", "golden", "floats.json"))
	if err != nil {
		t.Fatal(err)
	}
	values := []any{1.0, 0.1, 1e16, 1e-7, 123456789.0, math.Copysign(0, -1), 1e21, math.Inf(1), math.Inf(-1), math.NaN()}
	var output bytes.Buffer

	// When
	err = contract.Emit(&output, values)

	// Then
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), golden) {
		t.Fatalf("Python float golden mismatch:\nwant %s\ngot %s", golden, output.Bytes())
	}
}

func TestEmit_preservesNestedOrderAndPythonEscapes_whenObjectIsNested(t *testing.T) {
	// Given
	value := contract.Result{{Key: "z", Value: []any{contract.OrderedObject{{Key: "é", Value: "\b\f😀"}}}}, {Key: "a", Value: nil}}
	var output bytes.Buffer

	// When
	err := contract.Emit(&output, value)

	// Then
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"z\": [\n    {\n      \"\\u00e9\": \"\\b\\f\\ud83d\\ude00\"\n    }\n  ],\n  \"a\": null\n}\n"
	if output.String() != want {
		t.Fatalf("want %q, got %q", want, output.String())
	}
}
