package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// crw manage audit drafts turns the defects a graded audit recorded into follow-up issue
// draft files below the state directory, one per defect, so a management session can read
// them and open the issues itself. The product never writes to Linear: the draft file is the
// whole surface, and the management session posts it through its own connector.

// The schemas this surface writes: one follow-up issue draft, and the index that lists the
// fingerprints the drafts directory holds.
const (
	auditDraftSchema      = "crw-issue-draft/1"
	auditDraftIndexSchema = "crw-issue-draft-index/1"
)

// auditDraftIndexFile is the listing inside the drafts directory.
const auditDraftIndexFile = "index.json"

// The source every draft names, the two states a draft carries, and the token a draft whose
// path no owners entry claims carries.
const (
	auditDraftSource       = "audit"
	auditDraftStateDraft   = "draft"
	auditDraftStatePosted  = "posted"
	auditDraftOwnerUnknown = "owner_unknown"
)

// The values the issue fixes for a setting the configuration leaves out.
const (
	auditDraftDefaultMaxNewDrafts = 10
	auditDraftDefaultSeverity     = "P1"
)

// auditDraftFingerprintChars is how many leading hex characters of the digest a fingerprint
// keeps, and auditDraftTitleLimit is the longest title a draft carries.
const (
	auditDraftFingerprintChars = 16
	auditDraftTitleLimit       = 72
)

// auditDraftUsage is what the drafts subcommand prints.
const auditDraftUsage = "usage: crw manage audit drafts [--round R | --since T] [--severity P1]\n" +
	"       crw manage audit drafts mark --fingerprint F --posted ISSUE"

// auditDraftSeverityRank orders the severities from the most to the least severe. A defect
// whose severity is not one of the four is not a defect this surface drafts.
var auditDraftSeverityRank = map[string]int{"P0": 0, "P1": 1, "P2": 2, "P3": 3}

// auditDraftSection is the part of the audit section this command reads: the repository path
// prefix to project key map a defect's owner is resolved from, and how many new drafts one
// run may create.
type auditDraftSection struct {
	Owners       map[string]string `json:"owners"`
	MaxNewDrafts int               `json:"max_new_drafts"`
}

// auditDraftSeen is one graded result a defect was seen in, as the draft records it.
type auditDraftSeen struct {
	Mode    string `json:"mode"`
	Subject string `json:"subject"`
	Head    string `json:"head"`
	At      string `json:"at"`
}

// auditDraft is one follow-up issue draft, in the shape crw-issue-draft/1 fixes.
type auditDraft struct {
	Schema      string           `json:"schema"`
	Fingerprint string           `json:"fingerprint"`
	Source      string           `json:"source"`
	Project     string           `json:"project"`
	Title       string           `json:"title"`
	Severity    string           `json:"severity"`
	Body        string           `json:"body"`
	Labels      []string         `json:"labels"`
	Seen        []auditDraftSeen `json:"seen"`
	State       string           `json:"state"`

	// Posted is the issue key the management session recorded with mark. It is absent until
	// then, so a reader can tell a draft nobody opened from one already on Linear.
	Posted string `json:"posted,omitempty"`
}

// auditDraftIndex is the listing of the fingerprints the drafts directory holds.
type auditDraftIndex struct {
	Schema string   `json:"schema"`
	Drafts []string `json:"drafts"`
}

// auditDraftSummary is one draft as the command's report names it.
type auditDraftSummary struct {
	Fingerprint string `json:"fingerprint"`
	File        string `json:"file"`
	Project     string `json:"project"`
	Title       string `json:"title"`
	Severity    string `json:"severity"`
	Seen        int    `json:"seen"`
	State       string `json:"state"`
}

// auditDraftSkip is one ok ledger row this run could not draft from, with the reason it could
// not. A ledger is append-only and a bundle directory is mutable, so a row the product cannot
// trust is named here rather than silently dropped or turned into an empty pass.
type auditDraftSkip struct {
	Mode    string `json:"mode"`
	Subject string `json:"subject"`
	Head    string `json:"head"`
	Reason  string `json:"reason"`
}

