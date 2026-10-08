//go:build dev

package cxcfuzz

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	raw, _ := os.ReadFile(state.StatePath(env.Root, stateSessionID))
	s, unreadable := state.ReadStateStrict(env.Root, stateSessionID)
	from := time.Now()
	if err := state.WriteState(env.Root, s); err != nil {
		return maskTimestamps(stateAnswer(unreadable, s, "", err), readDefaultedUpdatedAt(raw)), nil
	}
	written, err := os.ReadFile(state.StatePath(env.Root, stateSessionID))
	if err != nil {
		return maskTimestamps(stateAnswer(unreadable, s, "", err), readDefaultedUpdatedAt(raw)), nil
	}
	return maskTimestamps(stateAnswer(unreadable, s, maskWrittenStamp(string(written), from, time.Now()), nil), readDefaultedUpdatedAt(raw)), nil
}

// isTimestampText reports whether text is an ISO-8601 instant with milliseconds, the shape a
// wall-clock stamp takes. Only a value of exactly this shape under a write-stamped key is masked;
// every other timestamp-shaped value (a persisted updatedAt, a recordedAt, a capturedAt, a review
// round's openedAt, a plan's finalGate.updatedAt) is compared as stored, because masking it by shape
// would hide a persisted timestamp the port rewrites to another instant — exactly the difference this
// target exists to catch (CRW-708 generation 3, c8). The mask is by key, at the document's top level,
// not by text shape.
//
// It is a hand-written matcher rather than a compiled regexp, and in particular not a package-level
// `var timestampText = regexp.MustCompile(...)`: that initializer runs in every dev binary that
// imports this package, whether or not it fuzzes anything, and this package must do no work at
// program start (c4; CRW-708 generation 5, d4).
func isTimestampText(text string) bool {
	// The reference layout is the whole matcher: a digit in it stands for any digit and every other
	// byte stands for itself, which is exactly the pattern the mask needs.
	const layout = "2006-01-02T15:04:05.000Z"
	if len(text) != len(layout) {
		return false
	}
	for i := 0; i < len(layout); i++ {
		if layout[i] >= '0' && layout[i] <= '9' {
			if text[i] < '0' || text[i] > '9' {
				return false
			}
			continue
		}
		if text[i] != layout[i] {
			return false
		}
	}
	return true
}

// timestampPlaceholder stands where a write timestamp was. It holds no regexp metacharacter, because
// ReplaceAllString reads $ in the replacement as a group reference (and ${TS} would expand to
// nothing); the shim uses the same literal.
const timestampPlaceholder = "@TS@"

// maskTimestamps masks an answer's document texts. "state" (a state answer) and "plan" (a plan
// answer) are the form the reader rebuilt, and "written" the form the writer published. The write
// always restamps the document's updatedAt from its own wall clock, so the written form's updatedAt
// is always masked. The read form's updatedAt is masked only when maskRead says the two sides' readers
// can disagree there (a state reader whose source carried no updatedAt string defaults it from the
// clock, so the two sides hold different instants); a value both sides keep as stored is never masked,
// so a port that drops or rewrites a persisted updatedAt is a difference rather than a Same. createdAt
// is never masked: the only wall-clock createdAt (buildGoalplan) is on neither path these targets
// drive, so it is stored data on both sides.
func maskTimestamps(answer any, maskRead bool) any {
	object, ok := answer.(pyjson.Object)
	if !ok {
		return answer
	}
	out := make(pyjson.Object, 0, len(object))
	for _, item := range object {
		switch item.Key {
		case "state", "plan":
			out = append(out, pyjson.Field{Key: item.Key, Value: maskDocumentText(item.Value, maskRead)})
		default:
			out = append(out, item)
		}
	}
	return out
}

// maskDocumentText masks the document text when stamp is true; a value that is not text is returned
// as it is.
func maskDocumentText(value any, stamp bool) any {
	text, ok := value.(string)
	if !ok || !stamp {
		return value
	}
	return maskTopLevelTimestamp(text, anyStamp)
}

// maskTopLevelTimestamp is text with the value of its top-level updatedAt rewritten to the
// placeholder, when that value is a wall-clock stamp. The span comes from a token walk of the text,
// so the replacement cannot touch a nested key or any other byte, and a value that is not a stamp is
// left as stored.
func maskTopLevelTimestamp(text string, accept func(stamp string) bool) string {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return text
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return text
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return text
		}
		if key != "updatedAt" {
			continue
		}
		var stamp string
		if json.Unmarshal(raw, &stamp) != nil || !isTimestampText(stamp) || !accept(stamp) {
			return text
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		return text[:start] + "\"" + timestampPlaceholder + "\"" + text[end:]
	}
	return text
}

