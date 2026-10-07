package manage

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

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

// improveInputBeforeRename runs after the bundle has been written to its temporary file and
// before the destination is resolved again and compared with the recorded input identities. It is
// the seam a test uses to change the destination inside that window and prove the second
// comparison refuses the new one. Production leaves it nil.
var improveInputBeforeRename func(improveOutputPlan)

// improveOutputBeforeCreate runs after the parent directory has been opened and the output checked,
// and before the temporary file is created on that descriptor. It is the seam a test uses to
// replace the directory the destination's spelling reaches inside that window, and prove the
// temporary file is still created, cleaned up and renamed through the directory that was opened.
// Production leaves it nil.
var improveOutputBeforeCreate func(improveOutputPlan)

// improveInputAfterRead runs after one source's reader has returned and before the identities are
// examined again. It is the seam a test uses to replace an input inside the window the post-read
// examination exists to close, and prove the run refuses there rather than only at the rename.
// Production leaves it nil.
var improveInputAfterRead func()

// improveStoreFile is the relay store's file name inside a state directory.
const improveStoreFile = "relay.sqlite3"

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
// and the origin locations of the rows behind it.
type improveRecord struct {
	Kind     string   `json:"kind"`
	Key      string   `json:"key"`
	Where    string   `json:"where"`
	What     string   `json:"what"`
	Count    int      `json:"count"`
	FirstAt  string   `json:"first_at"`
	LastAt   string   `json:"last_at"`
	Evidence []string `json:"evidence"`
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
	var plan improveOutputPlan
	if out != "" {
		plan, err = improvePlanOutput(out)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
			return 1
		}
		// The first output check runs here, before any source is read: the destination is
		// compared against the inputs the configuration names, resolved the way the readers
		// resolve them. The check before the rename repeats it against the identities the
		// read recorded.
		if err := improveRefuseInputOutput(plan.Dest, section); err != nil {
			fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
			return 1
		}
	}
	// Only a run that writes a file needs the identity set: a run that writes to stdout has no
	// destination to protect, so it opens nothing and holds no descriptor.
	bundle, ids, err := improveCollect(ctx, section, out != "")
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
		return 1
	}
	// The descriptors stay open until the rename is done, so the inode of every input is held
	// and its identity cannot be reused by another file while the bundle is written.
	defer ids.improveIdentityClose()
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
	if err := improveWriteFile(plan, ids, data); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve collect: error: %v\n", err)
		return 1
	}
	return 0
}

// improveOutputPlan is a resolved output destination: the path resolved the way the readers
// resolve theirs, its parent directory, and the parent's identity at the moment the plan was
// taken. The guard and the write use the same plan, so a destination that does not exist yet
// cannot slip past the containment check through an unresolved spelling, and a parent replaced
// between the plan and the rename is caught by comparing the identity recorded here with the one
// read again there.
type improveOutputPlan struct {
	Out    string
	Dest   string
	Parent string
	parent os.FileInfo
}

// improvePlanOutput resolves an output path the way the readers resolve theirs: each component
// from the left, a symbolic link followed where it stands before a later ".." is applied. A
// parent directory that does not exist is refused, and a destination that already exists and is
// a symbolic link is refused too, because the rename would replace whatever it points at. A
// destination that cannot be examined for any reason other than its absence is refused as well:
// a comparison that cannot be made is never a pass.
func improvePlanOutput(out string) (improveOutputPlan, error) {
	// The destination is examined first, and a failure that is not its absence refuses the run: an
	// output the collection cannot examine is one it cannot prove is not an input, and letting the
	// spelling through would let the resolution below step over the part it could not reach.
	if info, err := os.Lstat(out); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return improveOutputPlan{}, fmt.Errorf("%s: %s: %w", improveReasonOutputUnreadable, out, err)
		}
	} else if info.Mode()&os.ModeSymlink != 0 {
		return improveOutputPlan{}, fmt.Errorf("%s: %s", improveReasonOutputSymlink, out)
	}
	dest, err := store.Realpath(out)
	if err != nil {
		return improveOutputPlan{}, err
	}
	parent := filepath.Dir(dest)
	pinfo, err := os.Stat(parent)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return improveOutputPlan{}, fmt.Errorf("%s: %s: %w", improveReasonOutputParent, out, err)
	case err != nil:
		return improveOutputPlan{}, fmt.Errorf("%s: %s: %w", improveReasonOutputUnreadable, out, err)
	case !pinfo.IsDir():
		return improveOutputPlan{}, fmt.Errorf("%s: %s", improveReasonOutputParent, out)
	}
	if info, err := os.Lstat(dest); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return improveOutputPlan{}, fmt.Errorf("%s: %s: %w", improveReasonOutputUnreadable, out, err)
		}
	} else if info.Mode()&os.ModeSymlink != 0 {
		return improveOutputPlan{}, fmt.Errorf("%s: %s", improveReasonOutputSymlink, out)
	}
	return improveOutputPlan{Out: out, Dest: dest, Parent: parent, parent: pinfo}, nil
}

