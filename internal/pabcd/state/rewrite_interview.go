package state

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
)

// RewriteKeepsInterview says whether writing kept back over the session file raw would keep every interview record the file
// stores. kept is the tracker ReadStateStrict rebuilt from raw: ReconstructInterview caps contradictions and assumptions at
// interview.MaxTrackerArray (drop-oldest), caps each dimension's known/unknown list and each ontology entry's fields and
// relationships the same way, drops the strings of a list that are not text, and drops an ontology entity with no name and a
// relationship with no target, so a stored list longer than the rebuilt one is a record the write-back would lose. The two
// values cannot be compared instead: the reader's rebuild always normalises a dimension (an absent known/unknown list becomes
// empty, an unrecognised level becomes low), so a value comparison would refuse every write and silently stop the recovery. A
// tracker the file does not hold, or holds as null, holds no entry to lose.
//
// raw is read exactly as ReadStateStrict reads it (decodeObject: numbers stay json.Number, a repeated key keeps its last
// value, and anything after the top-level object is refused), so the document this judges is the document the reader rebuilt
// kept from. This is the judgement the post-compact hook has always made (its sessionHookInterviewKeepsStored), moved here so
// the cli writers share it.
func RewriteKeepsInterview(raw []byte, kept *interview.Tracker) bool {
	m := decodeObject(raw)
	if m == nil {
		return false
	}
	object, _ := m["interview"].(map[string]any)
	contradictions, _ := object["contradictions"].([]any)
	assumptions, _ := object["assumptions"].([]any)
	ontology, _ := object["ontologySchema"].([]any)
	// kept is nil only for a document that holds no tracker at all, so every stored list is then a record to lose.
	var keepContradictions, keepAssumptions, keepOntology int
	if kept != nil {
		keepContradictions, keepAssumptions, keepOntology = len(kept.Contradictions), len(kept.Assumptions), len(kept.OntologySchema)
	}
	if len(contradictions) > keepContradictions || len(assumptions) > keepAssumptions || len(ontology) > keepOntology {
		return false
	}
	dimensions, _ := object["dimensions"].(map[string]any)
	for _, d := range interview.DimensionOrder() {
		score, _ := dimensions[string(d)].(map[string]any)
		known, _ := score["known"].([]any)
		unknown, _ := score["unknown"].([]any)
		keepKnown, keepUnknown := 0, 0
		if kept != nil {
			keptScore := kept.Dimensions.Score(d)
			keepKnown, keepUnknown = len(keptScore.Known), len(keptScore.Unknown)
		}
		if len(known) > keepKnown || len(unknown) > keepUnknown {
			return false
		}
	}
	for i, entity := range ontology { // the entity count was checked above, so the rebuilt entry exists
		record, _ := entity.(map[string]any)
		fields, _ := record["fields"].([]any)
		relationships, _ := record["relationships"].([]any)
		keepFields, keepRelationships := 0, 0
		if kept != nil {
			keepFields, keepRelationships = len(kept.OntologySchema[i].Fields), len(kept.OntologySchema[i].Relationships)
		}
		if len(fields) > keepFields || len(relationships) > keepRelationships {
			return false
		}
	}
	return true
}
