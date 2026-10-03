package dagsched

import (
	"reflect"
	"testing"
)

// CRW-431: a delete, a rename or a hotspot is exclusive at its own place for the node that declared it, so a conflict in that place is exclusive for the pair even when the other node did not declare it (drift
// says so), while an ordinary edit the other node did not declare stays local.
func TestMergeOrderGradeOfAStructuralPlaceTheOtherNodeDidNotDeclare(t *testing.T) {
	deleted := Region{Repository: "owner/repo", Path: "c.txt", Kind: "file", Change: "delete"}
	for name, c := range map[string]struct {
		d, e      []Region
		wantGrade string
		wantDrift []string
	}{
		"the other node declared another path":                  {[]Region{deleted}, []Region{local("f.txt")}, GradeExclusive, []string{"E"}},
		"the node that deletes is the later one":                {[]Region{local("f.txt")}, []Region{deleted}, GradeExclusive, []string{"D"}},
		"an ordinary edit the other node did not declare":       {[]Region{local("c.txt")}, []Region{local("f.txt")}, GradeLocal, []string{"E"}},
		"both nodes declared the place, one of them deletes it": {[]Region{deleted}, []Region{local("c.txt")}, GradeExclusive, nil},
	} {
		t.Run(name, func(t *testing.T) {
			w := newSweepWorld(t)
			w.declareRegions("D", c.d...)
			w.declareRegions("E", c.e...)
			w.declareRegions("F", local("g.txt"))
			w.declareRegions("I", local("i.txt"))
			w.running("D", "E")
			w.measure(w.heads("D", "E"))
			row := w.k.read("g").node("E").MergeOrder.After[0]
			drift := row.DriftNodes
			if len(drift) == 0 {
				drift = nil
			}
			if row.Grade != c.wantGrade || !reflect.DeepEqual(drift, c.wantDrift) {
				t.Fatalf("row = %+v, want a %s conflict with drift %v", row, c.wantGrade, c.wantDrift)
			}
		})
	}
}
