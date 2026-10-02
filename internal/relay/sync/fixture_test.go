package sync

// The scenario tests in this package replay what the Python reference implementation's scenarios
// did and compare Go's answers with goldens (internal/testsupport/golden, testdata/golden). What
// those scenarios produced that Go cannot, the calls they made and the state they left (SQLite
// stores, packet, record and ledger files), is each test's fixture under testdata/fixtures/<kind>.
// A store is held as its rows (storeDump) in a pool shared by the fixture's stores and rebuilt on
// the frozen Python store (contract/fixtures/sqlite-ddl/python-store.sqlite3), never as a database
// file. Placeholders stand for this run's directories: <pytmp> for the scenarios' temporary
// directories, <base> for the working directory, <home> for the outbox scenarios' home.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
	_ "modernc.org/sqlite"
)

// readFixture reads the calling test's fixture of kind, testdata/fixtures/<kind>/<test>.json (or
// .json.gz).
func readFixture(t *testing.T, kind string) []byte {
	t.Helper()
	return golden.Fixture(t, filepath.Join(kind, t.Name()+".json"))
}

// sameScenarios fails the test when its fixture was taken from other scenarios than it names.
func sameScenarios(t *testing.T, fixture, names []string) {
	t.Helper()
	if !slices.Equal(fixture, names) {
		t.Fatalf("the fixture holds scenarios %v, not %v", fixture, names)
	}
}

// relocated moves every path under from to the same path under to. A fixture holds Python's
// working directory under a placeholder, and the Go side puts back a directory of its own.
func relocated(answer []byte, from, to string) []byte {
	return bytes.ReplaceAll(answer, []byte(from), []byte(to))
}

// storeDump is a SQLite database as Python left it: every non-empty table's rows with their
// rowids, and the schema unless it is the frozen Python store's
// (contract/fixtures/sqlite-ddl/python-store.sqlite3), which a restore then starts from.
type storeDump struct {
	Schema  [][4]*string         `json:"schema,omitempty"`
	Journal string               `json:"journal"`
	Tables  map[string]dumpTable `json:"tables"`
}

type dumpTable struct {
	Columns []string `json:"columns"`
	// Rows hold the rowid first, then each column: null, an integer, a string, or
	// {"real": text}, {"blob": hex}, {"textHex": hex} for the values JSON cannot carry exactly.
	Rows [][]any `json:"rows,omitempty"`
	// Refs, in a fixture, stand for Rows: indexes into the rows the fixture pools.
	Refs []int `json:"refs,omitempty"`
}

