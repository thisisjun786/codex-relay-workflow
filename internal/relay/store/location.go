package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"modernc.org/sqlite"
)

// boundedDB opens path in mode with a bounded busy timeout, then runs pragmas on every connection.
func boundedDB(path, mode string, timeout time.Duration, pragmas ...string) (*sql.DB, error) {
	return boundedURI(path, url.Values{"mode": {mode}}, timeout, pragmas...)
}

// boundedURI is boundedDB with every SQLite URI parameter given (mode, immutable).
func boundedURI(path string, params url.Values, timeout time.Duration, pragmas ...string) (*sql.DB, error) {
	d := &sqlite.Driver{}
	d.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		for _, pragma := range append([]string{fmt.Sprintf("PRAGMA busy_timeout=%d", timeout.Milliseconds())}, pragmas...) {
			if _, err := conn.ExecContext(context.Background(), pragma, []driver.NamedValue{}); err != nil {
				return err
			}
		}
		return nil
	})
	u := url.URL{Scheme: "file", Path: path}
	u.RawQuery = params.Encode()
	db := sql.OpenDB(dsnConnector{textGuard{d}, u.String()})
	db.SetMaxOpenConns(1)
	return db, nil
}
func storeSocket(path string) string {
	snapshot, cleanup, err := ownership.CopySnapshot(path)
	if err != nil {
		return ""
	}
	defer cleanup()
	db, err := boundedDB(snapshot, "ro", 5*time.Second)
	if err != nil {
		return ""
	}
	defer db.Close()
	var value string
	if db.QueryRowContext(context.Background(), "SELECT value FROM schema_meta WHERE key='socket_path'").Scan(&value) != nil {
		return ""
	}
	return value
}
func fillLocation(result *Location, real, opened string) {
	if real != "" {
		info, err := os.Stat(real)
		if err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				result.Device = uint64(st.Dev)
				result.Inode = st.Ino
				result.Links = uint64(st.Nlink)
			}
		}
	}
	directory := filepath.Dir(opened)
	result.LogName = filepath.Base(opened)
	info, err := os.Stat(directory)
	if err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			result.LogDevice = uint64(st.Dev)
			result.LogInode = st.Ino
		}
	}
}
