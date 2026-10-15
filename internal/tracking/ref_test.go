package tracking

import (
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/ref"
)

// The reference type and its error live in internal/tracking/ref; this package re-exports them so
// every consumer keeps one spelling. The parser's own tables are in the leaf's tests.
func TestTrackingReExportsTheLeaf(t *testing.T) {
	var fromLeaf ref.Ref = Ref{Repo: "o/r", Number: 3}
	var back Ref = fromLeaf
	if back != (Ref{Repo: "o/r", Number: 3}) {
		t.Errorf("round trip through the leaf type = %v", back)
	}
	if ErrInvalidRef != ref.ErrInvalid {
		t.Error("ErrInvalidRef is not the leaf's ErrInvalid")
	}

	got, err := ParseRef("thisisjun786/crw-work#49")
	if err != nil || got != (Ref{Repo: "thisisjun786/crw-work", Number: 49}) || got.String() != "thisisjun786/crw-work#49" {
		t.Errorf("ParseRef = %v, %v", got, err)
	}

	_, err = ParseRef("o/r")
	if !errors.Is(err, ErrInvalidRef) || !errors.Is(err, ref.ErrInvalid) {
		t.Fatalf("ParseRef(\"o/r\") error = %v; want both ErrInvalidRef and ref.ErrInvalid", err)
	}
	if !strings.HasPrefix(err.Error(), "tracking: invalid reference") {
		t.Errorf("error text = %q", err.Error())
	}
}
