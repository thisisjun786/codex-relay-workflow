package supervisor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Tree fixtures. Many of this package's tests start from files the Python reference
// implementation's test fixtures left, which Go cannot make: a store a fixture built, snapshots of
// it taken mid-test, the operations a capture driver listed. They are fixtures under
// testdata/fixtures/trees, one file per test holding each tree under the key the test names, and
// treeFixture restores one: its directories, files and links, and each SQLite store rebuilt from
// its rows, table by table, on the schema Go creates. A tree names the directory it is restored in
// as <root> and the repository root as <repo>. What the Python run answered is not in the
// fixtures: the tests compare Go's answers with goldens (internal/testsupport/golden).

// treeFixtureFile is the stored form of a test's tree fixture.
type treeFixtureFile struct {
	Note  string                     `json:"note"`
	Trees map[string]json.RawMessage `json:"trees"`
}

var treeFixtureFiles sync.Map

// treeFixtureText is the tree the calling test's fixture holds under key, as stored.
func treeFixtureText(t testing.TB, key string) []byte {
	t.Helper()
	name := "trees/" + testFileName(t.Name()) + ".json"
	trees, ok := treeFixtureFiles.Load(name)
	if !ok {
		var file treeFixtureFile
		if err := json.Unmarshal(golden.Fixture(t, name), &file); err != nil {
			t.Fatalf("tree fixture %s: %v", name, err)
		}
		trees, _ = treeFixtureFiles.LoadOrStore(name, file.Trees)
	}
	raw, ok := trees.(map[string]json.RawMessage)[key]
	if !ok {
		t.Fatalf("tree fixture %s holds no tree %q", name, key)
	}
	return append([]byte(nil), raw...)
}

// treeFixture empties root and restores in it the tree the calling test's fixture holds under
// key. Each placeholder pair names a value the tree holds as placeholder[1], put back as
// placeholder[0]; <root> and <repo> are always put back.
func treeFixture(t testing.TB, key, root string, placeholders ...[2]string) {
	t.Helper()
	restoreTreeFixture(t, key, root, false, placeholders)
}

// treeFixtureRevisions is treeFixture for a tree whose receipts carry a revision hash that is the
// hash of the receipt's manifest. The manifest names files under root, so the hash is stored as a
// placeholder per event and derived again from the restored manifest. It returns the golden
// options that write each derived hash, and its 12-digit prefix, back as its placeholder.
func treeFixtureRevisions(t testing.TB, key, root string, placeholders ...[2]string) []golden.Option {
	t.Helper()
	return restoreTreeFixture(t, key, root, true, placeholders)
}

func restoreTreeFixture(t testing.TB, key, root string, revisions bool, placeholders [][2]string) []golden.Option {
	t.Helper()
	text := treeFixtureText(t, key)
	text = bytes.ReplaceAll(text, []byte("<repo>"), []byte(repoRoot(t)))
	for i := len(placeholders) - 1; i >= 0; i-- {
		text = bytes.ReplaceAll(text, []byte(placeholders[i][1]), []byte(placeholders[i][0]))
	}
	text = bytes.ReplaceAll(text, []byte("<root>"), []byte(root))
	var options []golden.Option
	if revisions {
		var hashes map[string]string
		var err error
		if text, hashes, err = derivedRevisions(text); err != nil {
			t.Fatalf("tree fixture %q: %v", key, err)
		}
		events := make([]string, 0, len(hashes))
		for event := range hashes {
			events = append(events, event)
		}
		sort.Strings(events)
		for _, event := range events {
			options = append(options, golden.Substitute(hashes[event], "<revision of "+event+">"))
		}
		for _, event := range events {
			options = append(options, golden.Substitute(hashes[event][:12], "<revision of "+event+"/12>"))
		}
	}
	var record treeRecord
	if err := json.Unmarshal(text, &record); err != nil {
		t.Fatalf("tree fixture %q: %v", key, err)
	}
	if err := writeTree(t, root, record); err != nil {
		t.Fatalf("tree fixture %q: %v", key, err)
	}
	return options
}

// derivedRevisions puts back each revision placeholder a tree holds: the hash of the manifest the
// event's receipt carries, which names the files where this run's tree has them. It returns the
// hash of each event.
func derivedRevisions(text []byte) ([]byte, map[string]string, error) {
	var record treeRecord
	if err := json.Unmarshal(text, &record); err != nil {
		return nil, nil, err
	}
	hashes := map[string]string{}
	err := receiptRevisions(record, func(event, claimed, derived string) {
		if claimed == "<revision of "+event+">" {
			hashes[event] = derived
		}
	})
	for event, hash := range hashes {
		text = bytes.ReplaceAll(text, []byte("<revision of "+event+">"), []byte(hash))
		text = bytes.ReplaceAll(text, []byte("<revision of "+event+"/12>"), []byte(hash[:12]))
	}
	return text, hashes, err
}

