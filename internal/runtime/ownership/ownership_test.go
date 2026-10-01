package ownership_test

import (
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// ownership.classify over the signals a Go install has (the checkout signals absent) gives the
// same class with the same reasons, in the fixed precedence; only own's summary sentence is
// Go's, because a Go install has no revision or cleanliness to agree on, so the golden (which
// began as Python's classification) holds no reasons for own.
func TestClassifyIsPythons(t *testing.T) {
	yes, no := true, false
	for _, recorded := range []bool{true, false} {
		for _, digest := range []*bool{nil, &yes, &no} {
			for _, point := range []bool{true, false} {
				for _, conflict := range []string{"", "registration", "pointer"} {
					for _, unreadable := range []bool{false, true} {
						s := ownership.Signals{EntryPointRecorded: recorded, HasPoint: point, DigestMatches: digest}
						switch conflict {
						case "registration":
							s.RegistrationConflict = "registered elsewhere"
						case "pointer":
							s.PointerConflict = "the pointer names another runtime"
						}
						if unreadable {
							s.Unreadable = []string{"the host record"}
						}
						class, reasons := ownership.Classify(s)
						answer := map[string]any{"class": class}
						if class != ownership.Own {
							answer["reasons"] = reasons
						}
						digestText := "null"
						if digest != nil {
							digestText = fmt.Sprint(*digest)
						}
						golden.CheckJSON(t, fmt.Sprintf("recorded=%v digest=%s point=%v conflict=%q unreadable=%v", recorded, digestText, point, conflict, unreadable), answer)
					}
				}
			}
		}
	}
	for _, c := range ownership.Classes {
		if ownership.Reusable(c) != (c == ownership.Own) {
			t.Errorf("%s reusable", c)
		}
	}
}

// An entry point a host cannot launch is not this installation's, whatever its bytes and points
// say: foreign, after conflict and before fork, with the launch problem as its reason. That is
// the class Python reaches, because shutil.which does not find a file the user cannot execute.
func TestAnUnlaunchableEntryPointIsForeign(t *testing.T) {
	yes, no := true, false
	problem := "the entry point /rt/bin/crw is not executable by this user (-rw-r--r--)"
	for _, c := range []struct {
		signals ownership.Signals
		class   string
	}{
		{ownership.Signals{EntryPointRecorded: true, DigestMatches: &yes, HasPoint: true, Unlaunchable: []string{problem}}, ownership.Foreign},
		{ownership.Signals{EntryPointRecorded: true, DigestMatches: &no, HasPoint: true, Unlaunchable: []string{problem}}, ownership.Foreign},
		{ownership.Signals{EntryPointRecorded: true, DigestMatches: &yes, HasPoint: true, PointerConflict: "elsewhere", Unlaunchable: []string{problem}}, ownership.Conflict},
		{ownership.Signals{EntryPointRecorded: true, DigestMatches: &yes, HasPoint: true, Unlaunchable: []string{problem}, Unreadable: []string{"the host record"}}, ownership.Unreadable},
	} {
		class, reasons := ownership.Classify(c.signals)
		if class != c.class || (class == ownership.Foreign && (len(reasons) != 2 || reasons[0] != problem)) {
			t.Errorf("%+v: %s %v", c.signals, class, reasons)
		}
	}
}