// auditDraftReport is what crw manage audit drafts prints: the drafts this run created and
// the ones it appended a sighting to, the fingerprints whose owner the owners map does not
// name, and how many new drafts the cap left for a later run.
type auditDraftReport struct {
	Created      []auditDraftSummary `json:"created"`
	Updated      []auditDraftSummary `json:"updated"`
	OwnerUnknown []string            `json:"owner_unknown"`
	Skipped      []auditDraftSkip    `json:"skipped"`
	TornLines    int                 `json:"torn_lines"`
	Remaining    int                 `json:"remaining"`
}

// auditDraftScope is the range and the threshold one run works with. A run names at most one
// of Round and Since.
type auditDraftScope struct {
	Round    string
	Since    time.Time
	HasSince bool
	Severity string
}

// auditDraftCandidate is one defect a run selected, before it becomes a new draft or an
// appended sighting on an existing one.
type auditDraftCandidate struct {
	fingerprint string
	severity    string
	project     string
	path        string
	defect      AuditDefect
	criteria    []auditGradeCriterion
	seen        []auditDraftSeen
	order       int
}

// auditDraftKnownKeys is every key a draft document may carry. A file that carries another
// one was written by a version this build does not know, and rewriting it would drop that
// field, so such a file is refused rather than silently narrowed.
var auditDraftKnownKeys = map[string]bool{
	"schema": true, "fingerprint": true, "source": true, "project": true, "title": true,
	"severity": true, "body": true, "labels": true, "seen": true, "state": true, "posted": true,
}

