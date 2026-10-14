package planinput

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// The relay dag reads a plan's project key, node ids and edge ids as identifiers: a letter or a digit, then letters, digits, '.', '_',
// ':' and '-', at most dag.MaxIDLength characters (internal/relay/dag/decode.go, which this package does not change). An
// OWNER/NAME#N ref has a '/' and a '#', so the compile input carries the ref as OWNER:NAME:N: the ref stays the identity, and this
// encoding is its reversible spelling. A ':' cannot occur in a GitHub owner or repository name, so the form splits in exactly one way.
var dagIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// EncodeKey is the dag-safe key of a ref: OWNER/NAME#N written as OWNER:NAME:N. A ref whose key the dag cannot hold (a first character
// that is not a letter or a digit, or more than dag.MaxIDLength characters) is an input error, never a shortened key.
func EncodeKey(r tracking.Ref) (string, error) {
	if strings.Count(r.Repo, "/") != 1 || strings.Contains(r.Repo, ":") || r.Number < 1 {
		return "", fmt.Errorf("%s is not an OWNER/NAME#N ref and has no plan key", r)
	}
	key := strings.Replace(r.Repo, "/", ":", 1) + ":" + strconv.Itoa(r.Number)
	switch {
	case !dagIdentifier.MatchString(key):
		return "", fmt.Errorf("%s cannot be a plan key: the relay dag reads %q as an identifier that starts with a letter or a digit and holds letters, digits, '.', '_', ':' and '-': use a repository whose owner starts with a letter or a digit", r, key)
	case len(key) > dag.MaxIDLength:
		return "", fmt.Errorf("%s cannot be a plan key: its dag-safe form %q is %d characters, over the %d the relay dag allows for an identifier: use a repository with a shorter owner/name", r, key, len(key), dag.MaxIDLength)
	}
	return key, nil
}

// DecodeKey is the inverse of EncodeKey: OWNER:NAME:N read back as OWNER/NAME#N. A string EncodeKey does not make is an error.
func DecodeKey(s string) (tracking.Ref, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return tracking.Ref{}, fmt.Errorf("%q is not an OWNER:NAME:N plan key", s)
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || strconv.Itoa(n) != parts[2] {
		return tracking.Ref{}, fmt.Errorf("%q is not an OWNER:NAME:N plan key: %q is not an issue number", s, parts[2])
	}
	r, err := tracking.ParseRef(parts[0] + "/" + parts[1] + "#" + parts[2])
	if err != nil {
		return tracking.Ref{}, fmt.Errorf("%q is not an OWNER:NAME:N plan key: %w", s, err)
	}
	return r, nil
}

// edgeIDLength is the length of the id compile gives the order edge from a prerequisite to an issue ("e:FROM:TO"), which the dag holds
// to the same limit.
func edgeIDLength(from, to string) int { return len("e:") + len(from) + len(":") + len(to) }
