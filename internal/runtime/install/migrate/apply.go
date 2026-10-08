package migrate

// apply.go is the M3 write step of docs/port-cxc/state-migration.md: it publishes the plan classify() produced, in
// dependency order, through M1's Publisher, and never changes a payload byte. It creates the destination directories
// first, each at the private marker mode below, then publishes the files in rank order (artifacts and plans before the
// records that reference them, and inside a rank the artifacts an evidence manifest names and the config backups before
// the record that refers to them), and finishes the directories' modes deepest first. Every source is rechecked against
// the preflight inventory immediately before its own write and again after its own publication, and every copied file's
// source is checked once more when the run has finished, so a source that moved stops the run. A refusal or an error
// stops the run and leaves every published file whole, so a rerun skips what is already equal. No hook, installer,
// activation or startup path calls this.

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"golang.org/x/sys/unix"
)

// applyTempRaw is the raw mode, permission bits plus the sticky bit, that every directory this run creates is given
// before its children are published: private, and a marker no other writer in this repository uses, so a rerun can tell
// a directory this migration made from one it must leave alone. applyTempMode is the same mode as the permission bits a
// Go FileMode reports, which is what the comparisons below use.
const (
	applyTempRaw  = 0o1700
	applyTempMode = fs.FileMode(0o700)
)

// applyReasonChanged refuses a source that moved between classification and publication, which stops the run.
const applyReasonChanged Reason = "changed"

// migrateReviewFollowupReceiptReadCap is the size bound the receipt reader applies, which is what decides whether a planned
// record under evidence/ can be judged by its content at all: gate.ParseSourceBoundReceipt reads a receipt with
// readFile(path, maxText), maxText being V8's longest string, the most the oracle's readFileSync(path, "utf8") returns
// (internal/pabcd/gate/js.go:19-20). A record larger than this is one the receipt reader itself cannot read, so it keeps
// the name judgement instead of being opened here.
const migrateReviewFollowupReceiptReadCap = 0x1fffffe8

// migrateOwnedDirBeforeEnsureChild runs between the lookup that found a destination directory absent
// and the mkdir that would create it, so a case can put another actor's creation in that window; it is
// nil in a run, and only a test sets it.
var migrateOwnedDirBeforeEnsureChild func()

// ApplyItem is one plan item and what the run did with it. Result is empty for a skip row and for an item the run never
// reached; Note explains a directory that kept its mode, or a destination whose mode a publish left alone.
type ApplyItem struct {
	Item
	Result Result
	Note   string
}

// ApplyResult is one apply run: every plan item in plan order with its result, the number of writes this run completed,
// and whether every copy and transform source was opened, stat'ed and hashed in this run (false when the run stopped).
type ApplyResult struct {
	Items           []ApplyItem
	WritesCompleted int
	SourceVerified  bool
}

// apply publishes plan through M1's Publisher. A nil or empty plan writes nothing.
func apply(r *Roots, plan *Plan) (*ApplyResult, error) {
	pub, err := NewPublisher()
	if err != nil {
		return nil, err
	}
	return applyWith(r, plan, pub)
}

// applyWith is apply with a publisher the caller supplies, so a test can give it its own seams.
func applyWith(r *Roots, plan *Plan, pub *Publisher) (*ApplyResult, error) {
	a := &applyRun{roots: r, plan: plan, pub: pub, dirs: map[string]*Dir{}, srcs: map[string]*Dir{}, made: map[string]bool{}, denied: map[string]bool{}, result: &ApplyResult{}}
	if plan != nil {
		a.result.Items = make([]ApplyItem, len(plan.Items))
		for i, it := range plan.Items {
			a.result.Items[i].Item = it
		}
	}
	err := a.run()
	a.close()
	a.result.SourceVerified = err == nil
	return a.result, err
}

// applyRun is one run: the pinned roots, the publisher, the plan, the opened directories and the ones it created.
type applyRun struct {
	roots  *Roots
	plan   *Plan
	pub    *Publisher
	result *ApplyResult
	dirs   map[string]*Dir
	srcs   map[string]*Dir
	made   map[string]bool
	denied map[string]bool
}

func applyKey(scope Scope, rel string) string { return string(scope) + "\x00" + rel }

// applyRel normalises the plan's spelling of a scope root, ".", to the empty relative path.
func applyRel(dest string) string {
	if dest == "." {
		return ""
	}
	return dest
}

// applySplit cuts a root-relative path into its directory part and its last component.
func applySplit(rel string) (dir, base string) {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i], rel[i+1:]
	}
	return "", rel
}

// applyWrites reports whether the item is one the run writes; a skip row is only reported.
func applyWrites(it Item) bool { return it.Disposition == DispCopy || it.Disposition == DispTransform }

// applyDir reports whether a written item is a directory: the planner sets Size and Digest for every file and leaves both
// zero for a directory, and sha256 never returns the zero array.
func applyDir(it Item) bool {
	return applyWrites(it) && it.Size == 0 && it.Digest == ([sha256.Size]byte{})
}

// applyRank orders the files: 0 the plans and artifacts, 1 the rest (backups, and the freeze that names its plan files),
// 2 the bindings, ledgers and control records that reference them (state-migration.md Preflight 3).
func applyRank(it Item) int {
	if it.Scope == ScopeCodex { // the install manifest, the self-heal marker and the config backups
		return 0
	}
	p := it.Source
	switch {
	case p == "plan" || strings.HasPrefix(p, "plan/"), p == "evidence" || strings.HasPrefix(p, "evidence/"):
		return 0
	case p == "sessions" || strings.HasPrefix(p, "sessions/"), p == "ledger.jsonl", p == "interviews" || strings.HasPrefix(p, "interviews/"),
		p == "goalplans" || strings.HasPrefix(p, "goalplans/"), p == "sources" || strings.HasPrefix(p, "sources/"),
		p == "dispatches" || strings.HasPrefix(p, "dispatches/"), p == "bg/disabled", p == "bg/enabled-at", p == "bg/ledger.jsonl",
		strings.HasPrefix(p, "bg/") && strings.HasSuffix(p, ".json"), p == "attest.json", p == "subagents.json", p == "config.json":
		return 2
	}
	return 1
}

