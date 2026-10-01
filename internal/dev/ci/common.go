//go:build dev

package ci

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Helpers the CI checks share: the git runner, path resolution and the flag parser.

// runGit runs git in root and returns its standard output; a failure names the command, how it
// ended and what git wrote on its standard error.
func runGit(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if text := strings.TrimSpace(strings.ToValidUTF8(stderr.String(), "�")); text != "" {
		err = fmt.Errorf("%w: %s", err, text)
	}
	return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

// resolve is path made absolute with its symbolic links followed as far as the path exists, the
// missing remainder appended as written (os.path.realpath's non-strict reading). A missing tail is
// judged where the links before it lead, so a containment check cannot be passed by naming a
// file that does not exist yet below a link that points out of the tree.
func resolve(path string) string {
	if !filepath.IsAbs(path) {
		if wd, err := os.Getwd(); err == nil {
			path = wd + "/" + path
		}
	}
	resolved := "/"
	rest := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for hops := 0; len(rest) > 0; {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, part)
		info, err := os.Lstat(next)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			resolved = next
			continue
		}
		target, err := os.Readlink(next)
		if hops++; err != nil || hops > 40 {
			resolved = next
			continue
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		rest = append(strings.Split(target, "/"), rest...)
	}
	return resolved
}

// isRelativeTo reports whether path is root or inside it.
func isRelativeTo(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

// pathParts is a slash-separated relative path's components, without empty and "." ones.
func pathParts(text string) []string {
	var parts []string
	for _, part := range strings.Split(text, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// lines is text split into lines; "\r\n" ends a line as "\n" does.
func lines(text string) []string {
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

func sortedSet(items []string) []string {
	var out []string
	for _, item := range items {
		if item != "" {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// newFlags is a check's flag set, named as the command line names it.
func newFlags(check string) *flag.FlagSet {
	flags := flag.NewFlagSet("crw-dev ci "+check, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	return flags
}

// parseFlags parses args into flags. It returns -1 to go on, or the exit status: 0 after -h or
// --help printed the usage on stdout, 2 after a usage error printed on stderr.
func parseFlags(flags *flag.FlagSet, description string, args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		fmt.Fprintf(w, "usage: %s [flags]\n\n%s\n\nflags:\n", flags.Name(), description)
		flags.SetOutput(w)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
	}
	err := flags.Parse(args)
	if err == nil && flags.NArg() > 0 {
		err = fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		usage(stdout)
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "%s: %v\n", flags.Name(), err)
		usage(stderr)
		return 2
	}
	return -1
}

// given reports whether the command line set the named flag.
func given(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// nonNil is items, or an empty list for none, so a JSON encoding says [] rather than null.
func nonNil(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

// failf prints a check's refusal on w and returns exit status 1.
func failf(w io.Writer, format string, args ...any) int {
	fmt.Fprintf(w, format+"\n", args...)
	return 1
}
