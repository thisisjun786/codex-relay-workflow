package store

import (
	"encoding/json"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

var lowerDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var eventDigest = regexp.MustCompile(`^[0-9a-f]{32}$`)

// NoDeliverable is the revision hash every execution-only receipt carries.
var NoDeliverable = strings.Repeat("0", 64)

type TurnReference struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Status   string `json:"turnStatus"`
}

type ManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  *int64 `json:"bytes,omitempty"`
}

// ReceiptClaim is a completion receipt whose structure has been checked once at the boundary.
// It keeps the decoded document so the stored receipt is Python's json.dumps of the same dict.
type ReceiptClaim struct {
	EventID        string
	RelationshipID string
	Generation     int64
	Attempt        *int64
	bigGeneration  *big.Int
	bigAttempt     *big.Int
	RevisionHash   string
	Outcome        ObservationOutcome
	Producer       string
	Turn           TurnReference
	ManifestRef    *string
	manifest       any
	document       any
}

var receiptFields = []string{"eventId", "relationshipId", "executionGeneration", "attempt", "revisionHash", "outcome", "producer", "turnRef", "criteria", "emittedAt", "manifest", "manifestRef"}
var requiredReceiptFields = []string{"emittedAt", "eventId", "executionGeneration", "outcome", "producer", "relationshipId", "revisionHash", "turnRef"}

// pythonStr is str(value) for the scalars a regex check can see.
func pythonStr(v any) string {
	if text, ok := v.(string); ok {
		return text
	}
	if number, ok := jsonInteger(v); ok {
		return strconv.FormatInt(number, 10)
	}
	return ""
}

func nonBlank(v any) (string, bool) {
	text, ok := v.(string)
	return text, ok && strings.TrimSpace(text) != ""
}

// ParseReceipt is ReceiptIntake._validate_shape: structure before meaning, so a malformed field
// becomes a malformed_receipt refusal rather than a crash.
func ParseReceipt(data []byte) (ReceiptClaim, error) {
	document, err := decodeOrdered(data)
	object, isObject := document.(pyjson.Object)
	if err != nil || !isObject {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "a receipt is a JSON object")
	}
	var unknown, missing []string
	for _, f := range object {
		if !slices.Contains(receiptFields, f.Key) {
			unknown = append(unknown, f.Key)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "unknown fields %q", unknown)
	}
	for _, key := range requiredReceiptFields {
		if _, ok := object.Lookup(key); !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "missing fields %q", missing)
	}
	get := object.Get
	claim := ReceiptClaim{document: document, manifest: get("manifest")}
	claim.EventID = pythonStr(get("eventId"))
	if !eventDigest.MatchString(claim.EventID) {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "eventId must be 32 lowercase hex characters")
	}
	var ok bool
	if claim.RelationshipID, ok = get("relationshipId").(string); !ok || claim.RelationshipID == "" {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "relationshipId must be a string")
	}
	generation := receiptInteger(get("executionGeneration"))
	if generation == nil || generation.Sign() < 1 {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "executionGeneration must be a positive integer")
	}
	if generation.IsInt64() {
		claim.Generation = generation.Int64()
	} else {
		claim.bigGeneration = generation
	}
	if claim.RevisionHash = pythonStr(get("revisionHash")); !lowerDigest.MatchString(claim.RevisionHash) {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "revisionHash must be 64 lowercase hex characters")
	}
	outcome, _ := get("outcome").(string)
	if claim.Outcome = ObservationOutcome(outcome); !slices.Contains(receiptOutcomes, claim.Outcome) {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "unknown outcome %q", outcome)
	}
	if claim.Producer, _ = get("producer").(string); claim.Producer != ProducerChild && claim.Producer != ProducerDaemon {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "unknown producer %q", claim.Producer)
	}
	if _, ok := nonBlank(get("emittedAt")); !ok {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "emittedAt must be a timestamp")
	}
	if attempt, present := object.Lookup("attempt"); present && attempt != nil {
		n := receiptInteger(attempt)
		if n == nil || n.Sign() < 1 {
			return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "attempt must be a positive integer or null")
		}
		number := n.Int64()
		claim.Attempt = &number
		if !n.IsInt64() {
			claim.bigAttempt = n
		}
	}
	if reference, present := object.Lookup("manifestRef"); present && reference != nil {
		text, ok := nonBlank(reference)
		if !ok {
			return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "manifestRef must be a path")
		}
		claim.ManifestRef = &text
	}
	turn, err := parseTurnRef(get("turnRef"))
	if err != nil {
		return ReceiptClaim{}, err
	}
	claim.Turn = turn
	return claim, nil
}

func receiptInteger(v any) *big.Int {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return nil
	}
	value, _ := new(big.Int).SetString(string(n), 10)
	return value
}

func parseTurnRef(turn any) (TurnReference, error) {
	object, isObject := turn.(pyjson.Object)
	if !isObject {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "turnRef must be an object")
	}
	keys := make([]string, 0, len(object))
	for _, f := range object {
		keys = append(keys, f.Key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"threadId", "turnId", "turnStatus"}) {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "turnRef has fields %q", keys)
	}
	var ref TurnReference
	var ok bool
	if ref.ThreadID, ok = nonBlank(object.Get("threadId")); !ok {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "turnRef.threadId must be a non-empty string")
	}
	if ref.TurnID, ok = nonBlank(object.Get("turnId")); !ok {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "turnRef.turnId must be a non-empty string")
	}
	ref.Status, _ = object.Get("turnStatus").(string)
	if !slices.Contains([]string{"completed", "interrupted", "failed", "inProgress"}, ref.Status) {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "unknown turnStatus %q", ref.Status)
	}
	return ref, nil
}

// validateManifest is ReceiptIntake._validate_manifest.
func validateManifest(records []any) ([]ManifestEntry, error) {
	entries := make([]ManifestEntry, 0, len(records))
	for index, record := range records {
		object, isObject := record.(pyjson.Object)
		if !isObject {
			return nil, refuse(ReasonMalformedReceipt, "manifest[%d] must be an object", index)
		}
		for _, f := range object {
			if f.Key != "path" && f.Key != "sha256" && f.Key != "bytes" {
				return nil, refuse(ReasonMalformedReceipt, "manifest[%d] has unknown fields", index)
			}
		}
		pathValue, hasPath := object.Lookup("path")
		digestValue, hasDigest := object.Lookup("sha256")
		if !hasPath || !hasDigest {
			return nil, refuse(ReasonMalformedReceipt, "manifest[%d] is missing path or sha256", index)
		}
		declared, ok := pathValue.(string)
		if !ok || !strings.HasPrefix(declared, "/") {
			return nil, refuse(ReasonMalformedReceipt, "manifest[%d].path must be an absolute path", index)
		}
		entry := ManifestEntry{Path: declared, SHA256: pythonStr(digestValue)}
		if !lowerDigest.MatchString(entry.SHA256) {
			return nil, refuse(ReasonMalformedReceipt, "manifest[%d].sha256 must be 64 lowercase hex characters", index)
		}
		if size, present := object.Lookup("bytes"); present && size != nil {
			count, ok := jsonInteger(size)
			if !ok || count < 0 {
				return nil, refuse(ReasonMalformedReceipt, "manifest[%d].bytes must be a non-negative integer or null", index)
			}
			entry.Bytes = &count
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
