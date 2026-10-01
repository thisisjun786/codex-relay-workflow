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
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The Python reference implementation's answers are recorded (internal/testsupport/pyoracle).
// Most of this package's Python runs answer on stdout, and some also leave files the Go side
// then reads: a store the Python fixture built, snapshots of it taken mid-test, a capture file.
// pythonTree records both, the files as what the test reads from them - each SQLite store as its
// rows, table by table - and in every mode leaves the directory holding exactly what was
// recorded, each store rebuilt from its rows on the schema Go creates, which is Python's.

// repoRoot is the repository root, which Python's answers name wherever they name the program or
// a source file.
func repoRoot(t testing.TB) string {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// pyKeys numbers the answers a test asks for, for a test that asks the same question more than
// once in a fixed sequence.
var pyKeys sync.Map

// pyKey is a key unique within the running test: what, numbered by the order it is asked in.
func pyKey(t testing.TB, what string) string {
	counter, _ := pyKeys.LoadOrStore(t, new(int))
	n := counter.(*int)
	*n++
	return fmt.Sprintf("%d %s", *n, what)
}

// pythonOutput is what a live Python run printed, recorded under key with the repository root
// substituted. Check mode compares it with the recording up to random identifiers
// (sameUpToRandomIDs) unless opts name another comparison.
func pythonOutput(t testing.TB, key string, capture func() ([]byte, error), opts ...pyoracle.Option) []byte {
	t.Helper()
	opts = append([]pyoracle.Option{pyoracle.SameWhen(sameUpToRandomIDs)}, opts...)
	canonical := func() ([]byte, error) {
		start := time.Now()
		out, err := capture()
		return canonicalWallTimes(canonicalTempTrees(out), start, time.Now()), err
	}
	return pyoracle.Answer(t, key, canonical, append(opts, pyoracle.Substitute(repoRoot(t), "<repo>"))...)
}

// isoTime is an ISO 8601 date and time as Python's datetime.isoformat and the relay's clocks
// write it.
var isoTime = regexp.MustCompile(`\b(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d{1,9})?(Z|[+-]\d\d:\d\d)?`)

// wallTimeBase is where canonicalWallTimes puts the start of a Python run: after every fixed
// time the fixtures use and before any day a rerun happens on, so the wall-clock times keep
// their order among the fixtures' times and stay in the past.
var wallTimeBase = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

// canonicalWallTimes rewrites each time the Python run took from the wall clock - any time
// between start and end - to a fixed time after wallTimeBase, the n-th earliest n seconds after
// it, written with the same precision and zone. The recording then names no time of the run that
// made it, a rerun's answer names the same times, and the times keep their order.
func canonicalWallTimes(data []byte, start, end time.Time) []byte {
	parse := func(match []byte) (time.Time, string, string, bool) {
		parts := isoTime.FindSubmatch(match)
		fraction, zone := string(parts[2]), string(parts[3])
		location := time.Local
		if zone == "Z" {
			location = time.UTC
		} else if zone != "" {
			offset, err := time.Parse("-07:00", zone)
			if err != nil {
				return time.Time{}, "", "", false
			}
			_, seconds := offset.Zone()
			location = time.FixedZone("", seconds)
		}
		at, err := time.ParseInLocation("2006-01-02T15:04:05", string(parts[1]), location)
		if err != nil {
			return time.Time{}, "", "", false
		}
		if fraction != "" {
			nanos, _ := strconv.Atoi((fraction[1:] + "000000000")[:9])
			at = at.Add(time.Duration(nanos))
		}
		return at, fraction, zone, !at.Before(start) && !at.After(end)
	}
	var instants []time.Time
	for _, match := range isoTime.FindAll(data, -1) {
		if at, _, _, ok := parse(match); ok {
			instants = append(instants, at)
		}
	}
	if len(instants) == 0 {
		return data
	}
	sort.Slice(instants, func(i, j int) bool { return instants[i].Before(instants[j]) })
	rank := map[int64]int{}
	for _, at := range instants {
		if _, ok := rank[at.UnixNano()]; !ok {
			rank[at.UnixNano()] = len(rank)
		}
	}
	return isoTime.ReplaceAllFunc(data, func(match []byte) []byte {
		at, fraction, zone, ok := parse(match)
		if !ok {
			return match
		}
		moved := wallTimeBase.Add(time.Duration(rank[at.UnixNano()]+1) * time.Second).In(at.Location())
		out := moved.Format("2006-01-02T15:04:05")
		if fraction != "" {
			digits := fmt.Sprintf("%09d", moved.Nanosecond())
			out += "." + digits[:len(fraction)-1]
		}
		return []byte(out + zone)
	})
}

// pythonTempTree is a temporary tree a Python test fixture made for itself
// (tests/support.py's tempfile.mkdtemp(prefix="relay-test-")) where no capture driver redirects
// it: gone when the fixture is, but named in the rows and messages the fixture wrote.
var pythonTempTree = regexp.MustCompile(regexp.QuoteMeta(filepath.Clean(os.TempDir())) + `/relay-test-[A-Za-z0-9_]{8}`)

// canonicalTempTrees rewrites each Python fixture's temporary tree to a fixed name of the same
// length, numbered in order of appearance, so the recording names no path of the machine that
// made it and a rerun names the same trees. The length is kept because the answers measure the
// bytes of what they render; nothing reads the tree, which the fixture removed.
func canonicalTempTrees(data []byte) []byte {
	seen := map[string]string{}
	return pythonTempTree.ReplaceAllFunc(data, func(match []byte) []byte {
		tree, ok := seen[string(match)]
		if !ok {
			n := strconv.Itoa(len(seen) + 1)
			parent := len(match) - len("/relay-test-") - 8 - 1
			tree = "/" + strings.Repeat("t", parent) + "/relay-test-" + strings.Repeat("0", 8-len(n)) + n
			seen[string(match)] = tree
		}
		return []byte(tree)
	})
}

// randomID is a run of hex digits long enough to be a random identifier or a digest of one.
var randomID = regexp.MustCompile(`[0-9a-f]{12,}`)

// sameUpToRandomIDs is equality up to a consistent renaming of random identifiers. The Python
// fixtures mint their event ids at random, and every identifier and digest derived from one then
// differs on a rerun too, so a rerun's answer is the recorded one with each such hex run replaced,
// the same run by the same replacement everywhere. The Go side reads the recorded identifiers
// (the stores it restores carry them), so in replay nothing is renamed.
func sameUpToRandomIDs(recorded, live []byte) bool {
	if bytes.Equal(recorded, live) {
		return true
	}
	numbered := func(data []byte) []byte {
		seen := map[string]int{}
		return randomID.ReplaceAllFunc(data, func(match []byte) []byte {
			n, ok := seen[string(match)]
			if !ok {
				n = len(seen)
				seen[string(match)] = n
			}
			return []byte(fmt.Sprintf("<id %d/%d>", n, len(match)))
		})
	}
	return bytes.Equal(numbered(recorded), numbered(live))
}

// treeRecord is one recorded Python run over a directory: what it printed and the files it left.
type treeRecord struct {
	Output string            `json:"output"`
	Dirs   []string          `json:"dirs,omitempty"`
	Files  map[string]string `json:"files,omitempty"`
	// JSON holds the capture drivers' capture.json files as the JSON they are, which the Go
	// side decodes; written back compact.
	JSON   map[string]json.RawMessage `json:"json,omitempty"`
	Stores map[string]*storeDump      `json:"stores,omitempty"`
	Links  map[string]string          `json:"links,omitempty"`
	// Modes are the permissions of the directories (default 0700) and files (default 0600)
	// that have others.
	Modes map[string]string `json:"modes,omitempty"`
}

// pythonTree runs capture, a live Python run that writes under root and returns what it
// printed, in record and check mode, and records the output and the files under root. In every
// mode it then empties root and puts back what was recorded, so the Go side reads the same tree
// whether Python ran or not. root must belong to the run. Not recorded: hidden directories
// (a HOME's caches), a store's takeover.json mirror, lock files and SQLite's -wal, -shm and
// -journal files; a restored store gets its mirror from the test's ownership fixtures, as any
// copied store does.
func pythonTree(t testing.TB, key, root string, capture func() ([]byte, error), opts ...pyoracle.Option) []byte {
	t.Helper()
	return recordTree(t, key, root, false, capture, opts...)
}

// pythonTreeRevisions is pythonTree for a run whose Go half verifies a stored receipt's manifest
// against its revision hash. The manifest names the files under root, so the hash is a digest
// of root's path: it is recorded as a placeholder per event and derived again from the manifest
// the restored store holds.
func pythonTreeRevisions(t testing.TB, key, root string, capture func() ([]byte, error), opts ...pyoracle.Option) []byte {
	t.Helper()
	return recordTree(t, key, root, true, capture, opts...)
}

func recordTree(t testing.TB, key, root string, revisions bool, capture func() ([]byte, error), opts ...pyoracle.Option) []byte {
	t.Helper()
	raw := pythonOutput(t, key, func() ([]byte, error) {
		output, err := capture()
		if err != nil {
			return nil, err
		}
		record, err := readTree(root)
		if err != nil {
			return nil, err
		}
		record.Output = string(output)
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(record); err != nil {
			return nil, err
		}
		text := canonicalStores(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), record)
		if revisions {
			return revisionPlaceholders(text, record)
		}
		return text, nil
	}, append([]pyoracle.Option{pyoracle.Substitute(root, "<root>")}, opts...)...)
	if revisions {
		var err error
		if raw, err = rederivedRevisions(raw); err != nil {
			t.Fatalf("pythonTree %s: %v", key, err)
		}
	}
	var record treeRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("pythonTree %s: %v", key, err)
	}
	if err := writeTree(t, root, record); err != nil {
		t.Fatalf("pythonTree %s: %v", key, err)
	}
	return []byte(record.Output)
}

