// Package swapgate is scripts/crw_runtime/swapgate.py: whether it is safe to replace a runtime,
// read rather than assumed. Three cells answer it - a daemon that is not running (OPS-4.4), no
// attempt still open, and a store schema the candidate declares identically (OPS-4.5) - and
// each fills only its own cell. ALLOWED needs every cell established and none blocking;
// BLOCKED means a cell answered no; UNESTABLISHED means a cell could not answer. Both of the
// last two keep the existing installation. Nothing here starts or stops anything.
//
// The daemon and in-flight readings come from the SELECTED relay executable as a subprocess
// (scope.Relay). Store presence and the store's schema are read here without opening a store:
// an lstat of the path the relay's selection rule resolves, and a read-only sqlite_master read
// that creates no sidecar (readCatalog, docs/port/decisions.md 36), never store.Open. The
// candidate's schema comes from executing the candidate binary (`crw doctor declared-schema
// --json`), which applies its own DDL to an in-memory database.
package swapgate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// Object is a decoded JSON object.
type Object = record.Object

// The three verdicts.
const (
	Allowed       = "ALLOWED"
	Blocked       = "BLOCKED"
	Unestablished = "UNESTABLISHED"
)

// What comparing the store's schema with the candidate's can say.
const (
	Agrees  = "AGREES"
	Extends = "EXTENDS"
	Narrows = "NARROWS"
	Differs = "DIFFERS"
	NoStore = "NO_STORE"
	// ExtendsZone is an EXTENDS whose every added object belongs to the additive DAG zone and where
	// nothing else differs (D-01): it still refuses, unless the swap is run on the route that takes
	// the OPS-4.5 backup of the state directory first (DecideWithRelease). NarrowsZone is the
	// opposite, a store that holds only zone objects the candidate does not declare: an older
	// runtime ignores the zone's tables, so returning to it loses nothing and is not refused.
	ExtendsZone = "EXTENDS_ZONE"
	NarrowsZone = "NARROWS_ZONE"
)

// SchemaObjectsQuery is the one question both schema readings ask the catalog: every object
// except SQLite's own, keyed by kind and name.
const SchemaObjectsQuery = "SELECT type || ' ' || name AS object, sql FROM sqlite_master" +
	" WHERE lower(substr(name, 1, 7)) <> 'sqlite_' ORDER BY type, name"

// NoAttempts is the in-flight cell's established-absent answer.
const NoAttempts = int64(0)

// Cells are the gate's cells, in swapgate.GATE_CELLS order.
var Cells = []string{"daemon", "inFlight", "storeSchema"}

// Cell is one gate cell: answer, whether the question was answered at all, and why.
func Cell(answer any, readable bool, detail string, command, evidenceValue any) Object {
	return Object{
		{Key: "answer", Value: answer},
		{Key: "readable", Value: readable},
		{Key: "detail", Value: detail},
		{Key: "command", Value: command},
		{Key: "evidence", Value: evidenceValue},
	}
}

// Blocking is swapgate.blocking: whether this cell's established answer refuses the swap, nil
// when it did not answer. The predicate is the cell's declared one.
func Blocking(name string, cell Object) *bool {
	if !pyvalue.Truthy(record.Get(cell, "readable")) {
		return nil
	}
	answer := record.Get(cell, "answer")
	var refuses bool
	switch name {
	case "daemon":
		refuses = answer == scope.Running
	case "inFlight":
		refuses = !isZero(answer)
	case "storeSchema":
		refuses = answer == Narrows || answer == Extends || answer == Differs || answer == ExtendsZone
	}
	return &refuses
}

func isZero(v any) bool {
	switch n := v.(type) {
	case int64:
		return n == 0
	case int:
		return n == 0
	case bool:
		return !n
	case float64:
		return n == 0
	}
	return false
}

// DaemonCell is swapgate.daemon_cell: decided by the relay's own service reading.
func DaemonCell(envelope Object) Object {
	state := scope.ServiceState(envelope)
	answer := record.Get(state, "state")
	readable := answer == scope.Running || answer == scope.Stopped
	return Cell(answer, readable, record.Text(state, "detail"), record.Get(envelope, "command"), record.Get(state, "running"))
}

// Presence is whether a store exists at the resolved selection: readable, present, dbPath and
// detail, the keys runtime_install.store_presence answers with.
type Presence = Object