func (a *applyRun) run() error {
	if a.plan == nil {
		return nil
	}
	if err := a.ensureRoots(); err != nil {
		return err
	}
	if err := a.makeDirectories(); err != nil {
		return err
	}
	if err := a.writeFiles(); err != nil {
		return err
	}
	if err := a.finishModes(); err != nil {
		return err
	}
	return a.migrateApplyReviewVerifySources()
}

func (a *applyRun) close() {
	for _, d := range a.dirs {
		_ = d.Close()
	}
	for _, d := range a.srcs {
		_ = d.Close()
	}
}

// root returns the pinned root of a scope on the destination or the source side.
func (a *applyRun) root(scope Scope, dest bool) *Dir {
	switch scope {
	case ScopeProject:
		if a.roots.Project != nil {
			if dest {
				return a.roots.Project.Dest
			}
			return a.roots.Project.Source
		}
	case ScopeUser:
		if a.roots.User != nil {
			if dest {
				return a.roots.User.Dest
			}
			return a.roots.User.Source
		}
	case ScopeCodex:
		return a.roots.Codex
	}
	return nil
}

// open returns the pinned directory of a root-relative path, caching each component, and refuses a link or a non-directory.
func (a *applyRun) open(scope Scope, rel string, dest bool) (*Dir, error) {
	if rel == "" || rel == "." {
		root := a.root(scope, dest)
		if root == nil {
			return nil, errors.New("no " + map[bool]string{true: "destination", false: "source"}[dest] + " root for scope " + string(scope))
		}
		return root, nil
	}
	cache := a.dirs
	if !dest {
		cache = a.srcs
	}
	if d, ok := cache[applyKey(scope, rel)]; ok {
		return d, nil
	}
	parentRel, base := applySplit(rel)
	parent, err := a.open(scope, parentRel, dest)
	if err != nil {
		return nil, err
	}
	child, err := parent.Child(base)
	if err != nil {
		return nil, err
	}
	cache[applyKey(scope, rel)] = child
	return child, nil
}

// rootItem returns the plan item of a scope's root, whose Mode the source root is rechecked against.
func (a *applyRun) rootItem(scope Scope) (Item, bool) {
	for _, it := range a.plan.Items {
		if it.Scope == scope && applyDir(it) && applyRel(it.Source) == "" {
			return it, true
		}
	}
	return Item{}, false
}

// ensureRoots rechecks each source root, then creates and pins the destination root of every scope the plan covers before
// any state is written. The project root is created by EnsureProjectRoot, which must decide the initialization itself
// before anything has made the root: it captures whether .gitignore was already there when its call began, and a root
// this run creates cannot have a retained one, so a racer's differing .gitignore is refused instead of swallowed. Only a
// root this call's own mkdir made is chmodded to the private marker mode afterwards: whether a root is this run's comes
// from the mkdir result, never from the absence a lookup saw earlier, so a root another actor creates in that window keeps
// its mode. The Codex scope maps its files in place.
func (a *applyRun) ensureRoots() error {
	if a.plan == nil {
		return nil
	}
	for _, scope := range []Scope{ScopeProject, ScopeUser} {
		if !slices.ContainsFunc(a.plan.Items, func(it Item) bool { return it.Scope == scope }) {
			continue
		}
		pair := a.roots.Project
		if scope == ScopeUser {
			pair = a.roots.User
		}
		if pair == nil {
			continue
		}
		if it, ok := a.rootItem(scope); ok && pair.Source != nil {
			cur, err := classifyDirMode(pair.Source)
			if err != nil {
				return err
			}
			if cur != it.Mode.Perm() {
				return refuse(applyReasonChanged, it.Source, "the source root mode changed since classification")
			}
		}
		// Whether the root was there when the pair was pinned decides whether a directory this run did
		// not create may still be adopted below: a root that was absent then is another actor's when
		// this run's own creation does not end up owning it, whatever its mode, and that lookup is not
		// repeated - a retry reaches this with the pair's pinned root already set, so the answer has to
		// come from the pair rather than from a fresh look at Dest.
		absent := pair.pinnedAbsent
		var made bool
		var err error
		if scope == ScopeProject {
			// EnsureProjectRoot creates the root itself and gives it the private marker mode before its .gitignore
			// publication, so the initialization judgement runs against a root this run created.
			_, made, err = a.pub.EnsureProjectRoot(pair)
		} else {
			var root *Dir
			root, made, err = pair.EnsureDest(applyTempRaw)
			if err == nil && made {
				// mkdir's own result need not keep the sticky bit of the marker mode (Darwin drops it) and a umask can
				// mask the permission bits, so a root this run made is given the marker explicitly, as ensureDestDir
				// gives it to a child. Without it a rerun after an interruption would read the root as an existing
				// directory of another actor and never finish its mode.
				err = applyChmodRaw(root, applyTempRaw)
			}
		}
		if err != nil {
			return err
		}
		a.recordOwnership(scope, "", made, absent)
	}
	return nil
}

