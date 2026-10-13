package tracking

import (
	"errors"
	"testing"
)

func TestParseRefReadsOwnerNameAndNumber(t *testing.T) {
	for in, want := range map[string]Ref{
		"o/r#3":                    {"o/r", 3},
		"thisisjun786/crw-work#49": {"thisisjun786/crw-work", 49},
		"Org.x/repo_1.y-z#1234567": {"Org.x/repo_1.y-z", 1234567},
	} {
		got, err := ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %v, %v; want %v", in, got, err, want)
		}
		if got.String() != in {
			t.Errorf("String() = %q; want %q", got.String(), in)
		}
	}
}

func TestParseRefRefusesWhatIsNotAnIdentifier(t *testing.T) {
	for _, in := range []string{
		"", "#3", "o/r", "o/r#", "o/r#0", "o/r#-1", "o/r#1x", "o#1", "o/r/x#1", " o/r#1", "o/r#1 ",
		"o/r#99999999999999999999", "/r#1", "o/#1", "./r#1", "o/..#1", "o/r#+1", "o/r#01x", "o /r#1",
	} {
		if got, err := ParseRef(in); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("ParseRef(%q) = %v, %v; want ErrInvalidRef", in, got, err)
		}
	}
}
