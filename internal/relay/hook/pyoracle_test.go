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

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The Python reference implementation's answers are recorded (internal/testsupport/pyoracle) and
// replayed; only CRW_PYTHON_ORACLE=record or check runs it. A test whose fixture the Python
// implementation lays out (its Store, marker and receipt functions) records the tree Python left
// with the answers (pythonFixture) and lays that tree out again before Go reads it, in every mode,
// so the Go side reads the same files on replay as when the answers were recorded.

// pythonScript runs one of this package's Python drivers or an inline script with the workspace
// interpreter in env (nil: this process's), and returns its standard output; stderr is named in
// the error. Only a capture calls it.
func pythonScript(t *testing.T, env []string, stdin []byte, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(python(t), args...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("python %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, out, &stderr)
	}
	return out, nil
}

// fixtureEntry is one path of a recorded fixture tree.
type fixtureEntry struct {
	Kind   string   `json:"kind"` // dir, file, link or sqlite
	Mode   uint32   `json:"mode,omitempty"`
	Text   *string  `json:"text,omitempty"`
	Base64 string   `json:"base64,omitempty"`
	Target string   `json:"target,omitempty"`
	Schema string   `json:"schema,omitempty"`
	Rows   []string `json:"rows,omitempty"`
}

// fixtureAnswer is what a Python fixture step printed and the tree it left.
type fixtureAnswer struct {
	Output string                  `json:"output"`
	Tree   map[string]fixtureEntry `json:"tree"`
}

// pythonFixture records what prepare (the Python that lays a fixture out under root) printed and
// the tree it left under root, then lays that tree out again under root and returns the output
// and the fixture's names. The repository root is recorded as <REPO> and root as <ROOT>, and the
// digest of a path under root (a marker workspace key, a scope key) as that path's name
// (fixtureNames); in check mode ids and times a rerun mints anew are not compared (sameVolatile).
// transform, when given, runs after prepare and before the snapshot, to drop what the test does
// not read (a whole process environment). hashed names paths under root that the tree does not
// hold but whose digests it may (a socket path that was never bound).
func pythonFixture(t *testing.T, key, root string, prepare func() ([]byte, error), transform func() error, hashed ...string) ([]byte, *fixtureNames) {
	t.Helper()
	names := newFixtureNames(t, root, hashed...)
	raw := names.unspell(pyoracle.Answer(t, key, func() ([]byte, error) {
		out, err := prepare()
		if err != nil {
			return nil, err
		}
		if transform != nil {
			if err = transform(); err != nil {
				return nil, err
			}
		}
		tree, err := snapshotTree(root)
		if err != nil {
			return nil, err
		}
		for rel := range tree {
			names.rels = append(names.rels, rel)
		}
		raw, err := encodeJSON(fixtureAnswer{Output: string(out), Tree: tree})
		return names.spell(raw), err
	}, pyoracle.Substitute(root, "<ROOT>"), pyoracle.Substitute(testRoot, "<REPO>"), pyoracle.SameWhen(sameVolatile)))
	var answer fixtureAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("recorded fixture %q: %v", key, err)
	}
	if err := clearDir(root); err != nil {
		t.Fatal(err)
	}
	if err := restoreTree(t, root, answer.Tree); err != nil {
		t.Fatalf("restore the recorded fixture %q: %v", key, err)
	}
	return []byte(answer.Output), names
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

// answer is pyoracle.Answer with the fixture's digests spelled as names in the recording.
func (n *fixtureNames) answer(t *testing.T, key string, capture func() ([]byte, error), opts ...pyoracle.Option) []byte {
	t.Helper()
	return n.unspell(pyoracle.Answer(t, key, func() ([]byte, error) {
		out, err := capture()
		return n.spell(out), err
	}, opts...))
}

// encodeJSON is json.Marshal without HTML escaping, so a recording reads as the text it holds.
func encodeJSON(v any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

var (
	volatileTime = regexp.MustCompile(`[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})?`)
	volatileHex  = regexp.MustCompile(`[0-9a-f]{32}`)
)

// sameVolatile compares two answers apart from the timestamps and 32-hex identifiers (store ids,
// takeover ids) a rerun of the Python mints anew.
func sameVolatile(recorded, live []byte) bool {
	normal := func(b []byte) []byte {
		return volatileHex.ReplaceAll(volatileTime.ReplaceAll(b, []byte("T")), []byte("H"))
	}
	return bytes.Equal(normal(recorded), normal(live))
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

// snapshotTree reads every directory, regular file and link under root. A SQLite database is
// kept as its rows (sqliteRows); its -wal and -shm files are read through it. A mirror's
// physical identity is kept as the path it names alone, since a restored file has its own.
func snapshotTree(root string) (map[string]fixtureEntry, error) {
	tree := map[string]fixtureEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if path == root {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode := uint32(info.Mode().Perm())
		switch {
		case info.IsDir():
			tree[rel] = fixtureEntry{Kind: "dir", Mode: mode}
			if walkErr != nil {
				return fs.SkipDir // a directory this user may not read is kept empty
			}
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			tree[rel] = fixtureEntry{Kind: "link", Target: target}
		case info.Mode().IsRegular():
			if strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") || strings.HasSuffix(path, "-journal") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.HasPrefix(raw, []byte("SQLite format 3\x00")) {
				schema, rows, err := sqliteRows(path)
				if err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				tree[rel] = fixtureEntry{Kind: "sqlite", Mode: mode, Schema: schema, Rows: rows}
				return nil
			}
			if filepath.Base(path) == "takeover.json" {
				raw = mirrorWithoutIdentity(raw)
			}
			entry := fixtureEntry{Kind: "file", Mode: mode}
			if utf8.Valid(raw) {
				text := string(raw)
				entry.Text = &text
			} else {
				entry.Base64 = base64.StdEncoding.EncodeToString(raw)
			}
			tree[rel] = entry
		}
		return nil
	})
	return tree, err
}

// mirrorWithoutIdentity replaces a takeover.json mirror's database identity with the path it
// names, and its scope key with a mark: restoreTree gives it the restored file's identity and the
// scope key of its socket under this run's CODEX_SESSION_RELAY_SCOPE_DIR, which salts the key
// and differs from run to run. Anything else is kept as it is.
func mirrorWithoutIdentity(raw []byte) []byte {
	record, ok := decodeMirror(raw)
	if !ok {
		return raw
	}
	database, _ := record["database"].(map[string]any)
	if database == nil {
		return raw
	}
	record["database"] = map[string]any{"realPath": database["realPath"]}
	if _, ok := record["appServerSocket"].(string); ok && record["scopeKey"] != nil {
		record["scopeKey"] = "<scope key of appServerSocket>"
	}
	out, err := encodeJSON(record)
	if err != nil {
		return raw
	}
	return out
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

// restoreTree lays a recorded tree out under root.
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

// restoreDatabase writes the frozen empty store at path and inserts the recorded rows. The
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
		return fmt.Errorf("the recorded database's schema (%s) is not the frozen store's (%s)", entry.Schema, schema)
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

var (
	encodePosition = regexp.MustCompile(`(positions? )([0-9]+)(-([0-9]+))?`)
	rootPosition   = regexp.MustCompile(`(positions? )R\+([0-9]+)(-R\+([0-9]+))?`)
)

// toRootPositions spells each character position a codec error names ("position 73",
// "positions 75-76") relative to the length of root, which prefixes the path it counts in: a
// temporary directory's name is not always as long, so the recorded answer names R+k instead.
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

// fromRootPositions is toRootPositions undone for this run's root.
func fromRootPositions(raw []byte, root string) []byte {
	offset := utf8.RuneCountInString(root)
	return rootPosition.ReplaceAllFunc(raw, func(match []byte) []byte {
		parts := rootPosition.FindSubmatch(match)
		out := string(parts[1]) + fmt.Sprint(atoi(parts[2])+offset)
		if len(parts[4]) > 0 {
			out += fmt.Sprintf("-%d", atoi(parts[4])+offset)
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
// test whose Python answers count positions in paths under it (a decoder's column and char): a
// temporary directory's name is not always as long, and the recording names the directory by a
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

// pythonOutcome records what capture's process did. A stream that is UTF-8 is recorded as its
// text, which a substitution reaches; any other is recorded as base64, byte for byte.
func pythonOutcome(t *testing.T, key string, capture func() outcomeBytes, opts ...pyoracle.Option) outcomeBytes {
	t.Helper()
	type recorded struct {
		Code         int     `json:"code"`
		Stdout       *string `json:"stdout,omitempty"`
		Stderr       *string `json:"stderr,omitempty"`
		StdoutBase64 string  `json:"stdoutBase64,omitempty"`
		StderrBase64 string  `json:"stderrBase64,omitempty"`
	}
	raw := pyoracle.Answer(t, key, func() ([]byte, error) {
		o := capture()
		r := recorded{Code: o.Code}
		for _, s := range []struct {
			value  string
			text   **string
			base64 *string
		}{{o.Stdout, &r.Stdout, &r.StdoutBase64}, {o.Stderr, &r.Stderr, &r.StderrBase64}} {
			if utf8.ValidString(s.value) {
				value := s.value
				*s.text = &value
			} else {
				*s.base64 = base64.StdEncoding.EncodeToString([]byte(s.value))
			}
		}
		return encodeJSON(r)
	}, opts...)
	var r recorded
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("recorded outcome %q: %v", raw, err)
	}
	o := outcomeBytes{Code: r.Code}
	for _, s := range []struct {
		text   *string
		base64 string
		out    *string
	}{{r.Stdout, r.StdoutBase64, &o.Stdout}, {r.Stderr, r.StderrBase64, &o.Stderr}} {
		if s.text != nil {
			*s.out = *s.text
		} else if decoded, err := base64.StdEncoding.DecodeString(s.base64); err == nil {
			*s.out = string(decoded)
		} else {
			t.Fatalf("recorded outcome %q: %v", raw, err)
		}
	}
	return o
}

// canonicalJSON is a document as json.loads reads it, dumped with sorted keys and ASCII escapes,
// for comparing two runtimes' documents whatever their key order and spacing.
func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	value, err := Decode(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return evidence.Dumps(value, false, true, true)
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

// canonicalFixtureRoot is where a test lays out a fixture whose Python answers carry digests of
// documents that name its paths (a receipt's manifest revision, an event id): the path is the
// same on every run, so a recorded answer stays true of the fixture laid out again. One test at
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
