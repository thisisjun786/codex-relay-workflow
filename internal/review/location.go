package review

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// HeadReader reads the head checkout for location validation; the pipeline supplies one. Lines returns the number of lines of the
// file at path (slash separated, relative to the repository root): the number of newlines plus one when the last byte is not a
// newline, so an empty file has none. It returns an error wrapping fs.ErrNotExist when head has no such file, which is the case
// for a file the diff deleted; any other error makes Apply return it.
type HeadReader interface {
	Lines(path string) (int, error)
}

// locate checks where a finding points; file is its cleaned path.
func (r Rules) locate(file string, f Finding) (DropReason, string, error) {
	switch {
	case file == "." || strings.HasPrefix(file, "/") || strings.Contains(file, `\`) || !filepath.IsLocal(file) || f.Line < 1 || f.EndLine != 0 && f.EndLine < f.Line:
		return ReasonInvalidLocation, fmt.Sprintf("%s:%d-%d", f.File, f.Line, f.EndLine), nil
	case !r.Changed[file]:
		return ReasonNotInDiff, file, nil
	}
	n, err := r.Head.Lines(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ReasonMissingAtHead, file, nil
	case err != nil:
		return "", "", fmt.Errorf("review: lines of %s at head: %w", file, err)
	case max(f.Line, f.EndLine) > n:
		return ReasonLineBeyondHead, fmt.Sprintf("%s has %d lines, the finding points at line %d", file, n, max(f.Line, f.EndLine)), nil
	}
	return "", "", nil
}
