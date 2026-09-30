package cli_test

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
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The Python reference implementation's answers are read from recordings (pyoracle): a test
// asks for Python's answer under a key, and only CRW_PYTHON_ORACLE=record or check runs the
// live Python (the capture closure) to give it. Everything the Go side of a test does runs live
// in every mode, so the Go side meets the same state whether or not Python ran beside it.

var oracleCalls sync.Map // test name + label -> *atomic.Int64

// oracleKey names the next question a test asks the oracle: a readable label (never a temporary
// path or a generated identity) and, for a label the test asks again, how many times it asked
// before. Questions with different labels may come in any order.
func oracleKey(t testing.TB, label string) string {
	counter, _ := oracleCalls.LoadOrStore(t.Name()+"\x00"+label, new(atomic.Int64))
	if n := counter.(*atomic.Int64).Add(1); n > 1 {
		return fmt.Sprintf("%s #%d", label, n)
	}
	return label
}

// placeholders names every run-specific directory the anchors mention - the repository root
// and each temporary directory directly under os.TempDir() - so a recording holds a placeholder
// where a run holds a temporary path. The anchors are the strings a question is built from (its
// argv, its environment); the process's isolated HOME, XDG and relay directories are always
// anchors. The numbering follows first appearance, which is the same on every run.
func placeholders(anchors ...string) []pyoracle.Option {
	explicit := append([]string{}, anchors...)
	root := repositoryRootPath()
	options := []pyoracle.Option{sameFixture, pyoracle.Substitute(root, "<repo>")}
	tmp := filepath.Clean(os.TempDir())
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CODEX_HOME",
		"CODEX_SESSION_RELAY_STATE", "CODEX_SESSION_RELAY_SCOPE_DIR", "CODEX_SESSION_RELAY_MARKER_ROOT", "CRW_MARKER_ROOT"} {
		anchors = append(anchors, os.Getenv(key))
	}
	if wd, err := os.Getwd(); err == nil {
		anchors = append(anchors, wd)
	}
	for _, parent := range []struct{ dir, name string }{{fixedRoot, "fixed"}, {tmp, "tmp"}} {
		seen := map[string]bool{}
		var prefixes []string
		for _, anchor := range anchors {
			for rest := anchor; ; {
				i := strings.Index(rest, parent.dir+"/")
				if i < 0 {
					break
				}
				tail := rest[i+len(parent.dir)+1:]
				component, _, _ := strings.Cut(tail, "/")
				rest = tail
				prefix := parent.dir + "/" + component
				if component == "" || seen[prefix] || strings.HasPrefix(root+"/", prefix+"/") || strings.HasPrefix(fixedRoot+"/", prefix+"/") {
					continue
				}
				seen[prefix] = true
				prefixes = append(prefixes, prefix)
			}
		}
		for i, prefix := range prefixes {
			options = append(options, pyoracle.Substitute(prefix, fmt.Sprintf("<%s%d>", parent.name, i)))
		}
		// A test that names a directory after a path spells its separators as underscores.
		for i, prefix := range prefixes {
			options = append(options, pyoracle.Substitute(strings.ReplaceAll(prefix, "/", "_"), fmt.Sprintf("<%s%d_>", parent.name, i)))
		}
	}
	// An argument that names one store file or one challenge (a store id or nonce, a
	// device:inode pair) is echoed in the answer as the run gave it.
	identities := map[string]bool{}
	for _, anchor := range explicit {
		if generated.MatchString(anchor) && !identities[anchor] {
			identities[anchor] = true
			options = append(options, pyoracle.Substitute(anchor, fmt.Sprintf("<id%d>", len(identities)-1)))
		}
	}
	return options
}

