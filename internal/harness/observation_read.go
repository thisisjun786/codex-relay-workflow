package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// HookObservationMaxAgeMS is HOOK_OBSERVATION_MAX_AGE_MS (hook-observation.mjs:13): a record older than a
// day is stale.
const HookObservationMaxAgeMS = 24 * 60 * 60 * 1000

// observationMaxRecord is MAX_RECORD_BYTES (:12); a manifest may be as long as a hook input (MAX_RAW_BYTES, :11).
const observationMaxRecord = 8192

// ObservationQuery is the argument object of readHookObservations (:87-88). The caller states every input:
// the oracle's defaults (CODEX_HOME, the clock, a day) are the caller's, because zero is a value here
// (maxAgeMs 0 admits only a record of this very millisecond). A nil AgentID is the root session. NowMS and
// MaxAgeMS are the oracle's numbers, epoch and span in milliseconds.
type ObservationQuery struct {
	PluginRoot string
	CodexHome  string
	SessionID  string
	AgentID    *string
	NowMS      int64
	MaxAgeMS   int64
}

// HookObservation is the approved metadata of one record (:120-121); nothing else a record holds is passed on.
// ObservedAt stays the record's own text, which the doctor prints as it was written.
type HookObservation struct {
	SessionID  string
	AgentID    *string
	Component  string
	Event      string
	ObservedAt string
	Entrypoint string
	Outcome    string
}

// HookObservations is the answer of readHookObservations (:89): the records that hold, how many did not,
// and why nothing could be said at all (empty when the store was read).
type HookObservations struct {
	Observations []HookObservation
	Ignored      int
	Reason       string
}

// ReadHookObservations is readHookObservations (hook-observation.mjs:86-129): the invocation records of one
// session and actor that still describe this plugin, with no fallback to another session or actor. It reads
// what RecordInvocation writes, and where the two Go and oracle writers differ it follows the Go one: the
// store is <CODEX_HOME>/crw/hook-observations, and a record names the manifest as its entrypoint, so the
// oracle's component-script pattern is replaced by an equality with Entrypoint and the entrypoint digest is
// the manifest's. A record is read through a handle that is not a link, is a regular file and is at most
// 8192 bytes; one that is anything else, does not parse, or fails any check below counts as ignored.
// Events are of one entrypoint, so the oracle's sort by entrypoint then event is by event, in byte order,
// which is localeCompare's order for the [a-z][a-z0-9-]* slugs an event is.
func ReadHookObservations(q ObservationQuery) HookObservations {
	result := HookObservations{Observations: []HookObservation{}}
	if !metadata(q.SessionID) || (q.AgentID != nil && !metadata(*q.AgentID)) {
		result.Reason = "session/actor identity unavailable"
		return result
	}
	if q.MaxAgeMS < 0 {
		result.Reason = "invalid freshness filter"
		return result
	}
	root, version, manifest, inside, ok := observationPayload(q.PluginRoot)
	if !ok {
		result.Reason = "payload or invocation store unreadable"
		return result
	}
	agent := "null"
	if q.AgentID != nil {
		agent = jsString(*q.AgentID)
	}
	dir := filepath.Join(q.CodexHome, "crw", "hook-observations", observationDigest([]byte(q.SessionID)), observationDigest([]byte(agent)))
	names, err := os.ReadDir(dir)
	if err != nil {
		result.Reason = "invocation store unreadable"
		if errors.Is(err, fs.ErrNotExist) {
			result.Reason = "no invocation records"
		}
		return result
	}
	for _, entry := range names {
		if !observationSlotName(entry.Name()) {
			continue
		}
		observed, ok := observationRecord(filepath.Join(dir, entry.Name()), q, root, version, manifest, inside)
		if !ok {
			result.Ignored++
			continue
		}
		result.Observations = append(result.Observations, observed)
	}
	sort.SliceStable(result.Observations, func(i, j int) bool { return result.Observations[i].Event < result.Observations[j].Event })
	return result
}

func observationDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// observationSlotName is /^[a-f0-9]{64}\.json$/ (:106); any other name in the directory is not a record
// and is not counted.
func observationSlotName(name string) bool {
	hash, found := strings.CutSuffix(name, ".json")
	if !found || len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// observationRead is readBounded (:20-24) as the opened handle sees it: a link at the last element is
// refused (O_NOFOLLOW), a pipe does not block the open (O_NONBLOCK), the type is that of the file that
// was opened, and the read stops one byte past the bound. A file swapped in after its name was listed is
// not followed, and one that grows is not read whole.
func observationRead(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("invalid observation input file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = errors.New("invalid observation input file")
	}
	return data, err
}

// observationPayload is the realpathSync and payloadVersion of the oracle (:37-42, :97-98): the real plugin
// root, the manifest's version and the digest of its bytes, and whether the manifest's real path lies inside
// that root (the oracle's check of the entrypoint, :49, which here names the manifest).
func observationPayload(pluginRoot string) (root, version, digest string, inside, ok bool) {
	root, err := filepath.Abs(pluginRoot)
	if err != nil {
		return "", "", "", false, false
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return "", "", "", false, false
	}
	path := filepath.Join(root, Entrypoint)
	data, err := observationRead(path, MaxStdinBytes)
	if err != nil {
		return "", "", "", false, false
	}
	parsed, _ := decode(string(data))
	object, _ := parsed.(map[string]any)
	version, _ = object["version"].(string)
	if !metadata(version) {
		return "", "", "", false, false
	}
	real, err := filepath.EvalSymlinks(path)
	return root, version, observationDigest(data), err == nil && strings.HasPrefix(real, root+string(filepath.Separator)), true
}

// observationRecord is the body of the oracle's per-record try (:108-121): a record that does not match
// this plugin, session, actor and clock, or whose file is not the slot its fields name, is not evidence.
func observationRecord(path string, q ObservationQuery, root, version, manifest string, inside bool) (HookObservation, bool) {
	data, err := observationRead(path, observationMaxRecord)
	if err != nil {
		return HookObservation{}, false
	}
	parsed, _ := decode(string(data))
	record, ok := parsed.(map[string]any)
	if !ok {
		return HookObservation{}, false
	}
	component, _ := record["component"].(string)
	event, _ := record["event"].(string)
	entrypoint, _ := record["entrypoint"].(string)
	observedAt, _ := record["observedAt"].(string)
	agent, hasAgent := record["agentId"]
	if schema, _ := record["schemaVersion"].(json.Number); !isOne(schema) || record["outcome"] != "invoked" || record["sessionId"] != q.SessionID ||
		!hasAgent || (q.AgentID == nil && agent != nil) || (q.AgentID != nil && agent != *q.AgentID) ||
		record["pluginRoot"] != root || record["pluginVersion"] != version || record["manifestDigest"] != manifest ||
		!slug(component) || !slug(event) {
		return HookObservation{}, false
	}
	// Date.parse reads many spellings; the writer's, and any RFC 3339 one, is read here.
	at, err := time.Parse(time.RFC3339Nano, observedAt)
	if err != nil {
		return HookObservation{}, false
	}
	if when := at.UnixMilli(); when > q.NowMS || q.NowMS-when > q.MaxAgeMS {
		return HookObservation{}, false
	}
	slot := observationDigest([]byte("["+jsString(component)+","+jsString(event)+","+jsString(entrypoint)+"]")) + ".json"
	if entrypoint != Entrypoint || !inside || record["entrypointDigest"] != manifest || filepath.Base(path) != slot {
		return HookObservation{}, false
	}
	return HookObservation{SessionID: q.SessionID, AgentID: q.AgentID, Component: component, Event: event, ObservedAt: observedAt, Entrypoint: entrypoint, Outcome: "invoked"}, true
}

// isOne is schemaVersion === 1 for a JSON number: 1, 1.0 and 1e0 are all the number one.
func isOne(n json.Number) bool {
	f, err := strconv.ParseFloat(string(n), 64)
	return n != "" && err == nil && f == 1
}
