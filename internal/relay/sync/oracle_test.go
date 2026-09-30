package sync

// The parity tests in this package compare Go with what the Python reference implementation
// answered. Python's answers are recorded (internal/testsupport/pyoracle): CRW_PYTHON_ORACLE=record
// asks the live Python and writes testdata/python-oracle, =check asks it again and compares, and
// the default replays the recording. Some answers are state Python left behind, a SQLite store the
// Go side then runs a command against; such a store is recorded as its rows (storeDump) and
// rebuilt for the Go side, never as the database file.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
	_ "modernc.org/sqlite"
)

// pythonAnswer is pyoracle.AnswerInterned (these answers repeat themselves call after call) keyed
// by the driver script and a digest of the question the test put to it, so a question that
// changes finds no recorded answer instead of another one.
func pythonAnswer(t *testing.T, script string, question []byte, capture func() ([]byte, error), opts ...pyoracle.Option) []byte {
	t.Helper()
	sum := sha256.Sum256(question)
	return pyoracle.AnswerInterned(t, script+" "+hex.EncodeToString(sum[:8]), capture, opts...)
}

// relocated moves every path under from to the same path under to. A capture records Python's
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
	// Refs, in a recording, stand for Rows: indexes into the rows the recording pools.
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

func quoteName(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func readSchema(ctx context.Context, db *sql.DB) ([][4]*string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY rowid")
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
		frozen.raw, frozen.err = os.ReadFile(filepath.Join("..", "..", "..", "contract", "fixtures", "sqlite-ddl", "python-store.sqlite3"))
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

// dumpStore reads the database Python left at path.
func dumpStore(path string) (*storeDump, error) {
	ctx := context.Background()
	db, err := openSQLite(path, "ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	dump := &storeDump{Tables: map[string]dumpTable{}}
	if err = db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&dump.Journal); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	schema, err := readSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	_, frozenSchema, err := frozenStore()
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(schema, frozenSchema) {
		dump.Schema = schema
	}
	for _, entry := range schema {
		if *entry[0] != "table" {
			continue
		}
		if entry[3] != nil && strings.Contains(strings.ToUpper(*entry[3]), "WITHOUT ROWID") {
			return nil, fmt.Errorf("%s: table %s has no rowid, which a dump does not carry", path, *entry[1])
		}
		table, err := dumpRows(ctx, db, *entry[1])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if len(table.Rows) > 0 {
			dump.Tables[*entry[1]] = table
		}
	}
	return dump, nil
}

func dumpRows(ctx context.Context, db *sql.DB, name string) (dumpTable, error) {
	table := dumpTable{Columns: []string{}}
	info, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?) ORDER BY cid", name)
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
	if err = errors.Join(info.Err(), info.Close()); err != nil {
		return table, err
	}
	// A unary plus drops the declared type, so the driver answers each value by its storage
	// class rather than converting, say, a DATETIME column's text.
	selected := []string{"rowid"}
	for _, column := range table.Columns {
		selected = append(selected, "+"+quoteName(column))
	}
	rows, err := db.QueryContext(ctx, "SELECT "+strings.Join(selected, ", ")+" FROM "+quoteName(name)+" ORDER BY rowid")
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
		for i, value := range values {
			switch v := value.(type) {
			case nil, int64:
			case float64:
				values[i] = map[string]string{"real": strconv.FormatFloat(v, 'g', -1, 64)}
			case []byte:
				values[i] = map[string]string{"blob": hex.EncodeToString(v)}
			case string:
				if !utf8.ValidString(v) {
					values[i] = map[string]string{"textHex": hex.EncodeToString([]byte(v))}
				}
			default:
				return table, fmt.Errorf("table %s: value of type %T", name, value)
			}
		}
		table.Rows = append(table.Rows, values)
	}
	return table, rows.Err()
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
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+quoteName(name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		table, known := dump.Tables[name]
		if !known {
			continue
		}
		columns := []string{"rowid"}
		marks := []string{"?"}
		for _, column := range table.Columns {
			columns = append(columns, quoteName(column))
			marks = append(marks, "?")
		}
		statement := "INSERT INTO " + quoteName(name) + " (" + strings.Join(columns, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
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

// storeRecording is an answer that carries stores. Each store's rows are pooled: a row several
// stores hold (a scenario's store grows call by call) is recorded once.
type storeRecording[T any] struct {
	Rows   []json.RawMessage `json:"rows"`
	Answer T                 `json:"answer"`
}

// storeRecorder builds a storeRecording in a capture.
type storeRecorder struct {
	index map[string]int
	rows  []json.RawMessage
	dumps []*storeDump
	// stable pairs what a Python store is given at creation and a rerun changes, its store_id
	// and store_created_at, with a fixed stand-in, so the recording is the same on every run
	// and names no real time.
	stable   []string
	replacer *strings.Replacer
	stores   int
}

func newStoreRecorder() *storeRecorder {
	return &storeRecorder{index: map[string]int{}, replacer: strings.NewReplacer()}
}

// identify gives the store a dump holds its stand-ins.
func (r *storeRecorder) identify(dump *storeDump) {
	meta := dump.Tables["schema_meta"]
	key, value := slices.Index(meta.Columns, "key"), slices.Index(meta.Columns, "value")
	if key < 0 || value < 0 {
		return
	}
	for _, row := range meta.Rows {
		name, _ := row[key+1].(string)
		current, _ := row[value+1].(string)
		if current == "" || slices.Contains(r.stable, current) {
			continue
		}
		switch name {
		case "store_id":
			r.stores++
			r.stable = append(r.stable, current, fmt.Sprintf("5a1e%028x", r.stores))
		case "store_created_at":
			r.stable = append(r.stable, current, "2000-01-01T00:00:00Z")
		default:
			continue
		}
		r.replacer = strings.NewReplacer(r.stable...)
	}
}

func (r *storeRecorder) stabilize(text []byte) []byte {
	return []byte(r.replacer.Replace(string(text)))
}

// dump reads the store Python left at path and answers the form the recording carries.
func (r *storeRecorder) dump(path string) (json.RawMessage, error) {
	dump, err := dumpStore(path)
	if err != nil {
		return nil, err
	}
	r.dumps = append(r.dumps, dump)
	r.identify(dump)
	pooled := &storeDump{Schema: dump.Schema, Journal: dump.Journal, Tables: map[string]dumpTable{}}
	for _, name := range slices.Sorted(maps.Keys(dump.Tables)) {
		table := dump.Tables[name]
		refs := make([]int, 0, len(table.Rows))
		for _, row := range table.Rows {
			encoded, err := marshalPlain(row)
			if err != nil {
				return nil, err
			}
			// Pooled as recorded: two rows that differ only by a stand-in's value are one.
			encoded = r.stabilize(encoded)
			i, known := r.index[string(encoded)]
			if !known {
				i = len(r.rows)
				r.index[string(encoded)] = i
				r.rows = append(r.rows, encoded)
			}
			refs = append(refs, i)
		}
		pooled.Tables[name] = dumpTable{Columns: table.Columns, Refs: refs}
	}
	return marshalPlain(pooled)
}

// finish records answer with the pool. Python's store identities, and its temporary directories
// under pyTmp when it had them (in the order output then the stores name them), get stable names.
func (r *storeRecorder) finish(answer any, output []byte, pyTmp string) ([]byte, error) {
	encoded, err := marshalPlain(storeRecording[any]{Rows: r.rows, Answer: answer})
	if err != nil {
		return nil, err
	}
	encoded = r.stabilize(encoded)
	if pyTmp == "" {
		return encoded, nil
	}
	texts := [][]byte{output}
	for _, dump := range r.dumps {
		text, err := marshalPlain(dump)
		if err != nil {
			return nil, err
		}
		texts = append(texts, text)
	}
	if encoded, err = stableTemporaryNames(encoded, pyTmp, texts...); err != nil {
		return nil, err
	}
	return relocated(encoded, pyTmp, "<pytmp>"), nil
}

// storePool is the rows a storeRecording pools.
type storePool []json.RawMessage

// openStores reads a storeRecording: its answer, and the pool its stores draw rows from.
func openStores(t *testing.T, raw []byte) (json.RawMessage, storePool) {
	t.Helper()
	var recording storeRecording[json.RawMessage]
	if e := json.Unmarshal(raw, &recording); e != nil {
		t.Fatal(e)
	}
	return recording.Answer, recording.Rows
}

// dump reads a recorded store; numbers stay exact.
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

// stableTemporaryNames renames each temporary directory Python created under pyTmp (a random
// name such as relay-test-1a2b3c4d) by the order it first appears in texts, which follow Python's
// own output order, so a rerun records the same names. It fails when the answer still carries a
// name the texts never showed.
func stableTemporaryNames(answer []byte, pyTmp string, texts ...[]byte) ([]byte, error) {
	pattern := regexp.MustCompile(regexp.QuoteMeta(pyTmp+"/") + `([^/"\\\s']+)`)
	index := map[string]string{}
	var pairs []string
	for _, text := range texts {
		for _, match := range pattern.FindAllSubmatch(text, -1) {
			name := string(match[1])
			if _, done := index[name]; done {
				continue
			}
			stable := fmt.Sprintf("tmp-%d", len(index)+1)
			if prefix, _, found := strings.Cut(name, "-test-"); found {
				stable = fmt.Sprintf("%s-test-%d", prefix, len(index)+1)
			}
			index[name] = stable
			pairs = append(pairs, pyTmp+"/"+name, pyTmp+"/"+stable)
		}
	}
	if len(pairs) > 0 {
		answer = []byte(strings.NewReplacer(pairs...).Replace(string(answer)))
	}
	stable := map[string]bool{}
	for _, name := range index {
		stable[name] = true
	}
	for _, match := range pattern.FindAllSubmatch(answer, -1) {
		if !stable[string(match[1])] {
			return nil, fmt.Errorf("temporary directory %s appears only where its order is not Python's", match[0])
		}
	}
	return answer, nil
}

// callsWithStores records captured calls whose "database" names a store Python backed up: each
// becomes that store's dump.
func callsWithStores(output []byte, pyTmp string) ([]byte, error) {
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(output, &calls); err != nil {
		return nil, err
	}
	stores := newStoreRecorder()
	for _, call := range calls {
		var path *string
		if err := json.Unmarshal(call["database"], &path); err != nil || path == nil {
			continue
		}
		var err error
		if call["database"], err = stores.dump(*path); err != nil {
			return nil, err
		}
	}
	return stores.finish(calls, output, pyTmp)
}

var identifier = regexp.MustCompile(`[0-9a-f]{12,}`)

// sameUpToIdentifiers is the check-mode comparison for an answer that carries identifiers
// digested from a path Python chose at random (a scenario's temporary directory): the two answers
// must agree once each hex identifier is renamed by the order it first appears.
func sameUpToIdentifiers(recorded, live []byte) bool {
	canonical := func(data []byte) []byte {
		names := map[string]string{}
		return identifier.ReplaceAllFunc(data, func(match []byte) []byte {
			name, ok := names[string(match)]
			if !ok {
				name = fmt.Sprintf("<id-%d>", len(names)+1)
				names[string(match)] = name
			}
			return []byte(name)
		})
	}
	return bytes.Equal(canonical(recorded), canonical(live))
}

// marshalPlain is json.Marshal without HTML escaping, so a placeholder such as <pytmp> stays
// as written.
func marshalPlain(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// storeCapturesRecorded records store_capture.py's output: each call's store backup becomes its
// dump, and each file it read is kept as text (so a path in it is relocated with the rest) unless
// it is not UTF-8.
func storeCapturesRecorded(output []byte, pyTmp string) ([]byte, error) {
	var captures []storeCapture
	if err := json.Unmarshal(output, &captures); err != nil {
		return nil, err
	}
	stores := newStoreRecorder()
	for i := range captures {
		c := &captures[i]
		var path *string
		if len(c.Database) > 0 {
			if err := json.Unmarshal(c.Database, &path); err != nil {
				return nil, err
			}
		}
		c.Database = nil
		if path != nil {
			var err error
			if c.Database, err = stores.dump(*path); err != nil {
				return nil, err
			}
		}
		if digests := pathDigests(c, pyTmp); len(digests) > 0 {
			c.PathDigests = digests
		}
		for _, files := range []map[string]*string{c.Files, c.After} {
			for name, encoded := range files {
				if encoded == nil {
					continue
				}
				data, err := hex.DecodeString(*encoded)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				recorded := "hex:" + *encoded
				if utf8.Valid(data) {
					recorded = "text:" + string(data)
				}
				files[name] = &recorded
			}
		}
	}
	answer, err := stores.finish(captures, output, pyTmp)
	if err != nil {
		return nil, err
	}
	// Paths key the file maps: encoding again orders them by their stable names.
	var stable storeRecording[[]storeCapture]
	if err = json.Unmarshal(answer, &stable); err != nil {
		return nil, err
	}
	return marshalPlain(stable)
}

// recordedFileBytes reads a file's recorded content: "text:" then the text, or "hex:" then its
// bytes in hex.
func recordedFileBytes(recorded string) ([]byte, error) {
	if text, ok := strings.CutPrefix(recorded, "text:"); ok {
		return []byte(text), nil
	}
	if encoded, ok := strings.CutPrefix(recorded, "hex:"); ok {
		return hex.DecodeString(encoded)
	}
	return nil, fmt.Errorf("recorded file content %.40q is neither text: nor hex:", recorded)
}

// cliCapturesRecorded records cli_capture.py's output with each command's store backup as its
// dump, and every file a command read through an @path argument, all under the placeholder
// <base> for Python's working directory.
func cliCapturesRecorded(output []byte, base string) ([]byte, error) {
	var captures []map[string]json.RawMessage
	if err := json.Unmarshal(output, &captures); err != nil {
		return nil, err
	}
	files := map[string]string{}
	stores := newStoreRecorder()
	for _, capture := range captures {
		var path string
		if err := json.Unmarshal(capture["database"], &path); err != nil {
			return nil, err
		}
		var err error
		if capture["database"], err = stores.dump(path); err != nil {
			return nil, err
		}
		var argv []string
		if err = json.Unmarshal(capture["argv"], &argv); err != nil {
			return nil, err
		}
		for _, arg := range argv {
			if named, ok := strings.CutPrefix(arg, "@"); ok {
				data, err := os.ReadFile(named)
				if err != nil {
					return nil, err
				}
				files[named] = string(data)
			}
		}
	}
	answer, err := stores.finish(cliAnswer{Captures: captures, Files: files}, output, "")
	return relocated(answer, base, "<base>"), err
}

// cliAnswer is what cli_capture.py's Python left for the Go side: its captured commands, and the
// files they read.
type cliAnswer struct {
	Captures any               `json:"captures"`
	Files    map[string]string `json:"files"`
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

// pathDigests finds the packets a call read whose content names one of Python's temporary
// directories: their content digest is the path's as much as the packet's.
func pathDigests(c *storeCapture, pyTmp string) map[string]string {
	digests := map[string]string{}
	for i := 0; i+1 < len(c.Argv); i++ {
		if c.Argv[i] != "--packet" || c.Files[c.Argv[i+1]] == nil {
			continue
		}
		data, err := hex.DecodeString(*c.Files[c.Argv[i+1]])
		if err != nil || !bytes.Contains(data, []byte(pyTmp)) {
			continue
		}
		digest, err := packetDigest(data)
		if err != nil {
			continue
		}
		digests[digest] = c.Argv[i+1]
	}
	return digests
}

// relocatedDigests follows a relocation into the digests that depend on it: each packet digest
// Python gave for its own temporary path becomes the digest of the same packet at this run's
// path, wherever the answer carries it (a later call's ledger, a store, an expected output).
// Python's own digest function was compared with Go's where the packet carries no path.
func relocatedDigests(t *testing.T, answer []byte) []byte {
	t.Helper()
	var captures []storeCapture
	if e := json.Unmarshal(answer, &captures); e != nil {
		t.Fatal(e)
	}
	var pairs []string
	for _, c := range captures {
		for digest, path := range c.PathDigests {
			encoded := c.Files[path]
			if encoded == nil {
				t.Fatalf("no recorded packet %s", path)
			}
			data, e := recordedFileBytes(*encoded)
			if e != nil {
				t.Fatal(e)
			}
			relocatedDigest, e := packetDigest(data)
			if e != nil {
				t.Fatal(e)
			}
			pairs = append(pairs, digest, relocatedDigest)
		}
	}
	if len(pairs) == 0 {
		return answer
	}
	return []byte(strings.NewReplacer(pairs...).Replace(string(answer)))
}