// testFileName is the file name a test's golden or tree fixture takes from the test's
// name, without its extension (as internal/testsupport/golden names it).
func testFileName(name string) string {
	var b strings.Builder
	plain := len(name) <= 120 && !strings.Contains(name, "__")
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '/':
			b.WriteString("__")
			plain = false
		default:
			b.WriteByte('_')
			plain = false
		}
	}
	base := b.String()
	if !plain {
		sum := sha256.Sum256([]byte(name))
		if len(base) > 100 {
			base = base[:100]
		}
		base += "-" + hex.EncodeToString(sum[:])[:12]
	}
	return base
}

// goldenKeys numbers the goldens a test checks under the same name, for a test that checks one
// in a fixed sequence.
var goldenKeys sync.Map

// goldenKey is a key unique within the running test: what, numbered by the order it is asked in.
func goldenKey(t testing.TB, what string) string {
	counter, _ := goldenKeys.LoadOrStore(t, new(int))
	n := counter.(*int)
	*n++
	return fmt.Sprintf("%d %s", *n, what)
}

// treeGolden is the golden options for a value that names a restored tree's directory or the
// repository root.
func treeGolden(t testing.TB, root string) []golden.Option {
	t.Helper()
	return []golden.Option{golden.Substitute(root, "<root>"), golden.Substitute(repoRoot(t), "<repo>")}
}

// fixtureGolden is the golden options for a value that names a stage fixture's directory
// (fixture24) or the repository root.
func fixtureGolden(t testing.TB, root string) []golden.Option {
	t.Helper()
	return []golden.Option{golden.Substitute(root, "<fixture>"), golden.Substitute(repoRoot(t), "<repo>")}
}

// goldenWallTimes substitutes the wall-clock times Go wrote into output - the times of the last
// hour - for placeholders numbered in order of appearance, so a golden names none of them.
func goldenWallTimes(output []byte) []golden.Option {
	now := time.Now()
	var options []golden.Option
	seen := map[string]bool{}
	for _, match := range isoTime.FindAll(output, -1) {
		text := string(match)
		if seen[text] {
			continue
		}
		seen[text] = true
		parts := isoTime.FindSubmatch(match)
		at, err := time.Parse("2006-01-02T15:04:05Z07:00", string(parts[1])+string(parts[3]))
		if err != nil || at.Before(now.Add(-time.Hour)) || at.After(now.Add(time.Minute)) {
			continue
		}
		options = append(options, golden.Substitute(text, fmt.Sprintf("<go wall time %d>", len(options)+1)))
	}
	return options
}

// asJSON is value as JSON decodes it: objects as maps, numbers as float64.
func asJSON(t testing.TB, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

// repoRoot is the repository root, which answers name wherever they name the program or a source
// file.
func repoRoot(t testing.TB) string {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// isoTime is an ISO 8601 date and time as Python's datetime.isoformat and the relay's clocks
// write it.
var isoTime = regexp.MustCompile(`\b(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d{1,9})?(Z|[+-]\d\d:\d\d)?`)

// treeRecord is a tree as a fixture holds it: its directories, files and links, the JSON capture
// files a test reads as the JSON they are, and each SQLite store as its rows.
type treeRecord struct {
	Dirs  []string          `json:"dirs,omitempty"`
	Files map[string]string `json:"files,omitempty"`
	// JSON holds the capture drivers' capture.json files as the JSON they are, which the Go
	// side decodes; written back as they are stored.
	JSON   map[string]json.RawMessage `json:"json,omitempty"`
	Stores map[string]*storeDump      `json:"stores,omitempty"`
	Links  map[string]string          `json:"links,omitempty"`
	// Modes are the permissions of the directories (default 0700) and files (default 0600)
	// that have others.
	Modes map[string]string `json:"modes,omitempty"`
}

// writeTree empties root and writes record in it, each store rebuilt from its rows.
func writeTree(t testing.TB, root string, record treeRecord) error {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	for _, dir := range record.Dirs {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return err
		}
	}
	for rel, stored := range record.Files {
		var data []byte
		if rest, ok := strings.CutPrefix(stored, "b64:"); ok {
			if data, err = base64.StdEncoding.DecodeString(rest); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		} else {
			data = []byte(strings.TrimPrefix(stored, "txt:"))
		}
		if err := os.WriteFile(filepath.Join(root, rel), data, 0o600); err != nil {
			return err
		}
	}
	for rel, data := range record.JSON {
		if err := os.WriteFile(filepath.Join(root, rel), data, 0o600); err != nil {
			return err
		}
	}
	for rel, dump := range record.Stores {
		if err := buildStore(t, filepath.Join(root, rel), dump); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
	}
	for rel, target := range record.Links {
		if err := os.Symlink(target, filepath.Join(root, rel)); err != nil {
			return err
		}
	}
	// Deepest first, so a directory is made read-only after what is under it is written.
	var moded []string
	for rel := range record.Modes {
		moded = append(moded, rel)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(moded)))
	for _, rel := range moded {
		mode, err := strconv.ParseUint(record.Modes[rel], 8, 32)
		if err != nil {
			return err
		}
		if err := os.Chmod(filepath.Join(root, rel), fs.FileMode(mode)); err != nil {
			return err
		}
	}
	return nil
}

