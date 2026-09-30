package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// This package's parity tests read the answers the Python reference implementation gave them
// from recordings (internal/testsupport/pyoracle): the live Python runs only when
// CRW_PYTHON_ORACLE is record or check. pythonOracle is the one door every Python question of the
// package goes through. It names the question by what was asked, with every run-specific path
// spelled as a placeholder, so the same question gets the same recorded answer on every run.

var (
	// oraclePackageDir is this package's directory: the test binary starts in it, and pyoracle
	// finds a recording relative to it.
	oraclePackageDir = func() string { wd, _ := os.Getwd(); return wd }()
	// oracleRepository is the checkout this package is part of.
	oracleRepository = strings.TrimSuffix(oraclePackageDir, "/internal/relay/store")
	// oracleTemp is the temporary root the test binary started with, which t.TempDir and the
	// isolation root lie under.
	oracleTemp = filepath.Clean(os.TempDir())
	// oraclePasswdHome is this account's passwd home, which Path.home() answers with HOME unset.
	oraclePasswdHome = func() string {
		if u, err := user.Current(); err == nil {
			return u.HomeDir
		}
		return ""
	}()

	oracleAskedMu sync.Mutex
	oracleAsked   = map[testing.TB]map[string]int{}
)

// oracleEnvironment are the variables whose values a Python answer may spell.
var oracleEnvironment = []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CODEX_HOME",
	"CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR", "TMPDIR"}

// oracleEnvironmentNow spells the variables of oracleEnvironment as they stand, set or not, for a
// question whose Python inherits this process's environment.
func oracleEnvironmentNow() []string {
	out := make([]string, 0, len(oracleEnvironment))
	for _, name := range oracleEnvironment {
		out = append(out, spelledVariable(name))
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, "cwd="+wd)
	}
	return out
}

// spelledVariable spells one variable of a question's environment, set or not. TMPDIR is spelled
// <TMPDIR> whether it is unset or names the temporary root the test binary started with: either
// way it is that root, which differs from host to host (a runner leaves TMPDIR unset). A TMPDIR a
// test points elsewhere is spelled as it stands, and the substitutions name it.
func spelledVariable(name string) string {
	value, ok := os.LookupEnv(name)
	if name == "TMPDIR" && (!ok || value == "" || filepath.Clean(value) == oracleTemp) {
		return "TMPDIR=<TMPDIR>"
	}
	return name + "=" + strconv.FormatBool(ok) + ":" + value
}

var trailingDigits = regexp.MustCompile(`[0-9]+$`)

