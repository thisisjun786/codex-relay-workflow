package decisions

import (
	"encoding/json"
	"testing"
)

// CRW-1325: an answer that gives an option and prose keeps the option as the answer and stores the
// prose beside it as answer_note. The option still decides the apply; the note is read back, not applied.

// answerDocument is the record as its JSON object, the form the format's readers see.
func answerDocument(t *testing.T, record Record) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func TestCRW1325OptionAndTextStoreTheNoteBesideTheChoice(t *testing.T) {
	record := answerRecord(StateRaised, Authority{Kind: AuthorityUser})
	answered, err := ValidateAnswer(record, Answer{Option: "now", Text: "because it is idle", By: "task-a", Via: ViaDots})
	if err != nil {
		t.Fatal(err)
	}
	if document := answerDocument(t, answered); document["answer_text"] != "now" || document["answer_note"] != "because it is idle" {
		t.Fatalf("option and text: the stored answer is %v", document)
	}
	// An option-only answer stores no note, so it reads as the format did before the note existed.
	only, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDots})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := answerDocument(t, only)["answer_note"]; present {
		t.Fatal("an option-only answer stored a note")
	}
	// A text-only answer keeps the text as its answer and stores no note.
	text, err := ValidateAnswer(record, Answer{Text: "between runs", By: "task-a", Via: ViaFileImport})
	if err != nil {
		t.Fatal(err)
	}
	if document := answerDocument(t, text); document["answer_text"] != "between runs" || document["answer_note"] != nil {
		t.Fatalf("text only: the stored answer is %v", document)
	}
}