// makeDirectories creates, in plan order (the planner emits a parent before its children), every destination directory
// the plan names and does not already hold, gives each the private marker mode, and rechecks each source mode against the
// preflight inventory. The scope-root items belong to ensureRoots and are skipped.
func (a *applyRun) makeDirectories() error {
	for i, it := range a.plan.Items {
		if !applyDir(it) || applyRel(it.Destination) == "" {
			continue
		}
		src, err := a.open(it.Scope, it.Source, false)
		if err != nil {
			return a.stop(i, it, err)
		}
		cur, err := classifyDirMode(src)
		if err != nil {
			return a.stop(i, it, err)
		}
		if cur != it.Mode.Perm() {
			return a.stop(i, it, refuse(applyReasonChanged, it.Source, "the source directory mode changed since classification"))
		}
		if _, err := a.ensureDestDir(it.Scope, applyRel(it.Destination)); err != nil {
			return a.stop(i, it, err)
		}
	}
	return nil
}

// ensureDestDir returns the pinned destination directory, creating it with the private marker mode when it is absent, and
// records that this run created it. Only a directory this call's own mkdir made is this run's: a creation that ended in
// EEXIST is another actor's directory, so it is neither given the marker mode nor finished at the source mode.
func (a *applyRun) ensureDestDir(scope Scope, rel string) (*Dir, error) {
	if d, ok := a.dirs[applyKey(scope, rel)]; ok {
		return d, nil
	}
	parentRel, base := applySplit(rel)
	parent, err := a.open(scope, parentRel, true)
	if err != nil {
		return nil, err
	}
	child, err := parent.Child(base)
	switch {
	case err == nil:
		a.dirs[applyKey(scope, rel)] = child
		return child, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if migrateOwnedDirBeforeEnsureChild != nil {
		migrateOwnedDirBeforeEnsureChild()
	}
	var made bool
	if child, made, err = parent.EnsureChild(base, applyTempRaw); err != nil {
		// The run stops here, so a handle the creation handed back with its error - another actor's
		// directory when the creation ended in EEXIST, or this run's own when only the final check or
		// the sync failed - is not kept by anyone else and must not outlive the attempt.
		if child != nil {
			_ = child.Close()
		}
		return nil, err
	}
	if made {
		if err := applyChmodRaw(child, applyTempRaw); err != nil {
			_ = child.Close()
			return nil, err
		}
	}
	// The lookup above found the name absent, so a creation this run's own mkdir
	// refused with EEXIST made another actor's directory: record it so finishModes
	// never adopts it.
	a.recordOwnership(scope, rel, made, true)
	a.dirs[applyKey(scope, rel)] = child
	return child, nil
}

// writeFiles publishes every file item in rank order, keeping plan order inside a rank, and rechecks each source against
// the preflight inventory immediately before its own write, so a source that moved stops the run instead of publishing
// other bytes. Inside a rank a file another item's evidence manifest names, and a Codex config backup, come first, so a
// record is never published before the artifact or the backup it refers to.
func (a *applyRun) writeFiles() error {
	order := make([]int, 0, len(a.plan.Items))
	for i, it := range a.plan.Items {
		if applyWrites(it) && !applyDir(it) {
			order = append(order, i)
		}
	}
	refs, receipts, deps := a.migrateApplyReviewReferences()
	depths := migrateReviewFollowupDepths(deps)
	slices.SortStableFunc(order, func(x, y int) int {
		ix, iy := a.plan.Items[x], a.plan.Items[y]
		if c := cmp.Compare(applyRank(ix), applyRank(iy)); c != 0 {
			return c
		}
		if c := cmp.Compare(migrateApplyReviewSubRank(ix, refs, receipts), migrateApplyReviewSubRank(iy, refs, receipts)); c != 0 {
			return c
		}
		// The reference chain decides inside the sub-rank: a record another receipt names is still a referrer when it is
		// itself a receipt, so without this a chain of receipts would keep plan order and a receipt could publish before
		// the artifact it names (CRW-879).
		return cmp.Compare(depths[ix.Source], depths[iy.Source])
	})
	for _, i := range order {
		if err := a.publishFile(i, a.plan.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

// migrateApplyReviewReferences reads the plan's evidence records and returns two keys over source paths: the artifactManifest[]
// entries a receipt names, each resolved against the receipt's own directory, whose kinds are the verdict and
// artifact-identity files the receipt is judged with (internal/pabcd/gate/receipt.go:35-36, gate/manifest.go:89-143), and
// the records that are receipts. A record is a receipt by its content, never by its name (CRW-879): a planned written
// regular file under evidence/ whose bytes, read within the receipt reader's own bound, are a JSON object with a non-empty
// artifactManifest array. The receipt reader takes the name its caller chose (gate/receipt.go:104-108), so a dependency
// order that only knew the conventional qa-receipt.json name published a receipt under another name before the artifact it
// refers to. A record that cannot be read, is past that bound or holds no manifest array is not judged by content, and one
// carrying the conventional name keeps the place the name gave it, so an unreadable receipt still orders after its
// dependencies. deps is each receipt's own referenced paths, so a chain of records is ordered by its length rather than by
// plan order. Nothing here decides what is copied or fails the run: a record this run cannot read contributes no key and
// is reported by attention.
func (a *applyRun) migrateApplyReviewReferences() (refs, receipts map[string]bool, deps map[string][]string) {
	refs, receipts, deps = map[string]bool{}, map[string]bool{}, map[string][]string{}
	for _, it := range a.plan.Items {
		if !applyWrites(it) || applyDir(it) {
			continue
		}
		named := strings.HasSuffix(it.Source, "qa-receipt.json")
		if !named && !classifyEvidenceTree(it.Source) {
			continue
		}
		manifest, ok := a.migrateReviewFollowupReceiptManifest(it)
		if ok {
			receipts[it.Source] = true
		} else if named {
			receipts[it.Source] = true // an unreadable record keeps the name judgement and the order it gave
		}
		dir := classifyDirPart(it.Source)
		var own []string
		for _, e := range manifest {
			if e.Kind != "verdict" && e.Kind != "artifact-identity" {
				continue
			}
			rel := path.Clean(e.Path)
			if dir != "" {
				rel = path.Clean(dir + "/" + rel)
			}
			refs[rel] = true
			own = append(own, rel)
		}
		if len(own) > 0 {
			deps[it.Source] = own
		}
	}
	return refs, receipts, deps
}

// migrateReviewFollowupDepths is the longest reference chain below each record: 0 for a record that names nothing, and one
// more than the deepest record it names. A record another receipt names is still a referrer when it is itself a receipt, so
// without this a chain of receipts would keep plan order and a receipt could publish before the artifact it names
// (CRW-879). The walk starts from the records in sorted order, so the same plan gives the same keys on every run: a walk
// driven by Go's map order cached the first depth it reached for a record on a cycle, and two runs of one plan then gave
// that cycle two different keys and published it in two orders. A cycle is broken at the record already on the current
// path, so a record that names another keeps one key per cycle and the order stays the plan's.
func migrateReviewFollowupDepths(deps map[string][]string) map[string]int {
	sources := make([]string, 0, len(deps))
	for source := range deps {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	depths := make(map[string]int, len(deps))
	var walk func(source string, onPath map[string]bool) int
	walk = func(source string, onPath map[string]bool) int {
		if depth, done := depths[source]; done {
			return depth
		}
		if onPath[source] {
			return 0
		}
		onPath[source] = true
		depth := 0
		for _, dep := range deps[source] {
			if d := walk(dep, onPath) + 1; d > depth {
				depth = d
			}
		}
		delete(onPath, source)
		depths[source] = depth
		return depth
	}
	for _, source := range sources {
		walk(source, map[string]bool{})
	}
	return depths
}

// migrateReviewFollowupManifestEntry is one artifactManifest entry of a receipt: the relative path and the kind the receipt
// is judged with. Both are read by their exact spelling, as the receipt reader's own lookups read them.
type migrateReviewFollowupManifestEntry struct {
	Path string
	Kind string
}

// migrateReviewFollowupReceiptManifest reads one planned item as a receipt and returns the artifactManifest entries it
// names. ok is true for a referring receipt, which is any file whose content is a JSON object with a non-empty
// artifactManifest array, whatever the file is called: the receipt reader takes the caller's chosen name
// (internal/pabcd/gate/receipt.go:104-108), so a dependency order that only knew the conventional qa-receipt.json name left
// a receipt under another name publishing before the artifact it refers to (CRW-879). A file this run cannot open, one past
// migrateReviewFollowupReceiptReadCap, or one that is not such an object returns ok false with a nil manifest, and the
// caller falls back to the name judgement for it.
func (a *applyRun) migrateReviewFollowupReceiptManifest(it Item) ([]migrateReviewFollowupManifestEntry, bool) {
	if it.Size > migrateReviewFollowupReceiptReadCap {
		// The plan already measured it, so a record past the receipt reader's bound is refused without opening it: the
		// receipt reader could not read it either, so it keeps the name judgement.
		return nil, false
	}
	dir := classifyDirPart(it.Source)
	src, err := a.open(it.Scope, dir, false)
	if err != nil {
		return nil, false
	}
	_, base := applySplit(it.Source)
	f, _, err := src.OpenRegular(base)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	return migrateReviewFollowupDecodeManifest(f, migrateReviewFollowupReceiptReadCap)
}

// migrateReviewFollowupJSONSpace reports whether b is one of the four bytes JSON allows between tokens, which is what the
// receipt reader's own decoder skips: a byte outside that set between two tokens is not the JSON the reader would read.
func migrateReviewFollowupJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// migrateReviewFollowupRecord counts the bytes a decode has read from one record, so the judgement can apply the receipt
// reader's own size bound without holding the record: gate.readFile refuses a file with more than maxText bytes, and this
// order must refuse the same record for the same reason.
type migrateReviewFollowupRecord struct {
	src io.Reader
	n   int64
}

func (c *migrateReviewFollowupRecord) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	c.n += int64(n)
	return n, err
}

// migrateReviewFollowupManifestKey is the member a receipt's references live in, as the receipt reader spells it.
const migrateReviewFollowupManifestKey = "artifactManifest"

// migrateReviewFollowupDecodeManifest reads one record as a receipt within limit bytes and returns the artifactManifest
// entries it names. ok is false, with no entries, for a record that is not one JSON object with a non-empty
// artifactManifest array, for one with data after that object, and for one that is longer than limit. The key is read by its
// exact spelling from the decoded object, as the receipt reader's own map lookup does, so a record whose key differs in case
// is not judged a receipt here either. The judgement is the array's presence and length, never the shape of its entries: an
// entry that is not an object with a string path and kind names no dependency, and an array of such entries is still a
// receipt, so a receipt this run cannot read in full keeps the referrer's place instead of falling back to plan order.
//
// The record is decoded by encoding/json, which is what the receipt reader itself uses, so every JSON rule the reader
// applies is applied here too: a duplicate key keeps the last value, an escaped key is the key the reader's map lookup
// reads, a member of any other type is refused as the reader refuses it, and data after the object or a malformed value is
// refused rather than framed by hand. There is no cap of this order's own: a receipt the reader can read is a receipt here,
// and none of its references is dropped.
//
// The scan looks at every planned file under evidence/, up to the receipt reader's bound (the size of the largest string
// the oracle could hold), so the record is streamed rather than held: the judgement keeps the bytes of one token, never the
// record, and a record of any size costs a bounded pass. The first byte that is not whitespace decides whether the record
// can be an object at all, every member other than the manifest key is skipped token by token, and the manifest array is
// held only when the record really names one: the memory this costs is the size of that array, which is the memory the
// receipt reader spends on the same record. The record is the receipt reader's own text. A string is read as its raw bytes and
// normalised by source.DecodeUTF8, the normalisation the reader applies to its whole input, so an invalid UTF-8 path keeps the
// text the reader sees, and the order matches it against the plan's file names.
func migrateReviewFollowupDecodeManifest(rs io.ReadSeeker, limit int64) ([]migrateReviewFollowupManifestEntry, bool) {
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, false
	}
	// The record is counted as the decoder reads it, so the receipt reader's own bound applies to the bytes that reader
	// would have read: gate.readFile refuses a file with more than maxText bytes, and this order must refuse the same
	// record for the same reason, so a record that grew past the bound after the plan measured it is not judged here.
	record := &migrateReviewFollowupRecord{src: io.LimitReader(rs, limit+1)}
	br := bufio.NewReaderSize(record, 64<<10)
	// A receipt is a JSON object (gate/receipt.go:109-115), so the first byte that is not JSON whitespace decides: any
	// other byte means this record cannot be one, and it is refused here without being decoded or held.
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, false
		}
		if migrateReviewFollowupJSONSpace(b) {
			continue
		}
		if b != '{' {
			return nil, false
		}
		if err := br.UnreadByte(); err != nil {
			return nil, false
		}
		break
	}
	feed := &migrateReviewFollowupFeed{br: br}
	dec := json.NewDecoder(feed)
	dec.UseNumber()                        // the receipt reader decodes with UseNumber too (gate/js.go:37), so a huge literal is a number here
	if _, err := dec.Token(); err != nil { // the opening brace the walk above found
		return nil, false
	}
	var manifest []migrateReviewFollowupManifestEntry
	found := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, false
		}
		name, ok := key.(string)
		if !ok {
			return nil, false
		}
		if name != migrateReviewFollowupManifestKey {
			if err := migrateReviewFollowupSkipValue(dec, 1); err != nil {
				return nil, false
			}
			continue
		}
		entries, ok, err := migrateReviewFollowupReadManifest(dec, feed, 1)
		if err != nil {
			return nil, false
		}
		// A duplicated key keeps the last value, as the receipt reader's own map decode reads it, so an earlier
		// occurrence that is not a non-empty array is replaced by the later one rather than refusing the record.
		manifest, found = entries, ok
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, false
	}
	// The record must be exactly this one object: data after it is not the JSON the receipt reader would read, and the
	// reader refuses it too. What follows is walked byte by byte rather than decoded, so a long trailing run costs
	// nothing, and the count of the bytes read is what decides the bound above.
	rest := io.MultiReader(dec.Buffered(), br)
	buf := make([]byte, 64<<10)
	for {
		n, err := rest.Read(buf)
		for _, b := range buf[:n] {
			if !migrateReviewFollowupJSONSpace(b) {
				return nil, false
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, false
			}
			break
		}
	}
	if !found || record.n > limit {
		return nil, false
	}
	return manifest, true
}