// auditDraftUnknownKeys names the keys of a draft document this build does not read, in
// document order.
func auditDraftUnknownKeys(data []byte) ([]string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var unknown []string
	for key := range doc {
		if !auditDraftKnownKeys[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown, nil
}

// auditDraftLock takes the drafts directory's lock for the whole read-modify-write of one
// run, so two processes cannot create or grow one draft at once. It is non-blocking: a second
// caller is refused by name rather than waiting. The lock file itself is never removed,
// because another process may hold it.
func auditDraftLock(e *Env, cfg *Config) (func(), error) {
	dir := auditDraftDir(e, cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, "drafts.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("drafts_locked: %s is held by another process", filepath.Base(lockPath))
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// auditDraftDir is where the drafts live, below the state directory the configuration names.
func auditDraftDir(e *Env, cfg *Config) string {
	return filepath.Join(auditStateDir(e, cfg), "drafts")
}

// auditDraftSectionOf reads the audit section's owners map and cap. A configuration with no
// audit section leaves both at their defaults.
func auditDraftSectionOf(cfg *Config) (auditDraftSection, error) {
	var section auditDraftSection
	if cfg != nil {
		if err := cfg.Section("audit", &section); err != nil {
			return auditDraftSection{}, err
		}
	}
	if section.MaxNewDrafts <= 0 {
		section.MaxNewDrafts = auditDraftDefaultMaxNewDrafts
	}
	return section, nil
}

// auditDraftWherePath is the path part of a defect's where: a grader appends a line number
// after a colon, and the owners map and the fingerprint are about the file, not the line.
func auditDraftWherePath(where string) string {
	path := strings.TrimSpace(where)
	for {
		cut := strings.LastIndexByte(path, ':')
		if cut < 0 {
			return path
		}
		if !auditDraftAllDigits(path[cut+1:]) {
			return path
		}
		path = path[:cut]
	}
}

// auditDraftAllDigits reports whether s is a non-empty run of ASCII digits.
func auditDraftAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// auditDraftNormalizeWhat is the defect's what reduced to what a fingerprint compares: lower
// case, and every run of whitespace written as one space.
func auditDraftNormalizeWhat(what string) string {
	return strings.Join(strings.Fields(strings.ToLower(what)), " ")
}

// auditDraftFingerprint is the first 16 hex characters of the digest over the where's path
// and the normalized what, so the same defect two audits report is one draft. The two parts
// are written as a JSON array rather than joined: a plain concatenation would make the pairs
// ("a.go", "bug") and ("a.gob", "ug") the same bytes, and one defect would silently take
// another's draft.
func auditDraftFingerprint(where, what string) string {
	parts, err := json.Marshal([]string{auditDraftWherePath(where), auditDraftNormalizeWhat(what)})
	if err != nil {
		// A []string of two strings cannot fail to marshal; the fallback keeps the digest
		// defined rather than panicking in a command.
		parts = []byte(auditDraftWherePath(where) + "\x00" + auditDraftNormalizeWhat(what))
	}
	sum := sha256.Sum256(parts)
	return hex.EncodeToString(sum[:])[:auditDraftFingerprintChars]
}

// auditDraftOwner is the project key of the longest owners entry whose repository path prefix
// matches the defect's path at a segment boundary, or the empty project when none does. The
// boundary check keeps internal/manage from claiming internal/manager.
func auditDraftOwner(owners map[string]string, path string) string {
	best, bestLen := "", -1
	for prefix, project := range owners {
		if !auditPkgPrefixMatches(path, prefix) || len(prefix) <= bestLen {
			continue
		}
		best, bestLen = project, len(prefix)
	}
	return best
}

// auditDraftTitle is the English title of a draft: the severity and the defect's what, cut to
// the character limit the issue fixes.
func auditDraftTitle(severity, what string) string {
	text := auditDraftNormalizeWhat(what)
	if text == "" {
		text = "audit defect"
	}
	return auditDraftTruncateRunes(severity+": "+text, auditDraftTitleLimit)
}

// auditDraftTruncateRunes cuts s to at most limit characters, marking a cut with three dots.
func auditDraftTruncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

// auditDraftSeenHas reports whether the list already carries this sighting. A sighting is the
// graded result it came from, so running the same ledger twice never doubles a seen entry.
func auditDraftSeenHas(seen []auditDraftSeen, entry auditDraftSeen) bool {
	for _, have := range seen {
		if have == entry {
			return true
		}
	}
	return false
}

// auditDraftSeenLines is the seen list as the body's source-audit section writes it.
func auditDraftSeenLines(seen []auditDraftSeen) string {
	var b strings.Builder
	for _, entry := range seen {
		fmt.Fprintf(&b, "- mode=%s subject=%s head=%s at=%s\n", entry.Mode, entry.Subject, entry.Head, entry.At)
	}
	return b.String()
}

// auditDraftCriterionLines is the graded criteria as the body's criteria section writes them.
func auditDraftCriterionLines(criteria []auditGradeCriterion) string {
	if len(criteria) == 0 {
		return "- (the grade recorded no criteria)\n"
	}
	var b strings.Builder
	for _, criterion := range criteria {
		fmt.Fprintf(&b, "- %s: %s", criterion.ID, criterion.Verdict)
		if note := strings.TrimSpace(criterion.Note); note != "" {
			fmt.Fprintf(&b, " (%s)", note)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// auditDraftBody is the draft's body: what the defect is, where it is, how to reproduce it,
// which audit reported it, and the criteria that audit judged.
func auditDraftBody(c *auditDraftCandidate) string {
	var b strings.Builder
	b.WriteString("## What\n\n")
	b.WriteString(strings.TrimSpace(c.defect.What))
	b.WriteString("\n\n## Where\n\n")
	b.WriteString(strings.TrimSpace(c.defect.Where))
	b.WriteString("\n\n## Reproduction\n\n")
	if repro := strings.TrimSpace(c.defect.Repro); repro != "" {
		b.WriteString(repro)
	} else {
		b.WriteString("(the audit recorded no reproduction steps)")
	}
	b.WriteString("\n\n## Source audit\n\n")
	b.WriteString(auditDraftSeenLines(c.seen))
	b.WriteString("\n## Criteria\n\n")
	b.WriteString(auditDraftCriterionLines(c.criteria))
	if c.project == "" {
		fmt.Fprintf(&b, "\n## Owner\n\n%s: no owners entry in the audit section matches %s\n", auditDraftOwnerUnknown, c.path)
	}
	return b.String()
}

// auditDraftSeenSection replaces the Source audit section of a stored body with the rendered
// sighting list, so a draft a later audit reported names that audit in the body the
// management session reads. The defect description and the criteria stay as first written.
func auditDraftSeenSection(body string, seen []auditDraftSeen) string {
	const startMarker = "## Source audit\n\n"
	const endMarker = "\n## Criteria\n\n"
	start := strings.Index(body, startMarker)
	if start < 0 {
		return body
	}
	rest := body[start+len(startMarker):]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		return body
	}
	return body[:start+len(startMarker)] + auditDraftSeenLines(seen) + endMarker + rest[end+len(endMarker):]
}

// auditDraftLabels is the label list the issue fixes: the source and the severity.
func auditDraftLabels(severity string) []string {
	return []string{auditDraftSource, severity}
}

// auditDraftSummaryOf is one draft as the report names it.
func auditDraftSummaryOf(doc *auditDraft) auditDraftSummary {
	return auditDraftSummary{
		Fingerprint: doc.Fingerprint, File: doc.Fingerprint + ".json", Project: doc.Project,
		Title: doc.Title, Severity: doc.Severity, Seen: len(doc.Seen), State: doc.State,
	}
}

// auditDraftLedgerRows reads the audit ledger. A ledger that does not exist yet holds no
// rows, which is not an error: nothing has been graded. A line that is not a whole document
// is the torn tail the ledger writer leaves on its own line when it separates an interrupted
// append from the rows after it, so it is counted and skipped rather than failing every later
// run: the rows after it are whole and still count.
func auditDraftLedgerRows(e *Env, cfg *Config) ([]auditLedgerRow, int, error) {
	path := filepath.Join(auditStateDir(e, cfg), "audit", auditLedgerFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	var rows []auditLedgerRow
	torn := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row auditLedgerRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			torn++
			continue
		}
		rows = append(rows, row)
	}
	return rows, torn, nil
}

// auditDraftBundleOf checks that a ledger row's bundle still holds that row's audit. A bundle
// is a mutable directory: grading into it again replaces its grade.json, and the ledger row
// keeps only the path. A row whose bundle now declares another mode, subject or head has no
// grade of its own left to read, so it is named and skipped rather than drafted from another
// audit's defects.
func auditDraftBundleOf(row auditLedgerRow) error {
	bundle, err := auditReadBundle(row.Bundle)
	if err != nil {
		return err
	}
	if bundle.Mode != row.Mode || bundle.Subject != row.Subject || bundle.Head != row.Head {
		return fmt.Errorf("the bundle now declares another audit: mode=%s subject=%s head=%s", bundle.Mode, bundle.Subject, bundle.Head)
	}
	return nil
}

// auditDraftLoad reads one draft file. A missing file, malformed JSON or another schema is an
// error: a draft this product cannot trust is never appended to or marked.
func auditDraftLoad(path string) (*auditDraft, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc auditDraft
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if unknown, err := auditDraftUnknownKeys(data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	} else if len(unknown) > 0 {
		return nil, fmt.Errorf("%s: the keys %s are not part of %s, and rewriting the file would drop them", path, strings.Join(unknown, ", "), auditDraftSchema)
	}
	if doc.Schema != auditDraftSchema {
		return nil, fmt.Errorf("%s: schema %q is not %s", path, doc.Schema, auditDraftSchema)
	}
	if doc.Fingerprint == "" || doc.Fingerprint != strings.TrimSuffix(filepath.Base(path), ".json") {
		return nil, fmt.Errorf("%s: the fingerprint %q does not name this file", path, doc.Fingerprint)
	}
	return &doc, nil
}

// auditDraftWriteFile writes one file atomically: a temporary file beside it, fsynced, then
// renamed over it, so a reader never sees a half-written document.
func auditDraftWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// auditDraftSave writes one draft file atomically.
func auditDraftSave(path string, doc *auditDraft) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return auditDraftWriteFile(path, append(data, '\n'))
}

// auditDraftIndexLoad reads the drafts index. A missing index lists nothing.
func auditDraftIndexLoad(dir string) (auditDraftIndex, error) {
	data, err := os.ReadFile(filepath.Join(dir, auditDraftIndexFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return auditDraftIndex{Schema: auditDraftIndexSchema}, nil
		}
		return auditDraftIndex{}, err
	}
	var index auditDraftIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return auditDraftIndex{}, fmt.Errorf("%s: %w", filepath.Join(dir, auditDraftIndexFile), err)
	}
	if index.Schema != auditDraftIndexSchema {
		return auditDraftIndex{}, fmt.Errorf("%s: schema %q is not %s", filepath.Join(dir, auditDraftIndexFile), index.Schema, auditDraftIndexSchema)
	}
	return index, nil
}

// auditDraftIndexSave rewrites the index from the draft files the directory actually holds, so
// the listing and the artifacts never drift apart.
func auditDraftIndexSave(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	fingerprints := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || name == auditDraftIndexFile {
			continue
		}
		fingerprints = append(fingerprints, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(fingerprints)
	data, err := json.MarshalIndent(auditDraftIndex{Schema: auditDraftIndexSchema, Drafts: fingerprints}, "", "  ")
	if err != nil {
		return err
	}
	return auditDraftWriteFile(filepath.Join(dir, auditDraftIndexFile), append(data, '\n'))
}

// auditDraftFingerprintName accepts a fingerprint that is a plain 16-character lower-case hex
// name, so a fingerprint can never make mark read or write a file outside the drafts
// directory.
func auditDraftFingerprintName(fingerprint string) error {
	if len(fingerprint) != auditDraftFingerprintChars {
		return fmt.Errorf("the fingerprint must be %d characters", auditDraftFingerprintChars)
	}
	for i := 0; i < len(fingerprint); i++ {
		c := fingerprint[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return errors.New("the fingerprint must be lower-case hexadecimal")
		}
	}
	return nil
}

// auditDraftScopeMatches reports whether one ledger row is inside the run's range. Only an ok
// row carries a usable grade.json, so every other status is out of the range.
func auditDraftScopeMatches(row auditLedgerRow, scope auditDraftScope) (bool, error) {
	if row.Status != auditStatusOK {
		return false, nil
	}
	if scope.Round != "" && row.Round != scope.Round {
		return false, nil
	}
	if scope.HasSince {
		at, err := time.Parse(auditTimeFormat, row.GradedAt)
		if err != nil {
			return false, fmt.Errorf("the ledger row for %s at %s carries no usable graded_at: %w", row.Subject, row.Head, err)
		}
		if at.Before(scope.Since) {
			return false, nil
		}
	}
	return true, nil
}

// auditDraftsRun reads the ledger's ok rows and each bundle's grade.json, and writes one draft
// per defect at or above the threshold. A defect already in the index grows its seen list
// instead of becoming a new draft, and the new drafts the cap leaves uncreated are counted in
// the report rather than silently dropped.
func auditDraftsRun(e *Env, cfg *Config, scope auditDraftScope) (auditDraftReport, error) {
	report := auditDraftReport{
		Created:      []auditDraftSummary{},
		Updated:      []auditDraftSummary{},
		OwnerUnknown: []string{},
		Skipped:      []auditDraftSkip{},
	}
	section, err := auditDraftSectionOf(cfg)
	if err != nil {
		return report, err
	}
	threshold := scope.Severity
	if threshold == "" {
		threshold = auditDraftDefaultSeverity
	}
	thresholdRank, ok := auditDraftSeverityRank[threshold]
	if !ok {
		return report, fmt.Errorf("the severity %q is not P0, P1, P2 or P3", threshold)
	}
	dir := auditDraftDir(e, cfg)
	release, err := auditDraftLock(e, cfg)
	if err != nil {
		return report, err
	}
	defer release()
	// The index lists the drafts the directory holds, and it is rewritten from the directory
	// after every run, so the listing and the artifacts agree. Whether a draft exists is
	// judged by its file: a listing that names a file the directory no longer holds must not
	// make every later run fail before it rewrites the index.
	if _, err := auditDraftIndexLoad(dir); err != nil {
		return report, err
	}
	rows, torn, err := auditDraftLedgerRows(e, cfg)
	if err != nil {
		return report, err
	}
	// A bundle is a mutable directory: grading into it again replaces its grade.json, and the
	// ledger keeps every row that ever named it. Only the newest such row can still have its
	// grade there, so the older rows are named and skipped rather than given the newest
	// audit's defects as sightings they never made.
	newest := map[string]int{}
	for i, row := range rows {
		if row.Status != auditStatusOK || row.Bundle == "" {
			continue
		}
		newest[row.Bundle] = i
	}
	// A torn line is the fragment auditAppendLine leaves on its own line when it separates an
	// interrupted append from the rows after it, so it is counted rather than treated as a row.
	report.TornLines = torn
	candidates := map[string]*auditDraftCandidate{}
	var order []string
	for i, row := range rows {
		matches, err := auditDraftScopeMatches(row, scope)
		if err != nil {
			return report, err
		}
		if !matches {
			continue
		}
		if owner, ok := newest[row.Bundle]; ok && owner != i {
			report.Skipped = append(report.Skipped, auditDraftSkip{Mode: row.Mode, Subject: row.Subject, Head: row.Head,
				Reason: "the bundle was graded again after this row, so its " + auditGradeFile + " is that later audit's"})
			continue
		}
		// A ledger is append-only and unscoped runs read every ok row, so one row the product
		// cannot read must not stop the rows it can: the row is named in the report instead.
		if err := auditDraftBundleOf(row); err != nil {
			report.Skipped = append(report.Skipped, auditDraftSkip{Mode: row.Mode, Subject: row.Subject, Head: row.Head, Reason: err.Error()})
			continue
		}
		doc, ok := auditParseResult(filepath.Join(row.Bundle, auditGradeFile))
		if !ok {
			report.Skipped = append(report.Skipped, auditDraftSkip{Mode: row.Mode, Subject: row.Subject, Head: row.Head, Reason: "no usable " + auditGradeFile})
			continue
		}
		for _, defect := range doc.Defects {
			rank, knownSeverity := auditDraftSeverityRank[defect.Severity]
			if !knownSeverity || rank > thresholdRank {
				continue
			}
			fingerprint := auditDraftFingerprint(defect.Where, defect.What)
			entry := auditDraftSeen{Mode: row.Mode, Subject: row.Subject, Head: row.Head, At: row.GradedAt}
			candidate, seen := candidates[fingerprint]
			if !seen {
				path := auditDraftWherePath(defect.Where)
				candidate = &auditDraftCandidate{
					fingerprint: fingerprint, severity: defect.Severity,
					project: auditDraftOwner(section.Owners, path), path: path,
					defect: defect, criteria: doc.Criteria, order: len(order),
				}
				candidates[fingerprint] = candidate
				order = append(order, fingerprint)
			}
			if auditDraftSeverityRank[candidate.severity] > rank {
				// The higher severity carries its own evidence: the reproduction steps, the
				// where and the criteria notes are the ones that grade wrote, not the ones the
				// lower-severity grade wrote about the same defect.
				candidate.severity = defect.Severity
				candidate.defect = defect
				candidate.criteria = doc.Criteria
			}
			if !auditDraftSeenHas(candidate.seen, entry) {
				candidate.seen = append(candidate.seen, entry)
			}
		}
	}
	var fresh, existing []*auditDraftCandidate
	for _, fingerprint := range order {
		candidate := candidates[fingerprint]
		if _, err := os.Stat(filepath.Join(dir, fingerprint+".json")); err == nil {
			existing = append(existing, candidate)
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
		fresh = append(fresh, candidate)
	}
	// The cap holds back new drafts only, and it keeps the most severe first and then the ones
	// the audits reported most often, so what a later run creates is what matters most.
	sort.SliceStable(fresh, func(i, j int) bool {
		left, right := auditDraftSeverityRank[fresh[i].severity], auditDraftSeverityRank[fresh[j].severity]
		if left != right {
			return left < right
		}
		if len(fresh[i].seen) != len(fresh[j].seen) {
			return len(fresh[i].seen) > len(fresh[j].seen)
		}
		return fresh[i].order < fresh[j].order
	})
	if len(fresh) > section.MaxNewDrafts {
		report.Remaining = len(fresh) - section.MaxNewDrafts
		fresh = fresh[:section.MaxNewDrafts]
	}
	for _, candidate := range fresh {
		doc := &auditDraft{
			Schema: auditDraftSchema, Fingerprint: candidate.fingerprint, Source: auditDraftSource,
			Project: candidate.project, Title: auditDraftTitle(candidate.severity, candidate.defect.What),
			Severity: candidate.severity, Body: auditDraftBody(candidate),
			Labels: auditDraftLabels(candidate.severity), Seen: candidate.seen, State: auditDraftStateDraft,
		}
		if err := auditDraftSave(filepath.Join(dir, candidate.fingerprint+".json"), doc); err != nil {
			return report, err
		}
		report.Created = append(report.Created, auditDraftSummaryOf(doc))
		if doc.Project == "" {
			report.OwnerUnknown = append(report.OwnerUnknown, doc.Fingerprint)
		}
	}
	for _, candidate := range existing {
		path := filepath.Join(dir, candidate.fingerprint+".json")
		doc, err := auditDraftLoad(path)
		if err != nil {
			return report, err
		}
		changed := false
		for _, entry := range candidate.seen {
			if !auditDraftSeenHas(doc.Seen, entry) {
				doc.Seen = append(doc.Seen, entry)
				changed = true
			}
		}
		if changed {
			doc.Body = auditDraftSeenSection(doc.Body, doc.Seen)
		}
		// A later audit that reports the same defect at a higher severity escalates the
		// draft, so the management session prioritizes it as it now stands, and the draft
		// carries the evidence that raised it rather than the lower grade's. The posted state
		// and the issue key are kept: the escalation does not open a second issue.
		if stored, known := auditDraftSeverityRank[doc.Severity]; known && auditDraftSeverityRank[candidate.severity] < stored {
			promoted := *candidate
			promoted.seen = doc.Seen
			promoted.project = doc.Project
			doc.Severity = candidate.severity
			doc.Labels = auditDraftLabels(candidate.severity)
			doc.Title = auditDraftTitle(candidate.severity, candidate.defect.What)
			doc.Body = auditDraftBody(&promoted)
			changed = true
		}
		if !changed {
			continue
		}
		if err := auditDraftSave(path, doc); err != nil {
			return report, err
		}
		report.Updated = append(report.Updated, auditDraftSummaryOf(doc))
		if doc.Project == "" {
			report.OwnerUnknown = append(report.OwnerUnknown, doc.Fingerprint)
		}
	}
	if err := auditDraftIndexSave(dir); err != nil {
		return report, err
	}
	sort.Strings(report.OwnerUnknown)
	return report, nil
}

// auditRunDrafts is crw manage audit drafts. It prints the report one run produced, and the
// mark form records that the management session opened a draft on Linear.
func auditRunDrafts(_ context.Context, e *Env, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Fprintln(e.Stdout, auditDraftUsage)
			return 0
		case "mark":
			return auditDraftRunMark(e, args[1:])
		}
	}
	values, err := auditPkgParseArgs(args, map[string]bool{"round": true, "since": true, "severity": true})
	if err == nil && values["round"] != "" && values["since"] != "" {
		err = errors.New("--round and --since name one range each; give only one")
	}
	if err == nil && values["severity"] != "" {
		if _, ok := auditDraftSeverityRank[values["severity"]]; !ok {
			err = fmt.Errorf("--severity %q is not P0, P1, P2 or P3", values["severity"])
		}
	}
	scope := auditDraftScope{Round: values["round"], Severity: values["severity"]}
	if err == nil && values["since"] != "" {
		if scope.Since, err = time.Parse(auditTimeFormat, values["since"]); err != nil {
			err = fmt.Errorf("--since %q is not an RFC 3339 time", values["since"])
		} else {
			scope.HasSince = true
		}
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, auditDraftUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit drafts: error: %v\n", err)
		return usageExit
	}
	report, err := auditDraftsRun(e, coreDefaults(e), scope)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit drafts: error: %v\n", err)
		return 1
	}
	data, err := json.Marshal(report)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit drafts: error: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.Stdout, "%s\n", data)
	return 0
}

// auditDraftRunMark is crw manage audit drafts mark: it records that the management session
// opened this draft on Linear, with the issue key it was opened as.
func auditDraftRunMark(e *Env, args []string) int {
	values, err := auditPkgParseArgs(args, map[string]bool{"fingerprint": true, "posted": true})
	if err == nil && values["fingerprint"] == "" {
		err = errors.New("--fingerprint is required")
	}
	if err == nil && values["posted"] == "" {
		err = errors.New("--posted is required")
	}
	if err == nil {
		err = auditDraftFingerprintName(values["fingerprint"])
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, auditDraftUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit drafts mark: error: %v\n", err)
		return usageExit
	}
	cfg := coreDefaults(e)
	release, err := auditDraftLock(e, cfg)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit drafts mark: error: %v\n", err)
		return 1
	}
	defer release()
	path := filepath.Join(auditDraftDir(e, cfg), values["fingerprint"]+".json")
	doc, err := auditDraftLoad(path)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit drafts mark: error: %v\n", err)
		return 1
	}
	doc.State = auditDraftStatePosted
	doc.Posted = values["posted"]
	if err := auditDraftSave(path, doc); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit drafts mark: error: %v\n", err)
		return 1
	}
	return 0
}
