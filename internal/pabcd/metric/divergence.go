package metric

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The divergence mode and its candidate archive: the Go form of CXC v0.2.40 pabcd-state/src/divergence.ts (commit 3c1459ac) under the
// CRW names of contract/schema/cxc/name-substitution.json. A session's mode is .crw/divergence/<session>.mode.json and the candidates
// of every session are the rows of .crw/divergence/candidates.jsonl.
//
// Behaviour is ported as-is, oracle defects included (docs/port-cxc/known-defects.md, "Found by the divergence mode and candidate
// archive port"), except three defects that are fixed. An append starts its row on a new line when the archive does not end in one, or
// can not be read to see (as metrics.go does), and does not follow a link at the archive's path or write to a file that is not a
// regular one. WriteDivergenceMode does not replace a mode file that holds another session's id or U+FFFD, is not a regular file, is
// nested too deeply to read or can not be read (a file read without a string sessionId is replaced), and creates its temp file
// exclusively. The appender locks the archive and the mode writer the divergence directory: other writers of this package only.
//
// Not literal: a Go string can not hold a lone surrogate, so one in a stored row reads as U+FFFD; the readers do not read a file nested
// deeper than the Go decoder allows (10,000 levels); and CompareTimestamps knows the root order of ICU's collation for ASCII only.
// Rows are spelled as JSON.stringify spells them: HTML is not escaped and U+2028 and U+2029 are written literally.

// CollapsePoint is where the loop commits to one candidate: early at P for satisfy-spec work, late at D for maximize-metric work.
type CollapsePoint string

const CollapseP, CollapseD CollapsePoint = "P", "D"

// CandidateKind is the role of a candidate; the reader drops a row with any other value.
type CandidateKind string

const KindStrong1, KindAdd1, KindAlternative CandidateKind = "strong-1", "add-1", "alternative"

// CandidateStatus is how far a candidate got; the reader drops a row with any other value.
type CandidateStatus string

const StatusProposed, StatusBuilt, StatusChecked, StatusKept, StatusDiscarded CandidateStatus = "proposed", "built", "checked", "kept", "discarded"

// CandidateChangeClass is the kind of change a candidate makes; "" is absent, and the reader leaves any other value out.
type CandidateChangeClass string

const (
	ChangeParameterTweak, ChangeBranchToggle        CandidateChangeClass = "parameter-tweak", "branch-toggle"
	ChangeStateSpaceRedesign, ChangeEvaluatorChange CandidateChangeClass = "state-space-redesign", "evaluator-change"
)

// CandidateKilledAtPhase is the phase a discarded candidate died in; "" is absent, and the reader leaves any other value out.
type CandidateKilledAtPhase string

const PhaseP, PhaseA, PhaseB, PhaseC, PhaseD CandidateKilledAtPhase = "P", "A", "B", "C", "D"

// DivergenceDir and CandidatesFile are DIVERGENCE_DIR and CANDIDATES_FILE.
const (
	DivergenceDir  = "divergence"
	CandidatesFile = "candidates.jsonl"
)

// DivergenceMode is a session's mode file. ObjectiveKind is always Maximize.
type DivergenceMode struct {
	SessionID     string
	Active        bool
	ObjectiveKind ObjectiveKind
	CollapsePoint CollapsePoint
	Reason        string
	UpdatedAt     string
}

// ModeInput is what WriteDivergenceMode records. A nil Now reads the clock.
type ModeInput struct {
	SessionID     string
	Active        bool
	CollapsePoint CollapsePoint
	Reason        string
	Now           func() string
}

// DivergenceCandidate is one row of the archive. Worktree, MetricName, Note and MetricValue are nil when the row has none (the reader
// keeps an empty string); ChangeClass and KilledAtPhase are "" when it has none.
type DivergenceCandidate struct {
	TS            string
	SessionID     string
	ID            string
	Kind          CandidateKind
	Title         string
	Rationale     string
	SourceURLs    []string
	Status        CandidateStatus
	Worktree      *string
	MetricName    *string
	MetricValue   *float64
	Note          *string
	ChangeClass   CandidateChangeClass
	KilledAtPhase CandidateKilledAtPhase
}

// CandidateInput is a candidate to record (RecordDivergenceCandidateInput). "" is absent for ID, Worktree, MetricName and Note (the
// oracle's falsy tests). Status, ChangeClass and KilledAtPhase are pointers because the oracle tells an empty one from an absent one:
// a nil Status is proposed, an empty one is written, and a given ChangeClass or KilledAtPhase must be a value, "" included. A MetricValue
// that is not finite is left out; a nil Now reads the clock; Kind and Status are not checked.
type CandidateInput struct {
	SessionID     string
	ID            string
	Kind          CandidateKind
	Title         string
	Rationale     string
	SourceURLs    []string
	Status        *CandidateStatus
	Worktree      string
	MetricName    string
	MetricValue   *float64
	Note          string
	ChangeClass   *CandidateChangeClass
	KilledAtPhase *CandidateKilledAtPhase
	Now           func() string
}

