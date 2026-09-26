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

func main() {
	input, err := os.ReadFile(filepath.Join("..", "..", "contract", "schema", "relay-exit-codes.json"))
	if err != nil {
		fail(err)
	}
	var schema struct {
		Codes          map[string]int    `json:"codes"`
		RefusalReasons map[string]string `json:"refusalReasons"`
	}
	if err := json.Unmarshal(input, &schema); err != nil {
		fail(err)
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
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile("exit_codes_generated.go", formatted, 0644); err != nil {
		fail(err)
	}
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
