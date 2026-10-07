package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// improveBundleSchema names the document crw manage improve collect writes.
const improveBundleSchema = "crw-improve-bundle/1"

// The source kinds of a bundle, and the two states a source row carries. A source the
// configuration does not name is missing; a configured path that cannot be read is the
// named error improveReasonSourceUnreadable.
const (
	improveKindRelay        = "relay"
	improveKindRefusal      = "refusal"
	improveKindFault        = "fault"
	improveKindGeneration   = "generation"
	improveKindSplit        = "split"
	improveKindCriteria     = "criteria"
	improveKindDag          = "dag"
	improveKindAudit        = "audit"
	improveKindDraft        = "draft"
	improveKindIntervention = "intervention"
	improveKindIssue        = "issue"
)

const (
	improveStateRead    = "read"
	improveStateMissing = "missing"
)

// improveReasonSourceUnreadable is the named error of a configured source whose path
// cannot be read. A source the configuration does not name is not an error: it stays in
// the bundle as missing.
const improveReasonSourceUnreadable = "improve_source_unreadable"

// The named refusals of an output path: its parent directory is missing or cannot be
// resolved, or the destination is an existing symbolic link, or the destination is a file the
// collection itself opens.
const (
	improveReasonOutputParent  = "improve_output_parent_missing"
	improveReasonOutputSymlink = "improve_output_symlink"
	improveReasonOutputIsInput = "improve_output_is_input"
)

// improveInputBeforeRename runs between the output plan taken immediately before the rename
// and the refusal check that follows it. Production leaves it nil; a test sets it to replace
// the destination inside that window and prove the second comparison refuses the new one.
var improveInputBeforeRename func(improveOutputPlan)

// improveStoreFile is the relay store's file name inside a state directory.
const improveStoreFile = "relay.sqlite3"

// improveEvidenceIssuePrefix marks the evidence entry that names the issue a record belongs to,
// rather than a place the friction itself was seen. A split whose relationship carries no scope
// keeps its issue key this way, so the key is not mistaken for a project or for an occurrence.
const improveEvidenceIssuePrefix = "issue:"

// improveEvidenceAnswerPrefix marks the evidence entry that names the decision reply which answered
// a blockage. The reply's own record is folded into the blockage's, so the reply's event is cited
// as the source of the reason rather than counted as a second occurrence.
const improveEvidenceAnswerPrefix = "answer:"

// improveStoreTimeout bounds the read-only store open.
const improveStoreTimeout = 5 * time.Second

// improveSection is the configuration section of this feature: the per-kind sources and
// the path of the issue list a management session exported.
type improveSection struct {
	Sources      map[string]improveSourceConfig `json:"sources"`
	IssueList    string                         `json:"issue_list"`
	MaxNewDrafts int                            `json:"max_new_drafts"`
}

// improveSourceConfig is one configured source: a path and, where the source needs it, a
// pattern naming what to read from it.
type improveSourceConfig struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
}

// improveBundle is the crw-improve-bundle/1 document: where the evidence came from, and
// the normalized records read out of it.
type improveBundle struct {
	Schema  string             `json:"schema"`
	Sources []improveSourceRow `json:"sources"`
	Records []improveRecord    `json:"records"`
}

// improveSourceRow is one source: its kind, the path it was read from, its state, and how
// many raw rows it contributed.
type improveSourceRow struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	State string `json:"state"`
	Rows  int    `json:"rows"`
}

