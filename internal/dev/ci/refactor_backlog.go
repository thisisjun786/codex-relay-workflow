//go:build dev

package ci

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The generated refactor backlog (CRW-759). Every pull request that records an entry appends to the
// same few places of one file, so parallel branches collide there; the entries live in fragments
// instead and docs/port/refactor-backlog.md is assembled from them. The generated file stays
// committed for readers and links, and a base refresh settles it by regenerating it.

const (
	// refactorBacklogSource is the fragment tree, refactorBacklogTarget the file it builds.
	refactorBacklogSource = "docs/port/refactor-backlog.d"
	refactorBacklogTarget = "docs/port/refactor-backlog.md"
	// refactorBacklogMarker is a directory of this repository (the sources of the command that runs the check). A tree that has it
	// requires the backlog, whatever else it lost; a tree without it is not this repository and has none (every validate fixture).
	refactorBacklogMarker = "cmd/crw-dev"
	// refactorBacklogSection is the fragment a section directory holds first: its heading, prose
	// and the entries written before the tree was split.
	refactorBacklogSection = "_section.md"
	// refactorBacklogDrift is the refusal --check prints when the committed file is not what the
	// fragments assemble to.
	refactorBacklogDrift = "edit the fragments under docs/port/refactor-backlog.d and run crw-dev ci refactor-backlog --write"
)

// refactorBacklogEntry matches the tag an entry bullet opens with. It marks a line as an entry
// (refactorBacklogDropped); it is not the entry identity, because tags repeat.
var refactorBacklogEntry = regexp.MustCompile(`^- \[[^\]]+\]`)

// RefactorBacklog is the crw-dev ci refactor-backlog check: --write regenerates the committed file
// from the fragments, --check refuses when the committed file differs from them.
func RefactorBacklog(args []string, stdout, stderr io.Writer) int {
	flags := newFlags("refactor-backlog")
	write := flags.Bool("write", false, "regenerate "+refactorBacklogTarget+" from its fragments")
	check := flags.Bool("check", false, "refuse when the committed file differs from its fragments")
	if code := parseFlags(flags, "Assemble "+refactorBacklogTarget+" from "+refactorBacklogSource+
		" (--write), or refuse when the committed file differs from the fragments (--check).", args, stdout, stderr); code >= 0 {
		return code
	}
	if *write == *check {
		fmt.Fprintln(stderr, "crw-dev ci refactor-backlog: exactly one of --write or --check is required")
		return 2
	}
	root, err := repositoryRoot()
	if err != nil {
		return failf(stderr, "refactor-backlog: %s", err)
	}
	source, target, ok, err := refactorBacklogPaths(root)
	if err != nil {
		return failf(stderr, "refactor-backlog: %s", err)
	}
	if !ok {
		return 0 // neither the fragments nor the file are here: nothing to write or compare
	}
	if *write {
		text, err := assembleRefactorBacklog(source)
		if err != nil {
			return failf(stderr, "refactor-backlog: %s", err)
		}
		dropped, err := refactorBacklogDropped(target, text)
		if err != nil {
			return failf(stderr, "refactor-backlog: %s", err)
		}
		if len(dropped) > 0 {
			return failf(stderr, "%s was edited since it was generated and holds entries no fragment produces: %s; move each into the fragment of its section under %s (or delete the line if it is stale) and run --write again",
				refactorBacklogTarget, strings.Join(quoted(dropped), "; "), refactorBacklogSource)
		}
		if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
			return failf(stderr, "refactor-backlog: %s", err)
		}
		fmt.Fprintf(stdout, "wrote %s from %s\n", refactorBacklogTarget, refactorBacklogSource)
		return 0
	}
	if err := refactorBacklogError(root); err != nil {
		return failf(stderr, "%s", err)
	}
	return 0
}

// refactorBacklogPaths resolves the fragment tree and the generated file below root. ok is false
// only for a tree that is not this repository (no refactorBacklogMarker) and has neither the
// fragments nor the file. This repository without both is refused, however much of docs/port is
// left, and so is a stat error other than "does not exist": a permission error is not the answer
// "absent". The file without its source is refused too: nothing could then keep the two in step.
func refactorBacklogPaths(root string) (source, target string, ok bool, err error) {
	source = filepath.Join(root, refactorBacklogSource)
	target = filepath.Join(root, refactorBacklogTarget)
	_, sourceErr := os.Stat(source)
	if sourceErr == nil {
		return source, target, true, nil
	}
	if !errors.Is(sourceErr, fs.ErrNotExist) {
		return "", "", false, sourceErr
	}
	if _, targetErr := os.Stat(target); targetErr == nil {
		return "", "", false, fmt.Errorf("%s exists but its source %s is missing", refactorBacklogTarget, refactorBacklogSource)
	} else if !errors.Is(targetErr, fs.ErrNotExist) {
		return "", "", false, targetErr
	}
	if _, markerErr := os.Stat(filepath.Join(root, refactorBacklogMarker)); markerErr == nil {
		return "", "", false, fmt.Errorf("neither %s nor %s exists in a tree that holds %s", refactorBacklogSource, refactorBacklogTarget, refactorBacklogMarker)
	} else if !errors.Is(markerErr, fs.ErrNotExist) {
		return "", "", false, markerErr
	}
	return "", "", false, nil
}

