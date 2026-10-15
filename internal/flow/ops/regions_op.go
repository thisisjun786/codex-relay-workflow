package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// This file is the region judgement of the lane as Go ops (CRW-1441): crw.regionsOverlap answers whether two nodes' region lists may
// touch the same place by the relay's own rule (dagsched.Overlaps) and by Scope's reading of a region, and crw.admissionRegions makes
// a node's admission regions as AdmissionRegions does. A region is a text or a planned region's object (LaneRegion). Both are pure: they read only their arguments, send nothing to the world and write no field a proof reads, so
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

// LaneRegion is a region as the lane hands it to crw.regionsOverlap and crw.admissionRegions: its text (`a.go`, `src/`, `a.go#F`)
// and, for a region the plan states, the plan's change (edit, rename or delete; none is an edit) and exclusive flag (the declarer's
// whole-repository hold). In JSON it is either the bare text, which keeps the meaning a text always had (an edit, no hold), or the
// object {"text": ..., "change": ..., "exclusive": ...}; an object with another field, or a change the relay does not know, is refused.
// A region with no change and no hold is written back as its text.
type LaneRegion struct {
	Text      string `json:"text"`
	Change    string `json:"change,omitempty"`
	Exclusive bool   `json:"exclusive,omitempty"`
}

// UnmarshalJSON reads the text form and the object form.
func (r *LaneRegion) UnmarshalJSON(raw []byte) error {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		*r = LaneRegion{Text: s}
		return nil
	}
	type plain LaneRegion
	var p plain
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("a region is a text or an object {text, change, exclusive}: %w", err)
	}
	switch p.Change {
	case "", "edit", "rename", "delete":
	default:
		return fmt.Errorf("region %q: change %q is none of edit, rename and delete", p.Text, p.Change)
	}
	*r = LaneRegion(p)
	return nil
}

// MarshalJSON writes a region with no change and no hold as its text, any other as the object.
func (r LaneRegion) MarshalJSON() ([]byte, error) {
	if r.Change == "" && !r.Exclusive {
		return json.Marshal(r.Text)
	}
	type plain LaneRegion
	return json.Marshal(plain(r))
}

// laneRegions are the regions of bare texts.
func laneRegions(texts []string) []LaneRegion {
	if texts == nil {
		return nil
	}
	out := make([]LaneRegion, len(texts))
	for i, t := range texts {
		out[i] = LaneRegion{Text: t}
	}
	return out
}

// readings are the places a region names, each with the region's change and hold, as the relay judges them (dagsched.Overlaps):
//   - its normal form (RegionOf), the region cli/schedrelay.go declares for a DESIGN edit region read from its text, with the plan's
//     change and exclusive flag when the plan states it (declareRegions copies them onto the region of that form);
//   - its text as written, the reading Scope also gives it (inRegionText): a tree when it ends in "/", else the path it spells. A planned
//     tree or file whose path holds "#" (`gen#1/`, `pkg/a#b.go`) is that tree or file, where RegionOf reads a symbol of another file.
//
// The repository is left empty on both sides (a lane has one repository, and Overlaps only asks that both name the same one); a hotspot
// or a shared contract file still holds its whole file, which Overlaps reads from the path.
func (r LaneRegion) readings() []dagsched.Region {
	f := RegionOf(r.Text)
	form := dagsched.Region{Path: f.Path, Kind: f.Kind, Key: f.Key, Change: r.Change, Exclusive: r.Exclusive}
	text := dagsched.Region{Path: r.Text, Kind: "file", Change: r.Change, Exclusive: r.Exclusive}
	if strings.HasSuffix(r.Text, "/") {
		text.Path, text.Kind = strings.TrimSuffix(r.Text, "/"), "tree"
	}
	if text == form {
		return []dagsched.Region{form}
	}
	return []dagsched.Region{form, text}
}

// readingsOverlap is whether two readings may touch one place: by the relay's rule (dagsched.Overlaps), or because one of them is a
// pattern (a file or symbol whose path holds `*`, `?` or `[`) that matches the path of the other, as Scope matches a pattern on a whole
// path. The relay reads a pattern as the literal path it spells; the lane is more careful, never less.
func readingsOverlap(a, b dagsched.Region) bool {
	return dagsched.Overlaps(a, b) || patternCovers(a, b) || patternCovers(b, a)
}

