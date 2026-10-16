package decisions

import (
	"encoding/json"
	"testing"
)

func TestCRW1325OldRecordsReadWithoutTheNote(t *testing.T) {
	raised := answerRecord(StateRaised, Authority{Kind: AuthorityUser})
	answered, err := ValidateAnswer(raised, Answer{Option: "now", By: "task-a", Via: ViaDots})
	if err != nil {
		t.Fatal(err)
	}
	for _, before := range []Record{raised, answered} {
		raw, err := json.Marshal(before)
		if err != nil {
			t.Fatal(err)
		}
		var old Record
		if err := json.Unmarshal(raw, &old); err != nil {
			t.Fatal(err)
		}
		if old.AnswerNote != "" {
			t.Fatal("legacy record acquired a note")
		}
		if err := Validate(old); err != nil {
			t.Fatal(err)
		}
		if _, err := Merge(old, old); err != nil {
			t.Fatal(err)
		}
	}
}