// refactorBacklogDropped is the entries the committed file carries and the assembly does not, as
// the lines themselves (with their tag). Writing the assembly would delete those entries, which is
// how an entry a branch added by hand disappears when the generated file is regenerated. The write
// refuses instead, so the entry is moved into a fragment of its section rather than lost.
//
// What the fragments no longer produce is lost only if someone wrote it in the generated file, and
// the file says whether anyone did: it ends in a seal over everything above it
// (refactorBacklogSeal). A file that still matches its seal is exactly what --write made, so every
// line it holds that the assembly lacks is the old text of a fragment edited or removed since, and
// replacing it loses nothing; no pairing by tag or position is needed or attempted. A file that
// does not match (edited by hand, or written before the seal existed) is compared by whole line,
// because tags repeat: a line the assembly does not produce is dropped, whichever section it sits in. A generated file that is
// absent has nothing to lose; one that cannot be read is an error.
func refactorBacklogDropped(target, assembly string) ([]string, error) {
	committed, err := readText(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if refactorBacklogIntact(committed) {
		return nil, nil
	}
	produced := map[string]int{}
	for _, line := range lines(assembly) {
		if refactorBacklogEntry.MatchString(line) {
			produced[refactorBacklogNormal(line)]++
		}
	}
	var dropped []string
	for _, line := range lines(committed) {
		if !refactorBacklogEntry.MatchString(line) {
			continue
		}
		if key := refactorBacklogNormal(line); produced[key] > 0 {
			produced[key]--
			continue
		}
		dropped = append(dropped, refactorBacklogShown(line))
	}
	return dropped, nil
}

// refactorBacklogSeal is body followed by the line that seals it: a comment (invisible where the
// file is rendered) holding the SHA-256 of body. body is what the fragments assemble to.
func refactorBacklogSeal(body string) string {
	return fmt.Sprintf("%s<!-- refactor-backlog: generated; sha256 of the lines above: %x -->\n", body, sha256.Sum256([]byte(body)))
}

// refactorBacklogIntact reports whether text is a sealed file no one has changed since it was
// written: its last line is the seal of everything above it.
func refactorBacklogIntact(text string) bool {
	trimmed := strings.TrimSuffix(text, "\n")
	cut := strings.LastIndex(trimmed, "\n") + 1
	return refactorBacklogSeal(text[:cut]) == text
}

// refactorBacklogNormal is an entry line as it is compared: without trailing white space or a CR.
func refactorBacklogNormal(line string) string { return strings.TrimRight(line, " \t\r") }

// refactorBacklogShown is an entry line as the refusal prints it: the whole line up to a length that
// still identifies it.
func refactorBacklogShown(line string) string {
	line = refactorBacklogNormal(line)
	if runes := []rune(line); len(runes) > 120 {
		return string(runes[:120]) + "..."
	}
	return line
}

// refactorBacklogError is the drift refusal for root, or nil when the tree holds no fragments or
// when the committed file is what they assemble to. crw-dev ci validate runs it, so a fragment
// edited without regenerating the file fails the job CI gates on.
func refactorBacklogError(root string) error {
	source, target, ok, err := refactorBacklogPaths(root)
	if err != nil || !ok {
		return err
	}
	text, err := assembleRefactorBacklog(source)
	if err != nil {
		return err
	}
	committed, err := readText(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err != nil || committed != text {
		return errors.New(refactorBacklogDrift)
	}
	return nil
}

// assembleRefactorBacklog is the document the fragments under source build, sealed. Directories are read
// in name order, so the directory names carry the section order; inside a directory _section.md
// comes first and the other fragments follow by name.
func assembleRefactorBacklog(source string) (string, error) {
	entries, err := os.ReadDir(source)
	if err != nil {
		return "", err
	}
	var blocks []string
	for _, entry := range entries {
		if !entry.IsDir() {
			return "", fmt.Errorf("%s: not a section directory", filepath.Join(source, entry.Name()))
		}
		block, err := assembleRefactorBacklogSection(filepath.Join(source, entry.Name()))
		if err != nil {
			return "", err
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return "", fmt.Errorf("%s holds no sections", source)
	}
	return refactorBacklogSeal(strings.Join(blocks, "\n\n") + "\n"), nil
}

// assembleRefactorBacklogSection is one directory block: its _section.md, then the other
// fragments by name. Each piece loses its trailing newlines, so a fragment that ends in a newline
// and one that does not assemble the same way; one blank line separates the section from the
// entries, and the entries stay consecutive lines.
func assembleRefactorBacklogSection(dir string) (string, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	section, entries, found := "", "", false
	for _, file := range files {
		if file.IsDir() {
			return "", fmt.Errorf("%s: not a fragment", filepath.Join(dir, file.Name()))
		}
		path := filepath.Join(dir, file.Name())
		text, err := readText(path)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("%s: empty fragment", path)
		}
		if file.Name() == refactorBacklogSection {
			section, found = strings.TrimRight(text, "\n"), true
			continue
		}
		entries += strings.TrimRight(text, "\n") + "\n"
	}
	if !found {
		return "", fmt.Errorf("%s: no %s", dir, refactorBacklogSection)
	}
	if entries = strings.TrimRight(entries, "\n"); entries == "" {
		return section, nil
	}
	return section + "\n\n" + entries, nil
}

// quoted is each text between quotation marks, so an entry line reads as one item of a list.
func quoted(texts []string) []string {
	out := make([]string, len(texts))
	for i, text := range texts {
		out[i] = fmt.Sprintf("%q", text)
	}
	return out
}
