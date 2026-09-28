package ownership

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

func serviceLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err == nil && (stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0) {
		err = refuse("unsafe service lock")
	}
	if err == nil {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

type InventoryFile struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type Inventory struct {
	BackupMechanism string           `json:"backupMechanism"`
	Database        Database         `json:"database"`
	Stamp           Stamp            `json:"stamp"`
	Tables          map[string]int64 `json:"tables"`
	Files           []InventoryFile  `json:"files"`
}

func (c *Controller) inspect(ctx context.Context, db *sql.DB, r Record) error {
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return refuse("integrity check failed: %s", integrity)
	}
	// Validate the entire frozen table/column set, not version alone. Ownership
	// does not run DDL or repair a partially migrated database.
	if c.ValidateSchema == nil {
		return refuse("schema validator unavailable")
	}
	if err := c.ValidateSchema(ctx, db); err != nil {
		return err
	}
	s, err := ReadStamp(ctx, db)
	if err != nil {
		return err
	}
	inventory := Inventory{"sqlite3_backup via modernc NewBackup/Step/Finish", r.Database, s, map[string]int64{}, []InventoryFile{}}
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			break
		}
		tables = append(tables, name)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, name := range tables {
		var count int64
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&count); err != nil {
			return err
		}
		inventory.Tables[name] = count
	}
	// Inventory all state-local receiver/transport ledgers, sidecar locks and
	// immutable inbox entries; never traverse links or copy an operational ledger.
	root := filepath.Dir(c.Path)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if entry.IsDir() {
			if path == filepath.Join(root, "takeover-backups") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if strings.HasPrefix(rel, "takeover-inbox/.") {
			return nil
		}
		if !strings.HasPrefix(rel, "takeover-inbox/") && !strings.Contains(rel, "ledger") && !strings.Contains(rel, "receiver") && !strings.HasPrefix(filepath.Base(rel), "operations-") {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return refuse("inventory contains symlink: %s", rel)
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return refuse("inventory is not a regular file: %s", rel)
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		hash := sha256.New()
		n, e := io.Copy(hash, f)
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
		inventory.Files = append(inventory.Files, InventoryFile{rel, n, hex.EncodeToString(hash.Sum(nil))})
		return nil
	})
	if err != nil {
		return err
	}
	parent := filepath.Join(root, "takeover-backups")
	if err = os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	if err = syncDir(root); err != nil {
		return err
	}
	dir := filepath.Join(parent, r.Transition.ID)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err = syncDir(parent); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".snapshot-")
	if err != nil {
		return err
	}
	name := temp.Name()
	if err = temp.Close(); err != nil {
		return err
	}
	defer os.Remove(name)
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	err = conn.Raw(func(raw any) error {
		source, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite driver has no backup API")
		}
		backup, e := source.NewBackup(name)
		if e != nil {
			return e
		}
		for {
			if e = ctx.Err(); e != nil {
				break
			}
			var more bool
			more, e = backup.Step(128)
			if e != nil || !more {
				break
			}
		}
		return errors.Join(e, backup.Finish())
	})
	err = errors.Join(err, conn.Close())
	if err != nil {
		return err
	}
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(dir, "relay.sqlite3")); err != nil {
		return err
	}
	if err = syncDir(dir); err != nil {
		return err
	}
	if err = c.fault("snapshot", "backup-synced"); err != nil {
		return err
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	return publishFile(filepath.Join(dir, "inventory.json"), raw, func(point string) error { return c.fault("snapshot", point) })
}