// generated is an argument a run generates: a store id or nonce, or a device:inode pair.
var generated = regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9]{3,}:[0-9]{3,}(?::.+)?)$`)

func repositoryRootPath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// recordedRun is one Python process's answer as a recording holds it.
type recordedRun struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// packageDir is this package's directory, where its recordings are (pyoracle reads them
// relative to the working directory).
var packageDir = func() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return wd
}()

// askOracle is pyoracle.JSON from a test that may have changed its working directory (t.Chdir):
// the recording is read from the package directory, and capture runs in the test's directory.
func askOracle(t testing.TB, key string, out any, capture func() (any, error), options ...pyoracle.Option) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if wd != packageDir {
		if pyoracle.CurrentMode() == pyoracle.Record {
			// Runs after the recording is written (see below).
			t.Cleanup(func() { _ = os.Chdir(wd) })
		}
		if err = os.Chdir(packageDir); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Chdir(wd); err != nil {
				t.Fatal(err)
			}
		}()
		inPackage := capture
		capture = func() (any, error) {
			if err := os.Chdir(wd); err != nil {
				return nil, err
			}
			defer func() { _ = os.Chdir(packageDir) }()
			return inPackage()
		}
	}
	pyoracle.JSON(t, key, out, capture, options...)
	if wd != packageDir && pyoracle.CurrentMode() == pyoracle.Record {
		// The recording is written when the test ends, relative to the working directory then:
		// this cleanup runs before that write, and the one above after it.
		t.Cleanup(func() { _ = os.Chdir(packageDir) })
	}
}

// oracleRun is the answer the live Python gave for key (capture), from the recording unless
// CRW_PYTHON_ORACLE asks for the live one.
func oracleRun(t testing.TB, key string, capture func() (run, error), options ...pyoracle.Option) run {
	t.Helper()
	var recorded recordedRun
	askOracle(t, key, &recorded, func() (any, error) {
		answer, err := capture()
		if err != nil {
			return nil, err
		}
		return recordedRun{answer.code, answer.stdout, answer.stderr}, nil
	}, append([]pyoracle.Option{sameFixture}, options...)...)
	return run{recorded.Code, recorded.Stdout, recorded.Stderr}
}

// ------------------------------------------------------------------- fixed trees

// fixedRoot holds the trees whose absolute path a recorded answer depends on beyond its
// spelling: a hash of the path (an artifact manifest's revision, a workspace key) that no
// substitution can put back. Each tree has a fixed name under it, and a lock beside it keeps two
// test processes from sharing one.
const fixedRoot = "/tmp/crw-cli-parity"

// fixedTree is an empty directory at a path that is the same on every run for key, held until
// the test ends.
func fixedTree(t testing.TB, key string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(fixedRoot, hex.EncodeToString(sum[:])[:12])
	if err := os.MkdirAll(fixedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	release := func() error {
		err := testsupport.RemoveTempTree(dir)
		return errors.Join(err, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN), lock.Close())
	}
	if err = testsupport.RemoveTempTree(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = release()
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		_ = release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := release(); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Error(err)
		}
	})
	return dir
}

// ------------------------------------------------------------------- recorded trees

// treeImage is every file a Python fixture builder left under a directory, as the Go side of a
// test reads it: regular files by content, symbolic links by target, directories by mode, and
// each SQLite database as its schema and rows (never its pages). Sockets and SQLite's -wal and
// -shm sidecars are left out; the rows include what the log held.
type treeImage struct {
	Entries map[string]treeEntry `json:"entries"`
}

type treeEntry struct {
	Kind   string       `json:"kind"` // dir, file, link, sqlite
	Mode   uint32       `json:"mode"`
	Data   string       `json:"data,omitempty"` // text, or "b64:" + base64
	Link   string       `json:"link,omitempty"`
	SQLite *sqliteImage `json:"sqlite,omitempty"`
}

type sqliteImage struct {
	Journal string `json:"journal"`
	Version int64  `json:"userVersion"`
	// Schema is every CREATE statement in sqlite_master order (type, sql), or nil when it is
	// the relay's own schema: the frozen Python-created store's (frozenStore), which a rebuild
	// starts from.
	Schema [][2]string   `json:"schema,omitempty"`
	Tables []sqliteTable `json:"tables"` // the tables that hold rows
}

// frozenStore is the empty store the Python relay creates (contract/fixtures/sqlite-ddl), the
// schema every relay store has.
func frozenStore() string {
	return filepath.Join(repositoryRootPath(), "contract", "fixtures", "sqlite-ddl", "python-store.sqlite3")
}

var (
	frozenOnce   sync.Once
	frozenSchema [][2]string
	frozenErr    error
)

// relaySchema reads frozenStore's schema, without writing beside it.
func relaySchema() ([][2]string, error) {
	frozenOnce.Do(func() {
		var db *sql.DB
		if db, frozenErr = sql.Open("sqlite", "file:"+frozenStore()+"?mode=ro&immutable=1"); frozenErr != nil {
			return
		}
		defer db.Close()
		frozenSchema, frozenErr = readSchema(db)
	})
	return frozenSchema, frozenErr
}

func readSchema(db *sql.DB) ([][2]string, error) {
	rows, err := db.Query("SELECT type, name, sql FROM sqlite_master ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schema [][2]string
	for rows.Next() {
		var kind, name string
		var text sql.NullString
		if err = rows.Scan(&kind, &name, &text); err != nil {
			return nil, err
		}
		if text.Valid && !strings.HasPrefix(name, "sqlite_") {
			schema = append(schema, [2]string{kind, text.String})
		}
	}
	return schema, rows.Err()
}

type sqliteTable struct {
	Name    string     `json:"name"`
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"` // each value tagged: n (NULL), i:, r:, t:, x: (text, base64), b: (blob, base64)
}

