package engine

import (
	"fmt"
	"strconv"
	"strings"
)

// RootPath is the path of the script's top-level sequence.
const RootPath = "root"

// pathState is one path of the run: a sequence of positions with at most one outstanding. It is touched only by the
// goroutine that runs the script (a call settles when the engine delivers its result there), so it needs no lock and
// every position is a function of the script and the settled results only.
type pathState struct {
	id   string
	next int
	open *openPosition
	// The resume walk (F07): suffix is set once a position of the path (or a join it opened) was not replayed, so every
	// later position of the path runs live; join is the join whose item this path is, which learns that a position of the
	// path ran live; joins is the chain of the digests of the joins settled on the path so far, which every later position's
	// input digest carries.
	suffix bool
	join   *joinState
	joins  string
}

type openPosition struct {
	position int
	label    string
}

// PathConcurrencyError is the error opening a second position on a path whose earlier call or join has not settled.
// It carries both labels; the script sees it as CrwPathConcurrencyError.
type PathConcurrencyError struct {
	Path     string
	Open     string // the outstanding call or join
	Requests string // the call or join that was refused
}

func (e *PathConcurrencyError) Error() string {
	return fmt.Sprintf("path %s: cannot open %q while %q has not settled: a path has one outstanding position; run concurrent work in parallel(), pipeline() or crw.dag items", e.Path, e.Requests, e.Open)
}

// openPosition claims the next position of the path for label, or reports the outstanding one.
func (p *pathState) openPosition(label string) (int, error) {
	if p.open != nil {
		return 0, &PathConcurrencyError{Path: p.id, Open: p.open.label, Requests: label}
	}
	p.next++
	p.open = &openPosition{position: p.next, label: label}
	return p.next, nil
}

// settle frees the path once the position's call or join has settled.
func (p *pathState) settle(position int) {
	if p.open != nil && p.open.position == position {
		p.open = nil
	}
}

// child returns the path of a join's item: the join's position and the item's key name it, so two joins on one path
// and two items of one join never share a path.
func (p *pathState) child(position int, key string, join *joinState) *pathState {
	// a path a suffix position opens is suffix itself: only the positions of the parent before the call that created the path
	// decide it, and the call that created it (a join) does not count
	return &pathState{id: p.childID(position, key), suffix: p.suffix, join: join}
}

// childID is the id of the path of a join's item.
func (p *pathState) childID(position int, key string) string {
	return p.id + "/" + strconv.Itoa(position) + ":" + escapeKey(key)
}

// keyEscaper makes an item key safe as a path segment: the characters that separate segments and positions are
// percent-encoded (and the percent sign itself, so the encoding is reversible), which keeps the mapping from
// (position, key) sequences to path ids one-to-one whatever the keys hold.
var keyEscaper = strings.NewReplacer("%", "%25", "/", "%2F", ":", "%3A")

func escapeKey(key string) string { return keyEscaper.Replace(key) }

// joinState is a parallel() or pipeline() on a path: one position of the parent that settles when every item has.
type joinState struct {
	parent   *pathState
	position int
	label    string
	// live is set when a position under the join ran live (or a join under it did not replay): the join does not replay.
	live bool
}