// Streak is the answer of DiscardStreak (the oracle's DiscardStreak interface): ChangeClass is "" when there is no streak.
type Streak struct {
	ChangeClass CandidateChangeClass
	Length      int
}

func divergenceDir(cwd string) string { return filepath.Join(cwd, crwdir.DirName, DivergenceDir) }

func modePath(cwd, sessionID string) string {
	return filepath.Join(divergenceDir(cwd), state.SanitizeKey(sessionID)+".mode.json")
}

func candidatesPath(cwd string) string { return filepath.Join(divergenceDir(cwd), CandidatesFile) }

func isCollapsePoint(s string) bool { return s == string(CollapseP) || s == string(CollapseD) }

func isCandidateKind(s string) bool {
	return s == string(KindStrong1) || s == string(KindAdd1) || s == string(KindAlternative)
}

func isCandidateStatus(s string) bool {
	return slices.Contains([]string{"proposed", "built", "checked", "kept", "discarded"}, s)
}

func isCandidateChangeClass(s string) bool {
	return slices.Contains([]string{"parameter-tweak", "branch-toggle", "state-space-redesign", "evaluator-change"}, s)
}

func isCandidateKilledAtPhase(s string) bool {
	return slices.Contains([]string{"P", "A", "B", "C", "D"}, s)
}

// slug is the file-name key of a lower-cased value cut to 64 characters. The oracle's "|| candidate" is dead: SanitizeKey never
// returns "". JavaScript's toLowerCase makes U+0130 an "i" and a combining dot, which sanitising turns into "i-".
func slug(value string) string {
	key := state.SanitizeKey(strings.ToLower(strings.ReplaceAll(value, "\u0130", "i\u0307")))
	return key[:min(len(key), 64)]
}

