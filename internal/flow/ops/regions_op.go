package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// object {"text": ..., "change": ..., "exclusive": ...}. null, an empty or blank text, an object with no text or with another field,
// and a change the relay does not know are refused, so a malformed list is an error, never a declared list that names no place. An
// edit with no hold is written back as its text.
type LaneRegion struct {
	Text      string `json:"text"`
	Change    string `json:"change,omitempty"`
	Exclusive bool   `json:"exclusive,omitempty"`
}

// UnmarshalJSON reads the text form and the object form.
func (r *LaneRegion) UnmarshalJSON(raw []byte) error {
	if string(bytes.TrimSpace(raw)) == "null" {
		return errors.New("a region is a text or an object {text, change, exclusive}, not null")
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return errors.New("a region's text is empty")
		}
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
	if strings.TrimSpace(p.Text) == "" {
		return errors.New("a region object names no text")
	}
	switch p.Change {
	case "", "edit", "rename", "delete":
	default:
		return fmt.Errorf("region %q: change %q is none of edit, rename and delete", p.Text, p.Change)
	}
	*r = LaneRegion(p)
	return nil
}

// MarshalJSON writes a region that is an edit (no change, or "edit") with no hold as its text, any other as the object.
func (r LaneRegion) MarshalJSON() ([]byte, error) {
	if (r.Change == "" || r.Change == "edit") && !r.Exclusive {
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
// pattern (a path that holds `*`, `?` or `[`, which Scope matches on a whole path) and the two are not proven disjoint (globsMayMeet).
// The relay reads a pattern as the literal path it spells; the lane is more careful, never less.
func readingsOverlap(a, b dagsched.Region) bool {
	return dagsched.Overlaps(a, b) || globsMayMeet(a, b)
}

// globsMayMeet is whether two readings, one of them at least a pattern, may name one path: false only when they are proven
// disjoint. A file or symbol is the paths its path matches (a symbol is its file here, whatever its key); a tree is the paths under
// it and, as the relay holds it, its own path. A character class or an escape on either side is never proven disjoint (a class can
// match "/", and cleaning the path can move one); otherwise the proof compares the literal parts, since `*` and `?` never match "/":
// two readings are disjoint when the literal text before their first wildcard, or after their last for two files, cannot be the same
// on both, or when their path segments cannot pair up: a different number of segments for two files, a file with fewer segments
// than a tree, or two segments of one position that cannot match one name.
func globsMayMeet(a, b dagsched.Region) bool {
	if a.Repository != b.Repository || (!isGlob(a.Path) && !isGlob(b.Path)) {
		return false
	}
	if strings.ContainsAny(a.Path+b.Path, `[\`) {
		return true
	}
	ap, bp := path.Clean(a.Path), path.Clean(b.Path)
	if !literalEndsMeet(ap, bp, a.Kind != "tree" && b.Kind != "tree") {
		return false
	}
	as, bs := strings.Split(ap, "/"), strings.Split(bp, "/")
	switch {
	case a.Kind == "tree" && b.Kind == "tree":
	case a.Kind == "tree":
		if len(bs) < len(as) {
			return false
		}
	case b.Kind == "tree":
		if len(as) < len(bs) {
			return false
		}
	default:
		if len(as) != len(bs) {
			return false
		}
	}
	for i := 0; i < min(len(as), len(bs)); i++ {
		if !segmentsMeet(as[i], bs[i]) {
			return false
		}
	}
	return true
}

func isGlob(p string) bool { return strings.ContainsAny(p, "*?[") }

// literalEndsMeet is whether two paths' literal heads (the text before the first wildcard) can be the same, and, when both are files
// (a tree's paths go on past it), their literal tails (the text after the last wildcard) too.
func literalEndsMeet(a, b string, tails bool) bool {
	ha, hb := a, b
	if i := strings.IndexAny(a, "*?"); i >= 0 {
		ha = a[:i]
	}
	if i := strings.IndexAny(b, "*?"); i >= 0 {
		hb = b[:i]
	}
	if !strings.HasPrefix(ha, hb) && !strings.HasPrefix(hb, ha) {
		return false
	}
	if !tails {
		return true
	}
	ta, tb := a, b
	if i := strings.LastIndexAny(a, "*?"); i >= 0 {
		ta = a[i+1:]
	}
	if i := strings.LastIndexAny(b, "*?"); i >= 0 {
		tb = b[i+1:]
	}
	return strings.HasSuffix(ta, tb) || strings.HasSuffix(tb, ta)
}

// segmentsMeet is whether two path segments with no class and no escape can match one name: two literals when they are equal, a
// pattern and a literal when the pattern matches it, two patterns unless their literal ends differ.
func segmentsMeet(x, y string) bool {
	switch gx, gy := isGlob(x), isGlob(y); {
	case !gx && !gy:
		return x == y
	case !gy:
		ok, _ := path.Match(x, y)
		return ok
	case !gx:
		ok, _ := path.Match(y, x)
		return ok
	}
	return literalEndsMeet(x, y, true)
}

// RegionsOverlap is LaneRegionsOverlap of bare texts: every region an edit, none a hold.
func RegionsOverlap(a, b []string) bool {
	return LaneRegionsOverlap(laneRegions(a), laneRegions(b))
}

// LaneRegionsOverlap says whether two nodes with these region lists may edit the same place: two declared nodes overlap when any
// reading of a region of one overlaps any reading of a region of the other (readings, readingsOverlap). A side with no regions is
// undeclared: the overlap is unknown, not proven, so the answer is false — the release that admits the node warns of the unknown
// instead (CRW-1576). So a pair the relay judges overlapping, with the plan's change and hold, overlaps here, and so does a
// pair Scope reads as a region covering a path: the lane never runs at once two nodes that either judgement would put on one place.
// The answer does not depend on the order of the two lists.
func LaneRegionsOverlap(a, b []LaneRegion) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
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
// that names a planned region in another spelling is that planned region, as declareRegions reads it. [] when both sides are empty.
func LaneAdmissionRegions(plan, edit []LaneRegion) []LaneRegion {
	out := []LaneRegion{}
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
// undeclared: the overlap is unknown, not proven, so the answer is false (CRW-1576).
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
// is a list in that shape, [] when both sides are empty or null, never null: a region with no change and no hold is its text, a
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