// improveRecord is one normalized record: what kind of friction it is, its identity, where
// it happened, what it was, how many rows it merges, the first and last time it was seen,
// the origin locations of the rows behind it, and the draft fingerprint a source already
// recorded for the row when it carries one.
type improveRecord struct {
	Kind     string   `json:"kind"`
	Key      string   `json:"key"`
	Where    string   `json:"where"`
	What     string   `json:"what"`
	Count    int      `json:"count"`
	FirstAt  string   `json:"first_at"`
	LastAt   string   `json:"last_at"`
	Evidence []string `json:"evidence"`

	// Fingerprint is the shared draft fingerprint a row already carries, when its source records
	// one. The issue list a management session exports names it, so a candidate whose fingerprint
	// an issue already holds is not proposed again; every other kind leaves it empty.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// improveAccumulator merges records that share a (kind, key, where) identity, so the same
// friction seen many times is one record with a count rather than many records.
type improveAccumulator struct {
	index   map[string]int
	records []improveRecord
}

// improveNewAccumulator is an empty accumulator.
func improveNewAccumulator() *improveAccumulator {
	return &improveAccumulator{index: map[string]int{}}
}

// improveAdd merges one record. A record's what is kept from the first row of its identity,
// so the value does not depend on which row a later read happened to reach first. The
// identity is a JSON array rather than joined text, so a key that carries a delimiter
// cannot make two different identities read as one. The two times are compared as the
// instants they name, so a fractional second or a different offset cannot reverse them.
func (a *improveAccumulator) improveAdd(r improveRecord) {
	identity := improveIdentity(r.Kind, r.Key, r.Where)
	if at, ok := a.index[identity]; ok {
		current := &a.records[at]
		current.Count += r.Count
		if improveParseEarlier(r.FirstAt, current.FirstAt) {
			current.FirstAt = r.FirstAt
		}
		if improveParseLater(r.LastAt, current.LastAt) {
			current.LastAt = r.LastAt
		}
		if current.What == "" || (improveGenericReason(current.What) && !improveGenericReason(r.What)) {
			current.What = r.What
		}
		// A row that carries the fingerprint keeps it, so a record merged from several rows still
		// names the draft its source already holds.
		if current.Fingerprint == "" {
			current.Fingerprint = r.Fingerprint
		}
		current.Evidence = append(current.Evidence, r.Evidence...)
		return
	}
	a.index[identity] = len(a.records)
	a.records = append(a.records, r)
}

// improveParseInstant parses a stored time as the instant it names. The second result is false
// when the text is empty or is not an RFC3339Nano instant this build can read.
func improveParseInstant(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// improveParseEarlier reports whether candidate is the earlier of two stored times and should
// replace current as a record's first_at. An empty candidate never does and an empty current
// always does. A value that is not an instant goes after one that is, because comparing the text
// instead reverses "…00:00:00.5Z" and "…00:00:00Z": '.' sorts before 'Z'. Two values that are
// both unreadable fall back to their text, so the same input still writes the same bytes.
func improveParseEarlier(candidate, current string) bool {
	if candidate == "" {
		return false
	}
	if current == "" {
		return true
	}
	parsed, ok := improveParseInstant(candidate)
	if !ok {
		if _, currentOK := improveParseInstant(current); currentOK {
			return false
		}
		return candidate < current
	}
	if currentParsed, currentOK := improveParseInstant(current); currentOK {
		return parsed.Before(currentParsed)
	}
	return true
}

// improveParseLater reports whether candidate is the later of two stored times and should replace
// current as a record's last_at. It is improveParseEarlier's mirror, including where a value that
// is not an instant sits: after every value that is one.
func improveParseLater(candidate, current string) bool {
	if candidate == "" {
		return false
	}
	if current == "" {
		return true
	}
	parsed, ok := improveParseInstant(candidate)
	if !ok {
		if _, currentOK := improveParseInstant(current); currentOK {
			return true
		}
		return candidate > current
	}
	if currentParsed, currentOK := improveParseInstant(current); currentOK {
		return parsed.After(currentParsed)
	}
	return false
}

// improveGenericReason reports whether a split record description is only the bare outcome
// name, which a more specific reason from another row of the same record replaces.
func improveGenericReason(reason string) bool {
	switch reason {
	case "blocked_needs_input", "split_approval", "scope_change":
		return true
	}
	return false
}

// improveAnnotate appends evidence to an existing record and reports whether it was there,
// so one source can mark a record another created without counting it twice.
func (a *improveAccumulator) improveAnnotate(kind, key, where, evidence string) bool {
	at, ok := a.index[improveIdentity(kind, key, where)]
	if !ok {
		return false
	}
	a.records[at].Evidence = append(a.records[at].Evidence, evidence)
	return true
}

// improveIdentity is the merge key of a record: the three fields as a JSON array, which is
// unambiguous whatever characters the fields carry.
func improveIdentity(kind, key, where string) string {
	data, err := json.Marshal([]string{kind, key, where})
	if err != nil {
		return kind + "\n" + key + "\n" + where
	}
	return string(data)
}

// improveFinish sorts the records by (kind, key, where) and each record's evidence, and
// removes duplicate evidence, so the same input always writes the same bytes.
func (a *improveAccumulator) improveFinish() []improveRecord {
	records := a.records
	if records == nil {
		records = []improveRecord{}
	}
	for i := range records {
		records[i].Evidence = improveSortedEvidence(records[i].Evidence)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Kind != records[j].Kind {
			return records[i].Kind < records[j].Kind
		}
		if records[i].Key != records[j].Key {
			return records[i].Key < records[j].Key
		}
		return records[i].Where < records[j].Where
	})
	return records
}

// improveSortedEvidence is the evidence of one record, deduplicated and sorted.
func improveSortedEvidence(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

// improveRunCollect is crw manage improve collect. It reads every configured source and
// writes one bundle: to --out FILE, or to stdout when the option is absent.
func improveRunCollect(ctx context.Context, e *Env, args []string) int {
	out, help, err := improveParseArgs(args)
	if help {
		fmt.Fprintln(e.Stdout, improveUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, improveUsage)
		fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
		return usageExit
	}
	section, err := improveLoadSection(e)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
		return 1
	}
	if out != "" {
		plan, err := improvePlanOutput(out)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
			return 1
		}
		if err := improveRefuseInputOutput(plan.Dest, section); err != nil {
			fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
			return 1
		}
	}
	bundle, err := improveCollect(ctx, section)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
		return 1
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
		return 1
	}
	data = append(data, '\n')
	// A context that ended while the sources were read produces no bundle at all, so an
	// interrupted run cannot look like a completed one.
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
		return 1
	}
	if out == "" {
		if _, err := e.Stdout.Write(data); err != nil {
			fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
			return 1
		}
		return 0
	}
	if err := improveWriteFile(out, data, section); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
		return 1
	}
	return 0
}

// improveOutputPlan is a resolved output destination: the parent directory with its
// symlinks followed, and the destination built from it. The guard and the write use the
// same plan, so a destination that does not exist yet cannot slip past the containment
// check through an unresolved spelling.
type improveOutputPlan struct {
	Dest   string
	Parent string
}

