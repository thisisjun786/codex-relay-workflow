package ownership_test

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// ownership.classify over the signals a Go install has (the checkout signals absent) gives the
// same class with the same reasons, in the fixed precedence; only own's summary sentence is
// Go's, because a Go install has no revision or cleanliness to agree on.
func TestClassifyIsPythons(t *testing.T) {
	rows := golden.List(golden.Section(t, "ownership"))
	if len(rows) != 72 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, raw := range rows {
		row := golden.Obj(raw)
		s := ownership.Signals{EntryPointRecorded: record.Get(row, "recorded") == true, HasPoint: record.Get(row, "point") == true}
		if digest, ok := record.Get(row, "digest").(bool); ok {
			s.DigestMatches = &digest
		}
		switch record.Get(row, "conflict") {
		case "registration":
			s.RegistrationConflict = "registered elsewhere"
		case "pointer":
			s.PointerConflict = "the pointer names another runtime"
		}
		if record.Get(row, "unreadable") == true {
			s.Unreadable = []string{"the host record"}
		}
		class, reasons := ownership.Classify(s)
		if class != record.Get(row, "class") {
			t.Errorf("%s: go %s", golden.Canon(row), class)
			continue
		}
		if class != ownership.Own && golden.Canon(reasons) != golden.Canon(record.Get(row, "reasons")) {
			t.Errorf("%s: go reasons %s", golden.Canon(row), golden.Canon(reasons))
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
