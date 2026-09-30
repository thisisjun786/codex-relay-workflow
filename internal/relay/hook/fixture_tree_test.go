package hook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// A test's expected answers are goldens (internal/testsupport/golden): CRW_GOLDEN=update rewrites
// them from what the Go side answers. A test whose fixture the retired Python implementation laid
// out (its Store, marker and receipt files) reads the tree it left from testdata/fixtures
// (layFixture), which no mode rewrites, and lays it out again before Go reads it.

// goldenDumps is golden.Check of value as json.dumps(value, indent=2, sort_keys=sorted,
// ensure_ascii=True) spells it (evidence.DumpsIndent): a lone surrogate, a byte that is not UTF-8
// and NaN or Infinity stay the escapes and words Python writes, and sorted is the key order the
// comparison it stands for read the value in.
func goldenDumps(t *testing.T, key string, value any, sorted bool, opts ...golden.Option) {
	t.Helper()
	golden.Check(t, key, []byte(pyjson.Dumps(value, pyjson.Options{Indent: 2, SortKeys: sorted})+"\n"), opts...)
}

// goldenCanonical is goldenDumps of a JSON document as json.loads reads it, its keys sorted.
func goldenCanonical(t *testing.T, key string, raw []byte, opts ...golden.Option) {
	t.Helper()
	value, err := Decode(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	goldenDumps(t, key, value, true, opts...)
}

// goldenOutcome is golden.Check of how a process ended and what it wrote. A stream that is UTF-8
// is kept as its text, which a substitution reaches; any other as base64, byte for byte.
func goldenOutcome(t *testing.T, key string, o outcomeBytes, opts ...golden.Option) {
	t.Helper()
	type kept struct {
		Code         int     `json:"code"`
		Stdout       *string `json:"stdout,omitempty"`
		Stderr       *string `json:"stderr,omitempty"`
		StdoutBase64 string  `json:"stdoutBase64,omitempty"`
		StderrBase64 string  `json:"stderrBase64,omitempty"`
	}
	k := kept{Code: o.Code}
	for _, s := range []struct {
		value  string
		text   **string
		base64 *string
	}{{o.Stdout, &k.Stdout, &k.StdoutBase64}, {o.Stderr, &k.Stderr, &k.StderrBase64}} {
		if utf8.ValidString(s.value) {
			value := s.value
			*s.text = &value
		} else {
			*s.base64 = base64.StdEncoding.EncodeToString([]byte(s.value))
		}
	}
	golden.CheckJSON(t, key, k, opts...)
}

// fixtureEntry is one path of a fixture tree.
type fixtureEntry struct {
	Kind   string   `json:"kind"` // dir, file, link or sqlite
	Mode   uint32   `json:"mode,omitempty"`
	Text   *string  `json:"text,omitempty"`
	Base64 string   `json:"base64,omitempty"`
	Target string   `json:"target,omitempty"`
	Schema string   `json:"schema,omitempty"`
	Rows   []string `json:"rows,omitempty"`
}

// fixtureTree is what the step that laid a fixture out printed and the tree it left.
type fixtureTree struct {
	Output string                  `json:"output"`
	Tree   map[string]fixtureEntry `json:"tree"`
}

// layFixture lays the fixture tree testdata/fixtures/<name>.json out under root, everything
// under root removed first, and answers what the step that laid it out printed (the inputs a
// test reads from it) and the fixture's names. The tree spells the repository <REPO>, root
// <ROOT>, and the digest of a path under root (a marker workspace key, a scope key) by that path
// (fixtureNames); hashed names paths under root that the tree does not hold but whose digests it
// or an answer may (a socket path that was never bound).
func layFixture(t *testing.T, name, root string, hashed ...string) ([]byte, *fixtureNames) {
	t.Helper()
	names := newFixtureNames(t, root, hashed...)
	raw := golden.Fixture(t, name+".json")
	raw = bytes.ReplaceAll(raw, []byte("<REPO>"), []byte(testRoot))
	raw = names.unspell(bytes.ReplaceAll(raw, []byte("<ROOT>"), []byte(root)))
	var fixture fixtureTree
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	for rel := range fixture.Tree {
		names.rels = append(names.rels, rel)
	}
	slices.Sort(names.rels)
	if err := clearDir(root); err != nil {
		t.Fatal(err)
	}
	if err := restoreTree(t, root, fixture.Tree); err != nil {
		t.Fatalf("lay the fixture %s out: %v", name, err)
	}
	return []byte(fixture.Output), names
}

// fixtureNames spells the SHA-256 digest of a path under root (and its 16-digit prefix, a scope
// key) as that path relative to root, since a digest of a temporary directory's path differs
// from run to run: <SHA256:rel> and <SHA256_16:rel> for root joined with rel as it is spelled,
// <REALSHA256:rel> and <REALSHA256_16:rel> for the path the resolved root names.
type fixtureNames struct {
	root, real string
	rels       []string
}

func newFixtureNames(t *testing.T, root string, rels ...string) *fixtureNames {
	t.Helper()
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return &fixtureNames{root: root, real: real, rels: append([]string{""}, rels...)}
}

var fixtureName = regexp.MustCompile(`<(REAL)?SHA256(_16)?:([^<>]*)>`)

func digestOf(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

func (n *fixtureNames) join(base, rel string) string {
	if rel == "" {
		return base
	}
	return base + "/" + rel
}

// spell replaces each digest of a known path with its name, the full digest before its prefix.
func (n *fixtureNames) spell(raw []byte) []byte {
	var pairs []string
	for _, prefix := range []bool{false, true} {
		for _, rel := range n.rels {
			for _, base := range []struct{ path, mark string }{{n.root, "SHA256"}, {n.real, "REALSHA256"}} {
				digest, mark := digestOf(n.join(base.path, rel)), base.mark
				if prefix {
					digest, mark = digest[:16], mark+"_16"
				}
				pairs = append(pairs, digest, "<"+mark+":"+rel+">")
			}
		}
	}
	for i := 0; i < len(pairs); i += 2 {
		raw = bytes.ReplaceAll(raw, []byte(pairs[i]), []byte(pairs[i+1]))
	}
	return raw
}

// unspell puts this run's digests back.
func (n *fixtureNames) unspell(raw []byte) []byte {
	return fixtureName.ReplaceAllFunc(raw, func(match []byte) []byte {
		parts := fixtureName.FindSubmatch(match)
		base := n.root
		if len(parts[1]) > 0 {
			base = n.real
		}
		digest := digestOf(n.join(base, string(parts[3])))
		if len(parts[2]) > 0 {
			digest = digest[:16]
		}
		return []byte(digest)
	})
}

// encodeJSON is json.Marshal without HTML escaping, so a document reads as the text it holds.
func encodeJSON(v any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// clearDir removes everything under root, root itself kept.
func clearDir(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func decodeMirror(raw []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var record map[string]any
	if decoder.Decode(&record) != nil || record == nil {
		return nil, false
	}
	return record, true
}

// sqliteRows reads a database as the digest of its schema and one INSERT statement per row, in
// SQLite's own literal spelling (quote()).
func sqliteRows(path string) (string, []string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return "", nil, err
	}
	defer db.Close()
	schema, tables, err := sqliteSchema(db)
	if err != nil {
		return "", nil, err
	}
	var rows []string
	for _, table := range tables {
		columns, err := sqliteColumns(db, table)
		if err != nil {
			return "", nil, err
		}
		quoted := make([]string, len(columns))
		for i, column := range columns {
			quoted[i] = "quote(" + sqlName(column) + ")"
		}
		result, err := db.Query("SELECT 'INSERT INTO " + strings.ReplaceAll(sqlName(table), "'", "''") + " VALUES(' || " + strings.Join(quoted, " || ',' || ") + " || ')' FROM " + sqlName(table))
		if err != nil {
			return "", nil, err
		}
		for result.Next() {
			var row string
			if err = result.Scan(&row); err != nil {
				_ = result.Close()
				return "", nil, err
			}
			rows = append(rows, row)
		}
		if err = result.Close(); err != nil {
			return "", nil, err
		}
	}
	return schema, rows, nil
}

func sqlName(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// sqliteSchema is the digest of a database's schema and its tables, by name.
func sqliteSchema(db *sql.DB) (string, []string, error) {
	result, err := db.Query("SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		return "", nil, err
	}
	defer result.Close()
	digest := sha256.New()
	var tables []string
	for result.Next() {
		var kind, name, text string
		if err = result.Scan(&kind, &name, &text); err != nil {
			return "", nil, err
		}
		fmt.Fprintf(digest, "%s\x00%s\x00%s\x00", kind, name, text)
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), tables, result.Err()
}

func sqliteColumns(db *sql.DB, table string) ([]string, error) {
	result, err := db.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	var columns []string
	for result.Next() {
		var name string
		if err = result.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, result.Err()
}

// frozenStore is the frozen Python-produced empty store (contract/fixtures/sqlite-ddl), whose
// schema a restored database takes before its rows are inserted.
func frozenStore() string {
	return filepath.Join(testRoot, "contract", "fixtures", "sqlite-ddl", "python-store.sqlite3")
}

// restoreTree lays a fixture tree out under root.
func restoreTree(t *testing.T, root string, tree map[string]fixtureEntry) error {
	t.Helper()
	paths := make([]string, 0, len(tree))
	for rel := range tree {
		paths = append(paths, rel)
	}
	slices.Sort(paths)
	var mirrors, restricted []string
	for _, rel := range paths {
		entry, path := tree[rel], filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		switch entry.Kind {
		case "dir":
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
			restricted = append(restricted, rel)
		case "link":
			if err := os.Symlink(entry.Target, path); err != nil {
				return err
			}
		case "file":
			raw := []byte{}
			if entry.Text != nil {
				raw = []byte(*entry.Text)
			} else if entry.Base64 != "" {
				decoded, err := base64.StdEncoding.DecodeString(entry.Base64)
				if err != nil {
					return err
				}
				raw = decoded
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				return err
			}
			if err := os.Chmod(path, fs.FileMode(entry.Mode)); err != nil {
				return err
			}
			if filepath.Base(path) == "takeover.json" {
				mirrors = append(mirrors, path)
			}
		case "sqlite":
			if err := restoreDatabase(path, entry); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		default:
			return fmt.Errorf("%s: unknown kind %q", rel, entry.Kind)
		}
	}
	for _, path := range mirrors {
		if err := repointMirror(path); err != nil {
			return err
		}
	}
	// Directory modes last, deepest first, so a directory closed to this user was filled first.
	for i := len(restricted) - 1; i >= 0; i-- {
		path := filepath.Join(root, restricted[i])
		if err := os.Chmod(path, fs.FileMode(tree[restricted[i]].Mode)); err != nil {
			return err
		}
		if tree[restricted[i]].Mode&0o700 != 0o700 {
			t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
		}
	}
	return nil
}

// restoreDatabase writes the frozen empty store at path and inserts the fixture's rows. The
// schema must be the one the rows were read from.
func restoreDatabase(path string, entry fixtureEntry) error {
	raw, err := os.ReadFile(frozenStore())
	if err != nil {
		return err
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	schema, tables, err := sqliteSchema(db)
	if err != nil {
		return err
	}
	if schema != entry.Schema {
		return fmt.Errorf("the fixture database's schema (%s) is not the frozen store's (%s)", entry.Schema, schema)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	for _, table := range tables {
		if _, err = tx.Exec("DELETE FROM " + sqlName(table)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for _, row := range entry.Rows {
		if _, err = tx.Exec(row); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("%s: %w", row, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = db.Close(); err != nil {
		return err
	}
	return os.Chmod(path, fs.FileMode(entry.Mode))
}

// repointMirror gives a restored mirror the physical identity of the database it names.
func repointMirror(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	record, ok := decodeMirror(raw)
	if !ok {
		return nil
	}
	database, _ := record["database"].(map[string]any)
	named, _ := database["realPath"].(string)
	if named == "" {
		return nil
	}
	physical, err := ownership.Physical(named)
	if err != nil {
		return fmt.Errorf("%s names %s: %w", path, named, err)
	}
	record["database"] = physical
	if socket, ok := record["appServerSocket"].(string); ok && record["scopeKey"] == "<scope key of appServerSocket>" {
		if record["scopeKey"], err = ownership.ScopeKey(socket); err != nil {
			return err
		}
	}
	out, err := encodeJSON(record)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// storeRows is a database's rows (sqliteRows) for a test that compares a store before and after
// a read.
func storeRows(t *testing.T, path string) []string {
	t.Helper()
	_, rows, err := sqliteRows(path)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// storesUnder is every database under root, by path, as storeRows reads it.
func storesUnder(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || !strings.HasSuffix(path, ".sqlite3") {
			return nil
		}
		out[path] = storeRows(t, path)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

var encodePosition = regexp.MustCompile(`(positions? )([0-9]+)(-([0-9]+))?`)

// toRootPositions spells each character position a codec error names ("position 73",
// "positions 75-76") relative to the length of root, which prefixes the path it counts in: a
// temporary directory's name is not always as long, so the golden names R+k instead.
func toRootPositions(raw []byte, root string) []byte {
	offset := utf8.RuneCountInString(root)
	return encodePosition.ReplaceAllFunc(raw, func(match []byte) []byte {
		parts := encodePosition.FindSubmatch(match)
		out := string(parts[1]) + fmt.Sprintf("R+%d", atoi(parts[2])-offset)
		if len(parts[4]) > 0 {
			out += fmt.Sprintf("-R+%d", atoi(parts[4])-offset)
		}
		return []byte(out)
	})
}

func atoi(digits []byte) int {
	n := 0
	for _, d := range digits {
		n = n*10 + int(d-'0')
	}
	return n
}

// fixedLengthDir is a new directory under parent whose path is exactly length bytes long, for a
// test whose answers count positions in paths under it (a decoder's column and char): a
// temporary directory's name is not always as long, and the golden names the directory by a
// placeholder.
func fixedLengthDir(t *testing.T, parent string, length int) string {
	t.Helper()
	pad := length - len(parent) - 1
	if pad < 1 || pad > 255 {
		t.Fatalf("cannot pad %s to %d bytes", parent, length)
	}
	dir := filepath.Join(parent, strings.Repeat("p", pad))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// outcomeBytes is how a process ended and what it wrote.
type outcomeBytes struct {
	Code           int
	Stdout, Stderr string
}

// runOutcome runs cmd to its end; only a failure to start it fails the test.
func runOutcome(t *testing.T, cmd *exec.Cmd) outcomeBytes {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return outcomeBytes{cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()}
}

// canonicalJSON is a document as json.loads reads it, dumped with sorted keys and ASCII escapes,
// for comparing two runtimes' documents whatever their key order and spacing.
func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	value, err := Decode(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return pyjson.Dumps(value, pyjson.Options{SortKeys: true})
}

// withoutKeys is o without the named keys.
func withoutKeys(o Object, keys ...string) Object {
	out := Object{}
	for _, field := range o {
		if !slices.Contains(keys, field.Key) {
			out = append(out, field)
		}
	}
	return out
}

// canonicalFixtureRoot is where a test lays out a fixture that carries digests of documents that
// name its paths (a receipt's manifest revision, an event id), and whose goldens do too: the path
// is the same on every run, so the fixture's digests stay true of it laid out again. One test at
// a time, across processes, holds it (canonicalRoot).
const canonicalFixtureRoot = "/tmp/crw-oracle"

// canonicalRoot is an empty directory at a path fixed by the test's name under
// canonicalFixtureRoot, held under an exclusive lock until the test ends. On a host where another
// user owns canonicalFixtureRoot the test fails naming it.
func canonicalRoot(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll(canonicalFixtureRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(canonicalFixtureRoot+"/.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("%s is not this user's: %v", canonicalFixtureRoot, err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(canonicalFixtureRoot, digestOf(t.Name())[:8]) // short: a socket path under it must fit sun_path
	remove := func() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		_ = os.RemoveAll(dir)
	}
	remove()
	t.Cleanup(func() {
		remove()
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	})
	if err = os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