// improvePlanOutput resolves an output path. The parent directory must exist and is
// resolved with filepath.EvalSymlinks; a destination that already exists and is a symbolic
// link is refused, because the rename would replace whatever it points at.
func improvePlanOutput(out string) (improveOutputPlan, error) {
	absolute, err := filepath.Abs(out)
	if err != nil {
		return improveOutputPlan{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return improveOutputPlan{}, fmt.Errorf("%s: %s: %w", improveReasonOutputParent, out, err)
	}
	dest := filepath.Join(parent, filepath.Base(absolute))
	if info, err := os.Lstat(dest); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return improveOutputPlan{}, fmt.Errorf("%s: %s", improveReasonOutputSymlink, out)
	}
	return improveOutputPlan{Dest: dest, Parent: parent}, nil
}

// improveRefuseInputOutput refuses a resolved destination that is an input the collection
// opens or lies under one. Both sides are resolved, and an existing pair is also compared
// with os.SameFile, so a hard link cannot pass the spelling comparison. The inputs include
// the store file inside a configured relay or DAG directory, which is the file the collection
// actually opens there and is not the configured path itself.
func improveRefuseInputOutput(dest string, section improveSection) error {
	for _, source := range improveInputPaths(section) {
		if source == "" {
			continue
		}
		resolved, err := improveResolvedPath(source)
		if err != nil {
			// An unresolvable source is reported by the source read itself.
			continue
		}
		if dest == resolved || strings.HasPrefix(dest, improvePrefix(resolved)) {
			return fmt.Errorf("%s: the output %s is the input %s the collection opens: a bundle never overwrites its own evidence", improveReasonOutputIsInput, dest, source)
		}
		if same, err := improveSameFile(dest, resolved); err == nil && same {
			return fmt.Errorf("%s: the output %s is the input %s the collection opens: a bundle never overwrites its own evidence", improveReasonOutputIsInput, dest, source)
		}
	}
	return nil
}

// improvePrefix is the directory prefix a containment check compares against: the resolved
// directory with one trailing separator. A directory that is already a separator (the
// filesystem root) keeps it, so every absolute path is under it; appending another would
// build a prefix no cleaned path carries and let every destination through.
func improvePrefix(dir string) string {
	if strings.HasSuffix(dir, string(filepath.Separator)) {
		return dir
	}
	return dir + string(filepath.Separator)
}

// improveSameFile reports whether two paths name the same existing file.
func improveSameFile(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ai, bi), nil
}

// improveInputPaths is every path the collection opens: every path the configuration names,
// plus the files it reaches that the configuration does not name. For a relay or DAG source
// that is the store file inside its directory, and for either source the database's own
// write-ahead log and shared-memory index when they exist, because a store read examines them
// and reads their committed frames. For a drafts directory, every entry the collection would
// read, so a link to a file outside the directory is an input too.
func improveInputPaths(section improveSection) []string {
	paths := make([]string, 0, len(section.Sources)+8)
	for _, source := range section.Sources {
		paths = append(paths, source.Path)
	}
	paths = append(paths, section.IssueList)
	for _, kind := range []string{improveKindRelay, improveKindDag} {
		source := section.Sources[kind]
		if source.Path == "" {
			continue
		}
		opened, err := improveStorePath(source.Path)
		if err != nil {
			// A source path that cannot be resolved is reported by the source read itself.
			continue
		}
		// The sidecars are the ones beside the resolved store, which is the file the read
		// examines them next to (store.InPlaceRead resolves the path the same way).
		resolved, err := improveResolvedPath(opened)
		if err != nil {
			resolved = opened
		}
		paths = append(paths, opened, resolved+"-wal", resolved+"-shm")
	}
	// A drafts source that is a directory is enumerated, so every entry the collection would
	// read is an input; a link to a file elsewhere is reached that way.
	if drafts := section.Sources[improveKindDraft].Path; drafts != "" {
		if info, err := os.Stat(drafts); err == nil && info.IsDir() {
			if entries, err := os.ReadDir(drafts); err == nil {
				for _, entry := range entries {
					if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
						paths = append(paths, filepath.Join(drafts, entry.Name()))
					}
				}
			}
		}
	}
	return paths
}

