//go:build dev

package cxcfuzz

import (
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// stateSessionID is the session every generated case writes and rewrites.
const stateSessionID = "s"

// stateTarget is the PABCD session state read and rewrite (CRW-708): state.ReadStateStrict and
// state.WriteState against the oracle readStateStrict and writeState at CXC v0.2.40
// (pabcd-state/dist/state.js:494, :620). The case writes the same session bytes under the port own
// root (.crw/sessions/s.json) and the oracle own root (.codexclaw/sessions/s.json); each side reads
// its own, rewrites it, and reports the unreadable verdict, the rebuilt state and the rewritten file.
func stateTarget() Target {
	return Target{
		Name:     "state",
		Generate: stateGenerate,
		Go:       stateGo,
		Oracle:   Oracle{Command: "node", Shim: shimPath("state"), Root: DefaultOracleRoot},
		Compare:  stateCompare,
	}
}

// stateAnswer is what both sides answer for one case.
func stateAnswer(unreadable bool, s state.State, written string, writeErr error) any {
	body := pyjson.Object{
		{Key: "unreadable", Value: unreadable},
		{Key: "state", Value: stateText(s)},
	}
	if writeErr != nil {
		return body.Set("writeError", writeErr.Error())
	}
	return body.Set("written", written)
}

// stateText is the rebuilt state as JSON text, the same shape the oracle prints. Encode is
// JSON.stringify(s, null, 2); its error case (a hand-set NaN counter) falls back to the empty text.
func stateText(s state.State) string {
	encoded, err := state.Encode(s)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// stateGo is the Go side: ReadStateStrict then WriteState, then the rewritten file bytes. A refused
// write is reported as writeError, which is an intentionally-changed candidate when it stops a loss.
func stateGo(input any, env Env) (any, error) {
	s, unreadable := state.ReadStateStrict(env.Root, stateSessionID)
	if err := state.WriteState(env.Root, s); err != nil {
		return maskTimestamps(stateAnswer(unreadable, s, "", err)), nil
	}
	written, err := os.ReadFile(state.StatePath(env.Root, stateSessionID))
	if err != nil {
		return maskTimestamps(stateAnswer(unreadable, s, "", err)), nil
	}
	return maskTimestamps(stateAnswer(unreadable, s, string(written), nil)), nil
}

// timestampText is an ISO-8601 instant with milliseconds, the shape a defaulted or stamped updatedAt
// takes. Both sides stamp the wall clock, so the two values can never match; each is rewritten to one
// placeholder before comparison. A persisted timestamp is identical on both sides, so masking it too
// hides nothing.
var timestampText = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z`)

// timestampPlaceholder stands where a write timestamp was. It holds no regexp metacharacter, because
// ReplaceAllString reads $ in the replacement as a group reference (and ${TS} would expand to
// nothing); the shim uses the same literal.
const timestampPlaceholder = "@TS@"

func maskTimestamps(value any) any {
	switch v := value.(type) {
	case string:
		return timestampText.ReplaceAllString(v, timestampPlaceholder)
	case pyjson.Object:
		out := make(pyjson.Object, 0, len(v))
		for _, item := range v {
			out = append(out, pyjson.Field{Key: item.Key, Value: maskTimestamps(item.Value)})
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, maskTimestamps(item))
		}
		return out
	default:
		return value
	}
}

// stateCompare compares the answers. A rewritten file that drops a key the oracle kept is a data-loss
// differ and is named first in the detail, as the issue asks. Both sides mask their write timestamps
// before answering, so the comparison is a plain canonical one.
func stateCompare(goOut, oracleOut any) Verdict {
	if canonical(goOut) == canonical(oracleOut) {
		return Verdict{Kind: Same}
	}
	if lost := stateLostKeys(goOut, oracleOut); len(lost) > 0 {
		return Verdict{Kind: Differ, Detail: "data-loss: the Go rewrite drops " + strings.Join(lost, ", ")}
	}
	return Verdict{Kind: Differ, Detail: "the read state or the rewritten bytes differ"}
}

// stateLostKeys names the keys of the oracle written state that the Go written state lacks, sorted.
func stateLostKeys(goOut, oracleOut any) []string {
	goWritten := stateWrittenKeys(goOut)
	oracleWritten := stateWrittenKeys(oracleOut)
	if goWritten == nil || oracleWritten == nil {
		return nil
	}
	lost := []string{}
	for key := range oracleWritten {
		if !goWritten[key] {
			lost = append(lost, key)
		}
	}
	sort.Strings(lost)
	return lost
}

// stateWrittenKeys parses one answer written field as a JSON object and returns its keys, or nil.
func stateWrittenKeys(out any) map[string]bool {
	text, _ := field(out, "written")
	raw, ok := text.(string)
	if !ok || raw == "" {
		return nil
	}
	value, err := decode(raw)
	if err != nil {
		return nil
	}
	keys := map[string]bool{}
	for _, item := range pyjsonFields(value) {
		keys[item] = true
	}
	return keys
}

func pyjsonFields(value any) []string {
	obj, ok := value.(pyjson.Object)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(obj))
	for _, item := range obj {
		out = append(out, item.Key)
	}
	return out
}

// stateTexts are the session documents the issue names: valid states and the mutations a stored file
// can carry, plus broken JSON and an empty file.
func stateTexts(rng *rand.Rand) string {
	switch rng.Intn(14) {
	case 0:
		return "{"
	case 1:
		return ""
	case 2:
		return " \n"
	case 3:
		return `{"phase":"P"} x`
	case 4:
		return "\uFEFF" + `{"phase":"P"}`
	case 5:
		return stateUnverified(rng)
	case 6:
		return stateInterview(rng)
	case 7:
		return stateMarker(rng)
	case 8:
		return stateNumbers(rng)
	case 9:
		return stateLoneSurrogate(rng)
	default:
		return stateValid(rng)
	}
}

// stateValid is a plausible session file with a random phase and a random mix of the persisted fields.
func stateValid(rng *rand.Rand) string {
	phases := []string{"IDLE", "I", "P", "A", "B", "C", "D"}
	fields := []string{
		`"phase": ` + strconv.Quote(phases[rng.Intn(len(phases))]),
		`"slug": "my-slug"`,
		`"updatedAt": "2025-05-05T05:05:05.000Z"`,
		`"flags": {"auditPassed": true, "checkPassed": false}`,
		`"supersededBy": "next"`,
		`"injectedTurns": ["a", "b"]`,
		`"lastInjectedPhase": "B"`,
		`"orchestrationActive": true`,
		`"stopBlockPhase": "P"`,
		`"stopBlockCount": 3`,
		`"stopMetricCursor": 12`,
		`"stopBlockTotal": 4`,
		`"unknownKey": {"a": 1}`,
		`"phase": null`,
		`"slug": 5`,
	}
	parts := []string{}
	for i := 0; i <= rng.Intn(4); i++ {
		parts = append(parts, fields[rng.Intn(len(fields))])
	}
	if len(parts) == 0 {
		parts = append(parts, `"phase": "P"`)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// stateUnverified is a state whose unverified-subagent list sits at the retention cap the issue names
// (63 to 66 entries), with a receiptClaimed near its 256-unit cut and a lone surrogate among them.
func stateUnverified(rng *rand.Rand) string {
	count := 63 + rng.Intn(4)
	entries := make([]string, 0, count)
	for i := 0; i < count; i++ {
		claim := "r"
		switch rng.Intn(4) {
		case 0:
			claim = strings.Repeat("\u00e9", 255+rng.Intn(3))
		case 1:
			claim = strings.Repeat("a", 254) + "\\ud83d\\ude00" + "b"
		case 2:
			claim = "\\ud800"
		}
		entries = append(entries, `{"agentId": "a`+strconv.Itoa(i)+`", "turnId": "t", "agentType": "executor", "attempts": 3, "receiptClaimed": "`+claim+`", "recordedAt": "2026-01-01T00:00:00Z", "resolvable": true}`)
	}
	return `{"phase": "P", "unverifiedSubagents": [` + strings.Join(entries, ", ") + `]}`
}

// stateInterview is a state whose interview tracker sits near the 49-to-51 cap the issue names.
func stateInterview(rng *rand.Rand) string {
	count := 49 + rng.Intn(3)
	contradictions := make([]string, 0, count)
	for i := 0; i < count; i++ {
		contradictions = append(contradictions, `{"id": "c`+strconv.Itoa(i)+`", "text": "x", "recorded": true}`)
	}
	dims := `{"goal": {"level": "max", "known": ["k"], "unknown": [], "confidence": 1}, "constraint": {"level": "max", "known": [], "unknown": [], "confidence": 1}, "success": {"level": "max", "known": [], "unknown": [], "confidence": 1}, "ontology": {"level": "max", "known": [], "unknown": [], "confidence": 1}}`
	return `{"phase": "P", "interview": {"roundId": 2, "dimensions": ` + dims + `, "contradictions": [` + strings.Join(contradictions, ", ") + `], "assumptions": [], "scanRounds": 1, "lastScanRoundId": 1}}`
}

// stateMarker is a legacy D-close marker, in the shapes the issue names (a missing, null, empty,
// array or object successor, and the stored legacy flag the port keeps).
func stateMarker(rng *rand.Rand) string {
	next := []string{`"nextWorkPhaseId": "wp2"`, `"nextWorkPhaseId": null`, `"nextWorkPhaseId": ""`, `"nextWorkPhaseId": []`, `"nextWorkPhaseId": {}`, `"legacy": true`, ``}
	marker := `{"sessionId": "s", "checkEpoch": "c1", "closedWorkPhaseId": "wp1"`
	if extra := next[rng.Intn(len(next))]; extra != "" {
		marker += ", " + extra
	}
	marker += "}"
	return `{"phase": "IDLE", "checkEpoch": "c1", "dcloseRecovery": ` + marker + `}`
}

// stateNumbers is a state whose counters use the exponent and large-integer spellings the issue names.
func stateNumbers(rng *rand.Rand) string {
	forms := []string{"1e999", "2.9E0", "-0", "9007199254740991", "9007199254740993", "1e21", "3.7", "-1", `"5"`, "null", "true"}
	pick := func() string { return forms[rng.Intn(len(forms))] }
	return `{"phase": "P", "stopBlockCount": ` + pick() + `, "stopMetricCursor": ` + pick() + `, "stopBlockTotal": ` + pick() + `, "idleEditNudges": ` + pick() + `}`
}

// stateLoneSurrogate is a state holding a lone surrogate escape, which the oracle str keeps and the
// port holds as the three WTF-8 bytes.
func stateLoneSurrogate(rng *rand.Rand) string {
	values := []string{`"\ud800"`, `"a\ud800b"`, `"\udfff"`, `"\ud83d\ude00"`}
	return `{"phase": "P", "slug": ` + values[rng.Intn(len(values))] + `, "supersededBy": ` + values[rng.Intn(len(values))] + `}`
}

// stateGenerate builds one case: the session bytes at the Go side's own path, .crw/sessions/s.json.
// The document is stored once: the shim mirrors it to the oracle's .codexclaw path, so the shrinker
// cannot drop one copy and leave the two sides reading different documents.
func stateGenerate(rng *rand.Rand, size int) any {
	text := stateTexts(rng)
	return pyjson.Object{{Key: "fs", Value: []any{
		pyjson.Object{{Key: "path", Value: filepath.Join(".crw", "sessions", stateSessionID+".json")}, {Key: "kind", Value: "file"}, {Key: "content", Value: text}},
	}}}
}
