package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

const policyVariable = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"

var doctorCommand = dispatch.Command{Name: "doctor", Exempt: true, ReportsMismatch: true, ReadOnly: true, Run: runDoctor}

// runDoctor is cmd_doctor: what THIS process can actually do here, measured rather than
// assumed. It constructs no Store.
func runDoctor(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	probed := store.Probe(ctx, services.Selection)
	loc := probeStore(probed)
	report := contract.OrderedObject{
		{Key: "stateSelection", Value: selectionRecord(services.Selection)},
		{Key: "store", Value: locationRecord(loc, probed.Access.DBExists)},
		{Key: "access", Value: accessRecord(probed.Access)},
		{Key: "ownership", Value: ownershipReport(ctx, services)},
	}
	add := func(key string, value any) { report = append(report, contract.Field{Key: key, Value: value}) }
	info, err := os.Stat("/proc/self/fd")
	add("procAvailable", err == nil && info.IsDir())
	if services.AdapterRequested {
		add("adapter", "bridge")
	} else {
		add("adapter", "none (read-only, no --socket)")
	}
	ledger, err := ledgerLocation(services)
	if err != nil {
		return nil, err
	}
	add("ledger", ledger)
	add("actorReachability", reachability(services, probed.Access))
	add("contents", contents(ctx, services, probed.Access))
	siblings, err := siblingStores(services)
	if err != nil {
		return nil, err
	}
	add("siblingStores", siblings)
	add("accessReceipt", accessReceipt(ctx, services, loc, probed.Access))
	caller := declaredPolicy(os.Getenv(policyVariable))
	add("rolePolicy", rolePolicyReport(caller))
	worker := readWorkerPolicy(services, loc)
	add("workerPolicy", worker)
	add("launchPolicy", service.ResolveLaunchPolicyAt(services.Selection.Path, os.Getenv(policyVariable)))
	workerPolicy := get(worker, "policy")
	callerRecord := caller.summary()
	agreement := "unknown"
	if pyvalue.Truthy(get(callerRecord, "digest")) && pyEqual(orEmpty(workerPolicy), callerRecord) {
		agreement = "same"
	} else if pyvalue.Truthy(get(callerRecord, "digest")) && pyvalue.Truthy(get(workerPolicy, "digest")) {
		agreement = "different"
	}
	add("callerWorkerAgreement", agreement)
	requested, requiredWorker := args.String("require-worker-policy")
	ready := true
	if requiredWorker {
		requirements, err := settingsJSON(requested)
		if err != nil {
			return nil, &dispatch.UsageError{Detail: "invalid worker policy requirements: " + err.Error(), Code: contract.ExitUsage}
		}
		readiness := workerReadiness(worker, requirements, caller)
		ready = get(readiness, "ready") == true
		add("workerReadiness", readiness)
	}
	issueKey, _ := args.String("issue")
	var issue contract.OrderedObject
	if issueKey != "" {
		if issue, err = issueReading(ctx, services, loc, probed.Access, issueKey); err != nil {
			return nil, err
		}
		add("issue", issue)
	}
	expectations := store.CompareExpectations{}
	expectations.StoreID, expectations.StoreIDGiven = args.String("expect-store")
	expectations.Inode, expectations.InodeGiven = args.String("expect-inode")
	expectations.Log, expectations.LogGiven = args.String("expect-log")
	nonce, nonceGiven := args.String("expect-nonce")
	var nonceValue any
	if nonce != "" {
		reading := store.NonceLookup(ctx, services.Selection, nonce)
		if reading.Raised != nil {
			return nil, reading.Raised
		}
		expectations.Nonce = &reading
		nonceValue = nonceRecord(reading)
	}
	add("nonce", nonceValue)
	comparison := store.CompareStore(loc, expectations)
	add("sameStore", string(comparison.SameStore))
	add("detail", comparison.Detail)
	// The Python report has no slot a runtime reading could fill without changing a key, so it
	// is a new trailing top-level key (todo 20): every Python key keeps its place and value.
	add("runtime", runtimeBlock())
	// Present only where it has something to say, like issue and workerReadiness (decision 73).
	served, err := serviceStore(services)
	if err != nil {
		return nil, err
	}
	if served != nil {
		add("serviceStore", served)
	}
	// A store recording another socket than the one it must serve, which every other command
	// refuses (decision 73).
	if mismatch := dispatch.Mismatch(services); mismatch != nil {
		add("socketMismatch", mismatch)
	}
	asked := expectations.StoreIDGiven || expectations.InodeGiven || expectations.LogGiven || nonceGiven
	if (asked && comparison.SameStore != store.Proven) || (requiredWorker && !ready) {
		return nil, &dispatch.PayloadExit{Payload: report, Code: contract.ExitRefused}
	}
	if issueKey != "" && get(issue, "readable") == true && get(issue, "storeAgreement") != "same" {
		return nil, &dispatch.PayloadExit{Payload: report, Code: contract.ExitRefused}
	}
	return report, nil
}

