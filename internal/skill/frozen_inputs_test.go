package skill

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pythonInputs extracts testdata/python-inputs.tar.gz into a directory of t's and answers it: the
// decision, title and host fixtures and hook-contract.md as they stood when the Python answers
// were recorded (decisions/, titles/, host/, hook-contract.md), which the goldens were first
// taken over.
func pythonInputs(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	archive, err := os.Open(filepath.Join("testdata", "python-inputs.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	unzipped, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	entries := tar.NewReader(unzipped)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(path, 0o700)
		case tar.TypeReg:
			var data []byte
			if data, err = io.ReadAll(entries); err == nil {
				err = os.WriteFile(path, data, 0o600)
			}
		default:
			err = fmt.Errorf("%s: unexpected entry type %c", header.Name, header.Typeflag)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// frozenReplayArgs points a replay at inputs, a pythonInputs directory, for every path it would
// otherwise take from the live plugin: --fixtures, --contract and --host-fixtures for hook-probe,
// --fixtures for parent-title. Other commands, and arguments ending option parsing, are answered
// unchanged.
func frozenReplayArgs(inputs, family string, args []string) []string {
	if len(args) == 0 || args[0] != "replay" || slices.Contains(args, "--") {
		return args
	}
	var paths [][2]string
	switch family {
	case "hook-probe":
		paths = [][2]string{{"--fixtures", "decisions"}, {"--contract", "hook-contract.md"}, {"--host-fixtures", "host"}}
	case "parent-title":
		paths = [][2]string{{"--fixtures", "titles"}}
	default:
		return args
	}
	out := slices.Clone(args)
	for _, path := range paths {
		given := slices.ContainsFunc(args, func(arg string) bool { return arg == path[0] || strings.HasPrefix(arg, path[0]+"=") })
		if !given {
			out = append(out, path[0], filepath.Join(inputs, path[1]))
		}
	}
	return out
}