// normalizeSources trims each URL (as JavaScript does) and drops the empty ones and the repeats, keeping the first of each.
func normalizeSources(urls []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, u := range urls {
		if u = text.Trim(u); u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// jsonObject is JSON.stringify of an object whose members are already spelled: compact, or with the oracle's two-space indent.
func jsonObject(members [][2]string, indent bool) string {
	parts, colon, sep, open, shut := make([]string, len(members)), ":", ",", "{", "}"
	if indent {
		colon, sep, open, shut = ": ", ",\n  ", "{\n  ", "\n}"
	}
	for i, m := range members {
		parts[i] = rowQuote(m[0]) + colon + m[1]
	}
	return open + strings.Join(parts, sep) + shut
}

func encodeMode(m DivergenceMode, indent bool) string {
	return jsonObject([][2]string{{"sessionId", rowQuote(m.SessionID)}, {"active", strconv.FormatBool(m.Active)}, {"objectiveKind", rowQuote(string(m.ObjectiveKind))},
		{"collapsePoint", rowQuote(string(m.CollapsePoint))}, {"reason", rowQuote(m.Reason)}, {"updatedAt", rowQuote(m.UpdatedAt)}}, indent)
}

// EncodeMode is JSON.stringify of the mode, without a newline (the oracle's CLI prints it for --json; the file holds the indented form).
func EncodeMode(m DivergenceMode) string { return encodeMode(m, false) }

// EncodeCandidate is JSON.stringify of the candidate, without a newline: keys in the oracle's order, the optional ones only when set.
func EncodeCandidate(c DivergenceCandidate) string {
	urls := make([]string, len(c.SourceURLs))
	for i, u := range c.SourceURLs {
		urls[i] = rowQuote(u)
	}
	m := [][2]string{{"ts", rowQuote(c.TS)}, {"sessionId", rowQuote(c.SessionID)}, {"id", rowQuote(c.ID)}, {"kind", rowQuote(string(c.Kind))}, {"title", rowQuote(c.Title)},
		{"rationale", rowQuote(c.Rationale)}, {"sourceUrls", "[" + strings.Join(urls, ",") + "]"}, {"status", rowQuote(string(c.Status))}}
	if c.Worktree != nil {
		m = append(m, [2]string{"worktree", rowQuote(*c.Worktree)})
	}
	if c.MetricName != nil {
		m = append(m, [2]string{"metricName", rowQuote(*c.MetricName)})
	}
	if c.MetricValue != nil {
		m = append(m, [2]string{"metricValue", rowNumber(*c.MetricValue)})
	}
	if c.Note != nil {
		m = append(m, [2]string{"note", rowQuote(*c.Note)})
	}
	if c.ChangeClass != "" {
		m = append(m, [2]string{"changeClass", rowQuote(string(c.ChangeClass))})
	}
	if c.KilledAtPhase != "" {
		m = append(m, [2]string{"killedAtPhase", rowQuote(string(c.KilledAtPhase))})
	}
	return jsonObject(m, false)
}

// WriteDivergenceMode records a session's mode (writeDivergenceMode): a temp file renamed over the mode file, holding the indented JSON
// and no final newline. The file is named by SanitizeKey of the session id, so two ids can share one. The write is refused, and the
// file kept, when it holds another session's id or U+FFFD (not told from a lone surrogate), is nested too deeply to read, can not be
// read for any reason but its absence, or is not a regular file (the data-loss fix); a file read without a string sessionId is
// replaced. The temp file is created exclusively (the oracle's write follows a symlink at its name and truncates the target). A lock on
// the divergence directory, held from the read to the rename, excludes other writers.
func WriteDivergenceMode(cwd string, in ModeInput) (DivergenceMode, error) {
	return writeDivergenceMode(cwd, in, time.Now(), crwdir.Rename)
}

func writeDivergenceMode(cwd string, in ModeInput, wall time.Time, rename func(tmp, finalPath string) error) (DivergenceMode, error) {
	mode := DivergenceMode{in.SessionID, in.Active, Maximize, in.CollapsePoint, in.Reason, wall.UTC().Format(timestampLayout)}
	if in.Now != nil {
		mode.UpdatedAt = in.Now()
	}
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return DivergenceMode{}, err
	}
	if err := os.MkdirAll(divergenceDir(cwd), 0o777); err != nil {
		return DivergenceMode{}, err
	}
	dir, err := os.Open(divergenceDir(cwd))
	if err != nil {
		return DivergenceMode{}, err
	}
	defer dir.Close() // drops the lock
	if err = unix.Flock(int(dir.Fd()), unix.LOCK_EX); err != nil {
		return DivergenceMode{}, err
	}
	finalPath := modePath(cwd, in.SessionID)
	if info, err := os.Stat(finalPath); err == nil && !info.Mode().IsRegular() { // a FIFO would block the read below
		return DivergenceMode{}, fmt.Errorf("%s is not a regular file", finalPath)
	}
	raw, err := os.ReadFile(finalPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return DivergenceMode{}, err
	}
	doc := ledgerDoc(string(raw))
	var syntax *json.SyntaxError // a file nested deeper than the decoder allows is not told from one that is not JSON, so it must not be replaced
	if err := json.Unmarshal(raw, new(json.RawMessage)); doc == nil && errors.As(err, &syntax) && strings.HasSuffix(syntax.Error(), "exceeded max depth") {
		return DivergenceMode{}, fmt.Errorf("%s is nested too deeply to read its owner", finalPath)
	}
	if owner, named := doc["sessionId"].(string); named && (owner != in.SessionID || strings.ContainsRune(owner, utf8.RuneError)) {
		return DivergenceMode{}, fmt.Errorf("%s holds the divergence mode of session %q, not %q", finalPath, owner, in.SessionID)
	}
	// The oracle's temp name, so a long id fails or fits as it does there; the lock keeps two writers from sharing it.
	tmp := fmt.Sprintf("%s.%d.%d.tmp", finalPath, os.Getpid(), wall.UnixMilli())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return DivergenceMode{}, err
	}
	defer os.Remove(tmp) // best effort; gone after a rename
	_, err = f.WriteString(encodeMode(mode, true))
	if err = errors.Join(err, f.Close()); err != nil {
		return DivergenceMode{}, err
	}
	if err = rename(tmp, finalPath); err != nil {
		return DivergenceMode{}, err
	}
	return mode, nil
}

// ReadDivergenceMode is the session's mode, or false when the file is missing, is not one JSON document, is not an object, or lacks any
// of: this exact sessionId, a boolean active, objectiveKind maximize, a collapsePoint of P or D, and a text reason and updatedAt.
func ReadDivergenceMode(cwd, sessionID string) (DivergenceMode, bool) {
	raw, err := os.ReadFile(modePath(cwd, sessionID))
	if err != nil {
		return DivergenceMode{}, false
	}
	m := ledgerDoc(ledgerUTF8(raw))
	owner, _ := m["sessionId"].(string)
	active, isBool := m["active"].(bool)
	collapse, _ := m["collapsePoint"].(string)
	reason, hasReason := m["reason"].(string)
	updated, hasUpdated := m["updatedAt"].(string)
	if _, named := m["sessionId"].(string); !named || owner != sessionID || !isBool || m["objectiveKind"] != string(Maximize) || !isCollapsePoint(collapse) || !hasReason || !hasUpdated {
		return DivergenceMode{}, false
	}
	return DivergenceMode{sessionID, active, Maximize, CollapsePoint(collapse), reason, updated}, true
}