// readDefaultedUpdatedAt reports whether readStateStrict created the document's updatedAt from the
// wall clock rather than keeping a persisted one. The reader keeps the stored value only when raw is
// a JSON object with a valid phase and an updatedAt string (state.js:509-520, port
// state/restore.go:38-52); every other input — unreadable bytes, broken JSON, a missing or invalid
// phase, a non-text updatedAt — returns the default state, whose updatedAt each side stamps from its
// own clock. Only then can the two sides' read forms hold different instants, so only then is the
// read form's updatedAt masked; a persisted one is kept by both sides and compared as stored.
//
// It decodes with encoding/json, not the harness's pyjson, because the reader does: pyjson accepts
// the bare NaN and Infinity spellings encoding/json refuses (and a trailing value decodeObject
// rejects), so a source holding one would leave the predicate and the reader disagreeing.
func readDefaultedUpdatedAt(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil {
		return true
	}
	if _, err := dec.Token(); err != io.EOF {
		return true
	}
	object, ok := value.(map[string]any)
	if !ok {
		return true
	}
	name, _ := object["phase"].(string)
	if !slices.Contains(state.AllPhases(), state.Phase(name)) {
		return true
	}
	_, isText := object["updatedAt"].(string)
	return !isText
}

// stateCompare compares the answers. A rewritten file that drops something the oracle kept is a
// data-loss differ and is named first in the detail, as the issue asks. Both sides mask their write
// timestamps before answering, so the comparison is a plain canonical one.
func stateCompare(goOut, oracleOut any) Verdict {
	if canonical(goOut) == canonical(oracleOut) {
		return Verdict{Kind: Same}
	}
	if lost := writtenLosses(writtenText(goOut), writtenText(oracleOut)); len(lost) > 0 {
		return Verdict{Kind: Differ, Detail: "data-loss: the Go rewrite does not keep " + strings.Join(lost, ", ")}
	}
	return Verdict{Kind: Differ, Detail: "the read state or the rewritten bytes differ"}
}

// writtenText is an answer's published document text, or "" when the answer carries none (a refused
// write publishes nothing, and nothing to lose is not a loss).
func writtenText(out any) string {
	text, _ := field(out, "written")
	raw, ok := text.(string)
	if !ok {
		return ""
	}
	return raw
}

// writtenLosses names the paths at which the oracle's published document holds something the Go
// document does not keep, sorted. A value is lost when the Go document omits an object key or an
// array element the oracle kept, or replaces the code units of a string the oracle kept (a lone
// surrogate the Go reader turns into U+FFFD). It walks the oracle's whole document rather than its
// top level: a rewrite that loses one nested field, or the tail of a list, is the data loss this
// target exists to catch (CRW-708 generation 5, d1). A value that merely differs — a moved number, a
// changed string that is not the lossy form of the oracle's — is not a loss and stays a plain
// difference. An unparseable document on either side yields no verdict here, so a refusal stays the
// refusal the comparators name above.
//
// It is shared by the state and goalplan comparators: both compare a read-and-rewrite document, and
// the loss has the same shape in both (the issue's own example is a plan's
// workPhases[0].tasks[0].title).
func writtenLosses(goText, oracleText string) []string {
	if goText == "" || oracleText == "" {
		return nil
	}
	goValue, err := decode(goText)
	if err != nil {
		return nil
	}
	oracleValue, err := decode(oracleText)
	if err != nil {
		return nil
	}
	lost := []string{}
	collectWrittenLosses(goValue, oracleValue, "$", &lost)
	sort.Strings(lost)
	return lost
}

// collectWrittenLosses appends every path under which the oracle's value holds something the Go value
// does not keep. path is the JSON path of the two values, rooted at "$". Only the two shapes the
// criterion names count: a key or array element the Go document omits, and a string whose code units
// it replaced. Two values of different kinds, or two different scalars, are a plain difference — the
// comparators name those below — so this walk never turns an ordinary behaviour difference into a
// data-loss claim.
func collectWrittenLosses(goValue, oracleValue any, path string, lost *[]string) {
	switch oracle := oracleValue.(type) {
	case pyjson.Object:
		goObject, ok := goValue.(pyjson.Object)
		if !ok {
			// The oracle holds an object here and the Go document does not: every key below this path
			// is gone, so the path itself is the loss (CRW-708 generation 5, d2 of the pre-merge
			// evaluation). Returning instead would report the whole subtree as a plain difference.
			*lost = append(*lost, path)
			return
		}
		for _, item := range oracle {
			child := item.Key
			if path != "$" {
				child = path + "." + item.Key
			}
			goChild, found := goObject.Lookup(item.Key)
			if !found {
				*lost = append(*lost, child)
				continue
			}
			collectWrittenLosses(goChild, item.Value, child, lost)
		}
	case []any:
		goArray, ok := goValue.([]any)
		if !ok {
			*lost = append(*lost, path)
			return
		}
		for i, item := range oracle {
			child := path + "[" + strconv.Itoa(i) + "]"
			if i >= len(goArray) {
				*lost = append(*lost, child)
				continue
			}
			collectWrittenLosses(goArray[i], item, child, lost)
		}
	case string:
		goText, ok := goValue.(string)
		// A string the oracle keeps and the Go document holds as something else (null, a number, an
		// object) has lost its text, at any depth (CRW-708 generation 5, c10 d1).
		if !ok || lossyString(oracle, goText) {
			*lost = append(*lost, path)
		}
	}
}

