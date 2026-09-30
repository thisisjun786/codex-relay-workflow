package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// schemaPath and outputPath are relative to internal/contract, where `go generate` runs this.
var (
	schemaPath = filepath.Join("..", "..", "contract", "schema", "relay-exit-codes.json")
	outputPath = "exit_codes_generated.go"
)

func main() {
	input, err := os.ReadFile(schemaPath)
	if err != nil {
		fail(err)
	}
	formatted, err := render(input)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(outputPath, formatted, 0644); err != nil {
		fail(err)
	}
}

// render is exit_codes_generated.go for the schema's bytes.
func render(input []byte) ([]byte, error) {
	var schema struct {
		Codes          map[string]int    `json:"codes"`
		RefusalReasons map[string]string `json:"refusalReasons"`
	}
	if err := json.Unmarshal(input, &schema); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("// Code generated from contract/schema/relay-exit-codes.json; DO NOT EDIT.\npackage contract\n\n")
	out.WriteString("const (\n")
	keys := make([]string, 0, len(schema.Codes))
	for key := range schema.Codes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&out, "Exit%s = %d\n", title(key), schema.Codes[key])
	}
	out.WriteString(")\n\ntype RefusalReason string\n\nconst (\n")
	keys = keys[:0]
	for key := range schema.RefusalReasons {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&out, "Refusal%s RefusalReason = %q\n", title(key), schema.RefusalReasons[key])
	}
	out.WriteString(")\n")
	return format.Source(out.Bytes())
}

func title(key string) string {
	var result strings.Builder
	for _, part := range strings.Split(strings.ToLower(key), "_") {
		result.WriteString(strings.ToUpper(part[:1]))
		result.WriteString(part[1:])
	}
	return result.String()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