func skippedInTree(rel string, entry fs.DirEntry) bool {
	name := entry.Name()
	if strings.HasPrefix(name, ".") || name == "__pycache__" {
		return true
	}
	if entry.IsDir() {
		return false
	}
	return name == "takeover.json" || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal") || strings.HasSuffix(name, ".sock")
}

func readTree(root string) (treeRecord, error) {
	record := treeRecord{Files: map[string]string{}, JSON: map[string]json.RawMessage{}, Stores: map[string]*storeDump{}, Links: map[string]string{}, Modes: map[string]string{}}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if skippedInTree(rel, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			record.Links[rel] = target
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			record.Dirs = append(record.Dirs, rel)
			if info.Mode().Perm() != 0o700 {
				record.Modes[rel] = strconv.FormatUint(uint64(info.Mode().Perm()), 8)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if info.Mode().Perm() != 0o600 {
			record.Modes[rel] = strconv.FormatUint(uint64(info.Mode().Perm()), 8)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
			dump, err := dumpStore(path)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			record.Stores[rel] = dump
			return nil
		}
		if entry.Name() == "capture.json" && json.Valid(data) {
			var compact bytes.Buffer
			if err := json.Compact(&compact, data); err != nil {
				return err
			}
			record.JSON[rel] = compact.Bytes()
		} else if utf8.Valid(data) {
			record.Files[rel] = "txt:" + string(data)
		} else {
			record.Files[rel] = "b64:" + base64.StdEncoding.EncodeToString(data)
		}
		return nil
	})
	sort.Strings(record.Dirs)
	return record, err
}

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

