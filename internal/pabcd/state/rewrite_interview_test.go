package state

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
)

func rewriteInterviewFile(tracker string) string { return `{"phase":"B","interview":` + tracker + `}` }

func rewriteInterviewContradictions(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = `{"contradictionId":"k` + strconv.Itoa(i) + `","severity":"high","summary":"s"}`
	}
	return "[" + strings.Join(items, ",") + "]"
}

func rewriteInterviewAssumptions(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = `{"id":"a` + strconv.Itoa(i) + `","text":"t","recorded":true}`
	}
	return "[" + strings.Join(items, ",") + "]"
}

func rewriteInterviewOntology(entries string) string {
	return `{"contradictions":[],"assumptions":[],"ontologySchema":` + entries + `}`
}

// rewriteInterviewVerdict reads file as ReadStateStrict does and asks whether writing the rebuilt tracker back would keep the
// stored records. A file the reader calls unreadable is not a case here: RewriteKeepsInterview judges the document itself.
func rewriteInterviewVerdict(t *testing.T, file string) bool {
	t.Helper()
	s, _ := restore("s1", []byte(file), time.Now())
	return RewriteKeepsInterview([]byte(file), s.Interview)
}

// TestRewriteKeepsInterviewJudgesEveryStoredRecord is the table for the moved hook decision: a stored array longer than the
// rebuilt one, or an ontology entry whose relationship list is longer, is a record the rewrite would lose.
func TestRewriteKeepsInterviewJudgesEveryStoredRecord(t *testing.T) {
	for _, c := range []struct {
		name, file string
		keeps      bool
	}{
		{"no interview key", `{"phase":"B"}`, true},
		{"a null tracker", rewriteInterviewFile("null"), true},
		{"an empty tracker", rewriteInterviewFile(`{"contradictions":[],"assumptions":[]}`), true},
		{"a tracker of the wrong shape", rewriteInterviewFile(`"x"`), true},
		{"a tracker that is an array", rewriteInterviewFile("[]"), true},
		{"the most contradictions the reader keeps", rewriteInterviewFile(`{"contradictions":` + rewriteInterviewContradictions(interview.MaxTrackerArray) + `,"assumptions":[]}`), true},
		{"one contradiction past the cap", rewriteInterviewFile(`{"contradictions":` + rewriteInterviewContradictions(interview.MaxTrackerArray+1) + `,"assumptions":[]}`), false},
		{"the most assumptions the reader keeps", rewriteInterviewFile(`{"contradictions":[],"assumptions":` + rewriteInterviewAssumptions(interview.MaxTrackerArray) + `}`), true},
		{"one assumption past the cap", rewriteInterviewFile(`{"contradictions":[],"assumptions":` + rewriteInterviewAssumptions(interview.MaxTrackerArray+1) + `}`), false},
		{"a named ontology entry", rewriteInterviewFile(rewriteInterviewOntology(`[{"name":"n","relationships":[{"to":"y","kind":"k"}]}]`)), true},
		{"an ontology entry with no name", rewriteInterviewFile(rewriteInterviewOntology(`[{"fields":["f"],"relationships":[{"to":"y","kind":"k"}]}]`)), false},
		{"an ontology entry with no name and no relationship", rewriteInterviewFile(rewriteInterviewOntology(`[{"fields":["f"]}]`)), false},
		{"an ontology entry whose relationship has no target", rewriteInterviewFile(rewriteInterviewOntology(`[{"name":"n","relationships":[{"kind":"k"}]}]`)), false},
		{"an ontology entry past the relationship cap", rewriteInterviewFile(rewriteInterviewOntology(`[{"name":"n","relationships":` + rewriteInterviewRelationships(interview.MaxTrackerArray+1) + `}]`)), false},
		{"a tracker the reader cannot read at all", `{"phase":"B","interview":}`, false},
		{"a document that is not an object", `[]`, false},
		{"a document that is not JSON", `not json`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteInterviewVerdict(t, c.file); got != c.keeps {
				t.Errorf("RewriteKeepsInterview = %v, want %v for %.200s", got, c.keeps, c.file)
			}
		})
	}
}

func rewriteInterviewRelationships(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = `{"to":"y` + strconv.Itoa(i) + `","kind":"k"}`
	}
	return "[" + strings.Join(items, ",") + "]"
}

// TestRewriteKeepsInterviewComparesAgainstWhatTheCallerHolds pins the two arguments: the stored document and the tracker the
// caller would publish. A nil tracker keeps only an empty stored tracker, and a tracker the reader rebuilt from a longer one
// never lets the longer document through.
func TestRewriteKeepsInterviewComparesAgainstWhatTheCallerHolds(t *testing.T) {
	for _, c := range []struct {
		name string
		file string
		kept *interview.Tracker
		want bool
	}{
		{"nil tracker, no stored tracker", `{"phase":"B"}`, nil, true},
		{"nil tracker, a stored contradiction", rewriteInterviewFile(`{"contradictions":[{"contradictionId":"k"}]}`), nil, false},
		{"nil tracker, a stored ontology entry", rewriteInterviewFile(rewriteInterviewOntology(`[{"name":"n"}]`)), nil, false},
		{"the rebuilt tracker of the same document", rewriteInterviewFile(`{"contradictions":[{"contradictionId":"k","severity":"high","summary":"s"}]}`),
			&interview.Tracker{Contradictions: []interview.Contradiction{{ContradictionID: "k", Severity: interview.SeverityHigh, Summary: "s"}}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RewriteKeepsInterview([]byte(c.file), c.kept); got != c.want {
				t.Errorf("RewriteKeepsInterview = %v, want %v for %.200s", got, c.want, c.file)
			}
		})
	}
}
