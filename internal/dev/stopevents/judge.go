//go:build dev

package stopevents

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/pyerr"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	fspath "github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

const (
	perEventPredicate   = "one accepted record per Stop event (CRW-212)"
	supersededPredicate = "exactly one row per (session, turn); superseded by CRW-212 because a continuation is a new Stop event in the same turn"

	verdictTrue       = "TRUE"
	verdictFalse      = "FALSE"
	verdictUnreadable = "UNREADABLE"
)

// Window chooses the events a reading judges: every event one of whose records falls in it.
type Window struct {
	Since, Until, Session, Turn *string
}

// identity is the (device, inode) a path reaches.
type identity struct{ dev, ino uint64 }

func statIdentity(info os.FileInfo) identity {
	st := info.Sys().(*syscall.Stat_t)
	return identity{uint64(st.Dev), uint64(st.Ino)}
}

// identityOf is the identity a spelling reaches now, or nil.
func identityOf(spelled string) *identity {
	absolute, err := abspath(spelled)
	if err != nil || strings.IndexByte(absolute, 0) >= 0 {
		return nil
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil
	}
	found := statIdentity(info)
	return &found
}

// recordedIdentity is _identity_of for a path a record names: the str is handed to the system as
// os.fsencode gives it, so a byte that is not UTF-8, which the record spells as its surrogate
// escape, is that byte again; a surrogate that escapes no byte cannot be encoded, and Python's
// stat raises ValueError, which reaches nothing.
func recordedIdentity(spelled string) *identity {
	system, ok := fspath.FSEncode(spelled)
	if !ok {
		return nil
	}
	return identityOf(system)
}

