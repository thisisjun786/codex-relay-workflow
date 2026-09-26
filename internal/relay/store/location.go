package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"modernc.org/sqlite"
)

func boundedDB(path, mode string, timeout time.Duration) (*sql.DB, error) {
	d := &sqlite.Driver{}
	d.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, _ string) error {
		_, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", timeout.Milliseconds()), []driver.NamedValue{})
		return err
	})
	id, err := randomBytes(8)
	if err != nil {
		return nil, err
	}
	name := "crw-read-" + hex.EncodeToString(id)
	sql.Register(name, d)
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", mode)
	u.RawQuery = q.Encode()
	db, err := sql.Open(name, u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
func storeSocket(path string) string {
	db, err := boundedDB(path, "ro", 5*time.Second)
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