// oracleSubstitutions are the run-specific strings the question may carry, each with the
// placeholder that stands for it in a recording: the checkout, the passwd home, every directory
// directly under the temporary root that the question, the working directory or the environment
// names (a t.TempDir's parent or the isolation root), and the temporary root itself, longest
// first.
func oracleSubstitutions(parts []string) [][2]string {
	var subs [][2]string
	seen := map[string]bool{}
	placeholders := map[string]bool{}
	add := func(actual, placeholder string) {
		if actual == "" || seen[actual] {
			return
		}
		seen[actual] = true
		base, n := placeholder, 1
		for placeholders[placeholder] {
			n++
			placeholder = strings.TrimSuffix(base, ">") + "~" + strconv.Itoa(n) + ">"
		}
		placeholders[placeholder] = true
		subs = append(subs, [2]string{actual, placeholder})
	}
	add(oracleRepository, "<REPO>")
	candidates := append([]string{}, parts...)
	for _, name := range oracleEnvironment {
		candidates = append(candidates, os.Getenv(name))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	prefix := oracleTemp + "/"
	for _, candidate := range candidates {
		for rest := candidate; ; {
			i := strings.Index(rest, prefix)
			if i < 0 {
				break
			}
			rest = rest[i+len(prefix):]
			component, _, _ := strings.Cut(rest, "/")
			component, _, _ = strings.Cut(component, "\x00")
			actual := prefix + component
			if component == "" || actual == oracleRepository || strings.HasPrefix(oracleRepository+"/", actual+"/") {
				continue
			}
			add(actual, "<TMP:"+trailingDigits.ReplaceAllString(component, "")+">")
		}
	}
	if oraclePasswdHome != "" && oraclePasswdHome != "/" {
		add(oraclePasswdHome, "<PASSWD-HOME>")
	}
	if oracleTemp != "/" {
		add(oracleTemp, "<TMPDIR>")
	}
	sort.SliceStable(subs, func(i, j int) bool { return len(subs[i][0]) > len(subs[j][0]) })
	return subs
}

func substituted(text string, subs [][2]string) string {
	for _, s := range subs {
		text = strings.ReplaceAll(text, s[0], s[1])
	}
	return text
}

// pythonOracle is what the live Python answered the question parts spell (the script and its
// arguments, and anything else its answer depends on), recorded by pyoracle: capture is the live
// Python and runs only in record and check mode. A question asked again within one test is a
// question of its own, numbered in the order asked.
func pythonOracle(t testing.TB, parts []string, capture func() ([]byte, error), opts ...pyoracle.Option) []byte {
	t.Helper()
	subs := oracleSubstitutions(parts)
	spelled := make([]string, len(parts))
	for i, part := range parts {
		spelled[i] = substituted(part, subs)
	}
	sum := sha256.Sum256([]byte(strings.Join(spelled, "\x00")))
	key := "python " + hex.EncodeToString(sum[:])[:20]
	oracleAskedMu.Lock()
	asked, ok := oracleAsked[t]
	if !ok {
		asked = map[string]int{}
		oracleAsked[t] = asked
		t.Cleanup(func() {
			oracleAskedMu.Lock()
			delete(oracleAsked, t)
			oracleAskedMu.Unlock()
		})
	}
	asked[key]++
	if n := asked[key]; n > 1 {
		key += "#" + strconv.Itoa(n)
	}
	oracleAskedMu.Unlock()
	options := make([]pyoracle.Option, 0, len(subs)+len(opts))
	for _, s := range subs {
		options = append(options, pyoracle.Substitute(s[0], s[1]))
	}
	options = append(options, opts...)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if wd == oraclePackageDir {
		return pyoracle.Answer(t, key, capture, options...)
	}
	// The test has changed its working directory. pyoracle reads and writes a recording relative
	// to the working directory, so the question is asked from the package directory and the live
	// Python runs from the test's. A recording is written by a cleanup; the two registered around
	// the question put the package directory back for it and the test's afterwards.
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err = os.Chdir(oraclePackageDir); err != nil {
		t.Fatal(err)
	}
	answer := pyoracle.Answer(t, key, func() ([]byte, error) {
		if err := os.Chdir(wd); err != nil {
			return nil, err
		}
		defer func() { _ = os.Chdir(oraclePackageDir) }()
		return capture()
	}, options...)
	t.Cleanup(func() { _ = os.Chdir(oraclePackageDir) })
	if err = os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	return answer
}

// pythonNoise are the values a rerun of the same Python changes: a digest (of a temporary path,
// most often), the random name of a directory Python's tempfile made, a time read from the clock,
// and a file's inode number. A recording keeps the values one run gave, and replaying them is exact; only check mode,
// comparing a new run with the recording, reads past them (sameUpToNoise).
var pythonNoise = regexp.MustCompile(`[0-9a-f]{12,}|(?:relay-test-|decl-|tmp)[a-z0-9_]{8}|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:\+00:00|Z)|inode\\*"?: ?[0-9]+`)

// maskNoise spells every pythonNoise value by its kind and length alone, for a question's name.
func maskNoise(text string) string {
	return pythonNoise.ReplaceAllStringFunc(text, func(value string) string { return fmt.Sprintf("<noise%d>", len(value)) })
}

// indexNoise numbers the distinct pythonNoise values of text in the order they first appear, so
// two answers compare equal when they differ in those values alone and repeat them alike.
func indexNoise(text string) string {
	seen := map[string]int{}
	return pythonNoise.ReplaceAllStringFunc(text, func(value string) string {
		n, ok := seen[value]
		if !ok {
			n = len(seen)
			seen[value] = n
		}
		return fmt.Sprintf("<noise#%d>", n)
	})
}

// sameUpToNoise is check mode's comparison for an answer carrying pythonNoise.
var sameUpToNoise = pyoracle.SameWhen(func(recorded, live []byte) bool {
	return indexNoise(string(recorded)) == indexNoise(string(live))
})

// storeRows is every row of a store Python wrote but its schema_meta (its runtime's stamp),
// read as SQLite holds it. A test that read Python's store with Go reads these rows instead, in a
// store Go creates (restoreStore), since a recording keeps rows rather than a database file.
type storeRows struct {
	Tables []tableRows `json:"tables"`
}

type tableRows struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Rows    [][]cell `json:"rows"`
}

// cell is one SQLite value: null, an integer, a real, text (UTF-8, or base64 when it is not) or
// a blob.
type cell struct {
	Integer *string `json:"i,omitempty"`
	Real    *string `json:"r,omitempty"`
	Text    *string `json:"t,omitempty"`
	RawText *string `json:"tb,omitempty"`
	Blob    *string `json:"b,omitempty"`
}

