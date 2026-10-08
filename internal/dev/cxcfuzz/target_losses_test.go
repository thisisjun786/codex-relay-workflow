//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A written document answer in the shape both comparators read: the rewritten bytes under "written".
func writtenAnswer(text string) any {
	return pyjson.Object{{Key: "unreadable", Value: false}, {Key: "state", Value: ""}, {Key: "written", Value: text}}
}

func requireDataLoss(t *testing.T, verdict Verdict, want string) {
	t.Helper()
	if verdict.Kind != Differ || !strings.HasPrefix(verdict.Detail, "data-loss") || !strings.Contains(verdict.Detail, want) {
		t.Fatalf("verdict %+v, want a data-loss differ naming %s", verdict, want)
	}
}

// A string leaf the oracle keeps and the Go rewrite replaces with null is a loss at any depth, not
// only a missing key (CRW-708 generation 5, c10 d1). The key is present, so only the leaf's type shows it.
func TestStateCompareSeesAStringReplacedByNull(t *testing.T) {
	oracle := `{"workPhases":[{"tasks":[{"title":"keep"}]}]}`
	goText := `{"workPhases":[{"tasks":[{"title":null}]}]}`
	requireDataLoss(t, stateCompare(writtenAnswer(goText), writtenAnswer(oracle)), "workPhases[0].tasks[0].title")
}

func TestGoalplanCompareSeesAStringReplacedByNull(t *testing.T) {
	oracle := `{"workPhases":[{"tasks":[{"title":"keep"}]}]}`
	goText := `{"workPhases":[{"tasks":[{"title":null}]}]}`
	requireDataLoss(t, goalplanCompare(writtenAnswer(goText), writtenAnswer(oracle)), "workPhases[0].tasks[0].title")
}

// A truncated string is a loss: the Go side kept fewer code units than the oracle wrote.
func TestStateCompareSeesATruncatedString(t *testing.T) {
	requireDataLoss(t, stateCompare(writtenAnswer(`{"slug":"abc"}`), writtenAnswer(`{"slug":"abcdef"}`)), "slug")
}

// A surrogate pair the 256-unit cut splits: the oracle keeps the emoji, the Go side holds one
// replacement character, so it kept fewer code units (CRW-708 generation 5, d6 boundary).
func TestStateCompareSeesACutSurrogatePair(t *testing.T) {
	oracle := `{"x":"aa\ud83d\ude00"}`
	goText := `{"x":"aa\ufffd"}`
	requireDataLoss(t, stateCompare(writtenAnswer(goText), writtenAnswer(oracle)), "x")
}

// The Go read diagnostic carries its wording (detail) and the path, as the oracle's does; the answer
// keeps them so a wording difference is compared and pinned, not hidden (CRW-708 generation 5, d3).
func TestGoalplanGoAnswerKeepsTheDiagnosticDetail(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".crw", "goalplans", goalplanSlug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "goalplan.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	answer, err := goalplanGo(nil, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	if detail, ok := field(answer, "detail"); !ok || detail == "" {
		t.Fatalf("a broken plan answered without its diagnostic detail: %v", answer)
	}
}