// improveResolvedPath is a path with its symlinks followed, so two spellings of one file
// compare equal. A path that does not exist yet is its resolved parent directory joined with
// its final name, so a name reached through a symlinked directory still compares equal to the
// same name spelled through that directory's target; EvalSymlinks alone fails on the missing
// final component and would leave the parent's symlinks unresolved. A path whose parent cannot
// be resolved either keeps its absolute spelling, which the source read reports.
func improveResolvedPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	parent, parentErr := filepath.EvalSymlinks(filepath.Dir(absolute))
	if parentErr != nil {
		return absolute, nil
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

// improveResolvedPathOr is improveResolvedPath for an origin that must still name a file when the
// path cannot be resolved at all: the caller's own spelling is kept rather than dropping the
// origin, because a record with no location would fall back to a sighting of its where.
func improveResolvedPathOr(path string) string {
	resolved, err := improveResolvedPath(path)
	if err != nil {
		return path
	}
	return resolved
}

// improveWriteFile writes the bundle in the resolved parent directory and renames it onto
// the resolved destination. The temporary file is fsynced, and the refusal check runs again
// immediately before the rename, so a parent replaced between the first check and the write
// is still caught.
func improveWriteFile(out string, data []byte, section improveSection) error {
	plan, err := improvePlanOutput(out)
	if err != nil {
		return err
	}
	if err := improveRefuseInputOutput(plan.Dest, section); err != nil {
		return err
	}
	temp, err := os.CreateTemp(plan.Parent, "improve-bundle-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	fresh, err := improvePlanOutput(out)
	if err != nil {
		os.Remove(name)
		return err
	}
	if improveInputBeforeRename != nil {
		improveInputBeforeRename(fresh)
	}
	if err := improveRefuseInputOutput(fresh.Dest, section); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, fresh.Dest); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// improveParseArgs reads --out FILE and the help flags.
func improveParseArgs(args []string) (out string, help bool, err error) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-h" || args[i] == "--help":
			return "", true, nil
		case args[i] == "--out":
			if i+1 >= len(args) {
				return "", false, errors.New("the option --out needs a value")
			}
			i++
			out = args[i]
			if out == "" {
				return "", false, errors.New("the option --out needs a value")
			}
		case strings.HasPrefix(args[i], "--out="):
			out = strings.TrimPrefix(args[i], "--out=")
			if out == "" {
				return "", false, errors.New("the option --out needs a value")
			}
		default:
			return "", false, fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	return out, false, nil
}

// improveLoadSection reads this feature's configuration. The accepted decision puts it in
// the crw configuration file's manage section, key improve; the issue body names the
// section improve, so a file that carries no manage section is read at the top level.
func improveLoadSection(e *Env) (improveSection, error) {
	file, err := crwconfig.Load(e.Getenv, "")
	if err != nil {
		return improveSection{}, err
	}
	var manage map[string]json.RawMessage
	if err := file.Section("manage", &manage); err != nil {
		return improveSection{}, err
	}
	var section improveSection
	// A file with no manage section at all is read at the top level, because the issue body
	// names the section improve. A file whose manage section carries no improve key names no
	// source, so a stale top-level improve section is never read instead.
	if manage == nil {
		if err := file.Section("improve", &section); err != nil {
			return improveSection{}, err
		}
		return section, nil
	}
	if raw, ok := manage["improve"]; ok {
		if err := json.Unmarshal(raw, &section); err != nil {
			return improveSection{}, err
		}
	}
	return section, nil
}

// improveCollect reads every source and returns the bundle. It needs no Env of its own: the
// relay and DAG sources are read from the store file each names, and every other source is a
// file the configuration names.
func improveCollect(ctx context.Context, section improveSection) (improveBundle, error) {
	acc := improveNewAccumulator()
	sources := []improveSourceRow{}

	relayPath := section.Sources[improveKindRelay].Path
	if relayPath == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindRelay, State: improveStateMissing})
	} else {
		dbPath, err := improveStorePath(relayPath)
		if err != nil {
			return improveBundle{}, improveUnreadable(improveKindRelay, relayPath, err)
		}
		rows, err := improveReadRelay(ctx, dbPath, acc)
		if err != nil {
			return improveBundle{}, improveUnreadable(improveKindRelay, relayPath, err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindRelay, Path: relayPath, State: improveStateRead, Rows: rows})
	}

	dagSource := section.Sources[improveKindDag]
	if dagSource.Path == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindDag, State: improveStateMissing})
	} else {
		dbPath, err := improveStorePath(dagSource.Path)
		if err != nil {
			return improveBundle{}, improveUnreadable(improveKindDag, dagSource.Path, err)
		}
		rows, err := improveReadDag(ctx, dbPath, dagSource, acc)
		if err != nil {
			return improveBundle{}, improveUnreadable(improveKindDag, dagSource.Path, err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindDag, Path: dagSource.Path, State: improveStateRead, Rows: rows})
	}

	auditPath := section.Sources[improveKindAudit].Path
	if auditPath == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindAudit, State: improveStateMissing})
	} else {
		rows, err := improveReadAudit(auditPath, acc)
		if err != nil {
			return improveBundle{}, improveUnreadable(improveKindAudit, auditPath, err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindAudit, Path: auditPath, State: improveStateRead, Rows: rows})
	}

	interventionPath := section.Sources[improveKindIntervention].Path
	if interventionPath == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindIntervention, State: improveStateMissing})
	} else {
		rows, err := improveReadInterventions(interventionPath, acc)
		if err != nil {
			return improveBundle{}, improveUnreadable(improveKindIntervention, interventionPath, err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindIntervention, Path: interventionPath, State: improveStateRead, Rows: rows})
	}

	draftPath := section.Sources[improveKindDraft].Path
	if draftPath == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindDraft, State: improveStateMissing})
	} else {
		rows, err := improveReadDrafts(draftPath, acc)
		if err != nil {
			return improveBundle{}, improveUnreadable(improveKindDraft, draftPath, err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindDraft, Path: draftPath, State: improveStateRead, Rows: rows})
	}

	if section.IssueList == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindIssue, State: improveStateMissing})
	} else {
		rows, err := improveReadIssues(section.IssueList, acc)
		if err != nil {
			return improveBundle{}, improveUnreadable(improveKindIssue, section.IssueList, err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindIssue, Path: section.IssueList, State: improveStateRead, Rows: rows})
	}

	return improveBundle{Schema: improveBundleSchema, Sources: sources, Records: acc.improveFinish()}, nil
}

