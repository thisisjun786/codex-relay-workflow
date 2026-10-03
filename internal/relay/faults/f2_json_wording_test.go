package faults

import (
	"strings"
	"testing"
)

// A fault command's value or scope that is not JSON is refused with the reading's own error, as
// the observation and readings arguments already are, and not with a hand-built CPython
// JSONDecodeError line (line, column and character position). The refusal is returned before any
// write.
func TestUnreadableF2JSONIsRefusedWithGoReadingError(t *testing.T) {
	for _, raw := range []string{"not-json", "{", "[1,", ""} {
		_, err := f2JSON(raw, "scope")
		if err == nil || !strings.HasPrefix(err.Error(), "fault_observation_malformed: the scope is not readable JSON: ") {
			t.Errorf("f2JSON(%q) = %v", raw, err)
			continue
		}
		if strings.Contains(err.Error(), "Expecting value") || strings.Contains(err.Error(), "(char ") {
			t.Errorf("f2JSON(%q) still words the refusal as CPython does: %v", raw, err)
		}
	}
}
