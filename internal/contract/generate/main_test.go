package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The committed exit_codes_generated.go is what `go generate ./internal/contract` writes from the
// committed schema: a reason added to or removed from one is added to or removed from the other.
func TestTheGeneratedCodesAreTheSchemas(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("..", schemaPath))
	if err != nil {
		t.Fatal(err)
	}
	want, err := render(input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", outputPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("internal/contract/%s is not generated from contract/schema/relay-exit-codes.json; run go generate ./internal/contract", outputPath)
	}
}

// CRW-761 (CRW-314 piece 3): the schema's reasons are one block that every change adds a line to, so the block is kept in name order and a line lands among
// its alphabetical neighbours instead of at the end, where two changes meet. The generated file is sorted by name already; this holds the schema to the same order.
func TestTheSchemaListsItsRefusalReasonsByName(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("..", schemaPath))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	var names []string
	// read the top-level object token by token so that the order of the keys in the file is what is seen
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		key, _ := keyToken.(string)
		if key != "refusalReasons" {
			var skipped json.RawMessage
			if err := decoder.Decode(&skipped); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := decoder.Token(); err != nil {
			t.Fatal(err)
		}
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				t.Fatal(err)
			}
			var value string
			if err := decoder.Decode(&value); err != nil {
				t.Fatal(err)
			}
			names = append(names, nameToken.(string))
		}
		break
	}
	if len(names) == 0 {
		t.Fatal("the schema lists no refusal reasons")
	}
	if !sort.StringsAreSorted(names) {
		for i := 1; i < len(names); i++ {
			if names[i-1] > names[i] {
				t.Fatalf("refusalReasons is not in name order: %s comes after %s; keep the block sorted", names[i], names[i-1])
			}
		}
	}
}
