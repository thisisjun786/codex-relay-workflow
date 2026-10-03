//go:build dev

package stopevents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
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
	if err != nil {
		return nil
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil
	}
	found := statIdentity(info)
	return &found
}

// recordedIdentity is identityOf for a path a record names: a byte that is not UTF-8, which the
// record spells as its surrogate escape, is handed to the system as that byte again; a surrogate
// that escapes no byte names no path, which reaches nothing.
func recordedIdentity(spelled string) *identity {
	system, ok := pyvalue.FSEncode(spelled)
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

// expandUser expands a leading ~ or ~user as the runtime does (record.ExpandUser: HOME when it
// is set at all, an empty HOME the root, an unset one this user's passwd entry). A ~ nothing
// answers, or one that still leaves a ~, is an error: the literal ~ would be read as a relative
// path. Nothing else of the spelling is changed; "./~" names a directory called ~.
func expandUser(spelled string) (string, error) {
	expanded, err := record.ExpandUser(spelled, os.LookupEnv)
	if err != nil {
		return "", fmt.Errorf("cannot expand %q: %w", spelled, err)
	}
	if strings.HasPrefix(spelled, "~") && strings.HasPrefix(expanded, "~") {
		return "", fmt.Errorf("cannot expand %q: the home directory it names begins with ~", spelled)
	}
	return expanded, nil
}

// abspath is the absolute, normalized path a spelling given on the command line names.
func abspath(spelled string) (string, error) {
	expanded, err := expandUser(spelled)
	if err != nil {
		return "", err
	}
	return absolute(expanded)
}

// absolute joins a relative path to the working directory the kernel reports (os.Getwd would
// answer $PWD, a link's spelling of it) and cleans it.
func absolute(p string) (string, error) {
	if !isAbs(p) {
		cwd, err := syscall.Getwd()
		if err != nil {
			return "", fmt.Errorf("cannot make %q absolute: %w", p, os.NewSyscallError("getwd", err))
		}
		p = cwd + "/" + p
	}
	return filepath.Clean(p), nil
}

// join is root/name... for a normalized absolute root.
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

// shown is a path as the records spell one: a byte that is not UTF-8 is the lone surrogate
// escape (\udcXX) the adapter's records give it, so a path read from the system and one a
// record names compare and print alike.
func shown(p string) string { return pyvalue.FSDecode(p) }

// errorText is the detail of a failed stat or listing: the error's own text, its path spelled as
// the records spell one.
func errorText(err error) string { return shown(err.Error()) }

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

// reading is the answer being filled, in the order the reading fills it, so a reading that
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

// addShown adds a path already spelled as the records spell it.
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
			fault := shown(fmt.Sprint(p))
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
		info, err := os.Stat(root)
		if errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			e["state"] = "absent"
			continue
		}
		if err != nil {
			e["state"], e["detail"] = "unreadable", errorText(err)
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
			e["state"], e["detail"] = "unreadable", errorText(err)
			listingFailed = true
			continue
		}
		one := &rootRead{root: root, claims: map[string]object{}, outcomes: map[string]object{}, files: map[string]bool{}}
		for _, name := range ledger {
			if ledgerName.MatchString(name) {
				one.files[name[:len(name)-len(".json")]] = true
			}
		}
		for _, name := range ledger {
			p := join(root, hook.LedgerDirectory, name)
			var key string
			isOutcome := false
			switch {
			case outcomeName.MatchString(name):
				key, isOutcome = name[:len(name)-len(hook.OutcomeSuffix)], true
			case ledgerName.MatchString(name):
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
				e["state"], e["detail"] = "unreadable", errorText(err)
				listingFailed = true
				break
			}
			var names []string
			for _, found := range entries {
				if found.Type().IsRegular() && journalName.MatchString(found.Name()) {
					names = append(names, found.Name())
				} else {
					r.add("foreignJournalEntries", join(root, d, found.Name()))
				}
			}
			for _, name := range names {
				where := join(root, d, name)
				body, readable, isExact := readRecord(where)
				o, isObject := asObject(body)
				if !readable || !isObject || !(exact(o.Get("recordVersion"), 1) || exact(o.Get("recordVersion"), hook.RecordVersion)) || (exact(o.Get("recordVersion"), hook.RecordVersion) && !(isExact && rowShape(o))) {
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
	// and named as the records spell it (shown), and handed to the system as its bytes: one given
	// on the command line is its bytes already, and one a claim names is its JSON string, where a
	// byte that is not UTF-8 is its surrogate escape (pyvalue.FSEncode gives the byte back).
	type wantedLedger struct {
		spelled  string
		recorded bool
	}
	var wanted []wantedLedger
	for _, home := range hosts {
		// The home's host ledger, made absolute below.
		expanded, err := expandUser(home)
		if err != nil {
			panic(err)
		}
		wanted = append(wanted, wantedLedger{spelled: join(expanded, hook.HostLedgerParts...)})
	}
	for _, one := range read {
		for _, key := range one.claimOrder {
			claimedBy, _ := asObject(one.claims[key].Get("claimedBy"))
			wanted = append(wanted, wantedLedger{spelled: claimedBy.Get("hostLedger").(string), recorded: true})
		}
	}
	hostFiles := map[string][]hostFile{}
	ledgerIdentity := map[string]*identity{}
	ledgersReached := map[identity]bool{}
	for _, want := range wanted {
		// ledger is the spelling, system the bytes the system is handed for it.
		var ledger, system string
		if want.recorded {
			// Absolute, normalized and one the system takes (hostLedgerNamed), so it is read as
			// it is spelled and it encodes.
			ledger = want.spelled
			system, _ = pyvalue.FSEncode(ledger)
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
		info, err := os.Stat(system)
		if errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			e["state"] = "absent"
			continue
		}
		if err != nil {
			e["state"], e["detail"] = "unreadable", errorText(err)
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
			err = fmt.Errorf("%s is not a directory", system)
		} else {
			names, err = listNamesOf(system)
		}
		if err != nil {
			e["state"], e["detail"] = "unreadable", errorText(err)
			listingFailed = true
			continue
		}
		ledgersReached[id] = true
		e["state"] = "read"
		for _, name := range names {
			p := join(ledger, shown(name))
			if !ledgerName.MatchString(name) {
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
			if r.inWindow(body.Get("claimedAt"), body.Get("sessionId"), body.Get("turnId")) {
				selected[key] = true
			}
		}
		for key, body := range one.outcomes {
			if r.inWindow(body.Get("at"), body.Get("sessionId"), body.Get("turnId")) {
				selected[key] = true
			}
		}
	}
	for key, files := range hostFiles {
		for _, file := range files {
			if r.inWindow(file.body.Get("claimedAt"), file.body.Get("sessionId"), file.body.Get("turnId")) {
				selected[key] = true
			}
		}
	}
	for _, one := range rows {
		if exact(one.body.Get("recordVersion"), hook.RecordVersion) && one.body.Get("eventKey") != nil && r.inWindow(one.body.Get("at"), one.body.Get("sessionId"), one.body.Get("turnId")) {
			selected[one.body.Get("eventKey").(string)] = true
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
		chosen := r.inWindow(body.Get("at"), body.Get("sessionId"), body.Get("turnId"))
		if chosen && pyvalue.Truthy(body.Get("sessionId")) && pyvalue.Truthy(body.Get("turnId")) {
			pair := valueKey(body.Get("sessionId")) + "\x00" + valueKey(body.Get("turnId"))
			if _, seen := pairs[pair]; !seen {
				pairOrder = append(pairOrder, pair)
			}
			pairs[pair]++
		}
		if exact(body.Get("recordVersion"), 1) {
			if chosen {
				r.legacyRows++
			}
			continue
		}
		acceptance, key := body.Get("acceptance"), body.Get("eventKey")
		if key == nil {
			if !chosen {
				continue
			}
			var label string
			if acceptance == hook.Unestablished {
				identity, _ := asObject(body.Get("eventIdentity"))
				label = hook.Unestablished + ":" + identity.Get("reason").(string)
			} else {
				label = "no_event:" + pyvalue.Str(body.Get("adapterOutcome"))
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
			if body.Get("acceptedAs") == hook.LedgerDirectory+"/"+k+".json" && !filesOf[one.root][k] {
				// It found the accepted record in its own root, which does not hold one.
				r.add("recordsThatDisagree", one.where)
			}
			if body.Get("guardInvoked") != false {
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
			turn := [2]string{claim.Get("sessionId").(string), claim.Get("turnId").(string)}
			if eventsPerTurn[turn] == nil {
				eventsPerTurn[turn] = map[string]bool{}
			}
			eventsPerTurn[turn][key] = true
			// One owner wrote the host file, this claim, the outcome and the accepted row in one
			// run: they name one slot and one process, and the accepted row sits in that slot.
			claimedBy, _ := asObject(claim.Get("claimedBy"))
			slotName, pid := claimedBy.Get("attemptRow").(string), claimedBy.Get("pid")
			thisRoot := identityOf(one.root)
			for _, file := range hostFiles[key] {
				by, _ := asObject(file.body.Get("claimedBy"))
				owner, _ := by.Get("journalRoot").(string)
				if owner != "" && sameIdentity(recordedIdentity(owner), thisRoot) && (by.Get("attemptRow") != slotName || !pyvalue.ItemEqual(by.Get("pid"), pid)) {
					r.addShown("recordsThatDisagree", file.path)
				}
			}
			for _, where := range acceptedAt[[2]string{one.root, key}] {
				if where != join(one.root, slotName) {
					r.add("recordsThatDisagree", where)
				}
			}
			// The claim's ledger is keyed as it names it, absolute and normalized (hostLedgerNamed).
			id := ledgerIdentity[claimedBy.Get("hostLedger").(string)]
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
			if claim.Get("sessionId") != outcome.Get("sessionId").(string) || claim.Get("turnId") != outcome.Get("turnId").(string) {
				r.add("ledgerUnreadable", outcomePath)
			}
			policy := outcome.Get("journalPolicy")
			if policy == hook.NoJournal {
				r.add("invocationsUnrecorded", key)
			}
			// Whether the owner's policy wrote its row: every_invocation always, faults_only
			// for anything but a plain answer, no_journal never.
			kept := policy == hook.EveryInvocation || (policy == hook.FaultsOnly && !member(outcome.Get("adapterOutcome"), []string{hook.GuardAnswered, hook.DuplicateInvocation}))
			attemptRow := outcome.Get("attemptRow")
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
			if namedRow == nil || !exact(namedRow.Get("recordVersion"), hook.RecordVersion) || namedRow.Get("acceptance") != hook.Accepted || namedRow.Get("eventKey") != key {
				r.add("acceptedRowsMissing", key)
			} else {
				for _, field := range []string{"adapterOutcome", "guardDecision", "guardState", "held"} {
					if !pyvalue.ItemEqual(namedRow.Get(field), outcome.Get(field)) {
						r.add("recordsThatDisagree", outcomePath)
						break
					}
				}
			}
		}
		for _, file := range hostFiles[key] {
			by, _ := asObject(file.body.Get("claimedBy"))
			owner, _ := by.Get("journalRoot").(string)
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
		if found.IsDir() && journalDay.MatchString(found.Name()) {
			days = append(days, found.Name())
		} else {
			r.add("foreignJournalEntries", join(root, found.Name()))
		}
	}
	accepted := join(root, hook.LedgerDirectory)
	info, err := os.Lstat(accepted)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return nil, nil, fmt.Errorf("%s is a link, which the adapter never makes", accepted)
	case err == nil && info.IsDir():
		ledger, err = listNamesOf(accepted)
		return days, ledger, err
	case err == nil:
		return nil, nil, fmt.Errorf("%s is not a directory", accepted)
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