var sqliteHeader = []byte("SQLite format 3\x00")

// snapshotTree reads dir into an image: the paths include names under it, or all of it.
func snapshotTree(dir string, include []string) (treeImage, error) {
	image := treeImage{Entries: map[string]treeEntry{}}
	roots := []string{dir}
	if include != nil {
		roots = nil
		for _, name := range include {
			roots = append(roots, filepath.Join(dir, name))
		}
	}
	for _, root := range roots {
		if err := snapshotInto(image, dir, root); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return image, err
		}
	}
	return image, nil
}

func snapshotInto(image treeImage, dir, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode := uint32(info.Mode().Perm())
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			image.Entries[rel] = treeEntry{Kind: "link", Link: target}
		case info.IsDir():
			image.Entries[rel] = treeEntry{Kind: "dir", Mode: mode}
		case info.Mode().IsRegular():
			if strings.HasSuffix(rel, "-wal") || strings.HasSuffix(rel, "-shm") || strings.HasSuffix(rel, "-journal") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.HasPrefix(raw, sqliteHeader) {
				database, err := dumpSQLite(path)
				if err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				image.Entries[rel] = treeEntry{Kind: "sqlite", Mode: mode, SQLite: &database}
				return nil
			}
			image.Entries[rel] = treeEntry{Kind: "file", Mode: mode, Data: encodeData(raw)}
		}
		return nil
	})
}

func encodeData(raw []byte) string {
	if isText(raw) && !strings.HasPrefix(string(raw), "b64:") {
		return string(raw)
	}
	return "b64:" + base64.StdEncoding.EncodeToString(raw)
}

func decodeData(data string) ([]byte, error) {
	if rest, ok := strings.CutPrefix(data, "b64:"); ok {
		return base64.StdEncoding.DecodeString(rest)
	}
	return []byte(data), nil
}

func isText(raw []byte) bool {
	return !bytes.ContainsRune(raw, 0) && strings.ToValidUTF8(string(raw), "\uFFFD") == string(raw)
}

// dumpSQLite reads a disposable copy of the database (with its log), never the file itself.
func dumpSQLite(path string) (sqliteImage, error) {
	copyPath, cleanup, err := ownership.CopySnapshot(path)
	if err != nil {
		return sqliteImage{}, err
	}
	defer func() { _ = cleanup() }()
	db, err := sql.Open("sqlite", "file:"+copyPath+"?mode=ro")
	if err != nil {
		return sqliteImage{}, err
	}
	defer db.Close()
	var image sqliteImage
	if err = db.QueryRow("PRAGMA journal_mode").Scan(&image.Journal); err != nil {
		return image, err
	}
	if err = db.QueryRow("PRAGMA user_version").Scan(&image.Version); err != nil {
		return image, err
	}
	if image.Schema, err = readSchema(db); err != nil {
		return image, err
	}
	var tables []string
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY rowid")
	if err != nil {
		return image, err
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return image, err
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return image, err
	}
	for _, name := range tables {
		table, err := dumpTable(db, name)
		if err != nil {
			return image, fmt.Errorf("%s: %w", name, err)
		}
		if len(table.Rows) > 0 {
			image.Tables = append(image.Tables, table)
		}
	}
	if relay, err := relaySchema(); err == nil && image.Journal == "wal" && image.Version == 0 && slices.Equal(relay, image.Schema) {
		image.Schema = nil
	}
	return image, nil
}