// migrateReviewFollowupMaxDepth is the nesting encoding/json itself accepts, and so the deepest record the receipt reader
// can read: its own Decode refuses a value nested past this (encoding/json's maxNestingDepth). A record past it is one the
// reader refuses, so this order must refuse it too rather than walk it.
const migrateReviewFollowupMaxDepth = 10000

// migrateReviewFollowupTooDeep is the refusal of a record nested deeper than the receipt reader accepts.
func migrateReviewFollowupTooDeep() error {
	return errors.New("the record nests deeper than the receipt reader accepts")
}

// migrateReviewFollowupSkipValue consumes the value of a member this order does not use, one token at a time, so a member
// of any size costs the bytes of one token rather than a copy of itself. It refuses anything encoding/json refuses, which
// is what makes the whole record valid JSON: a trailing comma, a missing colon or a malformed literal ends the walk here
// and the record is not a receipt, exactly as the receipt reader's own decode of it would fail. depth is the nesting of
// the object holding the member, so the count of containers this walk has open is what the reader's own limit is applied
// to; the walk is iterative, so its stack use does not grow with the record, and a record the reader
// refuses must not be walked into a stack overflow here.
func migrateReviewFollowupSkipValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return migrateReviewFollowupSkipRest(dec, tok, depth)
}