// InflightCell is swapgate.inflight_cell: presence first (settled by looking at the path), then
// the relay doctor's contents.openAttempts. A nil presence is a caller that did not look.
func InflightCell(envelope, presence Object) Object {
	command := record.Get(envelope, "command")
	if presence != nil {
		if !pyvalue.Truthy(record.Get(presence, "readable")) {
			return Cell(reading.AccessError, false, "whether a store exists at the resolved selection could not be established: "+reading.Text(record.Get(presence, "detail")), record.Get(presence, "command"), nil)
		}
		if record.Get(presence, "present") == false {
			payload, _ := record.Get(envelope, "payload").(Object)
			contents, _ := record.Get(payload, "contents").(Object)
			if pyvalue.Truthy(record.Get(contents, "available")) {
				return Cell(reading.Unreadable, false, "no store exists at "+reading.Text(record.Get(presence, "dbPath"))+" and the relay reports readable contents for it", command, nil)
			}
			return Cell(NoAttempts, true, "no store exists at "+reading.Text(record.Get(presence, "dbPath"))+", so no attempt can be open. That is established absence rather than a count nobody could read", record.Get(presence, "command"), NoAttempts)
		}
	}
	if envelope == nil || !pyvalue.Truthy(record.Get(envelope, "ok")) {
		detail := any(nil)
		for _, key := range []string{"unreadable", "stderr"} {
			if v := record.Get(envelope, key); pyvalue.Truthy(v) {
				detail = v
				break
			}
		}
		if detail == nil {
			detail = "the command failed"
		}
		return Cell(reading.AccessError, false, "the relay could not be asked for its contents: "+reading.Text(detail), command, nil)
	}
	payload, ok := record.Get(envelope, "payload").(Object)
	if !ok {
		return Cell(reading.Unreadable, false, "the relay answered with no readable payload", command, nil)
	}
	contents, ok := record.Get(payload, "contents").(Object)
	if !ok || !pyvalue.Truthy(record.Get(contents, "available")) {
		detail := record.Get(contents, "detail")
		if !pyvalue.Truthy(detail) {
			detail = "no contents were reported"
		}
		return Cell(reading.Unreadable, false, "the store's contents could not be read: "+reading.Text(detail), command, nil)
	}
	open, ok := record.Get(contents, "openAttempts").(int64)
	if !ok {
		return Cell(reading.Unreadable, false, "the contents carry no integer openAttempts, found "+reading.JSONKind(record.Get(contents, "openAttempts")), command, nil)
	}
	detail := "no attempt is open"
	if open != 0 {
		detail = reading.Text(open) + " attempts are still open, so a handover is in flight and the runtime under it is not replaced"
	}
	return Cell(open, true, detail, command, open)
}

// schemaOf is swapgate._schema: object key -> CREATE statement, or nil when a reading carries
// no statements.
func schemaOf(objects any) map[string]any {
	o, ok := objects.(Object)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(o))
	for _, f := range o {
		out[f.Key] = f.Value
	}
	return out
}

