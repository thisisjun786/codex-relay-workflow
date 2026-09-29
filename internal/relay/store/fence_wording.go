package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// MirrorRefusal is ownership.mirror's refusal for takeover.json bytes that are present:
// "" when json.loads reads them as an object, otherwise the OwnershipRefused detail. It is
// the one reading of the mirror's bytes behind doctor's ownership block and its probe.
func MirrorRefusal(raw []byte) string {
	if _, err := DecodeUTF8(raw); err != nil {
		return "takeover record unreadable: UnicodeDecodeError: " + err.Error()
	}
	if message := PythonJSONError(string(raw)); message != "" {
		return "takeover record unreadable: JSONDecodeError: " + message
	}
	// The document is valid JSON, so its first non-whitespace byte names its type.
	if !strings.HasPrefix(strings.TrimLeft(string(raw), " \t\n\r"), "{") {
		return "takeover record is not an object"
	}
	return ""
}

// fenceRefusal is the detail ownership.py validate(path, meta, record) refuses with, worded
// for this runtime as the admitted owner (no candidate, no admitted epoch): "" when validate
// admits, or when a value cannot be read as Python reads it (the caller keeps its own words).
// It words a refusal; whether the store is refused stays the admission preflight's decision.
func fenceRefusal(resolved string, meta map[string]string, raw []byte) string {
	var record map[string]any
	if raw != nil {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&record) != nil {
			return ""
		}
		if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
			return ""
		}
	}
	protocol, isInt := pyInt(record["protocol"])
	if meta["writer_protocol"] != "1" || len(record) == 0 || !isInt || protocol.Cmp(big.NewInt(1)) != 0 {
		return "missing or unsupported writer protocol"
	}
	for _, key := range []string{"protocol", "storeId", "database", "appServerSocket", "scopeKey", "epoch", "owner", "phase", "transition", "holder", "controller", "rollbackAllowed", "pythonCompatibilityBuild", "relayRPCSocket", "updatedAt"} {
		if _, present := record[key]; !present {
			return "incomplete ownership record"
		}
	}
	for _, key := range ownership.Keys {
		if _, present := meta[key]; !present {
			return "incomplete ownership record"
		}
	}
	epoch, epochIsInt := pyInt(record["epoch"])
	if _, isBool := record["rollbackAllowed"].(bool); !epochIsInt || !isBool {
		return "mistyped ownership record"
	}
	phase, _ := record["phase"].(string)
	if phase != "active" && phase != "draining" && phase != "starting" {
		return "invalid takeover phase"
	}
	if socket, _ := record["relayRPCSocket"].(string); socket != filepath.Join(filepath.Dir(resolved), "control.sock") {
		return "the control socket belongs to another state directory"
	}
	if transition := record["transition"]; transition != nil {
		object, isObject := transition.(map[string]any)
		if !isObject || !pyStringIs(object["id"], meta["takeover_id"], true) && phase != "draining" {
			return "ownership transition disagrees"
		}
	}
	durable := meta["owner_epoch"]
	for _, r := range durable {
		if r < '0' || r > '9' {
			if r > 0x7f {
				return "" // str.isdecimal accepts other scripts' digits; not read here.
			}
			return "invalid ownership epoch"
		}
	}
	durableEpoch, parsed := new(big.Int).SetString(durable, 10)
	if !parsed || durableEpoch.Sign() < 1 {
		return "invalid ownership epoch"
	}
	if owner := meta["owner"]; owner != "python" && owner != "go" || meta["rollback_allowed"] != "0" && meta["rollback_allowed"] != "1" {
		return "invalid durable ownership record"
	}
	physical, err := ownership.Physical(resolved)
	if err != nil {
		return ""
	}
	storeID, hasStoreID := meta["store_id"]
	disagrees := !pyStringIs(record["owner"], meta["owner"], true) || epoch.Cmp(durableEpoch) != 0 ||
		!pyStringIs(record["storeId"], storeID, hasStoreID) || !pyDatabaseIs(record["database"], physical) ||
		!pyStringIs(record["pythonCompatibilityBuild"], meta["python_compatibility_build"], true) ||
		record["rollbackAllowed"] != (meta["rollback_allowed"] == "1")
	if disagrees {
		return "ownership record disagrees with the durable store"
	}
	if meta["python_compatibility_build"] != ownership.PythonBuild {
		return "unsupported Python compatibility build"
	}
	if meta["owner"] != "go" {
		return "the relay store belongs to another runtime"
	}
	switch phase {
	case "starting":
		return "only the designated candidate may enter starting"
	case "draining":
		return "the relay store is draining"
	}
	return ""
}

// pyInt is a JSON number json.loads reads as an int (no fraction, no exponent), with its value.
func pyInt(value any) (*big.Int, bool) {
	number, ok := value.(json.Number)
	if !ok || strings.ContainsAny(string(number), ".eE") {
		return nil, false
	}
	n, ok := new(big.Int).SetString(string(number), 10)
	return n, ok
}

// pyStringIs is record.get(key) == value for a str value, or for None when absent.
func pyStringIs(value any, want string, present bool) bool {
	if !present {
		return value == nil
	}
	got, ok := value.(string)
	return ok && got == want
}

// pyDatabaseIs is record.get("database") == physical(path): a dict with exactly physical's
// six keys, whose numbers compare by value as Python's do (1.0 == 1, True == 1).
func pyDatabaseIs(value any, physical ownership.Database) bool {
	object, ok := value.(map[string]any)
	if !ok || len(object) != 6 {
		return false
	}
	numbers := map[string]uint64{"device": physical.Device, "inode": physical.Inode, "walDirectoryDevice": physical.WALDirectoryDevice, "walDirectoryInode": physical.WALDirectoryInode}
	for key, want := range numbers {
		if !pyNumberIs(object[key], want) {
			return false
		}
	}
	return pyStringIs(object["realPath"], physical.RealPath, true) && pyStringIs(object["walBasename"], physical.WALBasename, true)
}

func pyNumberIs(value any, want uint64) bool {
	expected := new(big.Int).SetUint64(want)
	switch v := value.(type) {
	case bool:
		return v && want == 1 || !v && want == 0
	case json.Number:
		if n, isInt := pyInt(v); isInt {
			return n.Cmp(expected) == 0
		}
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			return false
		}
		return big.NewFloat(f).Cmp(new(big.Float).SetInt(expected)) == 0
	}
	return false
}