// serviceStore is the store the relay service registered for this selection's socket (the
// default App Server socket when no --socket was given) serves, from its scope claim, when
// discovery chose another directory: a reader that selected the wrong store is told where the
// service's store is and how to read it. nil when --state or CODEX_SESSION_RELAY_STATE chose the
// directory, when no claim is recorded, or when the claim names the selected directory.
func serviceStore(services dispatch.Services) (contract.OrderedObject, error) {
	if source := services.Selection.Source; source == "flag" || source == "env" {
		return nil, nil
	}
	socket := services.SocketPath
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return nil, err
		}
	}
	served := service.ServedStore(socket)
	if served == nil {
		return nil, nil
	}
	directory, _ := get(served, "stateDirectory").(string)
	if sameDirectory(directory, services.Selection.Path) {
		return nil, nil
	}
	claimed, _ := get(served, "socketPath").(string)
	if claimed == "" {
		claimed = socket
	}
	return append(served,
		contract.Field{Key: "detail", Value: "the relay service registered for this App Server socket serves another directory than the one discovery selected here; that store, not this one, is the one it reads and writes"},
		contract.Field{Key: "recover", Value: []any{
			services.Program + " --state=" + shellQuote(directory) + " --socket=" + shellQuote(claimed) + " doctor",
			"  reads the store that service serves",
		}},
	), nil
}