// SchemaCell is swapgate.schema_cell: compare every catalog object's CREATE statement, with
// whitespace collapsed outside quoted text only.
func SchemaCell(storeAnswer, candidate Object) Object {
	if storeAnswer == nil || candidate == nil {
		return Cell(reading.Unreadable, false, "a schema reading did not return an answer", nil, nil)
	}
	if !pyvalue.Truthy(record.Get(candidate, "readable")) {
		return Cell(reading.Unreadable, false, "the candidate's declared schema could not be read: "+reading.Text(record.Get(candidate, "detail")), record.Get(candidate, "command"), nil)
	}
	if !pyvalue.Truthy(record.Get(storeAnswer, "readable")) {
		return Cell(reading.Unreadable, false, "the store's schema could not be read: "+reading.Text(record.Get(storeAnswer, "detail")), record.Get(storeAnswer, "command"), nil)
	}
	declared := schemaOf(record.Get(candidate, "objects"))
	if declared == nil {
		return Cell(reading.Unreadable, false, "the candidate reported object names without their definitions, so the schemas could not be compared on anything but names", record.Get(candidate, "command"), nil)
	}
	command := record.Get(storeAnswer, "command")
	if record.Get(storeAnswer, "present") == false {
		return Cell(NoStore, true, "no store exists at the resolved selection, so there is nothing whose schema could disagree. That is absence and not agreement", command, Object{{Key: "dbPath", Value: record.Get(storeAnswer, "dbPath")}})
	}
	held := schemaOf(record.Get(storeAnswer, "objects"))
	if held == nil {
		return Cell(reading.Unreadable, false, "the store reported object names without their definitions, so the schemas could not be compared on anything but names", command, nil)
	}
	var lost, added, changed []string
	for name := range held {
		if _, ok := declared[name]; !ok {
			lost = append(lost, name)
		} else if !sameStatement(held[name], declared[name]) {
			changed = append(changed, name)
		}
	}
	for name := range declared {
		if _, ok := held[name]; !ok {
			added = append(added, name)
		}
	}
	sort.Strings(lost)
	sort.Strings(added)
	sort.Strings(changed)
	evidenceValue := Object{{Key: "dbPath", Value: record.Get(storeAnswer, "dbPath")}, {Key: "onlyInStore", Value: strs(lost)}, {Key: "onlyInCandidate", Value: strs(added)}, {Key: "definedDifferently", Value: strs(changed)}}
	backup := " OPS-4.5 makes a schema change its own decision, in its own issue, with a copied backup of the whole state directory taken first, so this update refuses rather than letting the new runtime apply it on its first write-open."
	// The additive zone is a decision of its own (D-01) and its arrival and its removal are the only two
	// differences with an answer of their own; a changed object, or any object outside the zone, never
	// reaches either and keeps the answers below.
	if len(changed) == 0 && (len(lost) == 0) != (len(added) == 0) {
		zone, err := zoneObjects()
		if err != nil {
			return Cell(reading.Unreadable, false, "which schema objects belong to the additive DAG zone could not be derived: "+err.Error(), command, nil)
		}
		zoneEvidence := append(evidenceValue, record.Object{{Key: "zoneOnly", Value: true}}...)
		switch {
		case len(lost) > 0 && inZone(lost, zone):
			return Cell(NarrowsZone, true, "the store holds objects of the additive DAG zone that this candidate does not declare: "+strings.Join(lost, ", ")+". An older runtime opens such a store and ignores the zone's tables (D-01), so returning to it loses nothing and is not refused; the zone stays in the store and a newer runtime reads it again.", command, zoneEvidence)
		case len(added) > 0 && inZone(added, zone):
			return Cell(ExtendsZone, true, "the candidate declares objects of the additive DAG zone that the store does not hold: "+strings.Join(added, ", ")+". Nothing in the store is lost or rewritten, and the candidate creates them on its first write-open (D-01). OPS-4.5 still requires a copied backup of the whole state directory first, so this refuses unless the command is run with --backup-state-to DIR: it copies the state directory (copy only, byte for byte) after the daemon and in-flight cells pass and before the swap, records the copy, and then allows the swap.", command, zoneEvidence)
		}
	}
	switch {
	case len(lost) > 0:
		return Cell(Narrows, true, "the store holds schema objects this candidate does not declare, so installing it would leave data no runtime can read: "+strings.Join(lost, ", "), command, evidenceValue)
	case len(changed) > 0:
		return Cell(Differs, true, "the store and the candidate define the same schema objects differently: "+strings.Join(changed, ", ")+"."+backup, command, evidenceValue)
	case len(added) > 0:
		return Cell(Extends, true, "the candidate declares schema objects the store does not hold: "+strings.Join(added, ", ")+". Nothing in the store would be lost, and that is why this is reported as its own answer rather than as a downgrade."+backup, command, evidenceValue)
	}
	return Cell(Agrees, true, "the store and the candidate declare the same schema objects identically", command, evidenceValue)
}

func strs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// zoneObjects are the schema objects (the keys SchemaObjectsQuery gives: kind, a space and the name) that
// the additive DAG zone creates: its statements applied alone to an empty in-memory database, the same
// statements a writable open runs and DeclaredSchema declares. They are derived, not matched by a name
// prefix, so a dag_ object this build does not know is not the zone's and keeps the plain answers.
var zoneObjects = sync.OnceValues(func() (map[string]bool, error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	for _, statement := range store.DAGZoneStatements() {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return nil, err
		}
	}
	rows, err := db.QueryContext(ctx, SchemaObjectsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := map[string]bool{}
	for rows.Next() {
		var name string
		var statement sql.NullString
		if err := rows.Scan(&name, &statement); err != nil {
			return nil, err
		}
		objects[name] = true
	}
	return objects, rows.Err()
})

