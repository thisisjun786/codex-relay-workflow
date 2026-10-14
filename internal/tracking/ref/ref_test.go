package ref

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestParseReadsOwnerNameAndNumber(t *testing.T) {
	for in, want := range map[string]Ref{
		"o/r#3":                    {"o/r", 3},
		"thisisjun786/crw-work#49": {"thisisjun786/crw-work", 49},
		"Org.x/repo_1.y-z#1234567": {"Org.x/repo_1.y-z", 1234567},
	} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", in, got, err, want)
		}
		if got.String() != in {
			t.Errorf("String() = %q; want %q", got.String(), in)
		}
	}
}

func TestParseRefusesWhatIsNotAnIdentifier(t *testing.T) {
	for _, in := range []string{
		"", "#3", "o/r", "o/r#", "o/r#0", "o/r#-1", "o/r#1x", "o#1", "o/r/x#1", " o/r#1", "o/r#1 ",
		"o/r#99999999999999999999", "/r#1", "o/#1", "./r#1", "o/..#1", "o/r#+1", "o/r#01x", "o /r#1",
	} {
		if got, err := Parse(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %v, %v; want ErrInvalid", in, got, err)
		}
	}
}

func TestValidRepo(t *testing.T) {
	for _, s := range []string{"o/r", "my.org/my_repo.go"} {
		if !ValidRepo(s) {
			t.Errorf("ValidRepo(%q) = false; want true", s)
		}
	}
	for _, s := range []string{"./r", "o/..", "o", "o/r/x", "/r", "o/", "o r/x", ""} {
		if ValidRepo(s) {
			t.Errorf("ValidRepo(%q) = true; want false", s)
		}
	}
}

// The leaf exists so that packages which only parse references (the relay's routing and fault
// ledger) need not import the GitHub client: its closure holds no network package and not the
// tracking package itself.
func TestLeafImportsOnlyThePlatform(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "net/http" || strings.HasPrefix(dep, "net/http/") {
			t.Errorf("dependency %q: the leaf must not reach a network client", dep)
		}
		if dep == "github.com/thisisjun786/codex-relay-workflow/internal/tracking" {
			t.Errorf("dependency %q: the leaf must not import the tracking package", dep)
		}
	}
}