// improveRefuseInputOutput refuses a resolved destination that is an input the collection opens
// or lies under one. The inputs are resolved and recorded the way the collection records the ones
// it reads — the store file the readers actually open, the directories they enumerate and the file
// sources — so a destination is compared against recorded identities rather than against two
// spellings of one name.
func improveRefuseInputOutput(dest string, section improveSection) error {
	ids := improveIdentityNew(true)
	defer ids.improveIdentityClose()
	if err := ids.improveIdentityRecord(section); err != nil {
		return err
	}
	return ids.improveIdentityRefuse(dest, nil)
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

// improveResolvedPath is a path resolved the way the readers resolve theirs: each component from
// the left, a symbolic link followed where it stands before a later ".." is applied. It is
// store.Realpath, the rule store.InPlaceRead applies to a store's path, so a spelling that mixes
// a link and ".." names the file the kernel would open rather than the different file
// filepath.Clean would name.
func improveResolvedPath(path string) (string, error) { return store.Realpath(path) }

// improveOpenDirectory opens a directory for the *at calls this file makes on it: creating the
// temporary file, removing it and renaming it into place. The open needs only search permission on
// the directory, which is what writing a file inside it requires, so a directory that may be
// searched but not read is a valid output location rather than a refusal. The platform headers give
// the search-only bit different names, and named numeric constants keep this one file buildable for
// both release platforms: Linux O_PATH is 0x200000, and Darwin's O_EXEC is 0x40000000.
func improveOpenDirectory(path string) (*os.File, error) {
	const linuxOPath = 0x200000
	const darwinOExec = 0x40000000
	search := linuxOPath
	if runtime.GOOS == "darwin" {
		search = darwinOExec
	}
	fd, err := unix.Open(path, search|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// improveWriteFile writes the bundle in the resolved parent directory and renames it onto the
// resolved destination.
//
// The parent directory is opened once and every step uses that descriptor rather than the spelling
// again: the destination is checked before the temporary file is created, the file is created with
// openat(O_CREAT|O_EXCL), and the write, the fsync, the cleanup and the rename go through the same
// descriptor. A parent replaced under the spelling therefore cannot make the cleanup unlink or the
// rename reach a different directory, which is what left a temporary file behind, or touched an
// input directory, before. The window between the last comparison and the rename itself is
// accepted: nothing closes it without holding the destination's directory against every other
// writer.
//
// The temporary file is created unnamed where the platform supports it, so a refusal releases it by
// closing the descriptor and cannot leave it behind: a removal that has to unlink a name needs the
// output directory's write permission again, and a directory whose permission was taken away after
// the file was created refused that removal and kept the file. The unnamed file is named once, by
// the link immediately before the rename, and a filesystem without the unnamed form keeps the named
// file and its reported cleanup.
func improveWriteFile(plan improveOutputPlan, ids *improveIdentitySet, data []byte) error {
	parent, err := improveOpenDirectory(plan.Parent)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	dirfd := int(parent.Fd())
	// The directory that will actually be written to is the one the descriptor names, so it is
	// compared with the identity the plan recorded before anything is created in it: a spelling
	// that now reaches a different directory is refused here rather than written through.
	held, err := parent.Stat()
	if err != nil {
		return fmt.Errorf("%s: the parent directory of %s could not be examined: %w", improveReasonOutputUnreadable, plan.Out, err)
	}
	if !held.IsDir() {
		return fmt.Errorf("%s: the parent directory of %s is not a directory", improveReasonOutputParent, plan.Out)
	}
	if plan.parent != nil && !os.SameFile(plan.parent, held) {
		return fmt.Errorf("%s: the parent directory of %s is not the directory the plan named", improveReasonOutputParent, plan.Out)
	}
	// The check runs before the temporary file is created, against the identities the read
	// recorded, so a destination that is one of them is refused without writing anything at all.
	if err := ids.improveIdentityRefuse(plan.Dest, held); err != nil {
		return err
	}
	if improveOutputBeforeCreate != nil {
		improveOutputBeforeCreate(plan)
	}
	// The temporary file is created unnamed when the platform has that form. Nothing names it until
	// the link below, so a refusal anywhere after this point releases it by closing the descriptor,
	// whatever the output directory's permissions have become.
	fd, err := improveCreateTemporary(dirfd)
	unnamed := err == nil
	name := ""
	if !unnamed {
		name = "improve-bundle-" + rand.Text()
		fd, err = unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	// named tracks whether the temporary file carries a name on disk. An unnamed file has none until
	// the link immediately before the rename, so a refusal up to that point releases it by closing
	// the descriptor alone; once it has a name, the refusal removes it through the descriptor the
	// file was created on, which a directory replaced under the spelling cannot make miss.
	named := !unnamed
	// discard releases the temporary file and reports the refusal it belongs to. A removal that fails
	// is reported with it, naming the file that was left: a refusal the caller is told about is not
	// one that silently leaves a temporary file in the output directory.
	discard := func(cause error) error {
		_ = file.Close()
		if !named {
			return cause
		}
		if err := unix.Unlinkat(dirfd, name, 0); err != nil {
			return fmt.Errorf("%w (the temporary file %s could not be removed: %v)", cause, name, err)
		}
		return cause
	}
	if _, err := file.Write(data); err != nil {
		return discard(err)
	}
	if err := file.Sync(); err != nil {
		return discard(err)
	}
	// The mode is set on the descriptor, not by spelling the name again.
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return discard(err)
	}
	if !unnamed {
		if err := file.Close(); err != nil {
			return discard(err)
		}
	}
	if improveInputBeforeRename != nil {
		improveInputBeforeRename(plan)
	}
	// The destination is resolved again here, after the bundle has been written and immediately
	// before the last comparison and the rename, so a parent directory replaced while the bundle
	// was written is caught rather than written through. The parent's identity is compared with
	// the one the plan recorded.
	fresh, err := improvePlanOutput(plan.Out)
	if err != nil {
		return discard(err)
	}
	// The rename lands in the directory the descriptor names, so that is the directory the
	// destination has to still be in: a spelling that now reaches somewhere else is refused.
	if fresh.parent != nil && !os.SameFile(held, fresh.parent) {
		return discard(fmt.Errorf("%s: the parent directory of %s is not the directory the plan named", improveReasonOutputParent, plan.Out))
	}
	// The destination is compared first, so an input moved onto the output's place is named as the
	// output it now is (the issue's decided answer 3); a path that reaches a different file without
	// reaching the destination is named as the changed input (answer 2).
	if err := ids.improveIdentityRefuse(fresh.Dest, held); err != nil {
		return discard(err)
	}
	if err := ids.improveIdentityVerify(); err != nil {
		return discard(err)
	}
	// The rename goes through the same descriptor the file was created on, so it lands in the
	// directory that was checked, whatever the spelling now reaches. An unnamed file is given its
	// one name here, immediately before the rename, and that link and the rename both go through the
	// held descriptor.
	if unnamed {
		name = "improve-bundle-" + rand.Text()
		if err := unix.Linkat(fd, "", dirfd, name, improveAtEmptyPath); err != nil {
			return discard(err)
		}
		named = true
	}
	if err := unix.Renameat(dirfd, name, dirfd, filepath.Base(fresh.Dest)); err != nil {
		return discard(err)
	}
	_ = file.Close()
	return nil
}

// improveCreateTemporary creates the bundle's temporary file unnamed in the directory the
// descriptor names, so the file has no name to remove and a refusal releases it by closing the
// descriptor. The unnamed form is Linux's O_TMPFILE; the named numeric constant keeps this file
// buildable for both release platforms, and a platform or filesystem without it reports an error
// and the caller falls back to a named file.
func improveCreateTemporary(dirfd int) (int, error) {
	if runtime.GOOS != "linux" {
		return -1, errors.New("the unnamed temporary file is unavailable on this platform")
	}
	return unix.Openat(dirfd, ".", unix.O_WRONLY|improveOtmpfile|unix.O_CLOEXEC, 0o600)
}

// improveOtmpfile is Linux's O_TMPFILE: the file is created in the directory the descriptor names
// and given no name. The platform headers name it, and the constant keeps this one file buildable
// for the release platforms that do not have it.
const improveOtmpfile = 0x410000

// improveAtEmptyPath names the file a descriptor holds, which is how the unnamed temporary file
// receives its one name immediately before the rename.
const improveAtEmptyPath = 0x1000

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

// improveCollect reads every source and returns the bundle together with the identity of every
// input it opened. It needs no Env of its own: the relay and DAG sources are read from the store
// file each names, and every other source is a file the configuration names.
//
// Every input is opened and its identity recorded before any reader runs, and each descriptor
// stays open until the caller has written and renamed the bundle, so an input moved aside between
// the read and the rename is still recognised for what it is. After each reader returns, that
// source's inputs are recorded again and every recorded path is examined: a draft or a store
// sidecar that appeared while the reader ran is an input too, and a path that no longer names the
// file the collection read refuses the run with improveReasonInputChanged rather than writing a
// bundle of bytes that are no longer there. A source the configuration names but the reader cannot
// read is reported as before; the caller closes the set.
func improveCollect(ctx context.Context, section improveSection, guarded bool) (improveBundle, *improveIdentitySet, error) {
	ids := improveIdentityNew(guarded)
	if err := ids.improveIdentityRecord(section); err != nil {
		ids.improveIdentityClose()
		return improveBundle{}, nil, err
	}
	fail := func(err error) (improveBundle, *improveIdentitySet, error) {
		ids.improveIdentityClose()
		return improveBundle{}, nil, err
	}
	// after records the inputs again and examines them, so a source that appeared or changed while
	// a reader ran is still an input of the set, and one that no longer names the file that was
	// read is refused rather than written over.
	after := func() error {
		if improveInputAfterRead != nil {
			improveInputAfterRead()
		}
		return ids.improveIdentityRefresh(section)
	}
	acc := improveNewAccumulator()
	sources := []improveSourceRow{}

	relayPath := section.Sources[improveKindRelay].Path
	if relayPath == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindRelay, State: improveStateMissing})
	} else {
		dbPath, err := improveStorePath(relayPath)
		if err != nil {
			return fail(improveUnreadable(improveKindRelay, relayPath, err))
		}
		rows, err := improveReadRelay(ctx, dbPath, acc)
		if err != nil {
			return fail(improveUnreadable(improveKindRelay, relayPath, err))
		}
		if err := after(); err != nil {
			return fail(err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindRelay, Path: relayPath, State: improveStateRead, Rows: rows})
	}

	dagSource := section.Sources[improveKindDag]
	if dagSource.Path == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindDag, State: improveStateMissing})
	} else {
		dbPath, err := improveStorePath(dagSource.Path)
		if err != nil {
			return fail(improveUnreadable(improveKindDag, dagSource.Path, err))
		}
		rows, err := improveReadDag(ctx, dbPath, dagSource, acc)
		if err != nil {
			return fail(improveUnreadable(improveKindDag, dagSource.Path, err))
		}
		if err := after(); err != nil {
			return fail(err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindDag, Path: dagSource.Path, State: improveStateRead, Rows: rows})
	}

	auditPath := section.Sources[improveKindAudit].Path
	if auditPath == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindAudit, State: improveStateMissing})
	} else {
		rows, err := improveReadAudit(auditPath, acc)
		if err != nil {
			return fail(improveUnreadable(improveKindAudit, auditPath, err))
		}
		if err := after(); err != nil {
			return fail(err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindAudit, Path: auditPath, State: improveStateRead, Rows: rows})
	}

	interventionPath := section.Sources[improveKindIntervention].Path
	if interventionPath == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindIntervention, State: improveStateMissing})
	} else {
		rows, err := improveReadInterventions(interventionPath, acc)
		if err != nil {
			return fail(improveUnreadable(improveKindIntervention, interventionPath, err))
		}
		if err := after(); err != nil {
			return fail(err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindIntervention, Path: interventionPath, State: improveStateRead, Rows: rows})
	}

	draftPath := section.Sources[improveKindDraft].Path
	if draftPath == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindDraft, State: improveStateMissing})
	} else {
		rows, err := improveReadDrafts(draftPath, acc)
		if err != nil {
			return fail(improveUnreadable(improveKindDraft, draftPath, err))
		}
		if err := after(); err != nil {
			return fail(err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindDraft, Path: draftPath, State: improveStateRead, Rows: rows})
	}

	if section.IssueList == "" {
		sources = append(sources, improveSourceRow{Kind: improveKindIssue, State: improveStateMissing})
	} else {
		rows, err := improveReadIssues(section.IssueList, acc)
		if err != nil {
			return fail(improveUnreadable(improveKindIssue, section.IssueList, err))
		}
		if err := after(); err != nil {
			return fail(err)
		}
		sources = append(sources, improveSourceRow{Kind: improveKindIssue, Path: section.IssueList, State: improveStateRead, Rows: rows})
	}

	return improveBundle{Schema: improveBundleSchema, Sources: sources, Records: acc.improveFinish()}, ids, nil
}

