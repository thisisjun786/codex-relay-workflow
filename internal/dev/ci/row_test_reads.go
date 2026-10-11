//go:build dev

package ci

// rowTestRead says which paths a check test reads: the test (a top-level `func <Test>(t *testing.T)`
// of a *_test.go file in Package, a repository-relative directory), and the paths it scans. A done
// fix, split or delete row of the inventory map (docs/skills/inventory-map.json) whose check names
// the test is covered by it when one of the row's paths is a registered path or lies below one.
// TestSkillInventoryMapComplete parses the package with go/parser, so a test that does not exist
// cannot be registered, and requires every registered path to appear as a Go string literal in the
// file that declares the test, so a path the test never reads cannot be claimed.
//
// Rows narrows a test that proves only some rows: when it is not empty the test is evidence for those
// row keys alone (each a Go string literal of the declaring file), and for a row without paths of its
// own as well as for one with a covered path. TestSkillDescriptionLength reads the descriptions table
// and the paths of its two rows, not the paths of any other row.
//
// An issue that turns a row to done adds its paths and asserts to the row and the registration of
// its own test here, in the same change. TestSkillAnchorsResolve and TestSkillInventoryMapComplete
// read the whole tree and prove no row, so they are not registered.
type rowTestRead struct {
	Test    string
	Package string
	Reads   []string
	Rows    []string
}

// rowTestReads is the registry.
var rowTestReads = []rowTestRead{
	{"TestTriggerOverlapTable", "internal/dev/ci", []string{"plugins/crw/skills/"}, nil},
	{"TestNoStaleCxcReferences", "internal/dev/ci", []string{"plugins/crw/skills/"}, nil},
	{"TestNoChangeHistoryNotes", "internal/dev/ci", []string{"plugins/crw/skills/"}, nil},
	{"TestSkillDescriptionLength", "internal/dev/ci", []string{"plugins/crw/skills/"}, []string{"triggerAmbiguity[13]", "sizeHotspots[4]"}},
}
