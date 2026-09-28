package store

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

func admitWrite(ctx context.Context, path string) (*ownership.Admission, error) {
	admission, err := ownership.Admit(ctx, path)
	if err != nil {
		return nil, &RefusedError{Reason: "store_owned_by_other", Detail: err.Error(), cause: err}
	}
	return admission, nil
}
func openFenced(ctx context.Context, path, socket string, options OpenOptions) (s *Store, err error) {
	resolved, err := refuseLiveState(path)
	if err != nil {
		return nil, err
	}
	admission, err := admitWrite(ctx, resolved)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, admission.Close())
		}
	}()
	r, err := ownership.ReadRecord(resolved)
	if err != nil {
		return nil, err
	}
	if socket != "" {
		canonical, e := canonicalSocket(socket)
		if e != nil {
			return nil, e
		}
		if r.AppServerSocket == nil || *r.AppServerSocket != canonical {
			return nil, &ownership.Refused{Detail: "requested socket disagrees with ownership record"}
		}
	}
	// The exact schema is checked before the legacy initializer can run any DDL.
	snapshot, cleanup, err := ownership.CopySnapshot(resolved)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	db, err := ownership.OpenExisting(ctx, snapshot, "ro")
	if err != nil {
		return nil, err
	}
	err = ValidateOwnershipSchema(ctx, db)
	err = errors.Join(err, db.Close())
	if err != nil {
		return nil, err
	}
	options.admission = admission
	s, err = open(ctx, path, socket, options)
	if err != nil {
		return nil, err
	}
	s.admission = admission
	return s, nil
}

// ValidateOwnershipSchema requires every table and column of the frozen v1 DDL.
// It does not repair a store or accept version=1 as proof of compatibility.
func ValidateOwnershipSchema(ctx context.Context, db ownership.Queryer) error {
	raw, err := schema.ReadFile("relay-sqlite.sql")
	if err != nil {
		return err
	}
	ddl := strings.SplitN(string(raw), guardMarker, 2)[0]
	var lines []string
	for _, line := range strings.Split(ddl, "\n") {
		code, _, _ := strings.Cut(line, "--")
		lines = append(lines, code)
	}
	ddl = strings.Join(lines, "\n")
	columnPattern := regexp.MustCompile(`(?:^|,)\s*([A-Za-z_][A-Za-z_0-9]*)\s+(?:TEXT|INTEGER|REAL|BLOB)\b`)
	for _, statement := range strings.Split(ddl, ";") {
		start := strings.Index(statement, "CREATE TABLE IF NOT EXISTS ")
		if start < 0 {
			continue
		}
		definition := strings.TrimSpace(statement[start+len("CREATE TABLE IF NOT EXISTS "):])
		name, body, ok := strings.Cut(definition, "(")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		rows, e := db.QueryContext(ctx, `PRAGMA table_info("`+name+`")`)
		if e != nil {
			return e
		}
		columns := map[string]bool{}
		for rows.Next() {
			var cid, notnull, pk int
			var column, typ string
			var defaultValue any
			if e = rows.Scan(&cid, &column, &typ, &notnull, &defaultValue, &pk); e != nil {
				break
			}
			columns[column] = true
		}
		e = errors.Join(e, rows.Err(), rows.Close())
		if e != nil {
			return e
		}
		if len(columns) == 0 {
			return &ownership.Refused{Detail: "required table missing: " + name}
		}
		for _, match := range columnPattern.FindAllStringSubmatch(body, -1) {
			column := match[1]
			if !columns[column] {
				return &ownership.Refused{Detail: "required column missing: " + name + "." + column}
			}
		}
	}
	return nil
}