// revisionPlaceholders writes each receipt's revision hash that is the hash of its manifest,
// and its 12-digit prefix, as a placeholder naming the event.
func revisionPlaceholders(text []byte, record treeRecord) ([]byte, error) {
	hashes := map[string]string{}
	err := receiptRevisions(record, func(event, claimed, derived string) {
		if claimed == derived {
			hashes[derived] = event
		}
	})
	for hash, event := range hashes {
		text = bytes.ReplaceAll(text, []byte(hash), []byte("<revision of "+event+">"))
	}
	for hash, event := range hashes {
		text = bytes.ReplaceAll(text, []byte(hash[:12]), []byte("<revision of "+event+"/12>"))
	}
	return text, err
}

// rederivedRevisions puts back each placeholder revisionPlaceholders wrote: the hash of the
// manifest the event's receipt carries, which names the files where this run's tree has them.
func rederivedRevisions(text []byte) ([]byte, error) {
	var record treeRecord
	if err := json.Unmarshal(text, &record); err != nil {
		return nil, err
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
	return text, err
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
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master")
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
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
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

func dumpStore(path string) (*storeDump, error) {
	ctx := context.Background()
	template, err := storeTemplate()
	if err != nil {
		return nil, err
	}
	db, err := openRaw(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	digest, err := schemaDigest(ctx, db)
	if err != nil {
		return nil, err
	}
	dump := &storeDump{Schema: digest}
	if digest != template.digest {
		// Not a relay store: kept whole.
		if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		dump.Raw = base64.StdEncoding.EncodeToString(data)
		return dump, nil
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&dump.UserVersion); err != nil {
		return nil, err
	}
	names, err := tableNames(ctx, db)
	if err != nil {
		return nil, err
	}
	dump.Tables = map[string]*tableDump{}
	for _, name := range names {
		rows, err := db.QueryContext(ctx, `SELECT rowid AS "<rowid>", * FROM "`+name+`" ORDER BY rowid`)
		if err != nil {
			return nil, err
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, err
		}
		table := &tableDump{Columns: columns[1:]}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(values))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				return nil, err
			}
			row := make([]any, len(values))
			for i, value := range values {
				switch v := value.(type) {
				case nil, int64, string:
					row[i] = v
				case float64:
					row[i] = map[string]string{"real": strconv.FormatFloat(v, 'g', -1, 64)}
				case []byte:
					row[i] = map[string]string{"blob": base64.StdEncoding.EncodeToString(v)}
				default:
					rows.Close()
					return nil, fmt.Errorf("%s: unexpected %T", name, value)
				}
			}
			table.Rows = append(table.Rows, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if len(table.Rows) > 0 {
			dump.Tables[name] = table
		}
	}
	return dump, nil
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
		return fmt.Errorf("recorded store schema %s is not the schema Go creates (%s)", dump.Schema, tmpl.digest)
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
	rows, err := db.QueryContext(ctx, "SELECT name, sql FROM sqlite_master WHERE type='trigger' ORDER BY rowid")
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
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER "`+name+`"`); err != nil {
			return err
		}
	}
	for _, name := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM "`+name+`"`); err != nil {
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
			quoted[i] = `"` + column + `"`
			marks[i] = "?"
		}
		quoted[0] = "rowid"
		if name == "sqlite_sequence" {
			quoted, marks = quoted[1:], marks[1:]
		}
		statement := `INSERT INTO "` + name + `" (` + strings.Join(quoted, ", ") + `) VALUES (` + strings.Join(marks, ", ") + `)`
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

var storeCreatedAt = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)

// canonicalStores rewrites, wherever the record names them, what a Python store's creation
// makes different on every run: each store's random store_id, to 32 hex digits numbering the
// stores in order of appearance, and the time it was created, to a fixed time. The Go side reads
// the stores as recorded, so it meets the same values Python's answers name.
func canonicalStores(encoded []byte, record treeRecord) []byte {
	var paths []string
	for rel := range record.Stores {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	ids, times := map[string]string{}, map[string]string{}
	for _, rel := range paths {
		meta := record.Stores[rel].Tables["schema_meta"]
		if meta == nil {
			continue
		}
		for _, row := range meta.Rows {
			key, _ := row[1].(string)
			value, _ := row[2].(string)
			switch {
			case key == "store_id" && value != "":
				if _, ok := ids[value]; !ok {
					ids[value] = fmt.Sprintf("%032x", len(ids)+1)
				}
			case key == "store_created_at" && storeCreatedAt.MatchString(value):
				if _, ok := times[value]; !ok {
					times[value] = fmt.Sprintf("2000-01-01T00:00:%02dZ", len(times))
				}
			}
		}
	}
	text := string(encoded)
	for from, to := range ids {
		text = strings.ReplaceAll(text, from, to)
	}
	for from, to := range times {
		text = strings.ReplaceAll(text, from, to)
	}
	return []byte(text)
}

// The recording helpers, for this package's external tests (package supervisor_test).
var (
	PythonTree          = pythonTree
	PythonTreeRevisions = pythonTreeRevisions
	PythonOutput        = pythonOutput
	RemoveStoreFiles    = removeStoreFiles
)
