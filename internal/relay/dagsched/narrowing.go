package dagsched

import (
	"path"
	"strings"
)

// Running narrowing (CRW-411). A node that holds its edit regions (its child runs) keeps them until its head lands, so a declaration that could only free another node's place early is
// fine and one that claims more than the node held is not: it may declare again only to narrow. A new declaration narrows the held one when every region of it lies inside a region the node
// holds, in the same repository spelling, and is held at least as strictly as every held region that covers it. The judgement reads declarations only; whether the node really stays inside
// them is what the conflict sweeps' drift marks say.

// wholeRepository is whether a region holds the whole of its repository: today a rename, a delete, a hotspot or the caller's word (Region.Exclusive, Overlaps). It is the only place this file
// depends on what the flag means, so a change to what Classify marks changes it here and nowhere else.
func wholeRepository(r Region) bool { return r.Exclusive }

// holdCovers is whether the place of next lies inside the place held covers: a region that holds the whole repository covers every region of it; a tree covers what lies under it, a file itself
// and the symbols in it, a symbol that symbol. A region that holds the whole repository is covered only by one that does. Different repository spellings are different repositories.
func holdCovers(held, next Region) bool {
	if held.Repository != next.Repository {
		return false
	}
	if wholeRepository(held) {
		return true
	}
	if wholeRepository(next) {
		return false
	}
	hp, np := path.Clean(held.Path), path.Clean(next.Path)
	switch held.Kind {
	case "tree":
		return within(np, hp)
	case "file":
		return np == hp && next.Kind != "tree"
	case "symbol":
		return np == hp && next.Kind == "symbol" && next.Key == held.Key
	}
	return false
}

// holdWeight is how strictly a region holds its place against a region that overlaps it, by the grade it is judged at (foldedGrade): mechanical 1, local 2, and independent or exclusive 3, since
// PairGrade judges every overlap with either of those exclusive. The rule of a mechanical region is its claim too, and a different rule is a different claim (PairGrade reads two rules as local).
func holdWeight(r Region) (weight int, rule string) {
	grade, rule := foldedGrade(r)
	switch grade {
	case GradeMechanical:
		return 1, rule
	case GradeLocal:
		return 2, ""
	}
	return 3, ""
}

// regionName is how a refusal names a region.
func regionName(r Region) string {
	name := r.Repository + " " + r.Path + " (" + r.Kind
	if r.Key != "" {
		name += " " + r.Key
	}
	return name + ")"
}

// widening is the first region of next that is not a narrowing of held, as a sentence, or "" when next narrows held. Both lists are as DeclareRegions stores them (sorted, grades folded).
// Two things are asked. Every new region lies inside a held one and is held at least as strictly as each held region that covers it (R1). And every held region that lies inside a new region
// is still held at least as strictly by the new declaration, by that region or another one that covers it (R2): regions nest or are disjoint, so a broad new region at a lower grade would
// otherwise lower the small strict region inside it, and the place would be held less strictly than before.
func widening(held, next []Region) string {
	for _, n := range next {
		covered := false
		nw, nrule := holdWeight(n)
		for _, h := range held {
			if !holdCovers(h, n) {
				continue
			}
			covered = true
			hw, hrule := holdWeight(h)
			switch {
			case nw < hw:
				return "region " + regionName(n) + " is declared " + EffectiveGrade(n) + " where the node holds " + h.Path + " as " + EffectiveGrade(h) + ": a narrowing never holds a place less strictly"
			case nw == 1 && hw == 1 && nrule != hrule:
				return "region " + regionName(n) + " names the rule " + strings.TrimSpace(nrule) + " where the node holds " + h.Path + " under " + strings.TrimSpace(hrule) + ": another rule is another claim, not a narrowing"
			}
		}
		if !covered {
			return "region " + regionName(n) + " is not inside any region the node holds"
		}
	}
	for _, h := range held {
		hw, hrule := holdWeight(h)
		var enclosing *Region
		kept := false
		for i := range next {
			if !holdCovers(next[i], h) {
				continue
			}
			if enclosing == nil {
				enclosing = &next[i]
			}
			nw, nrule := holdWeight(next[i])
			if nw > hw || (nw == hw && (hw != 1 || nrule == hrule)) {
				kept = true
				break
			}
		}
		if enclosing != nil && !kept {
			return "region " + regionName(*enclosing) + " is declared " + EffectiveGrade(*enclosing) + " over " + regionName(h) + ", which the node holds as " + EffectiveGrade(h) + ": a narrowing holds every place it keeps at least as strictly"
		}
	}
	return ""
}
