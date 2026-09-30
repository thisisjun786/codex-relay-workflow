package main

import (
	"bytes"
	"os"
	"path/filepath"
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