func inZone(names []string, zone map[string]bool) bool {
	for _, name := range names {
		if !zone[name] {
			return false
		}
	}
	return true
}

func sameStatement(a, b any) bool {
	x, y := Normalised(a), Normalised(b)
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	return *x == *y
}

// Normalised is swapgate._normalised: a CREATE statement with runs of whitespace collapsed
// OUTSIDE quoted text, not lowercased and not touched inside quotes. nil for a NULL statement.
func Normalised(statement any) *string {
	if statement == nil {
		return nil
	}
	text := reading.Text(statement)
	var out []rune
	var quote rune
	space := false
	for _, char := range text {
		if quote != 0 {
			out = append(out, char)
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' || char == '`' || char == '[' {
			quote = char
			if char == '[' {
				quote = ']'
			}
			out = append(out, char)
			space = false
			continue
		}
		if isSpace(char) {
			space = true
			continue
		}
		if space && len(out) > 0 {
			out = append(out, ' ')
		}
		space = false
		out = append(out, char)
	}
	result := string(out)
	return &result
}

// isSpace is str.isspace for one character: Unicode White_Space plus U+001C..U+001F.
func isSpace(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f }

// Decide is swapgate.decide: the verdict, and which cells produced it. An established refusal
// is reported as a refusal even when another cell could not answer.
func Decide(cells map[string]Object) Object {
	return DecideWithRelease(cells, nil)
}

// Release is the one thing that lets a refusing storeSchema cell stand: the record of the OPS-4.5
// backup of the state directory, taken because the only difference is the additive zone arriving.
type Release struct{ Backup Object }

// ZoneArrivalOnly is whether the swap refuses for one reason only: the candidate brings the additive
// DAG zone to a store that lacks it, with the daemon stopped and no attempt open both established.
// Anything else (a running daemon, an open attempt, a cell that could not be read, another schema
// difference) is not the arrival alone, and no backup is taken for it.
func ZoneArrivalOnly(cells map[string]Object) bool {
	schema := cells["storeSchema"]
	if schema == nil || !pyvalue.Truthy(record.Get(schema, "readable")) || record.Get(schema, "answer") != ExtendsZone {
		return false
	}
	for _, name := range []string{"daemon", "inFlight"} {
		cell := cells[name]
		if cell == nil {
			return false
		}
		if refuses := Blocking(name, cell); refuses == nil || *refuses {
			return false
		}
	}
	return true
}

// DecideWithRelease is Decide with a release: when the only difference is the zone arriving
// (ZoneArrivalOnly) the storeSchema cell does not refuse, and the verdict carries the backup. A nil
// release is Decide, and every other cell still refuses as it does.
func DecideWithRelease(cells map[string]Object, release *Release) Object {
	var blockers, unread []any
	reported := Object{}
	released := release != nil && ZoneArrivalOnly(cells)
	for _, name := range Cells {
		given, present := cells[name]
		cell := given
		if !present || cell == nil {
			cell = Cell(reading.AccessError, false, "this cell was not read at all", nil, nil)
		}
		refuses := Blocking(name, cell)
		if released && name == "storeSchema" && refuses != nil && *refuses {
			stands := false
			refuses = &stands
		}
		switch {
		case refuses == nil:
			unread = append(unread, name+": "+reading.Text(record.Get(cell, "detail")))
		case *refuses:
			blockers = append(blockers, name+": "+reading.Text(record.Get(cell, "detail")))
		}
		if present && given != nil {
			reported = append(reported, record.Object{{Key: name, Value: given}}...)
		} else {
			reported = append(reported, record.Object{{Key: name, Value: nil}}...)
		}
	}
	verdict := Allowed
	if len(blockers) > 0 {
		verdict = Blocked
	} else if len(unread) > 0 {
		verdict = Unestablished
	}
	if blockers == nil {
		blockers = []any{}
	}
	if unread == nil {
		unread = []any{}
	}
	out := Object{
		{Key: "verdict", Value: verdict},
		{Key: "cells", Value: reported},
		{Key: "blockedBy", Value: blockers},
		{Key: "unreadable", Value: unread},
		{Key: "note", Value: "OPS-4.4 replaces a runtime only with the daemon stopped and open attempts reconciled, and this command never starts or stops one: the service belongs to the scope operator (OPS-4.1). A cell that could not be read keeps the existing installation exactly as a refusal does."},
	}
	if released {
		out = append(out, record.Object{{Key: "stateBackup", Value: release.Backup}}...)
	}
	return out
}

// StorePresence is runtime_install.store_presence without the interpreter: whether a store
// exists at the selection the relay's rule resolves, from the path alone.
func StorePresence(state, socket string) Object {
	selection, err := store.ResolveStateDir(state, socket)
	if err != nil {
		return Object{{Key: "readable", Value: false}, {Key: "present", Value: nil}, {Key: "dbPath", Value: nil}, {Key: "detail", Value: err.Error()}, {Key: "command", Value: nil}}
	}
	database := selection.DBPath()
	if _, err := os.Lstat(database); errors.Is(err, os.ErrNotExist) {
		return Object{{Key: "readable", Value: true}, {Key: "present", Value: false}, {Key: "dbPath", Value: database}, {Key: "detail", Value: nil}, {Key: "command", Value: nil}}
	} else if err != nil {
		return Object{{Key: "readable", Value: false}, {Key: "present", Value: nil}, {Key: "dbPath", Value: database}, {Key: "detail", Value: err.Error()}, {Key: "command", Value: nil}}
	}
	return Object{{Key: "readable", Value: true}, {Key: "present", Value: true}, {Key: "dbPath", Value: database}, {Key: "detail", Value: nil}, {Key: "command", Value: nil}}
}

// StoreSchema is runtime_install.store_tables without the interpreter: the schema objects the
// store holds. Absence is settled by lstat first, and the catalog is read by readCatalog, which
// creates nothing in the state directory.
func StoreSchema(ctx context.Context, state, socket string) Object {
	presence := StorePresence(state, socket)
	if !pyvalue.Truthy(record.Get(presence, "readable")) || record.Get(presence, "present") == false {
		return append(presence, record.Object{{Key: "objects", Value: nil}}...)
	}
	database := record.Get(presence, "dbPath")
	objects, err := readCatalog(ctx, database.(string))
	if err != nil {
		return Object{{Key: "readable", Value: false}, {Key: "present", Value: true}, {Key: "dbPath", Value: database}, {Key: "objects", Value: nil}, {Key: "detail", Value: err.Error()}, {Key: "command", Value: nil}}
	}
	return Object{{Key: "readable", Value: true}, {Key: "present", Value: true}, {Key: "dbPath", Value: database}, {Key: "objects", Value: objects}, {Key: "detail", Value: nil}, {Key: "command", Value: nil}}
}

// readCatalog asks SchemaObjectsQuery of the database at path without creating anything beside
// it and without store.Open (which would run the schema script, and refuses the live state root
// under test isolation). It reads under the Stop path's no-sidecar rule (store.OpenInPlace): the
// path resolved as SQLite resolves it, so a symlinked relay.sqlite3 is read with the -wal and -shm
// beside the file it names; mode=ro when a WAL connection left both, immutable=1 when no -wal
// holds a frame, and no read at all when a -wal holds frames beside no usable -shm, whose commits
// an immutable read would miss. It takes no lock and writes nothing.
func readCatalog(ctx context.Context, path string) (Object, error) {
	db, err := store.OpenInPlace(ctx, path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, SchemaObjectsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := Object{}
	for rows.Next() {
		var name string
		var statement sql.NullString
		if err := rows.Scan(&name, &statement); err != nil {
			return nil, err
		}
		var value any
		if statement.Valid {
			value = statement.String
		}
		objects = append(objects, record.Object{{Key: name, Value: value}}...)
	}
	return objects, rows.Err()
}

// DeclaredSchema is the schema this build installs: its DDL script and then its guard indexes
// applied to an in-memory database, asked SchemaObjectsQuery. This is what the candidate binary
// prints for `crw doctor declared-schema --json`.
func DeclaredSchema(ctx context.Context) Object {
	unreadable := func(err error) Object {
		return Object{{Key: "readable", Value: false}, {Key: "objects", Value: nil}, {Key: "schemaVersion", Value: store.SchemaVersion}, {Key: "detail", Value: err.Error()}}
	}
	ddl, guards, err := store.SchemaStatements()
	if err != nil {
		return unreadable(err)
	}
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return unreadable(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return unreadable(err)
	}
	for _, statement := range guards {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return unreadable(err)
		}
	}
	// The additive DAG zone is part of what this build installs on every writable open, so a store
	// it opened agrees with what it declares.
	for _, statement := range store.DAGZoneStatements() {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return unreadable(err)
		}
	}
	rows, err := db.QueryContext(ctx, SchemaObjectsQuery)
	if err != nil {
		return unreadable(err)
	}
	defer rows.Close()
	objects := Object{}
	for rows.Next() {
		var name string
		var statement sql.NullString
		if err := rows.Scan(&name, &statement); err != nil {
			return unreadable(err)
		}
		var value any
		if statement.Valid {
			value = statement.String
		}
		objects = append(objects, record.Object{{Key: name, Value: value}}...)
	}
	if err := rows.Err(); err != nil {
		return unreadable(err)
	}
	return Object{{Key: "readable", Value: true}, {Key: "objects", Value: objects}, {Key: "schemaVersion", Value: store.SchemaVersion}, {Key: "detail", Value: nil}}
}

// CandidateSchema is runtime_install.candidate_tables for a Go candidate: executed as the
// candidate's own binary, so the answer is the schema that build would install.
//
// Only an answer from a candidate that exited 0 is read. `crw doctor declared-schema --json`
// exits 0 whenever it has printed its answer, including an answer of readable false, so a
// nonzero exit or a signal means the candidate failed, and whatever it printed first is kept as
// a diagnostic rather than read as its schema: a failed candidate must never make the gate
// ALLOWED. runtime_install._asked parses stdout whatever the exit status, which is the same
// defect on the Python side. The wait is bounded (120 s, then scope.WaitDelay for output a
// process the candidate started still holds), and an answer whose output stayed open past its
// exit is not read either.
func CandidateSchema(ctx context.Context, binary string) Object {
	argv := []string{binary, "doctor", "declared-schema", "--json"}
	command := strs(argv)
	fail := func(detail string) Object {
		return Object{{Key: "readable", Value: false}, {Key: "command", Value: command}, {Key: "objects", Value: nil}, {Key: "present", Value: nil}, {Key: "detail", Value: detail}}
	}
	run, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(run, argv[0], argv[1:]...)
	cmd.WaitDelay = scope.WaitDelay
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.Is(err, exec.ErrWaitDelay) {
		return fail("the candidate's declared tables were not answered: the candidate exited, but a process it left behind kept its output open " + scope.WaitDelay.String() + " past that, so nothing it printed is read as its schema" + diagnostics(stderr.String(), string(out)))
	}
	if errors.As(err, &exit) {
		ended := exit.ProcessState.String()
		if run.Err() != nil {
			ended += " after " + run.Err().Error()
		}
		return fail("the candidate's declared tables were not answered: the candidate ended with " + ended + ", so nothing it printed is read as its schema" + diagnostics(stderr.String(), string(out)))
	}
	if err != nil {
		return fail("the candidate's declared tables could not be asked: " + err.Error())
	}
	value, decodeErr := reading.Decode(out)
	answer, ok := value.(Object)
	if decodeErr != nil || !ok {
		said := strings.TrimSpace(stderr.String())
		if said == "" {
			said = strings.TrimSpace(string(out))
		}
		return fail("the candidate's declared tables did not answer with JSON: " + tail(said))
	}
	return record.Set(answer, "command", command)
}

// diagnostics is what a failed candidate printed, each stream's last 400 characters.
func diagnostics(stderr, stdout string) string {
	var said string
	if text := strings.TrimSpace(stderr); text != "" {
		said += "; stderr: " + tail(text)
	}
	if text := strings.TrimSpace(stdout); text != "" {
		said += "; stdout: " + tail(text)
	}
	return said
}

// tail is text[-400:], counted in characters as Python counts them.
func tail(text string) string {
	runes := []rune(text)
	if len(runes) > 400 {
		return string(runes[len(runes)-400:])
	}
	return text
}