// lossyString reports whether got is oracle with some or all of its unpaired UTF-16 surrogates replaced
// by U+FFFD, which is what a decoder that refuses a lone surrogate leaves behind (the CRW-556 class this
// target pins), or any string with fewer UTF-16 code units than the oracle's, which is a truncation or a
// dropped surrogate pair (CRW-708 generation 5, c10 d2). Each unpaired surrogate of the oracle is either
// kept as it is or replaced by U+FFFD, every other byte must match, and at least one replacement must
// have happened; a partial replacement is a loss just as a full one is (CRW-978 c2). A string that is
// equal, or whose difference is anything else, is not a loss.
func lossyString(oracle, got string) bool {
	if oracle == got {
		return false
	}
	if lossUTF16Units(got) < lossUTF16Units(oracle) {
		return true
	}
	replaced := 0
	j := 0
	for i := 0; i < len(oracle); {
		if size := unpairedSurrogateAt(oracle, i); size > 0 {
			switch {
			case j+size <= len(got) && got[j:j+size] == oracle[i:i+size]:
				j += size
			case j+len(replacementRune) <= len(got) && got[j:j+len(replacementRune)] == replacementRune:
				replaced++
				j += len(replacementRune)
			default:
				return false
			}
			i += size
			continue
		}
		if j >= len(got) || got[j] != oracle[i] {
			return false
		}
		i++
		j++
	}
	return replaced > 0 && j == len(got)
}

// replacementRune is U+FFFD as the bytes a Go string holds it in.
const replacementRune = "\uFFFD"

// lossUTF16Units counts the UTF-16 code units a string holds: an astral code point is two, a lone
// surrogate held as its three WTF-8 bytes is one, and every other code point is one.
func lossUTF16Units(s string) int {
	units := 0
	for i := 0; i < len(s); {
		if size := unpairedSurrogateAt(s, i); size > 0 {
			units++
			i += size
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
		i += size
	}
	return units
}

// unpairedSurrogateAt is the byte length of the WTF-8 encoding of an unpaired UTF-16 surrogate at
// position i of s, or 0 when the bytes there are not one. A properly paired surrogate is the astral
// code point's own four-byte UTF-8 form and is never matched here.
func unpairedSurrogateAt(s string, i int) int {
	if i+3 > len(s) || s[i] != 0xED || s[i+1] < 0xA0 || s[i+1] > 0xBF || s[i+2] < 0x80 || s[i+2] > 0xBF {
		return 0
	}
	return 3
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
		entries = append(entries, `{"agentId": "a`+strconv.Itoa(i)+`", "turnId": "t", "agentType": "executor", "attempts": 3, "receiptClaimed": "`+stateReceiptClaim(rng)+`", "recordedAt": "2026-01-01T00:00:00Z", "resolvable": true}`)
	}
	return `{"phase": "P", "unverifiedSubagents": [` + strings.Join(entries, ", ") + `]}`
}

// stateReceiptClaim is one receiptClaimed value. The issue body names the UTF-16 boundary of the
// reader's 256-unit cut: 255, 256 and 257 units ending in an emoji. The first two survive the cut
// whole and the third is cut inside the surrogate pair, which leaves a lone high surrogate the
// oracle keeps and the port writes as U+FFFD (CRW-708 generation 5, d2).
func stateReceiptClaim(rng *rand.Rand) string {
	const emoji = `\ud83d\ude00` // U+1F600, two UTF-16 units, written the way the file spells it
	switch rng.Intn(6) {
	case 0:
		return strings.Repeat("a", 253) + emoji // 255 units: below the cut
	case 1:
		return strings.Repeat("a", 254) + emoji // 256 units: exactly the cut
	case 2:
		return strings.Repeat("a", 255) + emoji // 257 units: the cut splits the pair
	case 3:
		return `\ud800`
	case 4:
		return strings.Repeat("\u00e9", 255+rng.Intn(3))
	default:
		return "r"
	}
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
