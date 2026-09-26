package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Services is cli.Services: the selection every command resolves before its handler runs, and
// the global flags. Nothing here opens a Store; a handler that needs one opens it itself.
type Services struct {
	Selection        store.StateSelection
	SocketPath       string
	AdapterRequested bool
	// Program is how the operator invoked this CLI, for printed recovery commands.
	Program string
}

// selectionRecord is StateSelection.to_record.
func selectionRecord(selection store.StateSelection) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "path", Value: selection.Path},
		{Key: "dbPath", Value: selection.DBPath()},
		{Key: "source", Value: selection.Source},
		{Key: "detail", Value: selection.Detail},
		{Key: "socketScope", Value: nullableText(selection.SocketScope)},
		{Key: "precedence", Value: []any{"flag", "env", "xdg", "home"}},
		{Key: "ambiguous", Value: stringList(selection.Ambiguous)},
		{Key: "unidentified", Value: stringList(selection.Unidentified)},
	}
}

func stringList(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// locationRecord is the `store` block probe() and locate() report.
func locationRecord(loc store.Location, exists bool) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "exists", Value: exists},
		{Key: "storeId", Value: nullableText(loc.StoreID)},
		{Key: "createdAt", Value: nullableText(loc.CreatedAt)},
		{Key: "dbPath", Value: loc.DBPath},
		{Key: "realPath", Value: nullableText(loc.RealPath)},
		{Key: "device", Value: nullableCount(loc.Device)},
		{Key: "inode", Value: nullableCount(loc.Inode)},
		{Key: "links", Value: nullableCount(loc.Links)},
		{Key: "logDevice", Value: nullableCount(loc.LogDevice)},
		{Key: "logInode", Value: nullableCount(loc.LogInode)},
		{Key: "logName", Value: nullableText(loc.LogName)},
		{Key: "schemaVersion", Value: nullableText(loc.SchemaVersion)},
	}
}

func accessRecord(access store.ProbeAccess) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "directoryExists", Value: access.DirectoryExists},
		{Key: "directoryReadable", Value: access.DirectoryReadable},
		{Key: "directoryWritable", Value: access.DirectoryWritable},
		{Key: "dbExists", Value: access.DBExists},
		{Key: "dbReadable", Value: access.DBReadable},
		{Key: "dbWritable", Value: access.DBWritable},
		{Key: "detail", Value: nullableText(access.Detail)},
	}
}

// probeStore is store.probe's `store` block: absent-DB probes keep dbPath with Python's
// str(Path) spelling and every measured field null.
func probeStore(result store.ProbeResult) store.Location {
	loc := result.Store
	loc.DBPath = result.Selection.DBPath()
	return loc
}

// ledgerLocation is _ledger_location: where the transport ledger will actually live.
func ledgerLocation(services Services) (contract.OrderedObject, error) {
	if services.SocketPath == "" {
		return contract.OrderedObject{
			{Key: "configured", Value: false}, {Key: "directory", Value: nil},
			{Key: "path", Value: nil}, {Key: "split", Value: false},
		}, nil
	}
	discovered, err := store.ResolveStateDir("", services.SocketPath)
	if err != nil {
		return nil, err
	}
	canonical, err := store.CanonicalSocket(services.SocketPath)
	if err != nil {
		return nil, err
	}
	endpoint := socketDigest(canonical)
	here, err := store.ResolvePath(discovered.Path)
	if err != nil {
		return nil, err
	}
	selected, err := store.ResolvePath(services.Selection.Path)
	if err != nil {
		return nil, err
	}
	return contract.OrderedObject{
		{Key: "configured", Value: true},
		{Key: "directory", Value: discovered.Path},
		{Key: "path", Value: filepath.Join(discovered.Path, "operations-"+endpoint+".sqlite3")},
		{Key: "split", Value: here != selected},
	}, nil
}

// siblingStores is _sibling_stores.
func siblingStores(services Services) (contract.OrderedObject, error) {
	if services.Selection.Source == "flag" || services.Selection.Source == "env" {
		return contract.OrderedObject{
			{Key: "checked", Value: false},
			{Key: "reason", Value: "the state directory was chosen explicitly"},
			{Key: "withoutProvenance", Value: []any{}},
		}, nil
	}
	root := filepath.Dir(services.Selection.Path)
	skip := filepath.Base(services.Selection.Path)
	claiming, err := store.StoresClaimingSocket(root, services.SocketPath, skip)
	if err != nil {
		return nil, err
	}
	return contract.OrderedObject{
		{Key: "checked", Value: true},
		{Key: "reason", Value: nil},
		{Key: "withoutProvenance", Value: stringList(store.StoresWithoutProvenance(root, skip))},
		{Key: "claimingThisSocket", Value: stringList(claiming)},
		{Key: "ambiguous", Value: len(claiming) > 1},
	}, nil
}

// contents is _contents: counts read through the held descriptor, never through a Store.
func contents(ctx context.Context, services Services, access store.ProbeAccess) contract.OrderedObject {
	unavailable := func(detail string) contract.OrderedObject {
		return contract.OrderedObject{
			{Key: "available", Value: false}, {Key: "relationships", Value: nil},
			{Key: "openAttempts", Value: nil}, {Key: "detail", Value: detail},
		}
	}
	if !access.DBReadable {
		return unavailable("the database is not readable from this process")
	}
	var relationships, open int64
	rows := 0
	read := store.ReadOnlyRows(ctx, services.Selection,
		"SELECT (SELECT COUNT(*) FROM relationships) AS relationships,"+
			"       (SELECT COUNT(*) FROM attempts a"+
			"          JOIN deliveries d ON d.event_id = a.event_id"+
			"         WHERE a.internal_state = 'in_flight'"+
			"            OR (a.state = 'held_uncertain'"+
			"                AND d.state IN ('held_uncertain','sending'))) AS open_attempts",
		nil, func(r store.RowScanner) error { rows++; return r.Scan(&relationships, &open) })
	if !read.Readable || read.Detail != "" || rows == 0 {
		if read.Detail != "" {
			return unavailable(read.Detail)
		}
		return unavailable("the store could not be read")
	}
	return contract.OrderedObject{
		{Key: "available", Value: true}, {Key: "relationships", Value: relationships},
		{Key: "openAttempts", Value: open}, {Key: "detail", Value: nil},
	}
}

// program is _program: argv[0] as typed when it carries a directory, else its base name.
func program(argv0 string) string {
	if strings.Contains(argv0, "/") {
		return shellQuote(argv0)
	}
	return shellQuote(filepath.Base(argv0))
}

// shellQuote is shlex.quote.
func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	safe := true
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)) {
			safe = false
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
