package state

import (
	"bytes"
	"encoding/json"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
)

// RewriteKeepsInterview says whether writing kept back over the session file raw would keep every interview record the file
// stores. kept is the tracker ReadStateStrict rebuilt from raw: ReconstructInterview caps contradictions and assumptions at
// interview.MaxTrackerArray (drop-oldest) and drops an ontology entity with no name and a relationship with no target, so a
// stored array longer than the rebuilt one is a record the write-back would lose. The two values cannot be compared instead:
// the reader's rebuild always normalises a dimension (an absent known/unknown array becomes empty, an unrecognised level
// becomes low), so a value comparison would refuse every write and silently stop the recovery. A tracker the file does not
// hold, or holds as null, holds no entry to lose.
//
// raw is read as ReadStateStrict reads it: a document that is not one JSON object is refused, and the key is decoded with
// numbers left as json.Number. This is the judgement the post-compact hook has always made (its sessionHookInterviewKeepsStored),
// moved here so the cli writers share it.
func RewriteKeepsInterview(raw []byte, kept *interview.Tracker) bool {
	stored, ok := rewriteJSONField(raw, "interview")
	if !ok {
		return false
	}
	object, _ := stored.(map[string]any)
	contradictions, _ := object["contradictions"].([]any)
	assumptions, _ := object["assumptions"].([]any)
	ontology, _ := object["ontologySchema"].([]any)
	if kept == nil {
		return len(contradictions) == 0 && len(assumptions) == 0 && len(ontology) == 0
	}
	if len(contradictions) > len(kept.Contradictions) || len(assumptions) > len(kept.Assumptions) || len(ontology) > len(kept.OntologySchema) {
		return false
	}
	for i, entity := range ontology {
		record, _ := entity.(map[string]any)
		relationships, _ := record["relationships"].([]any)
		if len(relationships) > len(kept.OntologySchema[i].Relationships) {
			return false
		}
	}
	return true
}

// rewriteJSONField is the value of one top-level key of a state document, decoded as the reader decodes it: numbers stay
// json.Number. A document that is not one JSON object is refused; a key the document does not hold is the nil value.
func rewriteJSONField(doc []byte, key string) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, false
	}
	for dec.More() {
		name, err := dec.Token()
		if err != nil {
			return nil, false
		}
		if field, ok := name.(string); ok && field == key {
			var value any
			if dec.Decode(&value) != nil {
				return nil, false
			}
			return value, true
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return nil, false
		}
	}
	return nil, true
}