// receiptRevisions yields, for each events row of each store in record, its event id, the
// revision hash its receipt claims and the hash of the manifest the receipt carries.
func receiptRevisions(record treeRecord, each func(event, claimed, derived string)) error {
	for _, dump := range record.Stores {
		table := dump.Tables["events"]
		if table == nil {
			continue
		}
		column := map[string]int{}
		for i, name := range table.Columns {
			column[name] = i + 1
		}
		for _, row := range table.Rows {
			event, _ := row[column["event_id"]].(string)
			text, _ := row[column["receipt"]].(string)
			var receipt struct {
				RevisionHash string                `json:"revisionHash"`
				Manifest     []store.ManifestEntry `json:"manifest"`
			}
			if json.Unmarshal([]byte(text), &receipt) != nil || len(receipt.Manifest) == 0 {
				continue
			}
			derived, err := store.ManifestRevision(receipt.Manifest)
			if err != nil {
				continue
			}
			each(event, receipt.RevisionHash, derived)
		}
	}
	return nil
}

// storeDump is a SQLite store as the test reads it: every table's rows in rowid order, each value
// with its SQLite type (an integer as a JSON number, a text as a JSON string, a real as
// {"real": text}, a blob as {"blob": base64}, NULL as null). Schema is the digest of the store's
// schema, which buildStore requires the schema Go creates to have. A database that is not a
// relay store is kept whole as Raw.
type storeDump struct {
	Schema      string                `json:"schema"`
	UserVersion int64                 `json:"userVersion,omitempty"`
	Tables      map[string]*tableDump `json:"tables,omitempty"`
	Raw         string                `json:"raw,omitempty"`
}

type tableDump struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