// improveUnreadable is the named error of a configured source whose path cannot be read.
func improveUnreadable(kind, path string, err error) error {
	return fmt.Errorf("%s: %s %s: %w", improveReasonSourceUnreadable, kind, path, err)
}

// improveStorePath is the relay store file a configured source names: the path itself when
// it is a file, or the store inside it when it is a directory.
func improveStorePath(configured string) (string, error) {
	info, err := os.Stat(configured)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return filepath.Join(configured, improveStoreFile), nil
	}
	return configured, nil
}

// improveStoreIdentity is the stable token one store's rows are read under, so an occurrence's
// origin names the store it came from: the store's own store_id, or the file it was opened from
// when the store carries none. A row number is unique only inside one store, so two stores can
// carry the same reason at the same row number and, without this token, the second store's row
// would read as an occurrence already seen.
//
// fallback is the store file the caller resolved before the snapshot, because the Store a
// ReadSnapshot hands its reader carries no path of its own: deriving the file from the reader's
// working directory would name every pathless store by the same directory.
func improveStoreIdentity(ctx context.Context, s *store.Store, fallback string) (string, error) {
	rows, err := s.All(ctx, "SELECT value FROM schema_meta WHERE key = 'store_id'")
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if id := strings.TrimSpace(row.Text("value")); id != "" {
			return id, nil
		}
	}
	// A store whose identity row is missing is named by the file it was read from, so two
	// different files still read as two different origins.
	if resolved, err := improveResolvedPath(fallback); err == nil {
		return resolved, nil
	}
	return fallback, nil
}

