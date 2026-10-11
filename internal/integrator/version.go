package integrator

import (
	"bytes"
	"errors"
	"regexp"
)

var versionLineRE = regexp.MustCompile(`^\s*"version"\s*:\s*"[^"]*",?\s*$`)

// ErrNotVersionOnly is the refusal of a conflicted file whose conflicts are anything but the
// version line.
var ErrNotVersionOnly = errors.New("the conflict is not only the version line")

// ResolveVersionConflict resolves a conflicted plugin manifest when it has at least one
// conflict block and every block holds exactly one line on each side and both are a "version"
// line. It keeps the current side's line; the integrator re-derives the line afterwards. Any
// other shape (no block, a longer side, another key, an unterminated block) is
// ErrNotVersionOnly and the file is left to the caller unchanged.
func ResolveVersionConflict(data []byte) ([]byte, error) {
	lines := bytes.Split(data, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	blocks := 0
	for i := 0; i < len(lines); {
		if !bytes.HasPrefix(lines[i], []byte("<<<<<<< ")) {
			out = append(out, lines[i])
			i++
			continue
		}
		j := i + 1
		var ours, theirs [][]byte
		for j < len(lines) && !bytes.HasPrefix(lines[j], []byte("|||||||")) && !bytes.HasPrefix(lines[j], []byte("=======")) {
			ours = append(ours, lines[j])
			j++
		}
		if j < len(lines) && bytes.HasPrefix(lines[j], []byte("|||||||")) {
			for j < len(lines) && !bytes.HasPrefix(lines[j], []byte("=======")) {
				j++
			}
		}
		j++
		for j < len(lines) && !bytes.HasPrefix(lines[j], []byte(">>>>>>> ")) {
			theirs = append(theirs, lines[j])
			j++
		}
		if j >= len(lines) || len(ours) != 1 || len(theirs) != 1 || !versionLineRE.Match(ours[0]) || !versionLineRE.Match(theirs[0]) {
			return nil, ErrNotVersionOnly
		}
		out = append(out, ours[0])
		blocks++
		i = j + 1
	}
	if blocks == 0 {
		return nil, ErrNotVersionOnly
	}
	return bytes.Join(out, []byte("\n")), nil
}