func openSQLite(path, mode string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", mode)
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func readSchema(ctx context.Context, db *sql.DB) ([][4]*string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM sqlite_master WHERE name NOT LIKE 'dag\\_%' ESCAPE '\\' AND tbl_name NOT LIKE 'dag\\_%' ESCAPE '\\' ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schema [][4]*string
	for rows.Next() {
		var entry [4]sql.NullString
		if err = rows.Scan(&entry[0], &entry[1], &entry[2], &entry[3]); err != nil {
			return nil, err
		}
		var out [4]*string
		for i, value := range entry {
			if value.Valid {
				out[i] = &value.String
			}
		}
		schema = append(schema, out)
	}
	return schema, rows.Err()
}

var frozen struct {
	once   stdsync.Once
	raw    []byte
	schema [][4]*string
	err    error
}

// frozenStore is the Python-produced empty store the contract freezes, and its schema.
func frozenStore() ([]byte, [][4]*string, error) {
	frozen.once.Do(func() {
		frozen.raw, frozen.err = testsupport.FrozenStore()
		if frozen.err != nil {
			return
		}
		dir, err := os.MkdirTemp("", "sync-frozen-store-")
		if err != nil {
			frozen.err = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "relay.sqlite3")
		if frozen.err = os.WriteFile(path, frozen.raw, 0o600); frozen.err != nil {
			return
		}
		db, err := openSQLite(path, "ro")
		if err != nil {
			frozen.err = err
			return
		}
		frozen.schema, frozen.err = readSchema(context.Background(), db)
		frozen.err = errors.Join(frozen.err, db.Close())
	})
	return frozen.raw, frozen.schema, frozen.err
}

func storedValue(value any) (any, error) {
	switch v := value.(type) {
	case nil, string:
		return v, nil
	case json.Number:
		return v.Int64()
	case float64:
		if v != float64(int64(v)) {
			return nil, fmt.Errorf("non-integer number %v", v)
		}
		return int64(v), nil
	case map[string]any:
		for kind, raw := range v {
			encoded, _ := raw.(string)
			switch kind {
			case "real":
				return strconv.ParseFloat(encoded, 64)
			case "blob":
				return hex.DecodeString(encoded)
			case "textHex":
				decoded, err := hex.DecodeString(encoded)
				return string(decoded), err
			}
		}
	}
	return nil, fmt.Errorf("value %#v", value)
}

// restoreStore writes the dumped database at path, which must not exist yet.
func restoreStore(t *testing.T, dump *storeDump, path string) {
	t.Helper()
	if err := dump.restore(path); err != nil {
		t.Fatalf("restore Python's store at %s: %v", path, err)
	}
}

func (dump *storeDump) restore(path string) (err error) {
	ctx := context.Background()
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, schema, err := frozenStore()
	if err != nil {
		return err
	}
	fresh := dump.Schema != nil
	if fresh {
		schema = dump.Schema
	} else if err = os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	db, err := openSQLite(path, "rwc")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var journal string
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode = "+dump.Journal).Scan(&journal); err != nil {
		return err
	}
	if journal != dump.Journal {
		return fmt.Errorf("journal mode %s, not %s", journal, dump.Journal)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	tables := []string{}
	for _, entry := range schema {
		kind, name := *entry[0], *entry[1]
		if kind == "trigger" {
			return fmt.Errorf("trigger %s: a restore does not replay triggers", name)
		}
		if kind == "table" && name != "sqlite_sequence" {
			tables = append(tables, name)
		}
		if !fresh || entry[3] == nil || strings.HasPrefix(name, "sqlite_") {
			continue
		}
		if _, err = tx.ExecContext(ctx, *entry[3]); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	// sqlite_sequence goes last: inserting into an AUTOINCREMENT table moves it.
	for _, name := range append(tables, "sqlite_sequence") {
		if _, known := dump.Tables[name]; !known && fresh {
			continue
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+testsupport.QuoteIdent(name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		table, known := dump.Tables[name]
		if !known {
			continue
		}
		columns := []string{"rowid"}
		marks := []string{"?"}
		for _, column := range table.Columns {
			columns = append(columns, testsupport.QuoteIdent(column))
			marks = append(marks, "?")
		}
		statement := "INSERT INTO " + testsupport.QuoteIdent(name) + " (" + strings.Join(columns, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
		for _, row := range table.Rows {
			values := make([]any, len(row))
			for i, value := range row {
				if values[i], err = storedValue(value); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
			}
			if _, err = tx.ExecContext(ctx, statement, values...); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if dump.Journal == "wal" {
		_, err = db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	}
	return err
}

// storePool is the rows a fixture's stores share: a row several stores hold (a scenario's store
// grows call by call) is held once, and a store's dumpTable names its rows by index (Refs).
type storePool []json.RawMessage

// dump reads a store the fixture holds; numbers stay exact.
func (p storePool) dump(t *testing.T, raw json.RawMessage) *storeDump {
	t.Helper()
	decode := func(raw []byte, out any) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if e := decoder.Decode(out); e != nil {
			t.Fatal(e)
		}
	}
	var dump storeDump
	decode(raw, &dump)
	for name, table := range dump.Tables {
		for _, ref := range table.Refs {
			if ref < 0 || ref >= len(p) {
				t.Fatalf("table %s names row %d of %d", name, ref, len(p))
			}
			var row []any
			decode(p[ref], &row)
			table.Rows = append(table.Rows, row)
		}
		table.Refs = nil
		dump.Tables[name] = table
	}
	return &dump
}

// fileBytes reads a file's content as a fixture or a golden holds it (fileContent).
func fileBytes(content string) ([]byte, error) {
	if text, ok := strings.CutPrefix(content, "text:"); ok {
		return []byte(text), nil
	}
	if encoded, ok := strings.CutPrefix(content, "hex:"); ok {
		return hex.DecodeString(encoded)
	}
	return nil, fmt.Errorf("file content %.40q is neither text: nor hex:", content)
}

// fileContent is data as a fixture or a golden holds a file: "text:" then the text, or "hex:"
// then its bytes in hex.
func fileContent(data []byte) *string {
	encoded := "hex:" + hex.EncodeToString(data)
	if utf8.Valid(data) {
		encoded = "text:" + string(data)
	}
	return &encoded
}

// packetDigest is the content digest packet-check gives the packet a file holds.
func packetDigest(data []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	packet, err := decodeValue(decoder)
	if err != nil {
		return "", err
	}
	return reception.ContentDigest(packet), nil
}

// packetDigests derives the content digest of each packet a fixture names in PathDigests from the
// packet at this run's path, since the path it carries moves the digest: placeholder then digest,
// pair by pair, in placeholder order.
func packetDigests(t *testing.T, captures []storeCapture) []string {
	t.Helper()
	digests := map[string]string{}
	for _, c := range captures {
		for placeholder, path := range c.PathDigests {
			encoded := c.Files[path]
			if encoded == nil {
				t.Fatalf("the fixture holds no packet %s", path)
			}
			data, e := fileBytes(*encoded)
			if e != nil {
				t.Fatal(e)
			}
			digest, e := packetDigest(data)
			if e != nil {
				t.Fatal(e)
			}
			if known, ok := digests[placeholder]; ok && known != digest {
				t.Fatalf("%s is both %s and %s", placeholder, known, digest)
			}
			digests[placeholder] = digest
		}
	}
	var pairs []string
	for _, placeholder := range slices.Sorted(maps.Keys(digests)) {
		pairs = append(pairs, placeholder, digests[placeholder])
	}
	return pairs
}