// dumpStore reads every row of the store at path, rowid included where the table has one of its
// own, in rowid order.
func dumpStore(path string) (storeRows, error) {
	var dump storeRows
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return dump, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	names, err := queryStrings(db, "SELECT name FROM sqlite_master WHERE type = 'table' AND name != 'schema_meta' ORDER BY name")
	if err != nil {
		return dump, err
	}
	for _, name := range names {
		query := "SELECT * FROM " + quoteIdent(name)
		aliased, err := rowidAliased(db, name)
		if err != nil {
			return dump, err
		}
		if !aliased && name != "sqlite_sequence" {
			query = "SELECT rowid AS rowid, * FROM " + quoteIdent(name) + " ORDER BY rowid"
		} else if name != "sqlite_sequence" {
			query += " ORDER BY rowid"
		}
		rows, err := db.Query(query)
		if err != nil {
			return dump, err
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			return dump, err
		}
		table := tableRows{Table: name, Columns: columns, Rows: [][]cell{}}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				_ = rows.Close()
				return dump, err
			}
			row := make([]cell, len(columns))
			for i, value := range values {
				if row[i], err = cellOf(value); err != nil {
					_ = rows.Close()
					return dump, fmt.Errorf("%s.%s: %w", name, columns[i], err)
				}
			}
			table.Rows = append(table.Rows, row)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return dump, err
		}
		if len(table.Rows) > 0 {
			dump.Tables = append(dump.Tables, table)
		}
	}
	return dump, nil
}

func queryStrings(db *sql.DB, query string) ([]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, value)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// rowidAliased reports whether a table's rowid is one of its columns (an INTEGER PRIMARY KEY).
func rowidAliased(db *sql.DB, table string) (bool, error) {
	rows, err := db.Query("SELECT type, pk FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	keys, integer := 0, false
	for rows.Next() {
		var kind string
		var pk int
		if err = rows.Scan(&kind, &pk); err != nil {
			_ = rows.Close()
			return false, err
		}
		if pk > 0 {
			keys++
			integer = strings.EqualFold(kind, "INTEGER")
		}
	}
	return keys == 1 && integer, errors.Join(rows.Err(), rows.Close())
}

func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func cellOf(value any) (cell, error) {
	text := func(s string) *string { return &s }
	switch v := value.(type) {
	case nil:
		return cell{}, nil
	case int64:
		return cell{Integer: text(strconv.FormatInt(v, 10))}, nil
	case float64:
		return cell{Real: text(strconv.FormatFloat(v, 'g', -1, 64))}, nil
	case string:
		if strings.ToValidUTF8(v, "�") == v {
			return cell{Text: text(v)}, nil
		}
		return cell{RawText: text(base64.StdEncoding.EncodeToString([]byte(v)))}, nil
	case []byte:
		return cell{Blob: text(base64.StdEncoding.EncodeToString(v))}, nil
	default:
		return cell{}, fmt.Errorf("unexpected SQLite value %T", value)
	}
}

func (c cell) value() (any, error) {
	switch {
	case c.Integer != nil:
		return strconv.ParseInt(*c.Integer, 10, 64)
	case c.Real != nil:
		f, err := strconv.ParseFloat(*c.Real, 64)
		if err != nil && !math.IsInf(f, 0) {
			return nil, err
		}
		return f, nil
	case c.Text != nil:
		return *c.Text, nil
	case c.RawText != nil:
		raw, err := base64.StdEncoding.DecodeString(*c.RawText)
		return string(raw), err
	case c.Blob != nil:
		return base64.StdEncoding.DecodeString(*c.Blob)
	default:
		return nil, nil
	}
}

// restoreStore creates the Go store at path and gives it exactly the rows of dump, every table
// but schema_meta emptied first.
func restoreStore(t *testing.T, path string, dump storeRows) *Store {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatalf("Go cannot create the store Python's rows go into: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tables, err := queryStrings(s.DB, "SELECT name FROM sqlite_master WHERE type = 'table' AND name != 'schema_meta' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		for _, table := range tables {
			if _, err := conn.ExecContext(ctx, "DELETE FROM "+quoteIdent(table)); err != nil {
				return err
			}
		}
		for _, table := range dump.Tables {
			names := make([]string, len(table.Columns))
			marks := make([]string, len(table.Columns))
			for i, column := range table.Columns {
				names[i], marks[i] = quoteIdent(column), "?"
			}
			insert := "INSERT INTO " + quoteIdent(table.Table) + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
			for _, row := range table.Rows {
				args := make([]any, len(row))
				for i, c := range row {
					value, err := c.value()
					if err != nil {
						return err
					}
					args[i] = value
				}
				if _, err := conn.ExecContext(ctx, insert, args...); err != nil {
					return fmt.Errorf("%s: %w", table.Table, err)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("restoring Python's rows: %v", err)
	}
	return s
}

// jsonAnswer is an answer recorded as JSON: the value capture builds from the live Python,
// decoded into out.
func jsonAnswer(t testing.TB, parts []string, out any, capture func() (any, error), opts ...pyoracle.Option) {
	t.Helper()
	raw := pythonOracle(t, parts, func() ([]byte, error) {
		value, err := capture()
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}, opts...)
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("recorded answer: %v", err)
	}
}