// migrateReviewFollowupSkipRest consumes the rest of a value whose first token is tok: nothing more for a scalar, and for
// an array or object every token up to its matching close. It carries its own count of the containers it has opened,
// because Token elides the separators and does not count the record's nesting against the receipt reader's own
// Decode would refuse for nesting too deeply.
func migrateReviewFollowupSkipRest(dec *json.Decoder, tok json.Token, depth int) error {
	delim, ok := tok.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	for open := 1; open > 0; {
		if depth+open > migrateReviewFollowupMaxDepth {
			return migrateReviewFollowupTooDeep()
		}
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				open++
			case '}', ']':
				open--
			}
		}
	}
	return nil
}

// migrateReviewFollowupReadManifest reads the artifactManifest member of the object being walked and returns the entries it
// names. The first token of the value decides: anything that is not an array is refused as the receipt reader refuses it
// (gate/manifest.go:110-139) and skipped token by token, so an object or a string under the key costs the bytes of one
// token and never the value. An array is walked element by element, and each element keeps only its path and kind strings,
// so an element of any size costs the bytes of its own two strings. depth is the number of containers open around the
// member, so the nesting is counted against the receipt reader's own limit. ok is false for a member that is not a
// non-empty array; err is non-nil only for a malformed stream, which refuses the record.
func migrateReviewFollowupReadManifest(dec *json.Decoder, feed *migrateReviewFollowupFeed, depth int) ([]migrateReviewFollowupManifestEntry, bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, false, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, false, migrateReviewFollowupSkipRest(dec, tok, depth)
	}
	if depth+1 > migrateReviewFollowupMaxDepth {
		return nil, false, migrateReviewFollowupTooDeep()
	}
	var manifest []migrateReviewFollowupManifestEntry
	items := 0
	for dec.More() {
		items++
		entry, err := migrateReviewFollowupReadEntry(dec, feed, depth+1)
		if err != nil {
			return nil, false, err
		}
		if entry != nil {
			manifest = append(manifest, *entry)
		}
	}
	if _, err := dec.Token(); err != nil { // the closing bracket
		return nil, false, err
	}
	// The judgement is the array's presence and length, never the shape of its entries.
	if items == 0 {
		return nil, false, nil
	}
	return manifest, true, nil
}

