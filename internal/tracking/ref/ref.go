// Package ref reads the tracking reference "<owner>/<name>#N". It is a leaf: it imports only the
// standard library's text and error packages, so a package that only parses references (the relay's
// routing and fault ledger) can use it without taking the GitHub client in internal/tracking.
package ref

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ErrInvalid is the error of a Ref that is not "<owner>/<name>#N" with N above 0.
var ErrInvalid = errors.New("tracking: invalid reference")

// Ref names an issue or milestone as "<repo>#N": Repo is "owner/name" and Number is the issue or
// milestone number.
type Ref struct {
	Repo   string
	Number int
}

// String is the identifier "<repo>#N".
func (r Ref) String() string { return r.Repo + "#" + strconv.Itoa(r.Number) }

var refPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([1-9][0-9]*)$`)

// Parse reads "<owner>/<name>#N".
func Parse(s string) (Ref, error) {
	m := refPattern.FindStringSubmatch(s)
	if m == nil || !ValidRepo(m[1]) {
		return Ref{}, fmt.Errorf("%w: %q is not <owner>/<name>#N", ErrInvalid, s)
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return Ref{}, fmt.Errorf("%w: %q has a number out of range", ErrInvalid, s)
	}
	return Ref{Repo: m[1], Number: n}, nil
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// ValidRepo reports whether s is "owner/name" with neither part "." or "..".
func ValidRepo(s string) bool {
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