// RecordDivergenceCandidate appends a candidate to the archive and returns its row. It is refused, writing nothing, when the sources
// are empty after normalising, or a given changeClass or killedAtPhase is not one of the values. The id is the slug of ID, else the
// kind and the slug of the title. The append is the ledger's (see appendRow) but does not follow a link at the archive's path.
func RecordDivergenceCandidate(cwd string, in CandidateInput) (DivergenceCandidate, error) {
	sources := normalizeSources(in.SourceURLs)
	if len(sources) == 0 {
		return DivergenceCandidate{}, errors.New("divergence candidate requires at least one grounding source URL")
	}
	if in.ChangeClass != nil && !isCandidateChangeClass(string(*in.ChangeClass)) {
		return DivergenceCandidate{}, errors.New("divergence candidate changeClass must be parameter-tweak, branch-toggle, state-space-redesign, or evaluator-change")
	}
	if in.KilledAtPhase != nil && !isCandidateKilledAtPhase(string(*in.KilledAtPhase)) {
		return DivergenceCandidate{}, errors.New("divergence candidate killedAtPhase must be P, A, B, C, or D")
	}
	c := DivergenceCandidate{TS: time.Now().UTC().Format(timestampLayout), SessionID: in.SessionID, ID: string(in.Kind) + "-" + slug(in.Title), Kind: in.Kind,
		Title: in.Title, Rationale: in.Rationale, SourceURLs: sources, Status: StatusProposed}
	if in.Now != nil {
		c.TS = in.Now()
	}
	if in.ID != "" {
		c.ID = slug(in.ID)
	}
	if in.Status != nil {
		c.Status = *in.Status
	}
	if in.ChangeClass != nil {
		c.ChangeClass = *in.ChangeClass
	}
	if in.KilledAtPhase != nil {
		c.KilledAtPhase = *in.KilledAtPhase
	}
	if in.Worktree != "" {
		c.Worktree = &in.Worktree
	}
	if in.MetricName != "" {
		c.MetricName = &in.MetricName
	}
	if v := in.MetricValue; v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) {
		value := *v
		c.MetricValue = &value
	}
	if in.Note != "" {
		c.Note = &in.Note
	}
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return DivergenceCandidate{}, err
	}
	if err := os.MkdirAll(divergenceDir(cwd), 0o777); err != nil {
		return DivergenceCandidate{}, err
	}
	return c, appendCandidateRow(candidatesPath(cwd), EncodeCandidate(c))
}

