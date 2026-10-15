package ops

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// This file is the region judgement of the lane as Go ops (CRW-1441): crw.regionsOverlap answers whether two nodes' region lists may
// touch the same place by the relay's own rule (dagsched.Overlaps), and crw.admissionRegions makes a node's admission regions as
// AdmissionRegions does. Both are pure: they read only their arguments, send nothing to the world and write no field a proof reads, so
// a replayed row is the answer a live run gives (provenAlways), and neither is a recording op of proofs.go.

// Op names of this file.
const (
	NameRegionsOverlap   = "regionsOverlap"
	NameAdmissionRegions = "admissionRegions"
)

// Ops of this file.
const (
	OpRegionsOverlap   = "crw." + NameRegionsOverlap
	OpAdmissionRegions = "crw." + NameAdmissionRegions
)

// relayRegion is the relay region of a region's text as cli/schedrelay.go declares a DESIGN edit region read from its text: the
// normal form (RegionOf) as its path, kind and key, no change and no whole-repository hold, and the repository left empty (a lane
// has one repository, and Overlaps only asks that both sides name the same one). A hotspot or a shared contract file still holds its
// whole file, which Overlaps reads from the path.
//
// Every argument is text, so this is not the region DeclareRegions stores for a region the plan states in structured form (a known,
// accepted gap): a planned rename, delete or exclusive flag is dropped and the region is judged as an edit, and a planned file or
// tree whose path holds "#" is read as a symbol (`pkg/a#b.go` is the symbol b.go of pkg/a), which is another place.
func relayRegion(text string) dagsched.Region {
	f := RegionOf(text)
	return dagsched.Region{Path: f.Path, Kind: f.Kind, Key: f.Key}
}

// RegionsOverlap says whether two nodes with these region lists may edit the same place: an undeclared node (no regions) overlaps
// every other, and two declared nodes overlap when any region of one overlaps any region of the other as the relay judges it
// (dagsched.Overlaps of relayRegion). The answer does not depend on the order of the two lists.
func RegionsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	rb := make([]dagsched.Region, len(b))
	for i, y := range b {
		rb[i] = relayRegion(y)
	}
	for _, x := range a {
		rx := relayRegion(x)
		for _, ry := range rb {
			if dagsched.Overlaps(rx, ry) {
				return true
			}
		}
	}
	return false
}

// regionsOverlapOp is crw.regionsOverlap(a, b): RegionsOverlap of two region lists; null or a missing list is undeclared.
type regionsOverlapOp struct{ l *Lane }

func (o *regionsOverlapOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var a, b []string
	if err := args(req.Args, &a, &b); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.regionsOverlap(a, b): %w", err)
	}
	return engine.OpResult{Result: result(RegionsOverlap(a, b))}, nil
}

func (o *regionsOverlapOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// admissionRegionsOp is crw.admissionRegions(plan, edit): AdmissionRegions of the plan's declared regions and the DESIGN's edit
// regions. The answer is a list, [] when either side is empty or null, never null.
type admissionRegionsOp struct{ l *Lane }

func (o *admissionRegionsOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var plan, edit []string
	if err := args(req.Args, &plan, &edit); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.admissionRegions(plan, edit): %w", err)
	}
	return engine.OpResult{Result: result(AdmissionRegions(plan, edit))}, nil
}

func (o *admissionRegionsOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}