func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func dumpTable(db *sql.DB, name string) (sqliteTable, error) {
	table := sqliteTable{Name: name, Rows: [][]string{}}
	info, err := db.Query("SELECT name FROM pragma_table_info(?)", name)
	if err != nil {
		return table, err
	}
	for info.Next() {
		var column string
		if err = info.Scan(&column); err != nil {
			info.Close()
			return table, err
		}
		table.Columns = append(table.Columns, column)
	}
	info.Close()
	var selected []string
	for _, column := range table.Columns {
		c := quoteIdent(column)
		selected = append(selected, "typeof("+c+")", "CASE WHEN typeof("+c+") IN ('text','blob') THEN CAST("+c+" AS BLOB) ELSE "+c+" END")
	}
	rows, err := db.Query("SELECT " + strings.Join(selected, ", ") + " FROM " + quoteIdent(name))
	if err != nil {
		return table, err
	}
	defer rows.Close()
	for rows.Next() {
		values := make([]any, len(selected))
		pointers := make([]any, len(selected))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			return table, err
		}
		row := make([]string, len(table.Columns))
		for i := range table.Columns {
			kind, _ := values[2*i].(string)
			value := values[2*i+1]
			switch kind {
			case "null":
				row[i] = "n"
			case "integer":
				row[i] = fmt.Sprintf("i:%d", value)
			case "real":
				row[i] = "r:" + strconvFloat(value)
			case "text":
				raw := asBytes(value)
				if utf8.Valid(raw) {
					row[i] = "t:" + string(raw)
				} else {
					row[i] = "x:" + base64.StdEncoding.EncodeToString(raw)
				}
			case "blob":
				row[i] = "b:" + base64.StdEncoding.EncodeToString(asBytes(value))
			default:
				return table, fmt.Errorf("column %s: storage class %q", table.Columns[i], kind)
			}
		}
		table.Rows = append(table.Rows, row)
	}
	return table, rows.Err()
}

func asBytes(v any) []byte {
	switch b := v.(type) {
	case []byte:
		return b
	case string:
		return []byte(b)
	}
	return []byte(fmt.Sprint(v))
}

func strconvFloat(v any) string {
	f, ok := v.(float64)
	if !ok {
		return fmt.Sprint(v)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// materializeTree writes image under dir (which exists): directories first, then files, links
// and databases, then each directory's recorded mode.
func materializeTree(dir string, image treeImage) error {
	names := make([]string, 0, len(image.Entries))
	for name := range image.Entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if entry := image.Entries[name]; entry.Kind == "dir" {
			if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
				return err
			}
		}
	}
	for _, name := range names {
		entry, path := image.Entries[name], filepath.Join(dir, name)
		switch entry.Kind {
		case "file":
			raw, err := decodeData(entry.Data)
			if err != nil {
				return err
			}
			if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err = os.WriteFile(path, raw, fs.FileMode(entry.Mode)); err != nil {
				return err
			}
		case "link":
			if err := os.Symlink(entry.Link, path); err != nil {
				return err
			}
		case "sqlite":
			if err := restoreSQLite(path, *entry.SQLite); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := os.Chmod(path, fs.FileMode(entry.Mode)); err != nil {
				return err
			}
		}
	}
	for i := len(names) - 1; i >= 0; i-- {
		if entry := image.Entries[names[i]]; entry.Kind == "dir" {
			if err := os.Chmod(filepath.Join(dir, names[i]), fs.FileMode(entry.Mode)); err != nil {
				return err
			}
		}
	}
	return nil
}

