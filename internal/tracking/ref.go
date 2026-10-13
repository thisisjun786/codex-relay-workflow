package tracking

import (
	"fmt"
	"regexp"
	"strconv"
)

// Ref names an issue or milestone as "<repo>#N": Repo is "owner/name" and Number is the issue or
// milestone number.
type Ref struct {
	Repo   string
	Number int
}

// String is the identifier "<repo>#N".
func (r Ref) String() string { return r.Repo + "#" + strconv.Itoa(r.Number) }

var refPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([1-9][0-9]*)$`)

// ParseRef reads "<owner>/<name>#N".
func ParseRef(s string) (Ref, error) {
	m := refPattern.FindStringSubmatch(s)
	if m == nil || !validRepo(m[1]) {
		return Ref{}, fmt.Errorf("%w: %q is not <owner>/<name>#N", ErrInvalidRef, s)
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return Ref{}, fmt.Errorf("%w: %q has a number out of range", ErrInvalidRef, s)
	}
	return Ref{Repo: m[1], Number: n}, nil
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// validRepo reports whether s is "owner/name" with neither part "." or "..".
func validRepo(s string) bool {
	if !repoPattern.MatchString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return !dots(s[:i]) && !dots(s[i+1:])
		}
	}
	return false
}

func dots(s string) bool { return s == "." || s == ".." }