func sameIdentity(a, b *identity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// expandUser is Path(spelled).expanduser() as Python 3.14 answers it. Path() spells the path
// first (pathlibForm), so "./~", ".//~/x" and "~//x" are "~", "~/x" and "~/x" before any ~ is
// looked at; then (record.ExpandUser) a leading ~ is HOME when HOME is set at all, an empty HOME
// the root, and an unset one this user's passwd entry. Where pathlib raises, because nothing
// answers the ~ or ~user or what answers it still starts with ~, it is that RuntimeError.
func expandUser(spelled string) (string, error) {
	spelled = pathlibForm(spelled)
	expanded, err := record.ExpandUser(spelled, os.LookupEnv)
	if err != nil || (strings.HasPrefix(spelled, "~") && strings.HasPrefix(expanded, "~")) {
		return "", &evidence.PythonError{Class: "RuntimeError", Detail: "Could not determine home directory."}
	}
	return expanded, nil
}

// abspath is os.path.abspath(str(Path(spelled).expanduser())).
func abspath(spelled string) (string, error) {
	expanded, err := expandUser(spelled)
	if err != nil {
		return "", err
	}
	return absolute(expanded)
}

// absolute is os.path.abspath: a relative path is joined to os.getcwd(), the directory the kernel
// reports (os.Getwd would answer $PWD, a link's spelling of it), and a working directory the
// kernel cannot report raises the OSError getcwd raises, which names no file.
func absolute(p string) (string, error) {
	if !isAbs(p) {
		cwd, err := syscall.Getwd()
		if err != nil {
			return "", pythonOSError(err)
		}
		p = cwd + "/" + p
	}
	return normpath(p), nil
}

// pythonOSError is the OSError Python raises for a failed call: its class from the errno and
// its str(). An error with no errno is a RuntimeError of its own text.
func pythonOSError(err error) *evidence.PythonError {
	if class, text, ok := pyerr.OSError(err); ok {
		return &evidence.PythonError{Class: class, Detail: text}
	}
	return &evidence.PythonError{Class: "RuntimeError", Detail: err.Error()}
}

// join is str(Path(root) / name) for a normalized absolute root.
func join(root string, names ...string) string {
	for _, name := range names {
		if strings.HasSuffix(root, "/") {
			root += name
		} else {
			root += "/" + name
		}
	}
	return root
}

// shown is how Python spells a path it read from the system: an undecodable byte is the lone
// surrogate surrogateescape gives it, which json.dumps writes as \udcXX.
func shown(p string) string { return store.FSDecode(p) }

// errorText is str(error) for what a stat or a listing raised.
func errorText(err error, p string) string {
	if strings.IndexByte(p, 0) >= 0 {
		return "embedded null byte"
	}
	var plain *plainError
	if errors.As(err, &plain) {
		return shown(plain.text) // a path formatted into the message, as str() holds it
	}
	return store.PythonOSErrorText(err)
}

// plainError is an OSError raised with a message only, whose str() is that message.
type plainError struct{ text string }

func (e *plainError) Error() string { return e.text }

// ledgerErrorText is str(error) for what a stat or a listing of a host ledger raised: an OSError
// names the ledger as Python holds it, and a message-only one already spells it so.
func ledgerErrorText(err error, ledger string) string {
	var plain *plainError
	if errors.As(err, &plain) {
		return plain.text
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return store.PythonOSErrorText(errno) + ": " + evidence.StrRepr(ledger)
	}
	return store.PythonOSErrorText(err)
}

// entry is one root or host ledger as the reading reports it.
type entry map[string]any

type rootRead struct {
	root       string
	claims     map[string]object
	claimOrder []string
	outcomes   map[string]object
	files      map[string]bool
}

type hostFile struct {
	identity identity
	path     string
	body     object
}

type row struct {
	root, where string
	body        object
}

// reading is the answer being filled, in the order the Python reader fills it, so a reading that
// cannot finish reports exactly what it had reached.
type reading struct {
	window                                                                                   Window
	verdict                                                                                  any
	roots, hostLedgers                                                                       []any
	events, duplicateInvocations, legacyRows, turnsWithMoreThanOneEvent, pairs, pairsOverOne int
	unjudged                                                                                 map[string]any
	lists                                                                                    map[string][]string
	readerFault                                                                              *string
}

var listNames = []string{"eventsWithMoreThanOneAcceptance", "acceptedWithoutOutcome", "outcomesWithoutClaim", "acceptedRowsWithoutLedger", "guardAskedOnDuplicate", "ledgerUnreadable", "rowsUnreadable", "foreignLedgerEntries", "foreignJournalEntries", "invocationsUnrecorded", "acceptedRowsMissing", "duplicatesWithoutClaim", "hostFilesWithoutClaim", "claimsWithoutHostFile", "recordsThatDisagree"}

// sortedLists are deduplicated and sorted once the reading is done; the others keep the order
// they were found in.
var sortedLists = []string{"eventsWithMoreThanOneAcceptance", "acceptedWithoutOutcome", "outcomesWithoutClaim", "invocationsUnrecorded", "acceptedRowsMissing", "ledgerUnreadable", "duplicatesWithoutClaim", "foreignLedgerEntries", "foreignJournalEntries", "hostFilesWithoutClaim", "claimsWithoutHostFile", "recordsThatDisagree"}

func (r *reading) add(list, value string) { r.addShown(list, shown(value)) }

// addShown adds a path already spelled as Python holds it.
func (r *reading) addShown(list, value string) { r.lists[list] = append(r.lists[list], value) }

// Answer is the reading as its JSON document.
func (r *reading) Answer() map[string]any {
	optional := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	answer := map[string]any{
		"predicate": perEventPredicate, "verdict": r.verdict, "roots": r.roots, "hostLedgers": r.hostLedgers,
		"window":                    map[string]any{"since": optional(r.window.Since), "until": optional(r.window.Until), "session": optional(r.window.Session), "turn": optional(r.window.Turn)},
		"events":                    r.events,
		"duplicateInvocations":      r.duplicateInvocations,
		"unjudgedInvocations":       r.unjudged,
		"legacyRows":                r.legacyRows,
		"turnsWithMoreThanOneEvent": r.turnsWithMoreThanOneEvent,
		"supersededPerTurn":         map[string]any{"predicate": supersededPredicate, "pairs": r.pairs, "pairsWithMoreThanOneRow": r.pairsOverOne},
	}
	for _, name := range listNames {
		items := make([]any, len(r.lists[name]))
		for i, v := range r.lists[name] {
			items[i] = v
		}
		answer[name] = items
	}
	if r.readerFault != nil {
		answer["readerFault"] = *r.readerFault
	}
	return answer
}

// Verdict is TRUE, FALSE or UNREADABLE.
func (r *reading) Verdict() string { return r.verdict.(string) }

// Read is whether every Stop event recorded under these journal roots was accepted exactly
// once (completion.stop_events, CRW-212). Read-only: it reads the roots, the host ledgers their
// claims name and those of the Codex homes given. An event is judged as a unit: the window
// chooses the events it reaches, and every record of a chosen event is then checked whatever
// its own time. FALSE when an event was accepted more than once; UNREADABLE when the reading
// cannot vouch for what it read, including any invocation it cannot judge; TRUE otherwise.
// A reading that cannot finish is UNREADABLE and says why in readerFault.
func Read(roots []string, window Window, hosts []string) (r *reading) {
	r = &reading{window: window, roots: []any{}, hostLedgers: []any{}, unjudged: map[string]any{}, lists: map[string][]string{}}
	defer func() {
		if p := recover(); p != nil {
			var fault string
			if python, ok := p.(*evidence.PythonError); ok {
				fault = python.Error()
			} else if err, ok := p.(error); ok {
				fault = pythonOSError(err).Error()
			} else {
				fault = fmt.Sprintf("RuntimeError: %v", p)
			}
			r.verdict = verdictUnreadable
			r.readerFault = &fault
		}
	}()
	r.read(roots, hosts)
	return r
}

func (r *reading) inWindow(at, owner, turn any) bool {
	since, until := r.window.Since, r.window.Until
	s, ok := at.(string)
	if !ok {
		if since != nil || until != nil {
			return false
		}
	} else if (since != nil && s < *since) || (until != nil && s >= *until) {
		return false
	}
	if r.window.Session != nil && owner != *r.window.Session {
		return false
	}
	return r.window.Turn == nil || turn == *r.window.Turn
}

func (r *reading) read(roots, hosts []string) {
	listingFailed := false
	reached := map[identity]string{}
	var read []*rootRead
	var rows []row
	for _, spelled := range roots {
		root, err := abspath(spelled)
		if err != nil {
			panic(err)
		}
		e := entry{"root": shown(root), "state": nil}
		r.roots = append(r.roots, map[string]any(e))
		if strings.IndexByte(root, 0) >= 0 {
			e["state"], e["detail"] = "unreadable", "embedded null byte"
			listingFailed = true
			continue
		}
		info, err := os.Stat(root)
		if errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			e["state"] = "absent"
			continue
		}
		if err != nil {
			e["state"], e["detail"] = "unreadable", errorText(err, root)
			listingFailed = true
			continue
		}
		id := statIdentity(info)
		if _, seen := reached[id]; seen {
			e["state"] = "same_root_as_another_spelling"
			continue
		}
		reached[id] = root
		e["state"] = "read"
		days, ledger, err := r.listRoot(root)
		if err != nil {
			e["state"], e["detail"] = "unreadable", errorText(err, root)
			listingFailed = true
			continue
		}
		one := &rootRead{root: root, claims: map[string]object{}, outcomes: map[string]object{}, files: map[string]bool{}}
		for _, name := range ledger {
			if pyMatch(ledgerName, name) {
				one.files[name[:len(name)-len(".json")]] = true
			}
		}
		for _, name := range ledger {
			p := join(root, hook.LedgerDirectory, name)
			var key string
			isOutcome := false
			switch {
			case pyMatch(outcomeName, name):
				key, isOutcome = name[:len(name)-len(hook.OutcomeSuffix)], true
			case pyMatch(ledgerName, name):
				key = name[:len(name)-len(".json")]
			default:
				r.add("foreignLedgerEntries", p)
				continue
			}
			body, readable, isExact := readRecord(p)
			if !readable || !isExact || !ledgerShape(body, key, isOutcome) {
				r.add("ledgerUnreadable", p)
				continue
			}
			if isOutcome {
				one.outcomes[key] = body.(object)
			} else {
				if _, again := one.claims[key]; !again {
					one.claimOrder = append(one.claimOrder, key)
				}
				one.claims[key] = body.(object)
			}
		}
		for _, d := range days {
			if !day(d) {
				r.add("rowsUnreadable", join(root, d))
				continue
			}
			entries, err := os.ReadDir(join(root, d))
			if err != nil {
				e["state"], e["detail"] = "unreadable", errorText(err, join(root, d))
				listingFailed = true
				break
			}
			var names []string
			for _, found := range entries {
				if found.Type().IsRegular() && pyMatch(journalName, found.Name()) {
					names = append(names, found.Name())
				} else {
					r.add("foreignJournalEntries", join(root, d, found.Name()))
				}
			}
			for _, name := range names {
				where := join(root, d, name)
				body, readable, isExact := readRecord(where)
				o, isObject := asObject(body)
				if !readable || !isObject || !(exact(get(o, "recordVersion"), 1) || exact(get(o, "recordVersion"), hook.RecordVersion)) || (exact(get(o, "recordVersion"), hook.RecordVersion) && !(isExact && rowShape(o))) {
					r.add("rowsUnreadable", where)
					continue
				}
				rows = append(rows, row{root, where, o})
			}
		}
		read = append(read, one)
	}
	filesOf := map[string]map[string]bool{}
	everyClaim := map[string]bool{}
	for _, one := range read {
		filesOf[one.root] = one.files
		for k := range one.files {
			everyClaim[k] = true
		}
	}

	// The host ledgers: every one a claim names, and those of the Codex homes given. Each is keyed
	// and named as Python holds it, a str, and handed to the system as os.fsencode gives it: one
	// given on the command line is its bytes, which Python holds surrogateescaped, and one a claim
	// names is its JSON string, where such a byte is its surrogate escape.
	type wantedLedger struct {
		spelled  string
		recorded bool
	}
	var wanted []wantedLedger
	for _, home := range hosts {
		// Path(home).expanduser().joinpath(*HOST_LEDGER_PARTS), made absolute below.
		expanded, err := expandUser(home)
		if err != nil {
			panic(err)
		}
		wanted = append(wanted, wantedLedger{spelled: join(pathlibForm(expanded), hook.HostLedgerParts...)})
	}
	for _, one := range read {
		for _, key := range one.claimOrder {
			claimedBy, _ := asObject(get(one.claims[key], "claimedBy"))
			wanted = append(wanted, wantedLedger{spelled: get(claimedBy, "hostLedger").(string), recorded: true})
		}
	}
	hostFiles := map[string][]hostFile{}
	ledgerIdentity := map[string]*identity{}
	ledgersReached := map[identity]bool{}
	for _, want := range wanted {
		// ledger is the str, system the bytes the system is handed for it.
		var ledger, system string
		if want.recorded {
			// Absolute, normalized and one the system takes (hostLedgerNamed), so abspath leaves
			// it as it is and it encodes.
			ledger = want.spelled
			system, _ = fspath.FSEncode(ledger)
		} else {
			var err error
			if system, err = absolute(want.spelled); err != nil {
				panic(err)
			}
			ledger = shown(system)
		}
		if _, seen := ledgerIdentity[ledger]; seen {
			continue
		}
		ledgerIdentity[ledger] = nil
		e := entry{"ledger": ledger, "state": nil}
		r.hostLedgers = append(r.hostLedgers, map[string]any(e))
		if strings.IndexByte(system, 0) >= 0 {
			e["state"], e["detail"] = "unreadable", "embedded null byte"
			listingFailed = true
			continue
		}
		info, err := os.Stat(system)
		if errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			e["state"] = "absent"
			continue
		}
		if err != nil {
			e["state"], e["detail"] = "unreadable", ledgerErrorText(err, ledger)
			listingFailed = true
			continue
		}
		id := statIdentity(info)
		ledgerIdentity[ledger] = &id
		if ledgersReached[id] {
			e["state"] = "same_ledger_as_another_spelling"
			continue
		}
		var names []string
		if !info.IsDir() {
			err = &plainError{ledger + " is not a directory"}
		} else {
			names, err = listNamesOf(system)
		}
		if err != nil {
			e["state"], e["detail"] = "unreadable", ledgerErrorText(err, ledger)
			listingFailed = true
			continue
		}
		ledgersReached[id] = true
		e["state"] = "read"
		for _, name := range names {
			p := join(ledger, shown(name)) // os.path.join(ledger, name)
			if !pyMatch(ledgerName, name) {
				r.addShown("foreignLedgerEntries", p)
				continue
			}
			key := name[:len(name)-len(".json")]
			body, readable, isExact := readRecord(join(system, name))
			if !readable || !isExact || !hostShape(body, key) {
				r.addShown("ledgerUnreadable", p)
				continue
			}
			hostFiles[key] = append(hostFiles[key], hostFile{id, p, body.(object)})
		}
	}

	// The events the window reaches: any of their records in it.
	selected := map[string]bool{}
	for _, one := range read {
		for key, body := range one.claims {
			if r.inWindow(get(body, "claimedAt"), get(body, "sessionId"), get(body, "turnId")) {
				selected[key] = true
			}
		}
		for key, body := range one.outcomes {
			if r.inWindow(get(body, "at"), get(body, "sessionId"), get(body, "turnId")) {
				selected[key] = true
			}
		}
	}
	for key, files := range hostFiles {
		for _, file := range files {
			if r.inWindow(get(file.body, "claimedAt"), get(file.body, "sessionId"), get(file.body, "turnId")) {
				selected[key] = true
			}
		}
	}
	for _, one := range rows {
		if exact(get(one.body, "recordVersion"), hook.RecordVersion) && get(one.body, "eventKey") != nil && r.inWindow(get(one.body, "at"), get(one.body, "sessionId"), get(one.body, "turnId")) {
			selected[get(one.body, "eventKey").(string)] = true
		}
	}

	prescanUnreachable := 0
	pairs := map[string]int{}
	var pairOrder []string
	acceptedRows := map[string][]string{}
	acceptedAt := map[[2]string][]string{}
	type duplicate struct{ key, where string }
	var duplicateRows []duplicate
	for _, one := range rows {
		body := one.body
		chosen := r.inWindow(get(body, "at"), get(body, "sessionId"), get(body, "turnId"))
		if chosen && evidence.Truthy(get(body, "sessionId")) && evidence.Truthy(get(body, "turnId")) {
			pair := dictKey("tuple", get(body, "sessionId")) + "\x00" + dictKey("tuple", get(body, "turnId"))
			if _, seen := pairs[pair]; !seen {
				pairOrder = append(pairOrder, pair)
			}
			pairs[pair]++
		}
		if exact(get(body, "recordVersion"), 1) {
			if chosen {
				r.legacyRows++
			}
			continue
		}
		acceptance, key := get(body, "acceptance"), get(body, "eventKey")
		if key == nil {
			if !chosen {
				continue
			}
			var label string
			if acceptance == hook.Unestablished {
				identity, _ := asObject(get(body, "eventIdentity"))
				label = hook.Unestablished + ":" + get(identity, "reason").(string)
			} else {
				label = "no_event:" + evidence.Text(get(body, "adapterOutcome"))
			}
			n, _ := r.unjudged[label].(int)
			r.unjudged[label] = n + 1
			if hook.NativePrescanUnreachable(body) {
				prescanUnreachable++
			}
			continue
		}
		k := key.(string)
		if !selected[k] {
			continue
		}
		switch acceptance {
		case hook.Accepted:
			acceptedRows[k] = append(acceptedRows[k], one.where)
			acceptedAt[[2]string{one.root, k}] = append(acceptedAt[[2]string{one.root, k}], one.where)
			if !filesOf[one.root][k] {
				r.add("acceptedRowsWithoutLedger", one.where)
			}
		case hook.Duplicate:
			r.duplicateInvocations++
			duplicateRows = append(duplicateRows, duplicate{k, one.where})
			if get(body, "acceptedAs") == hook.LedgerDirectory+"/"+k+".json" && !filesOf[one.root][k] {
				// It found the accepted record in its own root, which does not hold one.
				r.add("recordsThatDisagree", one.where)
			}
			if get(body, "guardInvoked") != false {
				r.add("guardAskedOnDuplicate", one.where)
			}
		default:
			label := acceptance.(string)
			n, _ := r.unjudged[label].(int)
			r.unjudged[label] = n + 1
		}
	}

	events := map[string]bool{}
	eventsPerTurn := map[[2]string]map[string]bool{}
	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		holding := 0
		for _, one := range read {
			if one.files[key] {
				holding++
			}
		}
		if holding > 1 || len(acceptedRows[key]) > 1 {
			r.add("eventsWithMoreThanOneAcceptance", key)
		}
		if holding > 0 {
			events[key] = true
		}
		for _, one := range read {
			claim, hasClaim := one.claims[key]
			outcome, hasOutcome := one.outcomes[key]
			if hasOutcome && !one.files[key] {
				r.add("outcomesWithoutClaim", key)
			}
			if !hasClaim {
				continue
			}
			turn := [2]string{get(claim, "sessionId").(string), get(claim, "turnId").(string)}
			if eventsPerTurn[turn] == nil {
				eventsPerTurn[turn] = map[string]bool{}
			}
			eventsPerTurn[turn][key] = true
			// One owner wrote the host file, this claim, the outcome and the accepted row in one
			// run: they name one slot and one process, and the accepted row sits in that slot.
			claimedBy, _ := asObject(get(claim, "claimedBy"))
			slotName, pid := get(claimedBy, "attemptRow").(string), get(claimedBy, "pid")
			thisRoot := identityOf(one.root)
			for _, file := range hostFiles[key] {
				by, _ := asObject(get(file.body, "claimedBy"))
				owner, _ := get(by, "journalRoot").(string)
				if owner != "" && sameIdentity(recordedIdentity(owner), thisRoot) && (get(by, "attemptRow") != slotName || !evidence.Equal(get(by, "pid"), pid)) {
					r.addShown("recordsThatDisagree", file.path)
				}
			}
			for _, where := range acceptedAt[[2]string{one.root, key}] {
				if where != join(one.root, slotName) {
					r.add("recordsThatDisagree", where)
				}
			}
			// os.path.abspath of the claim's ledger is the ledger as it names it (hostLedgerNamed).
			id := ledgerIdentity[get(claimedBy, "hostLedger").(string)]
			holds := false
			for _, file := range hostFiles[key] {
				holds = holds || (id != nil && file.identity == *id)
			}
			if !holds {
				r.add("claimsWithoutHostFile", join(one.root, hook.LedgerDirectory, key+".json"))
			}
			if !hasOutcome {
				r.add("acceptedWithoutOutcome", key)
				continue
			}
			outcomePath := join(one.root, hook.LedgerDirectory, key+hook.OutcomeSuffix)
			if get(claim, "sessionId") != get(outcome, "sessionId").(string) || get(claim, "turnId") != get(outcome, "turnId").(string) {
				r.add("ledgerUnreadable", outcomePath)
			}
			policy := get(outcome, "journalPolicy")
			if policy == hook.NoJournal {
				r.add("invocationsUnrecorded", key)
			}
			// Whether the owner's policy wrote its row: every_invocation always, faults_only
			// for anything but a plain answer, no_journal never.
			kept := policy == hook.EveryInvocation || (policy == hook.FaultsOnly && !member(get(outcome, "adapterOutcome"), []string{hook.GuardAnswered, hook.DuplicateInvocation}))
			attemptRow := get(outcome, "attemptRow")
			if attemptRow == nil {
				if kept {
					// The row was to be written, so the accepted one's is missing.
					r.add("acceptedRowsMissing", key)
				}
				if len(acceptedAt[[2]string{one.root, key}]) > 0 {
					// An accepted row the outcome says was never written.
					r.add("recordsThatDisagree", outcomePath)
				}
				continue
			}
			if !kept || attemptRow != slotName {
				r.add("recordsThatDisagree", outcomePath)
			}
			namedRow := readRow(one.root, attemptRow.(string))
			if namedRow == nil || !exact(get(namedRow, "recordVersion"), hook.RecordVersion) || get(namedRow, "acceptance") != hook.Accepted || get(namedRow, "eventKey") != key {
				r.add("acceptedRowsMissing", key)
			} else {
				for _, field := range []string{"adapterOutcome", "guardDecision", "guardState", "held"} {
					if !evidence.Equal(get(namedRow, field), get(outcome, field)) {
						r.add("recordsThatDisagree", outcomePath)
						break
					}
				}
			}
		}
		for _, file := range hostFiles[key] {
			by, _ := asObject(get(file.body, "claimedBy"))
			owner, _ := get(by, "journalRoot").(string)
			ownedBy := ""
			if owner != "" {
				if id := recordedIdentity(owner); id != nil {
					ownedBy = reached[*id]
				}
			}
			if ownedBy == "" || !filesOf[ownedBy][key] {
				r.addShown("hostFilesWithoutClaim", file.path)
			}
		}
	}
	for _, d := range duplicateRows {
		if !everyClaim[d.key] {
			r.add("duplicatesWithoutClaim", d.where)
		}
	}
	r.events = len(events)
	for _, keys := range eventsPerTurn {
		if len(keys) > 1 {
			r.turnsWithMoreThanOneEvent++
		}
	}
	r.pairs = len(pairs)
	for _, pair := range pairOrder {
		if pairs[pair] > 1 {
			r.pairsOverOne++
		}
	}
	for _, name := range sortedLists {
		values := slices.Clone(r.lists[name])
		sort.Strings(values)
		r.lists[name] = slices.Compact(values)
	}
	// Decision 22: a readable native pre-scan unreachable row proves the guard was unreachable,
	// not that an event was accepted. It keeps its unjudged count and never makes the reading
	// UNREADABLE on its own; every other unjudged row keeps the rule.
	otherUnjudged := -prescanUnreachable
	for _, n := range r.unjudged {
		otherUnjudged += n.(int)
	}
	nonEmpty := func(names ...string) bool {
		for _, name := range names {
			if len(r.lists[name]) > 0 {
				return true
			}
		}
		return false
	}
	switch {
	case nonEmpty("eventsWithMoreThanOneAcceptance", "acceptedRowsWithoutLedger", "guardAskedOnDuplicate"):
		r.verdict = verdictFalse
	case listingFailed || nonEmpty("ledgerUnreadable", "rowsUnreadable", "outcomesWithoutClaim", "acceptedWithoutOutcome", "invocationsUnrecorded", "acceptedRowsMissing", "foreignLedgerEntries", "foreignJournalEntries", "duplicatesWithoutClaim", "hostFilesWithoutClaim", "claimsWithoutHostFile", "recordsThatDisagree") || otherUnjudged != 0 || r.legacyRows != 0 || (len(events) == 0 && prescanUnreachable == 0):
		r.verdict = verdictUnreadable
	default:
		r.verdict = verdictTrue
	}
}