// migrateReviewFollowupReadEntry walks one artifactManifest element. depth counts the containers open around the element,
// the array included. An element that is not an object names no dependency and is skipped; an object keeps the last value
// of its path and of its kind, as the receipt reader's own map lookup does, and skips every other member.
func migrateReviewFollowupReadEntry(dec *json.Decoder, feed *migrateReviewFollowupFeed, depth int) (*migrateReviewFollowupManifestEntry, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, migrateReviewFollowupSkipRest(dec, tok, depth)
	}
	var entry migrateReviewFollowupManifestEntry
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := key.(string)
		if name != "path" && name != "kind" {
			if err := migrateReviewFollowupSkipValue(dec, depth+1); err != nil {
				return nil, err
			}
			continue
		}
		text, err := migrateReviewFollowupReadField(dec, feed, depth+1)
		if err != nil {
			return nil, err
		}
		if name == "path" {
			entry.Path = text
		} else {
			entry.Kind = text
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if entry.Path == "" || entry.Kind == "" {
		return nil, nil
	}
	return &entry, nil
}

// migrateReviewFollowupReadField returns the string a path or kind value holds, or "" when it holds anything else. A container
// is skipped token by token, so it is never held whole. A string or a scalar is read as its own bytes through the reader's
// UTF-8 normalisation (source.DecodeUTF8, gate/js.go:36-53), so a path holding invalid bytes names the plan file the
// reader's text holds; a string is bounded by its one token, which is the residual the issue's answer records. depth is the
// containers open around the value.
func migrateReviewFollowupReadField(dec *json.Decoder, feed *migrateReviewFollowupFeed, depth int) (string, error) {
	c, err := migrateReviewFollowupPeekValue(dec, feed)
	if err != nil {
		return "", err
	}
	if c == '{' || c == '[' {
		tok, err := dec.Token() // consumes the colon and opens the container
		if err != nil {
			return "", err
		}
		return "", migrateReviewFollowupSkipRest(dec, tok, depth)
	}
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return "", err
	}
	var text string
	if json.Unmarshal([]byte(source.DecodeUTF8(raw)), &text) != nil {
		return "", nil
	}
	return text, nil
}

// migrateReviewFollowupFeed is the reader the receipt decoder reads through. A byte the value peek takes from the buffered
// reader and hands back is served first, so the decoder reads it as if the peek had not happened.
type migrateReviewFollowupFeed struct {
	br      *bufio.Reader
	pending []byte
}

// Read serves the handed-back bytes first, then the buffered reader.
func (f *migrateReviewFollowupFeed) Read(p []byte) (int, error) {
	if len(f.pending) > 0 {
		n := copy(p, f.pending)
		f.pending = f.pending[n:]
		return n, nil
	}
	return f.br.Read(p)
}

// readByte takes the next byte of the stream.
func (f *migrateReviewFollowupFeed) readByte() (byte, error) {
	if len(f.pending) > 0 {
		c := f.pending[0]
		f.pending = f.pending[1:]
		return c, nil
	}
	return f.br.ReadByte()
}

// unread hands bytes back in front of the stream.
func (f *migrateReviewFollowupFeed) unread(b []byte) {
	f.pending = append(append([]byte(nil), b...), f.pending...)
}

// migrateReviewFollowupPeekValue returns the first byte of the value after the colon the decoder has just read a key for,
// without the decoder having taken it. The decoder's read-ahead is looked at first. Past it, whitespace is skipped one byte at
// a time, so a run of any length costs no memory and is not limited by the reader's window. A colon the decoder has not read
// yet and the value's first byte are handed back to the feed, so the decoder reads the same bytes it would read without the
// peek.
func migrateReviewFollowupPeekValue(dec *json.Decoder, feed *migrateReviewFollowupFeed) (byte, error) {
	ahead, err := io.ReadAll(dec.Buffered())
	if err != nil {
		return 0, err
	}
	for _, c := range ahead {
		if !migrateReviewFollowupJSONSpace(c) && c != ':' {
			return c, nil
		}
	}
	var kept []byte
	for {
		c, err := feed.readByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if migrateReviewFollowupJSONSpace(c) {
			continue
		}
		if c == ':' && len(kept) == 0 {
			kept = append(kept, c)
			continue
		}
		feed.unread(append(kept, c))
		return c, nil
	}
}

