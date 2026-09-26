package store

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSchema_embeds_frozen_contract(t *testing.T) {
	given, err := os.ReadFile(filepath.Join(repositoryRoot(t), "contract/schema/relay-sqlite.sql"))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := schema.ReadFile("relay-sqlite.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, given) {
		t.Fatal("embedded schema differs from frozen contract")
	}
}
func TestOpen_preserves_shipped_schema(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts/ci/tests/relay_schema_shipped.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Objects map[string]string `json:"objects"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Objects) <= 50 {
		t.Fatalf("snapshot has %d objects", len(snapshot.Objects))
	}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.DB.Query("SELECT type || ' ' || name, sql FROM sqlite_master WHERE lower(substr(name,1,7)) <> 'sqlite_'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	current := map[string]string{}
	for rows.Next() {
		var name, sql string
		if err := rows.Scan(&name, &sql); err != nil {
			t.Fatal(err)
		}
		current[name] = sql
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name, held := range snapshot.Objects {
		if normalizeSQL(current[name]) != normalizeSQL(held) {
			t.Errorf("shipped CREATE changed: %s", name)
		}
	}
}
func normalizeSQL(s string) string {
	var out strings.Builder
	var quote rune
	space := false
	for _, ch := range s {
		if quote != 0 {
			out.WriteRune(ch)
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' || ch == '[' {
			if ch == '[' {
				quote = ']'
			} else {
				quote = ch
			}
			out.WriteRune(ch)
			space = false
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			space = true
			continue
		}
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		out.WriteRune(ch)
	}
	return out.String()
}
func TestSQLiteOpen_uses_bounded_pool(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		tree, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(tree, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			opens, bounds, rw, timeout := 0, 0, file != "store.go", file != "location.go"
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "boundedDB" && fn.Name.Name == "storeSocket" && len(call.Args) == 3 {
					if binary, ok := call.Args[2].(*ast.BinaryExpr); ok {
						if value, ok := binary.X.(*ast.BasicLit); ok && value.Value == "5" {
							timeout = true
						}
					}
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "SetMaxOpenConns" && len(call.Args) == 1 {
					if value, ok := call.Args[0].(*ast.BasicLit); ok && value.Value != "0" {
						bounds++
					}
				}
				if sel.Sel.Name == "Set" {
					if receiver, ok := sel.X.(*ast.Ident); ok && receiver.Name == "q" && len(call.Args) == 2 {
						if value, ok := call.Args[1].(*ast.BasicLit); ok && value.Value == `"rw"` {
							rw = true
						}
					}
				}
				if sel.Sel.Name == "Open" {
					if receiver, ok := sel.X.(*ast.Ident); ok && receiver.Name == "sql" {
						opens++
					}
				}
				return true
			})
			if fn.Name.Name == "storeSocket" && !timeout {
				t.Errorf("storeSocket must use five second bounded open")
			}
			if opens > 0 && (bounds == 0 || !rw || file != "store.go" && file != "location.go") {
				t.Errorf("unbounded sql.Open in %s:%s", file, fn.Name.Name)
			}
			return false
		})
	}
}
func TestStoreSocket_uses_five_second_timeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	s, err := Open(context.Background(), path, "socket")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got := storeSocket(path); got == "" {
		t.Fatal("socket provenance absent")
	}
	db, err := boundedDB(path, "ro", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var timeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatalf("timeout=%d: %v", timeout, err)
	}
}