// listRoot lists a journal root: its day directories, every entry that is not one (foreign),
// and its ledger's names. A link or a non-directory where the ledger goes cannot be read, which
// is not the same answer as an empty ledger.
func (r *reading) listRoot(root string) (days, ledger []string, err error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	for _, found := range entries {
		if found.Name() == hook.LedgerDirectory {
			continue
		}
		if found.IsDir() && pyMatch(journalDay, found.Name()) {
			days = append(days, found.Name())
		} else {
			r.add("foreignJournalEntries", join(root, found.Name()))
		}
	}
	accepted := join(root, hook.LedgerDirectory)
	info, err := os.Lstat(accepted)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return nil, nil, &plainError{accepted + " is a link, which the adapter never makes"}
	case err == nil && info.IsDir():
		ledger, err = listNamesOf(accepted)
		return days, ledger, err
	case err == nil:
		return nil, nil, &plainError{accepted + " is not a directory"}
	}
	return days, nil, nil
}

func listNamesOf(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, found := range entries {
		names[i] = found.Name()
	}
	return names, nil
}

// readRow is the row an outcome names, read only from the shape a row takes under its root.
func readRow(root, named string) object {
	if !slot(named) {
		return nil
	}
	parts := strings.Split(named, "/")
	body, readable, isExact := readRecord(join(root, parts[0], parts[1]))
	o, ok := asObject(body)
	if !readable || !isExact || !ok {
		return nil
	}
	return o
}

// pathlibForm is str(Path(p)): repeated separators and "." components collapse, ".." stays, a
// trailing separator goes, and exactly two leading separators are kept.
func pathlibForm(p string) string {
	if p == "" {
		return "."
	}
	prefix := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		prefix = "//"
	case strings.HasPrefix(p, "/"):
		prefix = "/"
	}
	var kept []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			kept = append(kept, part)
		}
	}
	joined := prefix + strings.Join(kept, "/")
	if joined == "" {
		return "."
	}
	return joined
}