// improveUnreadable is the named error of a configured source whose path cannot be read.
func improveUnreadable(kind, path string, err error) error {
	return fmt.Errorf("%s: %s %s: %w", improveReasonSourceUnreadable, kind, path, err)
}

// improveStorePath is the relay store file a configured source names: the path itself when
// it is a file, or the store inside it when it is a directory. The directory's name is joined
// with crwconfig.JoinRoot, which cleans nothing, so a configured directory spelled through a
// symbolic link and ".." keeps the meaning the kernel gives that spelling instead of the
// different directory filepath.Join's clean would name.
func improveStorePath(configured string) (string, error) {
	info, err := os.Stat(configured)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return crwconfig.JoinRoot(configured, improveStoreFile), nil
	}
	return configured, nil
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
	rows := 0
	err = read.ReadSnapshot(ctx, func(ctx context.Context, s *store.Store) error {
		refusals, err := s.All(ctx, "SELECT id, at, relationship_id, reason, detail FROM refusals ORDER BY id")
		if err != nil {
			return err
		}
		rows += len(refusals)
		for _, row := range refusals {
			reason := row.Text("reason")
			acc.improveAdd(improveRecord{Kind: improveKindRefusal, Key: reason, What: reason, Count: 1,
				FirstAt: row.Text("at"), LastAt: row.Text("at"),
				Evidence: []string{fmt.Sprintf("refusals:%d", improveRowInt(row, "id"))}})
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
				Evidence: []string{"fault:" + row.Text("fault_id")}})
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
				Evidence: []string{fmt.Sprintf("generations:%s:%d", row.Text("relationship_id"), improveRowInt(row, "execution_generation"))}})
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
				Evidence: []string{fmt.Sprintf("journal:%d", improveRowInt(row, "seq"))}})
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
			evidence := "canonical_criteria:" + rid + ":" + digest
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
		// one prepass indexes the replies before the blocked rows are read.
		answered := map[improveParseBlockedKey]string{}
		for _, row := range splits {
			if row.Text("outcome") != "decision_reply" {
				continue
			}
			receipt := improveParseJSONObject(row.Text("receipt"))
			event := improveStringField(receipt, "answersEvent")
			if event == "" {
				continue
			}
			answered[improveParseBlockedKey{relationship: row.Text("relationship_id"), event: event}] = improveStringField(receipt, "note")
		}
		for _, row := range splits {
			outcome, receipt := row.Text("outcome"), improveParseJSONObject(row.Text("receipt"))
			project := row.Text("project_key")
			issue := row.Text("issue_key")
			var reason string
			if outcome == "decision_reply" {
				decision := improveStringField(receipt, "decision")
				if decision != "split_approval" && decision != "scope_change" {
					continue
				}
				reason = improveStringField(receipt, "reason", "detail", "note", "question", "summary")
				if reason == "" {
					reason = improveStringField(receipt, "outcome", "decision")
				}
			} else {
				reason = answered[improveParseBlockedKey{relationship: row.Text("relationship_id"), event: row.Text("event_id")}]
				if reason == "" {
					reason = string(store.BlockedNeedsInput)
				}
			}
			rows++
			acc.improveAdd(improveRecord{Kind: improveKindSplit, Key: improveSplitKey(project, issue), Where: row.Text("relationship_id"),
				What: reason, Count: 1, FirstAt: row.Text("first_seen_at"), LastAt: row.Text("last_seen_at"),
				Evidence: []string{"events:" + row.Text("event_id")}})
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rows, nil
}