// migrateApplyReviewSubRank is the ordering key inside a rank: 0 for the artifacts an evidence manifest names and for the
// Codex config backups, 2 for the records that refer to them (a QA receipt, by name or by content, and the Codex install
// record), and 1 for the rest. A receipt whose manifest this run could not read still takes the referrer's place, so its
// dependencies keep the dependencies-first order even when the graph is unknown.
func migrateApplyReviewSubRank(it Item, refs, receipts map[string]bool) int {
	if it.Scope == ScopeCodex {
		switch {
		case strings.HasPrefix(it.Source, backupSource) && strings.HasSuffix(it.Source, backupSuffix):
			return 0
		case it.Source == installSource:
			return 2
		}
	}
	if refs[it.Source] {
		return 0
	}
	if receipts[it.Source] {
		return 2
	}
	return 1
}

func (a *applyRun) publishFile(i int, it Item) error {
	src, err := a.open(it.Scope, classifyDirPart(it.Source), false)
	if err != nil {
		return a.stop(i, it, err)
	}
	_, base := applySplit(it.Source)
	f, info, err := src.OpenRegular(base)
	if err != nil {
		return a.stop(i, it, err)
	}
	defer f.Close()
	if info.Size() != it.Size || info.Mode().Perm() != it.Mode.Perm() {
		return a.stop(i, it, refuse(applyReasonChanged, it.Source, "the source changed since classification"))
	}
	got, err := sum(f)
	if err != nil {
		return a.stop(i, it, refuse(ReasonUnreadable, it.Source, err.Error()))
	}
	if got != it.Digest {
		return a.stop(i, it, refuse(applyReasonChanged, it.Source, "the source bytes changed since classification"))
	}
	parent, err := a.open(it.Scope, classifyDirPart(it.Destination), true)
	if err != nil {
		return a.stop(i, it, err)
	}
	_, leaf := applySplit(it.Destination)
	res, renamed, err := a.pub.migrateReviewFollowupPublish(parent, leaf, f, it.Size, it.Mode.Perm())
	a.result.Items[i].Result = res
	if renamed {
		// This run's own no-replace rename completed, so the file is this run's write whatever the source recheck below
		// finds and whether or not the directory sync after it failed: neither a moved source nor a failed sync erases a
		// completed rename from the report. A failure before the rename is never counted, whatever the destination holds,
		// because a racer can publish the plan's bytes between settle's look and this run's own create (CRW-879).
		a.result.WritesCompleted++
	}
	if err != nil {
		switch {
		case res == ResultAlreadyEqual:
			// The destination already held the plan's bytes, so this run wrote nothing: only the directory sync failed, and
			// the file must not be counted as a write of this run.
			a.result.Items[i].Note = "the destination was already equal; its directory sync failed"
			if got, derr := a.destMode(parent, leaf); derr == nil && got != it.Mode.Perm() {
				a.result.Items[i].Note += "; destination kept its mode " + got.String()
			}
		case renamed:
			// The rename completed before the failure, so the whole final file is at the destination and only its directory
			// sync failed.
			a.result.Items[i].Note = "the final file is whole but its directory sync failed"
		}
		return err
	}
	if err := a.migrateApplyReviewRecheckSource(i, it); err != nil {
		return err
	}
	if res == ResultCopied {
		return a.checkDest(parent, leaf, it)
	}
	if res == ResultAlreadyEqual {
		// Nothing was written, and equality never changes a destination's mode or times, so a difference is reported.
		if got, err := a.destMode(parent, leaf); err == nil && got != it.Mode.Perm() {
			a.result.Items[i].Note = "destination kept its mode " + got.String()
		}
	}
	return nil
}

// migrateApplyReviewRecheckSource re-reads an item's source after its publication and compares it with the plan, so a
// source that moved while its bytes were being published is reported as that file rather than only as the bytes that
// landed. The item is set to refused here because stop() never overwrites the result the publication already assigned.
func (a *applyRun) migrateApplyReviewRecheckSource(i int, it Item) error {
	src, err := a.open(it.Scope, classifyDirPart(it.Source), false)
	if err != nil {
		a.result.Items[i].Result = ResultRefused
		return err
	}
	_, base := applySplit(it.Source)
	f, info, err := src.OpenRegular(base)
	if err != nil {
		a.result.Items[i].Result = ResultRefused
		return err
	}
	defer f.Close()
	if info.Size() != it.Size || info.Mode().Perm() != it.Mode.Perm() {
		return a.migrateApplyReviewChanged(i, it, "the source changed after its publication")
	}
	got, err := sum(f)
	if err != nil {
		a.result.Items[i].Result = ResultRefused
		return refuse(ReasonUnreadable, it.Source, err.Error())
	}
	if got != it.Digest {
		return a.migrateApplyReviewChanged(i, it, "the source bytes changed after its publication")
	}
	return nil
}

// migrateApplyReviewChanged records that an item's source moved and returns the refusal that stops the run.
func (a *applyRun) migrateApplyReviewChanged(i int, it Item, detail string) error {
	a.result.Items[i].Result = ResultRefused
	return refuse(applyReasonChanged, it.Source, detail)
}

// migrateApplyReviewVerifySources re-reads every source this run copied or transformed once the run has finished, so a
// source that moved while a later file was publishing is still reported instead of passing as a successful copy.
func (a *applyRun) migrateApplyReviewVerifySources() error {
	if a.plan == nil {
		return nil
	}
	for i, it := range a.plan.Items {
		if !applyWrites(it) || applyDir(it) {
			continue
		}
		if err := a.migrateApplyReviewRecheckSource(i, it); err != nil {
			return err
		}
	}
	return nil
}

