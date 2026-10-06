//go:build dev

package ci

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// refactorBacklogSection is the fragment a section directory holds first: its heading, prose
	// and the entries written before the tree was split.
	refactorBacklogSection = "_section.md"
	// refactorBacklogDrift is the refusal --check prints when the committed file is not what the
	// fragments assemble to.
	refactorBacklogDrift = "edit the fragments under docs/port/refactor-backlog.d and run crw-dev ci refactor-backlog --write"
)

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
// when neither exists, which is a tree the check has nothing to say about. The file without its
// source is refused: nothing could then keep the two in step.
func refactorBacklogPaths(root string) (source, target string, ok bool, err error) {
	source = filepath.Join(root, refactorBacklogSource)
	target = filepath.Join(root, refactorBacklogTarget)
	if _, statErr := os.Stat(source); statErr != nil {
		if _, statErr := os.Stat(target); statErr == nil {
			return "", "", false, fmt.Errorf("%s exists but its source %s is missing", refactorBacklogTarget, refactorBacklogSource)
		}
		return "", "", false, nil
	}
	return source, target, true, nil
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
	if err != nil || committed != text {
		return errors.New(refactorBacklogDrift)
	}
	return nil
}

// assembleRefactorBacklog is the document the fragments under source build. Directories are read
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
	return strings.Join(blocks, "\n\n") + "\n", nil
}

// assembleRefactorBacklogSection is one directory block: its _section.md, then the other
// fragments by name. Each piece loses its trailing newlines and one blank line separates them, so
// a fragment that ends in a newline and one that does not assemble the same way.
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
		entries += text
	}
	if !found {
		return "", fmt.Errorf("%s: no %s", dir, refactorBacklogSection)
	}
	if entries = strings.TrimRight(entries, "\n"); entries == "" {
		return section, nil
	}
	return section + "\n\n" + entries, nil
}
