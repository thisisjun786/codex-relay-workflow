package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// schemaTables is every CREATE TABLE name in the embedded frozen schema, each once.
func schemaTables(t *testing.T) []string {
	t.Helper()
	ddl, err := schema.ReadFile("relay-sqlite.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Comments are prose, and several of them mention CREATE TABLE IF NOT EXISTS.
	code := regexp.MustCompile(`--[^\n]*`).ReplaceAllString(string(ddl), "")
	seen := map[string]bool{}
	for _, match := range regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)`).FindAllStringSubmatch(code, -1) {
		seen[strings.ToLower(match[1])] = true
	}
	var tables []string
	for table := range seen {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables
}

// productionSQL is every string literal in the package's non-test Go files: the query text the
// store runs, with the concatenated parts of one query kept as separate literals.
func productionSQL(t *testing.T, skip string) []string {
	t.Helper()
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var literals []string
	fset := token.NewFileSet()
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(literal.Value)
			if err == nil && !strings.Contains(text, skip) {
				literals = append(literals, text)
			}
			return true
		})
	}
	return literals
}

// unreferencedTables names the schema tables no production query reads or writes. A reference is
// the table named after FROM, INTO, UPDATE or JOIN in a query literal.
func unreferencedTables(tables, literals []string) []string {
	var missing []string
	for _, table := range tables {
		reference := regexp.MustCompile(`(?i)\b(FROM|INTO|UPDATE|JOIN)\s+` + table + `\b`)
		found := false
		for _, literal := range literals {
			if reference.MatchString(literal) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, table)
		}
	}
	return missing
}

func TestEverySchemaTable_has_a_go_query_referencing_it(t *testing.T) {
	// Given: the frozen schema the store executes on open, parsed and cross-checked against the
	// tables an opened store actually holds, so the parse cannot silently skip one.
	tables := schemaTables(t)
	s := recordStore(t)
	var created int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&created); err != nil {
		t.Fatal(err)
	}
	if len(tables) != created || created == 0 {
		t.Fatalf("parsed %d tables from the schema; an opened store holds %d", len(tables), created)
	}
	// When: every production query literal is searched for each table.
	missing := unreferencedTables(tables, productionSQL(t, "\x00"))
	// Then: no table is left without a Go query.
	if len(missing) != 0 {
		t.Fatalf("%d tables have no Go query: %v", len(missing), missing)
	}
}

func TestEverySchemaTable_check_fails_when_a_reference_is_removed(t *testing.T) {
	// Given: the production literals with the only statements naming fault_cursors removed.
	tables := schemaTables(t)
	literals := productionSQL(t, "fault_cursors")
	// When/Then: the same check names exactly that table.
	if missing := unreferencedTables(tables, literals); len(missing) != 1 || missing[0] != "fault_cursors" {
		t.Fatalf("removing the fault_cursors queries left %v unreferenced", missing)
	}
}