// appendCandidateRow is appendRow with the archive opened without following a symbolic link (the oracle's append writes into the target
// of one) and refused unless it is a regular file (the oracle writes to a FIFO that has a reader, which would block here); the
// security fix. metrics.go's appendRow, which this repeats, still follows a link.
func appendCandidateRow(path, row string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o666)
	if err != nil {
		return err
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() { // a FIFO would block the read below
		return errors.Join(err, fmt.Errorf("%s is not a regular file", path), f.Close())
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil { // dropped by the close
		return errors.Join(err, f.Close())
	}
	line := row + "\n"
	if raw, err := os.ReadFile(path); err != nil || len(raw) > 0 && raw[len(raw)-1] != '\n' {
		line = "\n" + line
	}
	_, err = f.WriteString(line)
	return errors.Join(err, f.Close())
}

// ReadDivergenceCandidates is the accepted rows of a session, or of every session when sessionID is empty (the oracle's falsy test), in
// file order. A row is accepted when it is a JSON object with text ts, sessionId, id, title and rationale, a known kind and status, and a
// sourceUrls array of texts (which may be empty); an optional field the row has in the wrong shape is left out. The result is never nil.
func ReadDivergenceCandidates(cwd, sessionID string) []DivergenceCandidate {
	out := []DivergenceCandidate{}
	raw, err := os.ReadFile(candidatesPath(cwd))
	if err != nil {
		return out
	}
	for _, line := range text.SplitLines(ledgerUTF8(raw)) {
		if c, ok := candidateRow(line); ok && (sessionID == "" || c.SessionID == sessionID) {
			out = append(out, c)
		}
	}
	return out
}

// candidateRow is one archive line as the reader accepts it. A number that overflows is infinite and is left out, as a text, an
// array or null is where a number is wanted; the keys match exactly, the last of a repeated key wins and other keys are ignored.
func candidateRow(line string) (DivergenceCandidate, bool) {
	m := ledgerDoc(line)
	var s [5]string
	for i, key := range [...]string{"ts", "sessionId", "id", "title", "rationale"} {
		v, ok := m[key].(string)
		if s[i] = v; !ok {
			return DivergenceCandidate{}, false
		}
	}
	kind, _ := m["kind"].(string)
	status, _ := m["status"].(string)
	urls, isArray := m["sourceUrls"].([]any)
	if !isArray || !isCandidateKind(kind) || !isCandidateStatus(status) {
		return DivergenceCandidate{}, false
	}
	sources := make([]string, 0, len(urls))
	for _, u := range urls {
		v, ok := u.(string)
		if !ok {
			return DivergenceCandidate{}, false
		}
		sources = append(sources, v)
	}
	c := DivergenceCandidate{TS: s[0], SessionID: s[1], ID: s[2], Kind: CandidateKind(kind), Title: s[3], Rationale: s[4], SourceURLs: normalizeSources(sources),
		Status: CandidateStatus(status), Worktree: optionalText(m, "worktree"), MetricName: optionalText(m, "metricName"), Note: optionalText(m, "note")}
	if n, ok := m["metricValue"].(json.Number); ok {
		if f, err := n.Float64(); err == nil {
			c.MetricValue = &f
		}
	}
	if v, _ := m["changeClass"].(string); isCandidateChangeClass(v) {
		c.ChangeClass = CandidateChangeClass(v)
	}
	if v, _ := m["killedAtPhase"].(string); isCandidateKilledAtPhase(v) {
		c.KilledAtPhase = CandidateKilledAtPhase(v)
	}
	return c, true
}

func optionalText(m map[string]any, key string) *string {
	if s, ok := m[key].(string); ok {
		return &s
	}
	return nil
}

// DiscardStreak is the run of discarded candidates of one change class that ends the list in time order (discardStreak): none when the
// latest candidate is not discarded or has no change class. Candidates with equal timestamps keep their order.
func DiscardStreak(candidates []DivergenceCandidate) Streak {
	sorted := slices.Clone(candidates)
	slices.SortStableFunc(sorted, func(a, b DivergenceCandidate) int { return CompareTimestamps(a.TS, b.TS) })
	if len(sorted) == 0 {
		return Streak{}
	}
	latest := sorted[len(sorted)-1]
	if latest.Status != StatusDiscarded || latest.ChangeClass == "" {
		return Streak{}
	}
	length := 0
	for i := len(sorted) - 1; i >= 0 && sorted[i].Status == StatusDiscarded && sorted[i].ChangeClass == latest.ChangeClass; i-- {
		length++
	}
	return Streak{latest.ChangeClass, length}
}

// asciiOrder is the ASCII characters that are not ignorable in the order of CLDR's root collation, letters in lower case.
const asciiOrder = "\t\n\v\f\r _-,;:!?.'\"()[]{}@*/\\&#%\x60^+<=>|~$0123456789abcdefghijklmnopqrstuvwxyz"

// CompareTimestamps is a.localeCompare(b) as ICU's root collation gives it for ASCII (the oracle sorts candidates with it): primary
// weights decide over the whole string, then the case (lower before upper), and the ASCII control characters are ignorable. A character
// beyond ASCII sorts after every ASCII one by code point; ICU interleaves them, and that part is not reproduced.
func CompareTimestamps(a, b string) int {
	pa, ta := collationKey(a)
	pb, tb := collationKey(b)
	if c := slices.Compare(pa, pb); c != 0 {
		return c
	}
	return slices.Compare(ta, tb)
}

// collationKey is the primary weights of s, one per character that is not ignorable, and the tertiary ones: 1 for an upper-case letter.
func collationKey(s string) (primary, tertiary []int) {
	for _, r := range s {
		weight, upper := 0, 0
		switch {
		case r >= 'A' && r <= 'Z':
			weight, upper = strings.IndexRune(asciiOrder, r+'a'-'A')+1, 1
		case r < utf8.RuneSelf:
			weight = strings.IndexRune(asciiOrder, r) + 1 // a control character is not in the string: weight 0, ignorable
		default:
			weight = 0x100 + int(r)
		}
		if weight > 0 {
			primary, tertiary = append(primary, weight), append(tertiary, upper)
		}
	}
	return primary, tertiary
}
