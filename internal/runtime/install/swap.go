package install

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// swapped is what one swap did: the commit, and, once it landed, whether the pointer reaches the
// runtime and what was put back when it does not.
type swapped struct {
	committed reading.Reading
	commitErr error
	// placeErr is why the pointer could not be placed, and why why it does not reach the runtime
	// once placed; landed is neither.
	placeErr error
	why      string
	landed   bool
	// pointerRestored is the pointer put back (restorePointer), when this swap moved it and it did
	// not land; restored is the selection put back (restoreSelection), when the delta selected
	// something and the pointer did not land.
	pointerRestored Object
	restored        Object
}

// commitFailed is whether the commit did not land, so nothing was moved.
func (s swapped) commitFailed() bool { return s.commitErr != nil || !s.committed.Usable() }

// placement is why the pointer does not reach the runtime: it could not be placed, or it does
// not reach runtime (as the caller names it) once placed.
func (s swapped) placement(runtime string) string {
	if s.placeErr != nil {
		return "the pointer could not be placed: " + s.placeErr.Error()
	}
	return "the pointer does not reach " + runtime + " after it was placed: " + s.why
}

// swap is the one commit-then-move step a promotion, a resumed promotion and a rollback take,
// under the promotion lock and on the reading the caller decided on (the caller holds its locks
// in the order of decision 33): delta - the selection, the pointer's placement and outgoing - is
// committed first (OPS-4.4: a transition is committed before its side effect), then the pointer
// is placed at environment when move is set, and read back as a host reaches it (landedAt). When
// it does not land, what this run changed is put back to what the reading found: the pointer
// and its ownership (before, ownedBefore) when this run moved it, and, when delta selected
// anything, the selection and outgoing (previous, outgoingBefore), only where the record still
// holds what this run wrote. A commit that does not land moves nothing and puts nothing back.
func swap(ctx context.Context, o Options, pointerPath, environment string, delta record.Delta, move bool, before pointer.Answer, ownedBefore, previous, outgoingBefore Object) swapped {
	var s swapped
	if s.committed, s.commitErr = commitSelection(ctx, o.RecordPath, definition.Version, delta); s.commitFailed() {
		return s
	}
	if move {
		s.placeErr = placePointer(pointerPath, environment)
	}
	if s.landed, s.why = landedAt(pointerPath, environment); s.placeErr == nil && s.landed {
		return s
	}
	s.landed = false
	if move {
		s.pointerRestored = restorePointer(o, pointerPath, before, environment, ownedBefore)
	}
	if len(delta.Select) > 0 {
		s.restored = restoreSelection(o, previous, delta.Select, outgoingBefore)
	}
	return s
}