// improveReadRelay reads the relay store read-only and normalizes its refusal, fault,
// generation and split rows. The store is opened with store.OpenInPlace, which never
// creates a -wal or -shm sidecar, and read through ReadSnapshot's one deferred snapshot.
func improveReadRelay(ctx context.Context, dbPath string, acc *improveAccumulator) (int, error) {
	read, err := store.OpenInPlace(ctx, dbPath, improveStoreTimeout)
	if err != nil {
		return 0, err
	}
	defer func() { _ = read.Close() }()
	// The store file, resolved once, names a store whose own identity row is missing. The Store a
	// snapshot hands its reader carries no path, so it cannot be taken from there.
	storeFile, err := improveResolvedPath(dbPath)
	if err != nil {
		storeFile = dbPath
	}
	rows := 0
	err = read.ReadSnapshot(ctx, func(ctx context.Context, s *store.Store) error {
		// An occurrence's origin has to name the store it was read from. A row number is unique
		// only inside one store: two stores can carry the same reason at the same row number, and
		// without the store the second one's row reads as an occurrence already seen.
		token, err := improveStoreIdentity(ctx, s, storeFile)
		if err != nil {
			return err
		}
		refusals, err := s.All(ctx, "SELECT id, at, relationship_id, reason, detail FROM refusals ORDER BY id")
		if err != nil {
			return err
		}
		rows += len(refusals)
		for _, row := range refusals {
			reason := row.Text("reason")
			acc.improveAdd(improveRecord{Kind: improveKindRefusal, Key: reason, What: reason, Count: 1,
				FirstAt: row.Text("at"), LastAt: row.Text("at"),
				Evidence: []string{fmt.Sprintf("refusals:%s:%d", token, improveRowInt(row, "id"))}})
		}

		faults, err := s.All(ctx, "SELECT fault_id, fault_class, signature, scope_key, occurrence_count, first_seen_at, last_seen_at FROM fault_ledger ORDER BY fault_id")
		if err != nil {
			return err
		}
		rows += len(faults)
		for _, row := range faults {
			acc.improveAdd(improveRecord{Kind: improveKindFault, Key: row.Text("fault_class"), Where: row.Text("scope_key"),
				What: row.Text("signature"), Count: improveRowInt(row, "occurrence_count"),
				FirstAt: row.Text("first_seen_at"), LastAt: row.Text("last_seen_at"),
				Evidence: []string{"fault:" + token + ":" + row.Text("fault_id")}})
		}

		generations, err := s.All(ctx, "SELECT relationship_id, execution_generation, reason, opened_at FROM generations ORDER BY relationship_id, execution_generation")
		if err != nil {
			return err
		}
		rows += len(generations)
		for _, row := range generations {
			reason := row.Text("reason")
			acc.improveAdd(improveRecord{Kind: improveKindGeneration, Key: reason, Where: row.Text("relationship_id"),
				What: reason, Count: 1, FirstAt: row.Text("opened_at"), LastAt: row.Text("opened_at"),
				Evidence: []string{fmt.Sprintf("generations:%s:%s:%d", token, row.Text("relationship_id"), improveRowInt(row, "execution_generation"))}})
		}

		// The criteria-registration round trip is in the journal: every registration records
		// the set digest and its size, so a re-registered relationship shows every set it has
		// worked under, not only the one canonical_criteria currently holds.
		registered, err := s.All(ctx, "SELECT seq, subject, detail, at FROM journal WHERE kind = 'criteria_registered' ORDER BY seq")
		if err != nil {
			return err
		}
		rows += len(registered)
		for _, row := range registered {
			detail := improveParseJSONObject(row.Text("detail"))
			digest := improveStringField(detail, "setDigest")
			size, ok := improveNumberField(detail, "count")
			if !ok {
				size = 0
			}
			acc.improveAdd(improveRecord{Kind: improveKindCriteria, Key: row.Text("subject"), Where: digest,
				What: "registered", Count: size,
				FirstAt: row.Text("at"), LastAt: row.Text("at"),
				Evidence: []string{fmt.Sprintf("journal:%s:%d", token, improveRowInt(row, "seq"))}})
		}

		// The current set is what canonical_criteria holds; a digest the journal already carries
		// gains that evidence, and one it does not (a pruned journal) becomes its own record.
		criteria, err := s.All(ctx, "SELECT relationship_id, set_digest, COUNT(*) AS criteria_count, MIN(recorded_at) AS first_at, MAX(recorded_at) AS last_at FROM canonical_criteria GROUP BY relationship_id, set_digest ORDER BY relationship_id, set_digest")
		if err != nil {
			return err
		}
		rows += len(criteria)
		for _, row := range criteria {
			rid, digest := row.Text("relationship_id"), row.Text("set_digest")
			evidence := "canonical_criteria:" + token + ":" + rid + ":" + digest
			if acc.improveAnnotate(improveKindCriteria, rid, digest, evidence) {
				continue
			}
			acc.improveAdd(improveRecord{Kind: improveKindCriteria, Key: rid, Where: digest,
				What: "current_set", Count: improveRowInt(row, "criteria_count"),
				FirstAt: row.Text("first_at"), LastAt: row.Text("last_at"),
				Evidence: []string{evidence}})
		}

		splits, err := s.All(ctx, "SELECT e.event_id, e.relationship_id, COALESCE(r.issue_key,'') AS issue_key, e.outcome, e.receipt, e.first_seen_at, e.last_seen_at, COALESCE(s.project_key,'') AS project_key FROM events e LEFT JOIN relationships r ON r.relationship_id = e.relationship_id LEFT JOIN relationship_scope s ON s.relationship_id = e.relationship_id WHERE e.stage = 'final' AND e.outcome IN ('blocked_needs_input','decision_reply') ORDER BY e.event_id")
		if err != nil {
			return err
		}
		// A child's blocked_needs_input receipt carries no reason field: receiptFields in
		// internal/relay/store/receipt_shape.go allows none, so reading one there found no
		// reason for any real blockage. The reason is the note of the decision reply that
		// answered the event, and that receipt names the answered event in its answersEvent, so
		// one prepass indexes the replies before the blocked rows are read. The same prepass
		// records which events are blockages: a reply that answered one of them is that
		// blockage's answer, not a second record, so one blockage stays one record.
		answered := map[improveParseBlockedKey]string{}
		blocked := map[improveParseBlockedKey]bool{}
		// answerEvents maps a blocked event to the decision reply that answered it, so the
		// blockage's record can cite the reply whose note gave it its reason.
		answerEvents := map[improveParseBlockedKey]string{}
		for _, row := range splits {
			key := improveParseBlockedKey{relationship: row.Text("relationship_id"), event: row.Text("event_id")}
			if row.Text("outcome") == "blocked_needs_input" {
				blocked[key] = true
				continue
			}
			receipt := improveParseJSONObject(row.Text("receipt"))
			event := improveStringField(receipt, "answersEvent")
			if event == "" {
				continue
			}
			answered[improveParseBlockedKey{relationship: key.relationship, event: event}] = improveStringField(receipt, "note")
			answerEvents[improveParseBlockedKey{relationship: key.relationship, event: event}] = key.event
		}
		for _, row := range splits {
			outcome, receipt := row.Text("outcome"), improveParseJSONObject(row.Text("receipt"))
			relationship := row.Text("relationship_id")
			project := row.Text("project_key")
			issue := row.Text("issue_key")
			var reason string
			if outcome == "decision_reply" {
				decision := improveStringField(receipt, "decision")
				if decision != "split_approval" && decision != "scope_change" {
					continue
				}
				// A reply whose answersEvent names a stored blockage is that blockage's own
				// record: the blocked row already carries the answer's reason, so this row
				// contributes no record of its own and one blockage counts once.
				if event := improveStringField(receipt, "answersEvent"); event != "" {
					if blocked[improveParseBlockedKey{relationship: relationship, event: event}] {
						continue
					}
				}
				reason = improveStringField(receipt, "reason", "detail", "note", "question", "summary")
				if reason == "" {
					reason = improveStringField(receipt, "outcome", "decision")
				}
			} else {
				reason = answered[improveParseBlockedKey{relationship: relationship, event: row.Text("event_id")}]
				if reason == "" {
					reason = string(store.BlockedNeedsInput)
				}
			}
			// The record's identity is the project the issue belongs to; a relationship with no
			// scope leaves it empty, and the issue key stays in the record's evidence rather than
			// taking the project's place.
			key := improveSplitKey(project)
			evidence := []string{"events:" + token + ":" + row.Text("event_id")}
			// The reply that answered this blockage is cited as the source of the reason the
			// record carries, so the reason is substantiated rather than asserted.
			if answer := answerEvents[improveParseBlockedKey{relationship: relationship, event: row.Text("event_id")}]; answer != "" {
				evidence = append(evidence, improveEvidenceAnswerPrefix+answer)
			}
			if key == "" && issue != "" {
				evidence = append(evidence, improveEvidenceIssuePrefix+issue)
			}
			rows++
			acc.improveAdd(improveRecord{Kind: improveKindSplit, Key: key, Where: relationship,
				What: reason, Count: 1, FirstAt: row.Text("first_seen_at"), LastAt: row.Text("last_seen_at"),
				Evidence: evidence})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rows, nil
}

// improveSplitKey is a split record's identity: the project key the issue belongs to, or the
// empty string when the relationship carries no scope. An issue key is never a project, so it
// never takes this place; the record's evidence keeps it instead. The relationship it happened at
// stays in the record's where, so one relationship is one record.
func improveSplitKey(project string) string {
	return strings.TrimSpace(project)
}

// improveParseBlockedKey names one blocked event inside one relationship. The two fields are
// compared as themselves rather than joined, so a value that carries a delimiter cannot make two
// different events read as one.
type improveParseBlockedKey struct {
	relationship string
	event        string
}

// improveReadDag reads each plan the source's pattern names out of the store at dbPath and
// normalizes each metric of the measurements into one record.
//
// The store is opened with store.OpenInPlace and read inside ReadSnapshot, the rule decision
// 36 states for a tool that reads the operational store: a read creates no -wal or -shm beside
// the database, and one whose committed state cannot be read that way is an error rather than
// a repair. The relay CLI is not called: its own read path opens mode=ro, which can create
// both sidecars. The scheduler reads through the snapshot's own connection, so a plan is read
// from one state.
func improveReadDag(ctx context.Context, dbPath string, source improveSourceConfig, acc *improveAccumulator) (int, error) {
	plans := improvePatterns(source.Pattern)
	if len(plans) == 0 {
		return 0, errors.New("the dag source names no plan pattern")
	}
	read, err := store.OpenInPlace(ctx, dbPath, improveStoreTimeout)
	if err != nil {
		return 0, err
	}
	defer func() { _ = read.Close() }()
	rows := 0
	err = read.ReadSnapshot(ctx, func(ctx context.Context, st *store.Store) error {
		scheduler := &dagsched.Scheduler{Store: st}
		for _, plan := range plans {
			measured, err := scheduler.Measure(ctx, st.Q(ctx), plan)
			if err != nil {
				return err
			}
			// The measurements are read into the document dag-measurements prints, so a
			// metric's name, values and absence reason reach the record as they did when the
			// relay CLI answered.
			printed, err := pyjson.Encode(measured.Object(), pyjson.Options{})
			if err != nil {
				return err
			}
			doc := improveParseJSONObject(string(printed))
			if doc == nil {
				return errors.New("the dag measurements did not read as a JSON document")
			}
			planID := improveStringField(doc, "plan_id")
			if planID == "" {
				planID = plan
			}
			names := make([]string, 0, len(doc))
			for name := range doc {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				metric, ok := doc[name].(map[string]any)
				if !ok {
					continue
				}
				samples, ok := improveNumberField(metric, "samples")
				if !ok {
					continue
				}
				values, err := json.Marshal(metric)
				if err != nil {
					return err
				}
				rows++
				acc.improveAdd(improveRecord{Kind: improveKindDag, Key: planID + ":" + name, Where: planID,
					What: string(values), Count: samples,
					Evidence: []string{"store:dag-measurements --plan " + plan}})
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rows, nil
}

// improvePatterns splits a source pattern into the names it carries.
func improvePatterns(pattern string) []string {
	var out []string
	for _, part := range strings.Split(pattern, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// improveReadAudit reads the audit ledger, one JSON line per graded result, and normalizes
// it by issue.
func improveReadAudit(path string, acc *improveAccumulator) (int, error) {
	lines, err := improveReadJSONLines(path)
	if err != nil {
		return 0, err
	}
	origin := improveResolvedPathOr(path)
	for i, line := range lines {
		acc.improveAdd(improveRecord{Kind: improveKindAudit, Key: improveStringField(line, "issue"),
			Where: improveStringField(line, "subject"), What: improveStringField(line, "status"), Count: 1,
			FirstAt: improveStringField(line, "graded_at"), LastAt: improveStringField(line, "graded_at"),
			Evidence: []string{fmt.Sprintf("%s:%d", origin, i+1)}})
	}
	return len(lines), nil
}

// improveReadInterventions reads the management intervention records, one JSON line per
// intervention, and normalizes them by the signal they observed.
func improveReadInterventions(path string, acc *improveAccumulator) (int, error) {
	lines, err := improveReadJSONLines(path)
	if err != nil {
		return 0, err
	}
	// The origin names the resolved file, so a second source spelling that reaches the same file
	// through a link is the same location rather than a new occurrence.
	origin := improveResolvedPathOr(path)
	for i, line := range lines {
		signal := improveStringField(line, "signal", "kind", "case")
		acc.improveAdd(improveRecord{Kind: improveKindIntervention, Key: signal, What: signal, Count: 1,
			FirstAt: improveStringField(line, "at", "date", "recorded_at"), LastAt: improveStringField(line, "at", "date", "recorded_at"),
			Evidence: []string{fmt.Sprintf("%s:%d", origin, i+1)}})
	}
	return len(lines), nil
}

// improveReadDrafts reads the audit drafts a management session keeps: one crw-issue-draft/1
// document per draft, under a directory or as a single file. A candidate that decodes to an object
// of another schema — the directory's index.json, or any other document — is skipped and not
// counted, so the listing is never read as a draft of its own. Each draft becomes one record keyed
// by its fingerprint, whose first and last sighting are the earliest and latest seen[].at. A
// document whose schema is right but whose fingerprint is empty is refused, because a record keyed
// on nothing would merge two different drafts.
//
// A candidate that is not a JSON object at all is refused rather than skipped: it is not a
// document of another schema, so reading it as nothing would report a complete bundle over a file
// the drafts directory could not be read from. That is the rule improveReadJSONLines already
// applies to a null line, and it is what this reader did before the schema gate existed.
func improveReadDrafts(path string, acc *improveAccumulator) (int, error) {
	files, err := improveParseDraftFiles(path)
	if err != nil {
		return 0, err
	}
	rows := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return 0, err
		}
		schema, err := improveParseDraftSchema(file, data)
		if err != nil {
			return 0, err
		}
		if schema != auditDraftSchema {
			continue
		}
		// auditDraftLoad (CRW-695) is the writer's own reader: it refuses an empty or mismatched
		// fingerprint and a key this build does not know, so a document that claims to be a draft
		// but is not one is named rather than read as something else.
		draft, err := auditDraftLoad(file)
		if err != nil {
			return 0, err
		}
		first, last := improveParseSeenBounds(draft.Seen)
		rows++
		acc.improveAdd(improveRecord{Kind: improveKindDraft, Key: draft.Fingerprint, Where: draft.Project, What: draft.Title, Count: 1,
			FirstAt: first, LastAt: last, Evidence: []string{file}})
	}
	return rows, nil
}

// improveParseDraftFiles is the candidate draft files of a configured path: the .json files
// directly inside a directory, or the path itself when it is a file.
func improveParseDraftFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

// improveParseDraftSchema is the schema a candidate file declares. A file that is not a JSON
// object carries no schema and is refused, because the drafts directory could not be read as what
// it holds rather than as a document of another schema.
func improveParseDraftSchema(path string, data []byte) (string, error) {
	object := improveParseJSONObject(string(data))
	if object == nil {
		return "", fmt.Errorf("%s: not a JSON object", path)
	}
	return improveStringField(object, "schema"), nil
}

// improveParseSeenBounds is the earliest and latest sighting of a draft's seen list, compared as
// instants so a fractional second or a different offset cannot reverse them.
func improveParseSeenBounds(seen []auditDraftSeen) (first, last string) {
	for _, entry := range seen {
		if improveParseEarlier(entry.At, first) {
			first = entry.At
		}
		if improveParseLater(entry.At, last) {
			last = entry.At
		}
	}
	return first, last
}

// improveReadIssues reads the issue list a management session exported: an array of issues,
// or an object carrying them under issues.
func improveReadIssues(path string, acc *improveAccumulator) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	issues, err := improveDecodeIssues(data)
	if err != nil {
		return 0, err
	}
	for _, issue := range issues {
		key := improveStringField(issue, "identifier", "key", "id")
		acc.improveAdd(improveRecord{Kind: improveKindIssue, Key: key, Where: improveStringField(issue, "state"),
			What: improveStringField(issue, "title"), Count: 1,
			// The issue list names the draft fingerprint it was registered under, so a candidate
			// whose fingerprint an issue already holds is suppressed by that fingerprint and not
			// only by its key or its title.
			Fingerprint: improveStringField(issue, "fingerprint"),
			FirstAt:     improveStringField(issue, "createdAt", "created_at"), LastAt: improveStringField(issue, "updatedAt", "updated_at"),
			Evidence: []string{path + ":" + key}})
	}
	return len(issues), nil
}

// improveDecodeIssues accepts the two shapes an exported issue list may carry.
func improveDecodeIssues(data []byte) ([]map[string]any, error) {
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Issues *[]map[string]any `json:"issues"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, err
	}
	if wrapped.Issues == nil {
		return nil, errors.New("the issue list carries neither a top-level array nor an issues array")
	}
	return *wrapped.Issues, nil
}

// improveReadJSONLines reads a JSON-lines file, refusing a line that is not a JSON object.
func improveReadJSONLines(path string) ([]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if object == nil {
			return nil, fmt.Errorf("line %d: not a JSON object", i+1)
		}
		out = append(out, object)
	}
	return out, nil
}

// improveParseJSONObject decodes a JSON object, or returns nil when the text is not one.
func improveParseJSONObject(text string) map[string]any {
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		return nil
	}
	return object
}

// improveStringField is the first named field that is a non-empty string.
func improveStringField(object map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := object[name].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// improveNumberField is the named field as an int, and whether it was a number.
func improveNumberField(object map[string]any, name string) (int, bool) {
	switch value := object[name].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

// improveRowInt is a store row's integer column, or zero.
func improveRowInt(row store.Row, name string) int {
	if value, ok := row.Get(name).(int64); ok {
		return int(value)
	}
	return 0
}