// improveSplitKey is a split record's identity: the project key the issue belongs to. The
// relationship it happened at stays in the record's where, so one issue is one record and
// the project key is on every one of them.
func improveSplitKey(project, issue string) string {
	if project != "" {
		return project
	}
	return issue
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
	for i, line := range lines {
		acc.improveAdd(improveRecord{Kind: improveKindAudit, Key: improveStringField(line, "issue"),
			Where: improveStringField(line, "subject"), What: improveStringField(line, "status"), Count: 1,
			FirstAt: improveStringField(line, "graded_at"), LastAt: improveStringField(line, "graded_at"),
			Evidence: []string{fmt.Sprintf("%s:%d", path, i+1)}})
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
	for i, line := range lines {
		signal := improveStringField(line, "signal", "kind", "case")
		acc.improveAdd(improveRecord{Kind: improveKindIntervention, Key: signal, What: signal, Count: 1,
			FirstAt: improveStringField(line, "at", "date", "recorded_at"), LastAt: improveStringField(line, "at", "date", "recorded_at"),
			Evidence: []string{fmt.Sprintf("%s:%d", path, i+1)}})
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
			FirstAt: improveStringField(issue, "createdAt", "created_at"), LastAt: improveStringField(issue, "updatedAt", "updated_at"),
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