func openRaw(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func schemaDigest(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master WHERE name NOT LIKE 'dag\\_%' ESCAPE '\\' AND tbl_name NOT LIKE 'dag\\_%' ESCAPE '\\'")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var kind, name, table, text string
		if err := rows.Scan(&kind, &name, &table, &text); err != nil {
			return "", err
		}
		lines = append(lines, kind+"\x00"+name+"\x00"+table+"\x00"+text)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

func tableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'dag\\_%' ESCAPE '\\' AND tbl_name NOT LIKE 'dag\\_%' ESCAPE '\\' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// template is an empty store as Go creates it, copied for every store buildStore rebuilds.
type template struct {
	data   []byte
	digest string
}

var (
	templateOnce sync.Once
	templateData template
	templateErr  error
)

func storeTemplate() (template, error) {
	templateOnce.Do(func() {
		templateErr = func() error {
			dir, err := os.MkdirTemp("", "crw-store-template-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "relay.sqlite3")
			s, err := store.Open(context.Background(), path, "")
			if err != nil {
				return err
			}
			if err := s.Close(); err != nil {
				return err
			}
			db, err := openRaw(path)
			if err != nil {
				return err
			}
			defer db.Close()
			ctx := context.Background()
			if templateData.digest, err = schemaDigest(ctx, db); err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				return err
			}
			if err := db.Close(); err != nil {
				return err
			}
			templateData.data, err = os.ReadFile(path)
			return err
		}()
	})
	return templateData, templateErr
}

// buildStore writes at path the store dump describes: the template's schema holding exactly the
// dumped rows, including schema_meta's and sqlite_sequence's, with triggers and foreign keys off
// while they are loaded so no row is added or refused on the way.
func buildStore(t testing.TB, path string, dump *storeDump) error {
	t.Helper()
	if dump.Raw != "" {
		data, err := base64.StdEncoding.DecodeString(dump.Raw)
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0o600)
	}
	tmpl, err := storeTemplate()
	if err != nil {
		return err
	}
	if dump.Schema != tmpl.digest {
		return fmt.Errorf("the fixture's store schema %s is not the schema Go creates (%s)", dump.Schema, tmpl.digest)
	}
	scratch, err := os.MkdirTemp("", "crw-store-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	built := filepath.Join(scratch, "relay.sqlite3")
	if err := os.WriteFile(built, tmpl.data, 0o600); err != nil {
		return err
	}
	if err := loadRows(built, dump); err != nil {
		return err
	}
	data, err := os.ReadFile(built)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func loadRows(path string, dump *storeDump) (err error) {
	ctx := context.Background()
	db, err := openRaw(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	var triggers []string
	rows, err := db.QueryContext(ctx, "SELECT name, sql FROM sqlite_master WHERE type='trigger' AND name NOT LIKE 'dag\\_%' ESCAPE '\\' AND tbl_name NOT LIKE 'dag\\_%' ESCAPE '\\' ORDER BY rowid")
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
		triggers = append(triggers, text)
	}
	rows.Close()
	tables, err := tableNames(ctx, db)
	if err != nil {
		return err
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
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, "DROP TRIGGER "+testsupport.QuoteIdent(name)); err != nil {
			return err
		}
	}
	for _, name := range tables {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+testsupport.QuoteIdent(name)); err != nil {
			return err
		}
	}
	// sqlite_sequence last: an AUTOINCREMENT table's inserts add its own row, which the dumped
	// rows then replace.
	var loaded []string
	for name := range dump.Tables {
		if name != "sqlite_sequence" {
			loaded = append(loaded, name)
		}
	}
	sort.Strings(loaded)
	if dump.Tables["sqlite_sequence"] != nil {
		loaded = append(loaded, "sqlite_sequence")
	}
	for _, name := range loaded {
		table := dump.Tables[name]
		if name == "sqlite_sequence" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM sqlite_sequence"); err != nil {
				return err
			}
		}
		columns := append([]string{"rowid"}, table.Columns...)
		quoted := make([]string, len(columns))
		marks := make([]string, len(columns))
		for i, column := range columns {
			quoted[i] = testsupport.QuoteIdent(column)
			marks[i] = "?"
		}
		quoted[0] = "rowid"
		if name == "sqlite_sequence" {
			quoted, marks = quoted[1:], marks[1:]
		}
		statement := `INSERT INTO ` + testsupport.QuoteIdent(name) + ` (` + strings.Join(quoted, ", ") + `) VALUES (` + strings.Join(marks, ", ") + `)`
		for _, row := range table.Rows {
			values := make([]any, len(row))
			for i, value := range row {
				if values[i], err = storedValue(value); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
			}
			if name == "sqlite_sequence" {
				values = values[1:]
			}
			if _, err := tx.ExecContext(ctx, statement, values...); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	for _, text := range triggers {
		if _, err := tx.ExecContext(ctx, text); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version="+strconv.FormatInt(dump.UserVersion, 10)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// storedValue turns a dumped value back into the value SQLite stored.
func storedValue(value any) (any, error) {
	switch v := value.(type) {
	case nil, string:
		return v, nil
	case json.Number:
		return v.Int64()
	case float64:
		if v != float64(int64(v)) {
			return nil, fmt.Errorf("integer %v", v)
		}
		return int64(v), nil
	case map[string]any:
		if text, ok := v["real"].(string); ok {
			return strconv.ParseFloat(text, 64)
		}
		if text, ok := v["blob"].(string); ok {
			return base64.StdEncoding.DecodeString(text)
		}
	}
	return nil, fmt.Errorf("unexpected dumped value %#v", value)
}

// UnmarshalJSON keeps integers exact.
func (d *tableDump) UnmarshalJSON(data []byte) error {
	var raw struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	d.Columns, d.Rows = raw.Columns, raw.Rows
	return nil
}

// The tree fixture helpers, for this package's external tests (package supervisor_test).
var (
	TreeFixture          = treeFixture
	TreeFixtureRevisions = treeFixtureRevisions
	TreeGolden           = treeGolden
)