// checkDest verifies a file this run published against the preflight inventory: its size, its bytes and its mode. The
// source was hashed before the publish, so this is the check that the bytes that landed are the bytes the plan
// authorized, even if a writer changed the source while the temporary was being filled.
func (a *applyRun) checkDest(parent *Dir, leaf string, it Item) error {
	f, info, err := parent.OpenRegular(leaf)
	if err != nil {
		return err
	}
	defer f.Close()
	got, err := sum(f)
	if err != nil {
		return err
	}
	if info.Size() != it.Size || got != it.Digest {
		return &fs.PathError{Op: "verify", Path: parent.join(leaf), Err: errors.New("the published bytes differ from the preflight inventory")}
	}
	if info.Mode().Perm() != it.Mode.Perm() {
		return &fs.PathError{Op: "stat", Path: parent.join(leaf), Err: errors.New("the destination mode is " + info.Mode().Perm().String() + ", want " + it.Mode.Perm().String())}
	}
	return nil
}

func (a *applyRun) destMode(parent *Dir, leaf string) (fs.FileMode, error) {
	f, info, err := parent.OpenRegular(leaf)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return info.Mode().Perm(), nil
}

// finishModes applies the source mode to every directory the plan names, deepest first. A directory this run created, or
// one an interrupted run left at the private marker mode, is finished; any other directory keeps its mode and is reported.
// A directory whose creation this run's own mkdir refused with EEXIST is another actor's, so
// it keeps its mode whatever that mode is and is reported too, never adopted through the marker branch.
// The marker is the raw mode, sticky bit included, which nothing else in this repository writes, so a directory this
// migration did not create is never chmodded.
func (a *applyRun) finishModes() error {
	order := make([]int, 0, len(a.plan.Items))
	for i, it := range a.plan.Items {
		if applyDir(it) {
			order = append(order, i)
		}
	}
	depth := func(i int) int {
		if rel := applyRel(a.plan.Items[i].Destination); rel != "" {
			return strings.Count(rel, "/") + 1
		}
		return 0
	}
	slices.SortStableFunc(order, func(x, y int) int { return cmp.Compare(depth(y), depth(x)) })
	for _, i := range order {
		it := a.plan.Items[i]
		rel := applyRel(it.Destination)
		dir, err := a.open(it.Scope, rel, true)
		if err != nil {
			return err
		}
		raw, err := applyDirRaw(dir)
		if err != nil {
			return err
		}
		switch cur := fs.FileMode(raw).Perm(); {
		case a.made[applyKey(it.Scope, rel)]:
			err = applyChmod(dir, it.Mode.Perm())
		case a.denied[applyKey(it.Scope, rel)]:
			// This run's own creation ended in EEXIST, so the directory is another
			// actor's whatever its mode is: it is never adopted through the marker
			// branch below, only reported.
			a.result.Items[i].Note = "existing directory kept its mode " + cur.String()
		case raw == applyTempRaw && a.entriesExpected(it.Scope, rel, dir):
			err = applyChmod(dir, it.Mode.Perm())
		case cur == it.Mode.Perm():
		default:
			a.result.Items[i].Note = "existing directory kept its mode " + cur.String()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// recordOwnership records what a lookup saw and what this run's own creation then reported for a
// destination directory. made marks a directory this run created; a directory the lookup saw absent
// whose creation ended in EEXIST is another actor's, so it is marked denied and finishModes
// never adopts it; a directory that was already there at the lookup keeps neither mark, so the
// marker-mode adoption stays available to one an interrupted earlier run may have left.
func (a *applyRun) recordOwnership(scope Scope, rel string, made, absent bool) {
	switch {
	case made:
		a.made[applyKey(scope, rel)] = true
	case absent:
		a.denied[applyKey(scope, rel)] = true
	}
}

// entriesExpected reports whether every entry of an existing destination directory is one the plan accounts for: a
// written or reported item names it, or it is the canonical .gitignore of a project root, which EnsureProjectRoot
// publishes and no plan item names when the source .codexclaw holds none.
func (a *applyRun) entriesExpected(scope Scope, rel string, dir *Dir) bool {
	names, err := dir.Names()
	if err != nil {
		return false
	}
	for _, name := range names {
		if scope == ScopeProject && rel == "" && name == ".gitignore" {
			continue
		}
		p := name
		if rel != "" {
			p = rel + "/" + name
		}
		named := slices.ContainsFunc(a.plan.Items, func(it Item) bool {
			if it.Scope != scope {
				return false
			}
			if applyWrites(it) {
				return applyRel(it.Destination) == p
			}
			return it.Disposition == DispSkip && (applyRel(it.Destination) == p || applyRel(it.Source) == p)
		})
		if !named {
			return false
		}
	}
	return true
}

// stop records the item the run stopped on and returns the error unchanged.
func (a *applyRun) stop(i int, it Item, err error) error {
	if a.result.Items[i].Result == "" {
		a.result.Items[i].Result = ResultFailed
		if refusal(err) != nil {
			a.result.Items[i].Result = ResultRefused
		}
	}
	return err
}

// applyChmod sets a pinned directory's permission bits through its descriptor, so a replaced ancestor cannot redirect it.
func applyChmod(d *Dir, mode fs.FileMode) error { return applyChmodRaw(d, uint32(mode.Perm())) }

// applyChmodRaw sets a pinned directory's raw mode, the sticky marker included.
func applyChmodRaw(d *Dir, raw uint32) error {
	if err := unix.Fchmod(d.fd(), raw); err != nil {
		return &fs.PathError{Op: "fchmod", Path: d.path, Err: err}
	}
	return nil
}

// applyDirRaw returns a pinned directory's raw mode: its permission bits and its sticky bit.
func applyDirRaw(d *Dir) (uint32, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd(), &st); err != nil {
		return 0, &fs.PathError{Op: "stat", Path: d.path, Err: err}
	}
	return uint32(st.Mode) & 0o7777, nil
}
