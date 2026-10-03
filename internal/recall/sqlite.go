// CXC v0.2.40 recall/src/sqlite.ts, backed by the existing pure-Go SQLite engine.
// Direct engine calls preserve Node's raw values, errmsg and first-statement
// prepare, which database/sql's DATE/TIMESTAMP conversion would change.
package recall

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"modernc.org/libc"
	sqlite "modernc.org/sqlite/lib"
)

// RwDb owns a synchronous connection. The lock covers TLS and whole operations,
// including copying SQLite-owned values before reset and releasing them on Close.
type RwDb struct {
	mu         sync.Mutex
	tls        *libc.TLS
	handle     uintptr
	statements []*Stmt
}

// Stmt is a prepared first statement; Exec on its database runs whole scripts.
type Stmt struct {
	db     *RwDb
	handle uintptr
	bare   map[string]string
}

// NamedParam is one entry in a JavaScript binding object's enumeration order.
type NamedParam struct {
	Name  string
	Value any
}

// NamedParams preserves enumeration order; Go maps cannot carry alias-write order.
type NamedParams []NamedParam

// RunResult has JavaScript number semantics, including lastInsertRowid rounding.
type RunResult struct{ Changes, LastInsertRowid float64 }

func openDbReadOnly(path string) (*RwDb, error) { return openDb(path, sqlite.SQLITE_OPEN_READONLY) }
func openDbReadWrite(path string) (*RwDb, error) {
	return openDb(path, sqlite.SQLITE_OPEN_READWRITE|sqlite.SQLITE_OPEN_CREATE)
}

func openDb(path string, flags int32) (*RwDb, error) {
	if strings.ContainsRune(path, 0) {
		//lint:ignore ST1005 Exact node:sqlite diagnostic, pinned by the oracle.
		return nil, errors.New(`The "path" argument must be a string, Uint8Array, or URL without null bytes.`)
	}
	d := &RwDb{tls: libc.NewTLS()}
	name, err := libc.CString(source.DecodeUTF8([]byte(path)))
	if err != nil {
		d.tls.Close()
		return nil, err
	}
	cell := libc.Xcalloc(d.tls, 1, strconv.IntSize/8)
	if cell == 0 {
		libc.Xfree(d.tls, name)
		d.tls.Close()
		return nil, errors.New("out of memory")
	}
	opened := false
	defer func() {
		libc.Xfree(d.tls, cell)
		libc.Xfree(d.tls, name)
		if !opened {
			if d.handle != 0 {
				sqlite.Xsqlite3_close_v2(d.tls, d.handle)
			}
			d.tls.Close()
		}
	}()
	rc := sqlite.Xsqlite3_open_v2(d.tls, name, cell, flags|sqlite.SQLITE_OPEN_URI|sqlite.SQLITE_OPEN_FULLMUTEX, 0)
	d.handle = sqlitePointer(cell)
	if rc != sqlite.SQLITE_OK {
		return nil, d.sqliteError()
	}
	for _, setting := range [][2]int32{{sqlite.SQLITE_DBCONFIG_ENABLE_FKEY, 1}, {sqlite.SQLITE_DBCONFIG_DQS_DML, 0}, {sqlite.SQLITE_DBCONFIG_DQS_DDL, 0}} {
		args := libc.NewVaList(setting[1], uintptr(0))
		if args == 0 {
			return nil, errors.New("out of memory")
		}
		rc = sqlite.Xsqlite3_db_config(d.tls, d.handle, setting[0], args)
		libc.Xfree(d.tls, args)
		if rc != sqlite.SQLITE_OK {
			return nil, d.sqliteError()
		}
	}
	opened = true
	return d, nil
}

func sqlitePointer(cell uintptr) uintptr {
	b := libc.GoBytes(cell, strconv.IntSize/8)
	if strconv.IntSize == 32 {
		return uintptr(binary.NativeEndian.Uint32(b))
	}
	return uintptr(binary.NativeEndian.Uint64(b))
}

func (d *RwDb) sqliteError() error {
	return errors.New(libc.GoString(sqlite.Xsqlite3_errmsg(d.tls, d.handle)))
}

func (d *RwDb) Prepare(query string) (*Stmt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handle == 0 {
		return nil, errors.New("database is not open")
	}
	sql, err := libc.CString(source.DecodeUTF8([]byte(query)))
	if err != nil {
		return nil, err
	}
	defer libc.Xfree(d.tls, sql)
	cell := libc.Xcalloc(d.tls, 1, strconv.IntSize/8)
	if cell == 0 {
		return nil, errors.New("out of memory")
	}
	defer libc.Xfree(d.tls, cell)
	rc := sqlite.Xsqlite3_prepare_v2(d.tls, d.handle, sql, -1, cell, 0)
	if rc != sqlite.SQLITE_OK {
		return nil, d.sqliteError()
	}
	s := &Stmt{db: d, handle: sqlitePointer(cell)}
	d.statements = append(d.statements, s)
	return s, nil
}

