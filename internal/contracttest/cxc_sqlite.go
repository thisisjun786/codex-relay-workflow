package contracttest

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
)

// SeedSQLite creates the given databases as the recorder's Node seeder does: open each file, run its
// statements in order, close. The file is chmod-ed 0644, the mode umask 022 gives, so the test
// process's umask does not decide the mode a tree entry records.
func (r *cxcReplayer) SeedSQLite(_ *cxccorpus.Case, seeds map[string][]string) error {
	for path, statements := range seeds {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		for _, statement := range statements {
			if _, execErr := db.Exec(statement); execErr != nil {
				err = fmt.Errorf("seed %s: %q: %w", path, statement, execErr)
				break
			}
		}
		if err = errors.Join(err, db.Close()); err == nil {
			err = os.Chmod(path, 0o644)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// DumpSQLite prints a database as the recorder's Node dumper does: {"tables": [{"name", "sql",
// "rows"}]} in JSON.stringify's spelling, tables ordered by type then name, rows in rowid order for
// a table that is not virtual, null otherwise. It opens the file read-only, which leaves the -wal and
// -shm files of a WAL database where the tree walk listed them. Not reproduced, and absent from the
// corpus: Node lists numeric-looking column names first, in numeric order.
func (r *cxcReplayer) DumpSQLite(path string) (string, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		return "", err
	}
	defer db.Close()
	type object struct{ name, ddl string }
	var objects []object
	listing, err := db.Query("SELECT name, sql FROM sqlite_master WHERE type IN ('table','index','view','trigger') AND sql IS NOT NULL ORDER BY type, name")
	if err != nil {
		return "", err
	}
	for listing.Next() {
		var o object
		if err = listing.Scan(&o.name, &o.ddl); err != nil {
			break
		}
		objects = append(objects, o)
	}
	if err = errors.Join(err, listing.Err(), listing.Close()); err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString(`{"tables":[`)
	for i, o := range objects {
		if i > 0 {
			out.WriteByte(',')
		}
		rows := "null"
		if upper := strings.ToUpper(o.ddl); strings.HasPrefix(upper, "CREATE TABLE") && !strings.Contains(upper, "VIRTUAL TABLE") {
			if text, err := tableRows(db, o.name); err == nil {
				rows = text
			}
		}
		fmt.Fprintf(&out, `{"name":%s,"sql":%s,"rows":%s}`, jsString(o.name), jsString(o.ddl), rows)
	}
	out.WriteString("]}")
	return out.String(), nil
}

// tableRows is a table's rows as a JSON array of objects in column order. Each column is read as
// "+col": unary plus returns the stored value, and an expression column has no declared type, so the
// driver does not turn DATETIME text into a time.Time.
func tableRows(db *sql.DB, name string) (string, error) {
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	head, err := db.Query("SELECT * FROM " + quote(name) + " LIMIT 0")
	if err != nil {
		return "", err
	}
	cols, err := head.Columns()
	if err = errors.Join(err, head.Close()); err != nil {
		return "", err
	}
	exprs := make([]string, len(cols))
	for i, col := range cols {
		exprs[i] = "+" + quote(col) + " AS " + quote(col)
	}
	rows, err := db.Query("SELECT " + strings.Join(exprs, ", ") + " FROM " + quote(name))
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out strings.Builder
	out.WriteByte('[')
	for n := 0; rows.Next(); n++ {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		if n > 0 {
			out.WriteByte(',')
		}
		out.WriteByte('{')
		for i, col := range cols {
			if i > 0 {
				out.WriteByte(',')
			}
			text, err := jsValue(vals[i])
			if err != nil {
				return "", err
			}
			out.WriteString(jsString(col) + ":" + text)
		}
		out.WriteByte('}')
	}
	out.WriteByte(']')
	return out.String(), rows.Err()
}

// jsValue is a SQLite value as JSON.stringify prints what node:sqlite reads: an integer outside the
// safe range is an error there (the dumper then records null rows), a BLOB is a Uint8Array and so an
// object keyed by byte index, and a REAL that is not finite is null.
func jsValue(v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "null", nil
	case int64:
		if v > 1<<53-1 || v < -(1<<53-1) {
			return "", fmt.Errorf("integer %d is outside the JavaScript safe range", v)
		}
		return strconv.FormatInt(v, 10), nil
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return "null", nil
		}
		if v == 0 { // JSON.stringify prints -0 as 0
			return "0", nil
		}
		text, err := json.Marshal(v)
		return string(text), err
	case []byte:
		var b strings.Builder
		b.WriteByte('{')
		for i, c := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%d":%d`, i, c)
		}
		b.WriteByte('}')
		return b.String(), nil
	case string:
		return jsString(v), nil
	}
	return "", fmt.Errorf("unexpected SQLite value %T", v)
}

// jsString is s as JSON.stringify spells a string: only the quote, the backslash and the control
// characters are escaped (markup characters and U+2028 and U+2029 stay as they are).
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			fmt.Fprintf(&b, `\u%04x`, c)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