// sameDirectory is whether a and b name one directory once every symbolic link is followed; a
// path that cannot be resolved is compared as spelled.
func sameDirectory(a, b string) bool {
	resolvedA, errA := store.ResolvePath(a)
	resolvedB, errB := store.ResolvePath(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return resolvedA == resolvedB
}

// ownershipReport is ownership.report: the six schema_meta keys and the mirror's raw phase,
// all or nothing. Any failure to read either half nulls every key and phase and names it in
// detail; runtime_build is always the answering runtime's own build.
func ownershipReport(ctx context.Context, services dispatch.Services) contract.OrderedObject {
	meta, phase, detail := readOwnership(ctx, services.Selection.DBPath())
	report := contract.OrderedObject{}
	// A failed reading has no meta: every key is null beside its detail. The torn stamp
	// (below) keeps its keys, as report() prints meta.get(key) whatever detail says.
	for _, key := range ownership.Keys {
		value, found := meta[key]
		if found {
			report = append(report, contract.Field{Key: key, Value: value})
		} else {
			report = append(report, contract.Field{Key: key, Value: nil})
		}
	}
	return append(report,
		contract.Field{Key: "phase", Value: phase},
		contract.Field{Key: "runtime_build", Value: runtimeBuild()},
		contract.Field{Key: "detail", Value: detail},
	)
}

// readOwnership reads like report(): ownership.metadata, then ownership.mirror. The phase is
// the mirror's "phase" value of any JSON type, nil without a (non-empty) mirror; detail is
// str(error) of the first failure (with nil meta), "takeover record missing" for the torn
// state "initial stamp committed, mirror absent" (cutover.md Record: an ownership key in
// schema_meta and no takeover.json), or nil.
func readOwnership(ctx context.Context, dbPath string) (map[string]string, any, any) {
	meta, err := store.OwnershipMetadata(ctx, dbPath)
	if err != nil {
		return nil, nil, err.Error()
	}
	raw, err := store.OwnershipMirror(dbPath)
	if err != nil {
		return nil, nil, err.Error()
	}
	if raw == nil {
		for _, key := range ownership.Keys {
			if _, stamped := meta[key]; stamped {
				// Stamped but unpublished is not healthy (ownership.py report).
				return meta, nil, "takeover record missing"
			}
		}
		return meta, nil, nil
	}
	// The probe's preflight reads the same bytes the same way (store.MirrorRefusal).
	text, why := store.MirrorDocument(raw)
	if why != "" {
		return nil, nil, "store_owned_by_other: " + why
	}
	value, err := decodeJSON([]byte(text))
	if err != nil {
		return nil, nil, "store_owned_by_other: takeover record unreadable: " + err.Error()
	}
	record, ok := value.(contract.OrderedObject)
	if !ok {
		return nil, nil, "store_owned_by_other: takeover record is not an object"
	}
	var phase any
	if at := fieldIndex(record, "phase"); at >= 0 {
		phase = record[at].Value
	}
	return meta, phase, nil
}

// runtimeBuild is the build this runtime publishes as its holder identity (takeover.json's
// holder.build and a candidate's ready): the build cmd/crw stamped, else its version.
func runtimeBuild() string {
	if Build != "" {
		return Build
	}
	return Version
}

func orEmpty(v any) any {
	if v == nil {
		return contract.OrderedObject{}
	}
	return v
}

// nonceRecord is nonce_lookup's dict, keys in Python's insertion order for each shape.
func nonceRecord(n store.NonceReading) contract.OrderedObject {
	identity := contract.OrderedObject{
		{Key: "device", Value: nullableCount(n.Device)}, {Key: "inode", Value: nullableCount(n.Inode)},
		{Key: "links", Value: nullableCount(n.Links)},
		{Key: "logDevice", Value: nullableCount(n.LogDevice)}, {Key: "logInode", Value: nullableCount(n.LogInode)},
		{Key: "logName", Value: nullableText(n.LogName)},
	}
	if !n.Readable {
		return append(identity, contract.Field{Key: "nonce", Value: n.Nonce}, contract.Field{Key: "found", Value: false},
			contract.Field{Key: "readable", Value: false}, contract.Field{Key: "detail", Value: n.Detail})
	}
	// {**opened, **located, "links": ...}: device, inode, links, then the log fields.
	record := append(identity, contract.Field{Key: "nonce", Value: n.Nonce}, contract.Field{Key: "found", Value: n.Found},
		contract.Field{Key: "readable", Value: true}, contract.Field{Key: "detail", Value: nil})
	if n.Found {
		record = append(record, contract.Field{Key: "writtenBy", Value: n.WrittenBy}, contract.Field{Key: "writtenAt", Value: n.WrittenAt})
	}
	return record
}

// reachability is _reachability.
func reachability(services dispatch.Services, access store.ProbeAccess) contract.OrderedObject {
	connect := "not configured"
	if services.SocketPath != "" {
		path, err := store.ExpandUser(services.SocketPath)
		if err == nil {
			var conn net.Conn
			conn, err = net.DialTimeout("unix", path, 2*time.Second)
			if err == nil {
				_ = conn.Close()
			}
		}
		connect = "ok"
		if err != nil {
			connect = err.Error()
		}
	}
	return contract.OrderedObject{
		{Key: "stateDirectoryWritable", Value: access.DirectoryWritable},
		{Key: "stateDirectoryDetail", Value: nullableText(access.Detail)},
		{Key: "socketConfigured", Value: services.SocketPath != ""},
		{Key: "socketConnect", Value: connect},
		{Key: "offlineCommands", Value: stringList(offlineCommands)},
		{Key: "hostRequiredCommands", Value: stringList(hostRequiredCommands)},
	}
}

// accessReceipt is _access_receipt: identity and participants from ONE read.
func accessReceipt(ctx context.Context, services dispatch.Services, loc store.Location, access store.ProbeAccess) contract.OrderedObject {
	recorded := contract.OrderedObject{{Key: "available", Value: false}, {Key: "participants", Value: contract.OrderedObject{}}, {Key: "detail", Value: nil}}
	if access.DBReadable {
		type row struct{ kind, taskID, settings, source, recordedAt any }
		var rows []row
		read := store.ReadOnlyRows(ctx, services.Selection,
			"SELECT 'meta' AS kind, key AS task_id, value AS settings,"+
				"       NULL AS source, NULL AS recorded_at"+
				"  FROM schema_meta WHERE key = 'store_id'"+
				" UNION ALL"+
				" SELECT 'settings', task_id, settings, source, recorded_at"+
				"   FROM authorized_settings"+
				" ORDER BY kind, task_id", nil,
			func(r store.RowScanner) error {
				var one row
				if err := r.Scan(&one.kind, &one.taskID, &one.settings, &one.source, &one.recordedAt); err != nil {
					return err
				}
				rows = append(rows, one)
				return nil
			})
		switch {
		case !read.Readable || read.Detail != "":
			detail := read.Detail
			if detail == "" {
				detail = "the authorized settings could not be read"
			}
			recorded[2].Value = detail
		default:
			var seen any
			for _, r := range rows {
				if r.kind == "meta" {
					seen = r.settings
					break
				}
			}
			switch {
			case !pyEqual(sqlValue(seen), nullableText(loc.StoreID)):
				recorded[2].Value = fmt.Sprintf("the store changed under this command: identity %s was measured, settings were read from %s",
					shown(nullableText(loc.StoreID)), shown(sqlValue(seen)))
			case read.Device != loc.Device || read.Inode != loc.Inode:
				recorded[2].Value = fmt.Sprintf("the store changed under this command: device:inode %s:%s was measured, rows were read from %d:%d",
					shown(nullableCount(loc.Device)), shown(nullableCount(loc.Inode)), read.Device, read.Inode)
			default:
				participants := contract.OrderedObject{}
				for _, r := range rows {
					if r.kind != "settings" {
						continue
					}
					key := fmt.Sprint(sqlValue(r.taskID))
					summary := sandboxSummary(sqlValue(r.settings), sqlValue(r.source), sqlValue(r.recordedAt))
					if at := fieldIndex(participants, key); at >= 0 {
						participants[at].Value = summary
					} else {
						participants = append(participants, contract.Field{Key: key, Value: summary})
					}
				}
				recorded[0].Value = true
				recorded[1].Value = participants
			}
		}
	}
	return contract.OrderedObject{
		{Key: "storeId", Value: nullableText(loc.StoreID)},
		{Key: "dbPath", Value: loc.DBPath},
		{Key: "realPath", Value: nullableText(loc.RealPath)},
		{Key: "device", Value: nullableCount(loc.Device)},
		{Key: "inode", Value: nullableCount(loc.Inode)},
		{Key: "links", Value: nullableCount(loc.Links)},
		{Key: "logDevice", Value: nullableCount(loc.LogDevice)},
		{Key: "logInode", Value: nullableCount(loc.LogInode)},
		{Key: "logName", Value: nullableText(loc.LogName)},
		{Key: "selectedBy", Value: contract.OrderedObject{{Key: "source", Value: services.Selection.Source}, {Key: "detail", Value: services.Selection.Detail}}},
		{Key: "observedAccess", Value: contract.OrderedObject{
			{Key: "read", Value: access.DBReadable}, {Key: "write", Value: access.DBWritable},
			{Key: "directoryWritable", Value: access.DirectoryWritable}, {Key: "detail", Value: nullableText(access.Detail)},
		}},
		{Key: "recordedSandbox", Value: recorded},
	}
}

// sqlValue maps a scanned SQLite value to the value Python's sqlite3 would hand back.
func sqlValue(v any) any {
	switch value := v.(type) {
	case []byte:
		return string(value)
	case int64:
		return value
	case float64:
		return value
	}
	return v
}

// issueReading is _issue_reading. Its error is the UnicodeEncodeError read_only_rows lets
// escape for a key sqlite3 cannot bind, which cli.main answers as a host error.
func issueReading(ctx context.Context, services dispatch.Services, loc store.Location, access store.ProbeAccess, key string) (contract.OrderedObject, error) {
	blank := func(readable bool, storeID any, agreement, detail string) contract.OrderedObject {
		return contract.OrderedObject{
			{Key: "key", Value: key}, {Key: "readable", Value: readable}, {Key: "holds", Value: nil},
			{Key: "responsibleChild", Value: nil}, {Key: "responsibleRelationship", Value: nil},
			{Key: "storeId", Value: storeID}, {Key: "storeAgreement", Value: agreement}, {Key: "detail", Value: detail},
		}
	}
	if !access.DBReadable {
		return blank(false, nil, "unknown", "the database is not readable from this process"), nil
	}
	var storeID, relationship, child any
	rows := 0
	read := store.ReadOnlyRows(ctx, services.Selection,
		"SELECT (SELECT value FROM schema_meta WHERE key = 'store_id') AS store_id,"+
			"       (SELECT relationship_id FROM relationships"+
			"         WHERE issue_key = ? AND status IN ('active','paused')"+
			"           AND superseded_by IS NULL"+
			"         ORDER BY created_at LIMIT 1) AS relationship_id,"+
			"       (SELECT child_task_id FROM relationships"+
			"         WHERE issue_key = ? AND status IN ('active','paused')"+
			"           AND superseded_by IS NULL"+
			"         ORDER BY created_at LIMIT 1) AS child_task_id",
		[]any{key, key}, func(r store.RowScanner) error { rows++; return r.Scan(&storeID, &relationship, &child) })
	if read.Raised != nil {
		return nil, read.Raised
	}
	if !read.Readable || read.Detail != "" || rows == 0 {
		detail := read.Detail
		if detail == "" {
			detail = "the store could not be read"
		}
		return blank(false, nil, "unknown", detail), nil
	}
	storeID, relationship, child = sqlValue(storeID), sqlValue(relationship), sqlValue(child)
	agreement := "same"
	for _, pair := range [][2]any{
		{storeID, nullableText(loc.StoreID)},
		{nullableCount(read.Device), nullableCount(loc.Device)},
		{nullableCount(read.Inode), nullableCount(loc.Inode)},
	} {
		if pair[0] == nil || pair[1] == nil {
			agreement = "unknown"
			break
		}
		if !pyEqual(pair[0], pair[1]) {
			agreement = "changed"
			break
		}
	}
	if agreement != "same" {
		return blank(true, storeID, agreement, "the rows did not come from the store this process measured"), nil
	}
	return contract.OrderedObject{
		{Key: "key", Value: key}, {Key: "readable", Value: true}, {Key: "holds", Value: relationship != nil},
		{Key: "responsibleChild", Value: child}, {Key: "responsibleRelationship", Value: relationship},
		{Key: "storeId", Value: storeID}, {Key: "storeAgreement", Value: "same"}, {Key: "detail", Value: nil},
	}, nil
}

// settingsJSON is a JSON document given on the command line, or @path to a file holding one.
func settingsJSON(raw string) (any, error) {
	data := []byte(raw)
	if path, found := strings.CutPrefix(raw, "@"); found {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		data = content
	}
	return decodeInput(data)
}

// socketDigest is sha256(canonical).hexdigest()[:16].
func socketDigest(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:16]
}

// runtimeBlock is {language, version, build}: build is the VCS revision the binary was
// built from, or null when the build recorded none.
func runtimeBlock() contract.OrderedObject {
	var build any
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				build = setting.Value
			}
		}
	}
	return contract.OrderedObject{{Key: "language", Value: "go"}, {Key: "version", Value: Version}, {Key: "build", Value: build}}
}