func (d *RwDb) Exec(query string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handle == 0 {
		return errors.New("database is not open")
	}
	sql, err := libc.CString(source.DecodeUTF8([]byte(query)))
	if err != nil {
		return err
	}
	defer libc.Xfree(d.tls, sql)
	if sqlite.Xsqlite3_exec(d.tls, d.handle, sql, 0, 0, 0) != sqlite.SQLITE_OK {
		return d.sqliteError()
	}
	return nil
}

func (d *RwDb) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handle == 0 {
		return errors.New("database is not open")
	}
	for _, s := range d.statements {
		sqlite.Xsqlite3_finalize(d.tls, s.handle)
		s.handle = 0
	}
	d.statements = nil
	rc := sqlite.Xsqlite3_close_v2(d.tls, d.handle)
	var err error
	if rc != sqlite.SQLITE_OK {
		err = d.sqliteError()
	}
	d.handle = 0
	d.tls.Close()
	return err
}

func (s *Stmt) reset() {
	sqlite.Xsqlite3_reset(s.db.tls, s.handle)
	sqlite.Xsqlite3_clear_bindings(s.db.tls, s.handle)
}

func (s *Stmt) All(params ...any) ([]map[string]any, error) { return s.query(false, params) }
func (s *Stmt) Get(params ...any) (map[string]any, error) {
	rows, err := s.query(true, params)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func (s *Stmt) query(first bool, params []any) ([]map[string]any, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if s.handle == 0 {
		return nil, errors.New("statement has been finalized")
	}
	defer s.reset()
	if err := s.bind(params); err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for {
		rc := sqlite.Xsqlite3_step(s.db.tls, s.handle)
		if rc == sqlite.SQLITE_DONE {
			return rows, nil
		}
		if rc != sqlite.SQLITE_ROW {
			return nil, s.db.sqliteError()
		}
		row, err := s.row()
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
		if first {
			return rows, nil
		}
	}
}

func (s *Stmt) Run(params ...any) (RunResult, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if s.handle == 0 {
		return RunResult{}, errors.New("statement has been finalized")
	}
	defer s.reset()
	if err := s.bind(params); err != nil {
		return RunResult{}, err
	}
	rc := sqlite.Xsqlite3_step(s.db.tls, s.handle)
	if rc != sqlite.SQLITE_DONE && rc != sqlite.SQLITE_ROW {
		return RunResult{}, s.db.sqliteError()
	}
	sqlite.Xsqlite3_reset(s.db.tls, s.handle)
	return RunResult{float64(sqlite.Xsqlite3_changes64(s.db.tls, s.db.handle)), float64(sqlite.Xsqlite3_last_insert_rowid(s.db.tls, s.db.handle))}, nil
}

func (s *Stmt) row() (map[string]any, error) {
	row := make(map[string]any)
	for i := int32(0); i < sqlite.Xsqlite3_column_count(s.db.tls, s.handle); i++ {
		var value any
		switch sqlite.Xsqlite3_column_type(s.db.tls, s.handle, i) {
		case sqlite.SQLITE_INTEGER:
			n := sqlite.Xsqlite3_column_int64(s.db.tls, s.handle, i)
			// Node's absolute-value check overflows for int64's minimum; parity keeps it.
			if n != -1<<63 && (n > 9007199254740991 || n < -9007199254740991) {
				//lint:ignore ST1005 Exact node:sqlite diagnostic, pinned by the oracle.
				return nil, fmt.Errorf("Value is too large to be represented as a JavaScript number: %d", n)
			}
			value = float64(n)
		case sqlite.SQLITE_FLOAT:
			value = sqlite.Xsqlite3_column_double(s.db.tls, s.handle, i)
		case sqlite.SQLITE_TEXT:
			p := sqlite.Xsqlite3_column_text(s.db.tls, s.handle, i)
			value = source.DecodeUTF8(libc.GoBytes(p, int(sqlite.Xsqlite3_column_bytes(s.db.tls, s.handle, i))))
		case sqlite.SQLITE_BLOB:
			p := sqlite.Xsqlite3_column_blob(s.db.tls, s.handle, i)
			value = append([]byte{}, libc.GoBytes(p, int(sqlite.Xsqlite3_column_bytes(s.db.tls, s.handle, i)))...)
		}
		name := source.DecodeUTF8([]byte(libc.GoString(sqlite.Xsqlite3_column_name(s.db.tls, s.handle, i))))
		row[name] = value
	}
	return row, nil
}

func (s *Stmt) parameterName(i int32) string {
	return libc.GoString(sqlite.Xsqlite3_bind_parameter_name(s.db.tls, s.handle, i))
}

func (s *Stmt) bind(params []any) error {
	sqlite.Xsqlite3_clear_bindings(s.db.tls, s.handle)
	count := sqlite.Xsqlite3_bind_parameter_count(s.db.tls, s.handle)
	if len(params) > 0 {
		var named NamedParams
		_, ordered := params[0].(NamedParams)
		if ordered {
			named = params[0].(NamedParams)
		}
		if values, ok := params[0].(map[string]any); ok {
			keys := make([]string, 0, len(values))
			for key := range values {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				named = append(named, NamedParam{key, values[key]})
			}
			ordered = true
		}
		if ordered {
			// The oracle retains this partial cache if alias construction throws.
			if s.bare == nil {
				s.bare = map[string]string{}
				for i := int32(1); i <= count; i++ {
					name := s.parameterName(i)
					if name == "" {
						continue
					}
					bare := name[1:]
					if prior := s.bare[bare]; prior != "" && prior != name {
						//lint:ignore ST1005 Exact node:sqlite diagnostic, pinned by the oracle.
						return fmt.Errorf("Cannot create bare named parameter '%s' because of conflicting names '%s' and '%s'.", bare, prior, name)
					}
					s.bare[bare] = name
				}
			}
			seen := map[int32]bool{}
			_, unordered := params[0].(map[string]any)
			for _, arg := range named {
				key := arg.Name
				alias := s.bare[key]
				index := int32(0)
				for i := int32(1); i <= count; i++ {
					name := s.parameterName(i)
					if name == key {
						index = i
						break
					}
					if alias != "" && name == alias {
						index = i
					}
				}
				if index == 0 {
					//lint:ignore ST1005 Exact node:sqlite diagnostic, pinned by the oracle.
					return fmt.Errorf("Unknown named parameter '%s'", key)
				}
				if unordered && seen[index] {
					return errors.New("use NamedParams to preserve the order of aliases for one SQLite parameter")
				}
				seen[index] = true
				if err := s.bindValue(index, arg.Value); err != nil {
					return err
				}
			}
			params = params[1:]
		}
	}
	index := int32(1)
	for _, value := range params {
		for index <= count {
			name := s.parameterName(index)
			if name == "" || name[0] == '?' {
				break
			}
			index++
		}
		if err := s.bindValue(index, value); err != nil {
			return err
		}
		index++
	}
	return nil
}

func (s *Stmt) bindValue(index int32, value any) error {
	tls := s.db.tls
	var rc int32
	switch v := value.(type) {
	case nil:
		rc = sqlite.Xsqlite3_bind_null(tls, s.handle, index)
	case string, []byte:
		var data string
		if str, ok := v.(string); ok {
			data = source.DecodeUTF8([]byte(str))
		} else {
			data = string(v.([]byte))
		}
		p, err := libc.CString(data)
		if err != nil {
			return err
		}
		if _, ok := v.(string); ok {
			rc = sqlite.Xsqlite3_bind_text64(tls, s.handle, index, p, uint64(len(data)), sqlite.SQLITE_TRANSIENT, sqlite.SQLITE_UTF8)
		} else {
			rc = sqlite.Xsqlite3_bind_blob64(tls, s.handle, index, p, uint64(len(data)), sqlite.SQLITE_TRANSIENT)
		}
		libc.Xfree(tls, p)
	case *big.Int:
		if v == nil || !v.IsInt64() {
			//lint:ignore ST1005 Exact node:sqlite diagnostic, pinned by the oracle.
			return errors.New("BigInt value is too large to bind.")
		}
		rc = sqlite.Xsqlite3_bind_int64(tls, s.handle, index, v.Int64())
	default:
		number := reflect.ValueOf(value)
		var n float64
		switch number.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n = float64(number.Int())
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			n = float64(number.Uint())
		case reflect.Float32, reflect.Float64:
			n = number.Float()
		default:
			//lint:ignore ST1005 Exact node:sqlite diagnostic, pinned by the oracle.
			return fmt.Errorf("Provided value cannot be bound to SQLite parameter %d.", index)
		}
		rc = sqlite.Xsqlite3_bind_double(tls, s.handle, index, n)
	}
	if rc != sqlite.SQLITE_OK {
		return s.db.sqliteError()
	}
	return nil
}