func patternCovers(pat, r dagsched.Region) bool {
	if pat.Repository != r.Repository || pat.Kind == "tree" || r.Kind == "tree" || !strings.ContainsAny(pat.Path, "*?[") {
		return false
	}
	ok, _ := path.Match(pat.Path, r.Path)
	return ok
}

// RegionsOverlap is LaneRegionsOverlap of bare texts: every region an edit, none a hold.
func RegionsOverlap(a, b []string) bool {
	return LaneRegionsOverlap(laneRegions(a), laneRegions(b))
}

// LaneRegionsOverlap says whether two nodes with these region lists may edit the same place: an undeclared node (no regions) overlaps
// every other, and two declared nodes overlap when any reading of a region of one overlaps any reading of a region of the other
// (readings, readingsOverlap). So a pair the relay judges overlapping, with the plan's change and hold, overlaps here, and so does a
// pair Scope reads as a region covering a path: the lane never runs at once two nodes that either judgement would put on one place.
// The answer does not depend on the order of the two lists.
func LaneRegionsOverlap(a, b []LaneRegion) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	rb := make([][]dagsched.Region, len(b))
	for i, y := range b {
		rb[i] = y.readings()
	}
	for _, x := range a {
		for _, rx := range x.readings() {
			for _, ys := range rb {
				for _, ry := range ys {
					if readingsOverlap(rx, ry) {
						return true
					}
				}
			}
		}
	}
	return false
}

// LaneAdmissionRegions are the regions the lane admits a node by, as AdmissionRegions makes them from texts: the plan's declared regions
// as given (a planned change and hold kept) and then the edit regions whose normal form no region before them has, so an edit region
// that names a planned region in another spelling is that planned region, as declareRegions reads it. [] when either side is empty.
func LaneAdmissionRegions(plan, edit []LaneRegion) []LaneRegion {
	out := []LaneRegion{}
	if len(plan) == 0 || len(edit) == 0 {
		return out
	}
	out = append(out, plan...)
	seen := map[RegionForm]bool{}
	for _, r := range plan {
		seen[RegionOf(r.Text)] = true
	}
	for _, r := range edit {
		if form := RegionOf(r.Text); !seen[form] {
			seen[form] = true
			out = append(out, r)
		}
	}
	return out
}

// regionsOverlapOp is crw.regionsOverlap(a, b): LaneRegionsOverlap of two region lists, answered true or false. Each list is an array
// whose items are a region's text or the object {text, change, exclusive} a planned region carries (LaneRegion), mixed as the caller
// has them: `crw.regionsOverlap(["src/", {"text": "a.go#F", "change": "delete"}], ["a.go#G"])` is true. null or a missing list is
// undeclared, and an undeclared list overlaps every other.
type regionsOverlapOp struct{ l *Lane }

func (o *regionsOverlapOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var a, b []LaneRegion
	if err := args(req.Args, &a, &b); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.regionsOverlap(a, b): %w", err)
	}
	return engine.OpResult{Result: result(LaneRegionsOverlap(a, b))}, nil
}

func (o *regionsOverlapOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// admissionRegionsOp is crw.admissionRegions(plan, edit): LaneAdmissionRegions of the plan's declared regions and the DESIGN's edit
// regions, each list in the argument shape of crw.regionsOverlap (a text or the object {text, change, exclusive} per item). The answer
// is a list in that shape, [] when either side is empty or null, never null: a region with no change and no hold is its text, a
// planned delete, rename or hold stays the object, so the answer passed to crw.regionsOverlap keeps them.
type admissionRegionsOp struct{ l *Lane }

func (o *admissionRegionsOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var plan, edit []LaneRegion
	if err := args(req.Args, &plan, &edit); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.admissionRegions(plan, edit): %w", err)
	}
	return engine.OpResult{Result: result(LaneAdmissionRegions(plan, edit))}, nil
}

func (o *admissionRegionsOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}
