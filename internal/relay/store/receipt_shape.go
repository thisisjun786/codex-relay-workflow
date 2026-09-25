package store

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	RevisionHash   string
	Outcome        ObservationOutcome
	Producer       string
	Turn           TurnReference
	ManifestRef    *string
	manifest       jsonValue
	document       jsonValue
}

var receiptFields = []string{"eventId", "relationshipId", "executionGeneration", "attempt", "revisionHash", "outcome", "producer", "turnRef", "criteria", "emittedAt", "manifest", "manifestRef"}
var requiredReceiptFields = []string{"emittedAt", "eventId", "executionGeneration", "outcome", "producer", "relationshipId", "revisionHash", "turnRef"}

// pythonStr is str(value) for the scalars a regex check can see.
func pythonStr(v jsonValue) string {
	if text, ok := v.text(); ok {
		return text
	}
	if number, ok := v.integer(); ok {
		return strconv.FormatInt(number, 10)
	}
	return ""
}

func nonBlank(v jsonValue) (string, bool) {
	text, ok := v.text()
	return text, ok && strings.TrimSpace(text) != ""
}

// ParseReceipt is ReceiptIntake._validate_shape: structure before meaning, so a malformed field
// becomes a malformed_receipt refusal rather than a crash.
func ParseReceipt(data []byte) (ReceiptClaim, error) {
	document, err := decodeOrdered(data)
	if err != nil || document.kind != jsonObject {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "a receipt is a JSON object")
	}
	var unknown, missing []string
	for _, f := range document.object {
		if !slices.Contains(receiptFields, f.key) {
			unknown = append(unknown, f.key)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "unknown fields %q", unknown)
	}
	for _, key := range requiredReceiptFields {
		if _, ok := document.field(key); !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "missing fields %q", missing)
	}
	get := func(key string) jsonValue { v, _ := document.field(key); return v }
	claim := ReceiptClaim{document: document, manifest: get("manifest")}
	claim.EventID = pythonStr(get("eventId"))
	if !eventDigest.MatchString(claim.EventID) {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "eventId must be 32 lowercase hex characters")
	}
	var ok bool
	if claim.RelationshipID, ok = get("relationshipId").text(); !ok || claim.RelationshipID == "" {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "relationshipId must be a string")
	}
	if claim.Generation, ok = get("executionGeneration").integer(); !ok || claim.Generation < 1 {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "executionGeneration must be a positive integer")
	}
	if claim.RevisionHash = pythonStr(get("revisionHash")); !lowerDigest.MatchString(claim.RevisionHash) {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "revisionHash must be 64 lowercase hex characters")
	}
	outcome, _ := get("outcome").text()
	if claim.Outcome = ObservationOutcome(outcome); !slices.Contains(receiptOutcomes, claim.Outcome) {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "unknown outcome %q", outcome)
	}
	if claim.Producer, _ = get("producer").text(); claim.Producer != ProducerChild && claim.Producer != ProducerDaemon {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "unknown producer %q", claim.Producer)
	}
	if _, ok := nonBlank(get("emittedAt")); !ok {
		return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "emittedAt must be a timestamp")
	}
	if attempt, present := document.field("attempt"); present && !attempt.isNull() {
		number, ok := attempt.integer()
		if !ok || number < 1 {
			return ReceiptClaim{}, refuse(ReasonMalformedReceipt, "attempt must be a positive integer or null")
		}
		claim.Attempt = &number
	}
	if reference, present := document.field("manifestRef"); present && !reference.isNull() {
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

func parseTurnRef(turn jsonValue) (TurnReference, error) {
	if turn.kind != jsonObject {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "turnRef must be an object")
	}
	keys := make([]string, 0, len(turn.object))
	for _, f := range turn.object {
		keys = append(keys, f.key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"threadId", "turnId", "turnStatus"}) {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "turnRef has fields %q", keys)
	}
	var ref TurnReference
	var ok bool
	thread, _ := turn.field("threadId")
	if ref.ThreadID, ok = nonBlank(thread); !ok {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "turnRef.threadId must be a non-empty string")
	}
	id, _ := turn.field("turnId")
	if ref.TurnID, ok = nonBlank(id); !ok {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "turnRef.turnId must be a non-empty string")
	}
	status, _ := turn.field("turnStatus")
	ref.Status, _ = status.text()
	if !slices.Contains([]string{"completed", "interrupted", "failed", "inProgress"}, ref.Status) {
		return TurnReference{}, refuse(ReasonMalformedReceipt, "unknown turnStatus %q", ref.Status)
	}
	return ref, nil
}

// validateManifest is ReceiptIntake._validate_manifest.
func validateManifest(records []jsonValue) ([]ManifestEntry, error) {
	entries := make([]ManifestEntry, 0, len(records))
	for index, record := range records {
		if record.kind != jsonObject {
			return nil, refuse(ReasonMalformedReceipt, "manifest[%d] must be an object", index)
		}
		for _, f := range record.object {
			if f.key != "path" && f.key != "sha256" && f.key != "bytes" {
				return nil, refuse(ReasonMalformedReceipt, "manifest[%d] has unknown fields", index)
			}
		}
		pathValue, hasPath := record.field("path")
		digestValue, hasDigest := record.field("sha256")
		if !hasPath || !hasDigest {
			return nil, refuse(ReasonMalformedReceipt, "manifest[%d] is missing path or sha256", index)
		}
		declared, ok := pathValue.text()
		if !ok || !strings.HasPrefix(declared, "/") {
			return nil, refuse(ReasonMalformedReceipt, "manifest[%d].path must be an absolute path", index)
		}
		entry := ManifestEntry{Path: declared, SHA256: pythonStr(digestValue)}
		if !lowerDigest.MatchString(entry.SHA256) {
			return nil, refuse(ReasonMalformedReceipt, "manifest[%d].sha256 must be 64 lowercase hex characters", index)
		}
		if size, present := record.field("bytes"); present && !size.isNull() {
			count, ok := size.integer()
			if !ok || count < 0 {
				return nil, refuse(ReasonMalformedReceipt, "manifest[%d].bytes must be a non-negative integer or null", index)
			}
			entry.Bytes = &count
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
