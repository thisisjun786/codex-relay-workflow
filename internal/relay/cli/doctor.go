package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const policyVariable = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"

var doctorCommand = Command{
	Name:   "doctor",
	Exempt: true,
	Flags: func(f *flag.FlagSet) {
		f.String("require-worker-policy", "", "JSON list (or @file) of {role,model,reasoningEffort}")
		f.String("expect-store", "", "the store id another participant reported")
		f.String("expect-inode", "", "the device:inode another participant reported")
		f.String("expect-log", "", "the device:inode:name another participant reported for its write-ahead log")
		f.String("expect-nonce", "", "a nonce another participant wrote here")
		f.String("issue", "", "also answer whether this store holds an assignment for this issue identity")
	},
	Run: runDoctor,
}

// runDoctor is cmd_doctor: what THIS process can actually do here, measured rather than
// assumed. It constructs no Store.
func runDoctor(ctx context.Context, services Services, args Args) (any, error) {
	probed := store.Probe(ctx, services.Selection)
	loc := probeStore(probed)
	report := contract.OrderedObject{
		{Key: "stateSelection", Value: selectionRecord(services.Selection)},
		{Key: "store", Value: locationRecord(loc, probed.Access.DBExists)},
		{Key: "access", Value: accessRecord(probed.Access)},
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
	caller, err := declaredPolicy(os.Getenv(policyVariable))
	if err != nil {
		return nil, err
	}
	add("rolePolicy", rolePolicyReport(caller))
	worker := readWorkerPolicy(services, loc)
	add("workerPolicy", worker)
	launch, err := resolveLaunchPolicy(services)
	if err != nil {
		return nil, err
	}
	add("launchPolicy", launch)
	workerPolicy := get(worker, "policy")
	callerRecord := caller.summary()
	agreement := "unknown"
	if truthy(get(callerRecord, "digest")) && pyEqual(orEmpty(workerPolicy), callerRecord) {
		agreement = "same"
	} else if truthy(get(callerRecord, "digest")) && truthy(get(workerPolicy, "digest")) {
		agreement = "different"
	}
	add("callerWorkerAgreement", agreement)
	requested, requiredWorker := args.String("require-worker-policy")
	ready := true
	if requiredWorker {
		requirements, err := settingsJSON(requested)
		if err != nil {
			return nil, &UsageError{Detail: "invalid worker policy requirements: " + err.Error(), Code: contract.ExitUsage}
		}
		readiness := workerReadiness(worker, requirements, caller)
		ready = get(readiness, "ready") == true
		add("workerReadiness", readiness)
	}
	issueKey, _ := args.String("issue")
	var issue contract.OrderedObject
	if issueKey != "" {
		issue = issueReading(ctx, services, loc, probed.Access, issueKey)
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
	asked := expectations.StoreIDGiven || expectations.InodeGiven || expectations.LogGiven || nonceGiven
	if (asked && comparison.SameStore != store.Proven) || (requiredWorker && !ready) {
		return nil, &PayloadExit{Payload: report, Code: contract.ExitRefused}
	}
	if issueKey != "" && get(issue, "readable") == true && get(issue, "storeAgreement") != "same" {
		return nil, &PayloadExit{Payload: report, Code: contract.ExitRefused}
	}
	return report, nil
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
func reachability(services Services, access store.ProbeAccess) contract.OrderedObject {
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
			connect = socketFailure(err)
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

// socketFailure is f"{type(error).__name__}: {error}" for socket.connect: no filename part.
func socketFailure(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		full := store.PythonOSError(errno)
		return full
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return "TimeoutError: timed out"
	}
	return err.Error()
}

// accessReceipt is _access_receipt: identity and participants from ONE read.
func accessReceipt(ctx context.Context, services Services, loc store.Location, access store.ProbeAccess) contract.OrderedObject {
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
					pyRepr(nullableText(loc.StoreID)), pyRepr(sqlValue(seen)))
			case read.Device != loc.Device || read.Inode != loc.Inode:
				recorded[2].Value = fmt.Sprintf("the store changed under this command: device:inode %s:%s was measured, rows were read from %d:%d",
					pyRepr(nullableCount(loc.Device)), pyRepr(nullableCount(loc.Inode)), read.Device, read.Inode)
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

// issueReading is _issue_reading.
func issueReading(ctx context.Context, services Services, loc store.Location, access store.ProbeAccess, key string) contract.OrderedObject {
	blank := func(readable bool, storeID any, agreement, detail string) contract.OrderedObject {
		return contract.OrderedObject{
			{Key: "key", Value: key}, {Key: "readable", Value: readable}, {Key: "holds", Value: nil},
			{Key: "responsibleChild", Value: nil}, {Key: "responsibleRelationship", Value: nil},
			{Key: "storeId", Value: storeID}, {Key: "storeAgreement", Value: agreement}, {Key: "detail", Value: detail},
		}
	}
	if !access.DBReadable {
		return blank(false, nil, "unknown", "the database is not readable from this process")
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
	if !read.Readable || read.Detail != "" || rows == 0 {
		detail := read.Detail
		if detail == "" {
			detail = "the store could not be read"
		}
		return blank(false, nil, "unknown", detail)
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
		return blank(true, storeID, agreement, "the rows did not come from the store this process measured")
	}
	return contract.OrderedObject{
		{Key: "key", Value: key}, {Key: "readable", Value: true}, {Key: "holds", Value: relationship != nil},
		{Key: "responsibleChild", Value: child}, {Key: "responsibleRelationship", Value: relationship},
		{Key: "storeId", Value: storeID}, {Key: "storeAgreement", Value: "same"}, {Key: "detail", Value: nil},
	}
}

// settingsJSON is _settings_json: a JSON document, or @path to a file holding one.
func settingsJSON(raw string) (any, error) {
	data := []byte(raw)
	if path, found := strings.CutPrefix(raw, "@"); found {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New(store.PythonOSErrorText(err))
		}
		if !utf8.Valid(content) {
			return nil, errors.New("'utf-8' codec can't decode the file")
		}
		data = content
	}
	value, err := decodeJSON(data)
	if err != nil {
		return nil, errors.New(jsonErrorText(data, err))
	}
	return value, nil
}

// jsonErrorText is str(json.JSONDecodeError) for data json.loads refuses.
func jsonErrorText(data []byte, err error) string {
	if message := store.PythonJSONError(string(data)); message != "" {
		return message
	}
	return err.Error()
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