func restoreSQLite(path string, image sqliteImage) (err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if image.Schema == nil {
		return restoreRelayStore(path, image)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, pragma := range []string{"PRAGMA journal_mode=" + image.Journal, fmt.Sprintf("PRAGMA user_version=%d", image.Version)} {
		if _, err = db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("%s: %w", pragma, err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	for _, entry := range image.Schema {
		if entry[0] == "table" {
			if _, err = tx.Exec(entry[1]); err != nil {
				return fmt.Errorf("%s: %w", entry[1], err)
			}
		}
	}
	for _, table := range image.Tables {
		if table.Name == "sqlite_sequence" || len(table.Rows) == 0 {
			continue
		}
		if err = insertRows(tx, table); err != nil {
			return fmt.Errorf("%s: %w", table.Name, err)
		}
	}
	for _, table := range image.Tables {
		if table.Name != "sqlite_sequence" {
			continue
		}
		if _, err = tx.Exec("DELETE FROM sqlite_sequence"); err != nil {
			return err
		}
		if err = insertRows(tx, table); err != nil {
			return fmt.Errorf("%s: %w", table.Name, err)
		}
	}
	for _, entry := range image.Schema {
		if entry[0] != "table" {
			if _, err = tx.Exec(entry[1]); err != nil {
				return fmt.Errorf("%s: %w", entry[1], err)
			}
		}
	}
	return tx.Commit()
}

// restoreRelayStore rebuilds a store of the relay's own schema from the frozen empty store:
// every table emptied, then the recorded rows put back.
func restoreRelayStore(path string, image sqliteImage) (err error) {
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
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	var tables []string
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	rows.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	for _, name := range tables {
		if _, err = tx.Exec("DELETE FROM " + quoteIdent(name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	for _, last := range []bool{false, true} {
		// sqlite_sequence last: inserting the other tables' rows writes it too.
		for _, table := range image.Tables {
			if (table.Name == "sqlite_sequence") != last {
				continue
			}
			if last {
				if _, err = tx.Exec("DELETE FROM sqlite_sequence"); err != nil {
					return err
				}
			}
			if err = insertRows(tx, table); err != nil {
				return fmt.Errorf("%s: %w", table.Name, err)
			}
		}
	}
	return tx.Commit()
}

func insertRows(tx *sql.Tx, table sqliteTable) error {
	columns := make([]string, len(table.Columns))
	for i, column := range table.Columns {
		columns[i] = quoteIdent(column)
	}
	for _, row := range table.Rows {
		values := make([]any, len(row))
		marks := make([]string, len(row))
		for i, value := range row {
			marks[i] = "?"
			kind, rest, _ := strings.Cut(value, ":")
			switch kind {
			case "n":
				values[i] = nil
			case "i":
				var n int64
				if _, err := fmt.Sscan(rest, &n); err != nil {
					return err
				}
				values[i] = n
			case "r":
				f, err := strconv.ParseFloat(rest, 64)
				if err != nil {
					return err
				}
				values[i] = f
			case "t":
				values[i] = rest
			case "x":
				raw, err := base64.StdEncoding.DecodeString(rest)
				if err != nil {
					return err
				}
				values[i], marks[i] = raw, "CAST(? AS TEXT)"
			case "b":
				raw, err := base64.StdEncoding.DecodeString(rest)
				if err != nil {
					return err
				}
				values[i] = raw
			default:
				return fmt.Errorf("value %q", value)
			}
		}
		if _, err := tx.Exec("INSERT INTO "+quoteIdent(table.Name)+" ("+strings.Join(columns, ", ")+") VALUES ("+strings.Join(marks, ", ")+")", values...); err != nil {
			return err
		}
	}
	return nil
}

// volatile is what a live Python fixture answers differently on every run: temporary names and
// generated identifiers (numbered by first appearance, so which ones repeat still counts), and
// wall-clock instants (ISO, or a fault occurrence's recorded_ts in epoch seconds), ages and device
// and inode numbers (masked outright).
var (
	volatile = regexp.MustCompile(`relay-test-[A-Za-z0-9_]{8}|tmp[A-Za-z0-9_]{8}|[0-9a-f]{12,}`)
	measured = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:\+00:00|Z)|\\?"(?:device|inode|logDevice|logInode|walDirectoryDevice|walDirectoryInode)\\?":\s*\d+|[0-9]{3,}:[0-9]{3,}|(?:ageSeconds|oldestStagedAgeSeconds|recorded_ts)\\?":\s*[0-9.e+-]+`)
)

// canonicalVolatile renames each distinct volatile token by its first appearance and masks each
// measurement, so two answers that differ only in generated names and readings compare equal.
func canonicalVolatile(raw []byte) string {
	names := map[string]string{}
	text := measured.ReplaceAllString(string(raw), "<measured>")
	return volatile.ReplaceAllStringFunc(text, func(token string) string {
		if name, ok := names[token]; ok {
			return name
		}
		name := fmt.Sprintf("<v%d>", len(names))
		names[token] = name
		return name
	})
}

// sameFixture is pyoracle.SameWhen for a recorded fixture or answer (check mode): equal up to
// its volatile tokens. Every answer this package records is checked so; a caller's own SameWhen
// given later replaces it.
var sameFixture = pyoracle.SameWhen(func(recorded, live []byte) bool {
	return canonicalVolatile(recorded) == canonicalVolatile(live)
})

// pythonFixture is a tree a Python fixture builder (build) leaves under dir, which must be a
// fixedTree or a directory whose path the recording substitutes: in record and check mode build
// runs and its tree is recorded; on replay the recorded tree is written under dir. It returns
// what build printed. Each fenced store in it is rehomed (its mirror republished with the
// restored file's identity), as a copy of a store would be.
//
// include names the paths under dir the fixture is made of (nil for all of dir), so what else a
// Python run leaves there (uv's cache under an isolated XDG_CACHE_HOME) is not recorded.
func pythonFixture(t testing.TB, key, dir string, include []string, build func() (string, error), options ...pyoracle.Option) string {
	t.Helper()
	var fixture struct {
		Output string    `json:"output"`
		Tree   treeImage `json:"tree"`
	}
	options = append(append([]pyoracle.Option{pyoracle.Substitute(dir, "<fixture>")}, placeholders(dir)...), options...)
	options = append(options, sameFixture)
	pyoracle.JSON(t, key, &fixture, func() (any, error) {
		output, err := build()
		if err != nil {
			return nil, err
		}
		image, err := snapshotTree(dir, include)
		if err != nil {
			return nil, err
		}
		fixture.Output, fixture.Tree = output, image
		return fixture, nil
	}, options...)
	if !pyoracle.Live() {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := materializeTree(dir, fixture.Tree); err != nil {
			t.Fatalf("pyoracle: rebuild the recorded fixture %s: %v", key, err)
		}
		for name, entry := range fixture.Tree.Entries {
			if entry.Kind == "sqlite" && fixture.Tree.Entries[filepath.Join(filepath.Dir(name), "takeover.json")].Kind == "file" {
				testsupport.Rehome(t, filepath.Join(dir, name))
			}
		}
	}
	return fixture.Output
}

// ------------------------------------------------------------------- ownership stamps

// stampStore puts a stopped store in owner's hands at epoch in phase, in both halves:
// schema_meta's owner, owner_epoch and takeover_id, and the takeover.json mirror, republished
// with the file's physical identity (the Python fence's own stamp, test_fence.py, which
// testdata/readonly_matrix.py stamp ran). takeover names the transition a phase starting
// completes; a phase draining is a transition to the other runtime.
func stampStore(t testing.TB, db, owner, phase string, epoch int64, takeover string) {
	t.Helper()
	connection, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"owner": owner, "owner_epoch": strconv.FormatInt(epoch, 10), "takeover_id": takeover} {
		if _, err = connection.Exec("UPDATE schema_meta SET value=? WHERE key=?", value, key); err != nil {
			_ = connection.Close()
			t.Fatal(err)
		}
	}
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(filepath.Dir(db), "takeover.json")
	raw, err := os.ReadFile(mirror)
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{}
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	other := map[string]string{"python": "go", "go": "python"}[owner]
	var transition any
	switch {
	case phase == "draining":
		transition = map[string]any{"id": "t-drain", "from": owner, "to": other, "targetEpoch": epoch + 1}
	case takeover != "":
		transition = map[string]any{"id": takeover, "from": other, "to": owner, "targetEpoch": epoch}
	}
	physical, err := ownership.Physical(db)
	if err != nil {
		t.Fatal(err)
	}
	record["owner"], record["epoch"], record["phase"], record["transition"] = owner, epoch, phase, transition
	record["database"], record["relayRPCSocket"] = physical, filepath.Join(filepath.Dir(db), "control.sock")
	if phase == "starting" {
		record["controller"] = map[string]any{"bootId": "boot", "pid": 1, "startTicks": 1}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(record); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(mirror, bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ------------------------------------------------------------------- the Python relay

// oracleLabel spells words for a key: a word naming a temporary directory or the repository
// (in any spelling a test builds from one) as <path>, and every byte that is not printable UTF-8
// escaped.
func oracleLabel(words ...string) string {
	tmp := filepath.Clean(os.TempDir())
	root := repositoryRootPath()
	spelled := make([]string, len(words))
	for i, word := range words {
		spelled[i] = word
		for _, path := range []string{tmp, root, fixedRoot} {
			if strings.Contains(word, path) || strings.Contains(word, strings.ReplaceAll(path, "/", "_")) {
				spelled[i] = "<path>"
			}
		}
		if generated.MatchString(word) {
			spelled[i] = "<id>"
		}
	}
	quoted := fmt.Sprintf("%q", spelled)
	if len(quoted) > 300 {
		sum := sha256.Sum256([]byte(quoted))
		quoted = quoted[:240] + "..." + hex.EncodeToString(sum[:8])
	}
	return quoted
}

// fence is the answer the live Python fence's console script (the worktree's .venv) gave for
// argv exactly as given: unlike python(), it hands no store over first, so it sees each state as
// a host would.
func fence(t *testing.T, argv ...string) run {
	t.Helper()
	program := filepath.Join(repositoryRoot(t), ".venv", "bin", "codex-session-relay")
	options := placeholders(argv...)
	if id := selectedStoreID(t, "", argv); id != "" {
		options = append(options, pyoracle.Substitute(id, "<store-id>"))
	}
	return oracleRun(t, oracleKey(t, "fence "+oracleLabel(argv...)), func() (run, error) {
		return execute(program, argv...)
	}, options...)
}

// pythonCreates makes the store at state as the fence's absent-store initializer creates one
// (testsupport.Create, the frozen Python-created store, stamped python at epoch 1). That is a
// writer form's work: a read-only form never creates a store (cutover.md Record).
func pythonCreates(t *testing.T, state string) {
	t.Helper()
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "python")
}

// storeIdentity is what names one store file rather than its contents: its generated id and
// creation instant, and its device and inode numbers (as fields, or as a device:inode pair).
var (
	storeIdentity = regexp.MustCompile(`("storeId": )"[0-9a-f]{32}"|("createdAt": )"[^"]*"|("(?:device|inode|logDevice|logInode)": )[0-9]+`)
	identityPair  = regexp.MustCompile(`[0-9]{3,}:[0-9]{3,}`)
)

// identityNeutral masks storeIdentity and identity pairs in a printed answer, keeping it JSON: a
// recorded answer was read from another store file with the same contents.
func identityNeutral(text string) string {
	return identityPair.ReplaceAllString(storeIdentity.ReplaceAllString(text, `${1}${2}${3}"<identity>"`), "<identity>")
}

// pythonProcess is what a Python process (program args..., with env and input on stdin) answered
// (recorded: see oracleRun). The environment's temporary directories are placeholders too.
func pythonProcess(t *testing.T, key string, env []string, input, program string, args ...string) processResult {
	t.Helper()
	anchors := append([]string{}, args...)
	for _, entry := range env {
		if _, value, ok := strings.Cut(entry, "="); ok && (strings.Contains(value, os.TempDir()) || strings.Contains(value, fixedRoot)) {
			anchors = append(anchors, value)
		}
	}
	answer := oracleRun(t, key, func() (run, error) {
		ran := runParityProcessInput(t, env, input, program, args...)
		return run{ran.code, ran.out, ran.err}, nil
	}, placeholders(anchors...)...)
	return processResult{answer.code, answer.stdout, answer.stderr}
}

var (
	processTreesMu sync.Mutex
	processTrees   = map[string]string{}
	processLocks   []*os.File // held (and so locked) until the process ends
)

// processFixedTree is fixedTree for the whole test process: the same directory for key in every
// test, removed when the process ends (TestMain).
func processFixedTree(key string) (string, error) {
	processTreesMu.Lock()
	defer processTreesMu.Unlock()
	if dir, ok := processTrees[key]; ok {
		return dir, nil
	}
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(fixedRoot, hex.EncodeToString(sum[:])[:12])
	if err := os.MkdirAll(fixedRoot, 0o755); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", errors.Join(err, lock.Close())
	}
	processLocks = append(processLocks, lock)
	if err = testsupport.RemoveTempTree(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	processTrees[key] = dir
	return dir, nil
}

// removeProcessTrees removes every processFixedTree (TestMain, after the tests).
func removeProcessTrees() error {
	processTreesMu.Lock()
	defer processTreesMu.Unlock()
	var err error
	for _, dir := range processTrees {
		err = errors.Join(err, testsupport.RemoveTempTree(dir))
	}
	return err
}
