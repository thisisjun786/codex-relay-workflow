package crwconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Schema is the configuration document's schema name: the value a file's "schema" key
// must carry when the file has one. A file without the key is read as this schema.
const Schema = "crw-config/1"

// usageExit is the status of a command line this command cannot use.
const usageExit = 2

// File is the loaded configuration: where the file is, where that path came from, the
// roots it resolved, and the document verbatim for a section this package does not
// know. A session with no file carries a nil document and the defaults.
type File struct {
	path   string
	source Source
	roots  map[string]Root
	doc    map[string]json.RawMessage
	raw    []byte
}

// Path is the configuration file's path and where that path came from: the file named
// with --config is "config", one named by CRW_CONFIG or reached through
// XDG_CONFIG_HOME is "env", and the built-in location is "default".
func (f *File) Path() (string, Source) { return f.path, f.source }

// Roots is every resolved root by name. The map is a copy, so a caller cannot change
// what the file resolved.
func (f *File) Roots() map[string]Root {
	out := make(map[string]Root, len(f.roots))
	for name, root := range f.roots {
		out[name] = root
	}
	return out
}

// Section decodes the top-level key name of the configuration document into v. A key
// the document does not carry leaves v untouched and reports no error, so a
// subcommand reads its own section without this file knowing about it.
func (f *File) Section(name string, v any) error {
	raw, ok := f.doc[name]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// Load resolves the configuration. The file is located by flagPath, then by the
// CRW_CONFIG environment variable, then at the configuration home below
// ${XDG_CONFIG_HOME:-$HOME/.config}. A file that is not there leaves the defaults in
// place; a file that is there must be a JSON object and, when it carries a schema,
// must carry this package's. The roots themselves come from Resolve, with the
// document's paths section as its overrides.
func Load(getenv func(string) string, flagPath string) (*File, error) {
	path, source, err := locate(getenv, flagPath)
	if err != nil {
		return nil, err
	}
	file := &File{path: path, source: source}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data = nil
	case err != nil:
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if data != nil {
		doc := map[string]json.RawMessage{}
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("%s: the configuration is not a JSON object: %v", path, err)
		}
		if doc == nil {
			return nil, fmt.Errorf("%s: the configuration is not a JSON object", path)
		}
		if raw, ok := doc["schema"]; ok {
			var schema string
			if err := json.Unmarshal(raw, &schema); err != nil || schema != Schema {
				return nil, fmt.Errorf("%s: schema %s is not %q", path, strings.TrimSpace(string(raw)), Schema)
			}
		}
		file.doc, file.raw = doc, data
	}
	overrides := map[string]string{}
	if raw, ok := file.doc["paths"]; ok {
		if err := json.Unmarshal(raw, &overrides); err != nil {
			return nil, fmt.Errorf("%s: paths is not an object of paths: %v", path, err)
		}
		// A present paths key that is JSON null decodes without an error and leaves the
		// map nil, which would silently read as no overrides at all: refuse it as the
		// section that is not an object of paths, while an absent key keeps the defaults.
		if overrides == nil {
			return nil, fmt.Errorf("%s: paths is not an object of paths", path)
		}
	}
	roots, err := Resolve(getenv, overrides)
	if err != nil {
		return nil, err
	}
	file.roots = roots
	return file, nil
}

// locate is the file location chain: the flag, then CRW_CONFIG, then the default
// location below the configuration home.
func locate(getenv func(string) string, flagPath string) (string, Source, error) {
	if flagPath != "" {
		return flagPath, SourceConfig, absolute(flagPath, "--config")
	}
	if value := getenv("CRW_CONFIG"); value != "" {
		return value, SourceEnv, absolute(value, "CRW_CONFIG")
	}
	home, fromEnv := baseDir(getenv, "XDG_CONFIG_HOME", ".config")
	// The default location is joined the way every root is, raw text and one separator: a
	// base that mixes a symbolic link and ".." keeps the meaning the filesystem gives that
	// spelling rather than the different directory filepath.Clean would name.
	path := rootJoin(home, "crw", "config.json")
	what := "the configuration home"
	if fromEnv {
		what = "XDG_CONFIG_HOME"
	}
	return path, sourceOf(fromEnv), absolute(path, what)
}

// absolute refuses a path that is not absolute, naming the input that supplied it.
func absolute(path, input string) error {
	if filepath.IsAbs(path) {
		return nil
	}
	return fmt.Errorf("%s %q: not an absolute path", input, path)
}

// Run is crw config. A subcommand writes its report to stdout and exits 0; no
// subcommand, an unknown one, an unknown flag or a missing --config value writes the
// usage to stderr and exits 2, as does a configuration the command cannot use.
func Run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 {
		usage(stderr)
		fmt.Fprintln(stderr, "crw config: error: the following arguments are required: command")
		return usageExit
	}
	verb := args[0]
	switch verb {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	case "paths", "show":
	default:
		usage(stderr)
		fmt.Fprintf(stderr, "crw config: error: invalid command %q (choose from 'paths', 'show')\n", verb)
		return usageExit
	}
	flagPath, rest, err := parseConfigFlag(args[1:])
	if err != nil {
		usage(stderr)
		fmt.Fprintf(stderr, "crw config: error: %v\n", err)
		return usageExit
	}
	for _, arg := range rest {
		if arg == "-h" || arg == "--help" {
			usage(stdout)
			return 0
		}
	}
	if len(rest) > 0 {
		usage(stderr)
		fmt.Fprintf(stderr, "crw config: error: unexpected argument %q\n", rest[0])
		return usageExit
	}
	file, err := Load(getenv, flagPath)
	if err != nil {
		fmt.Fprintf(stderr, "crw config: error: %v\n", err)
		return usageExit
	}
	if verb == "paths" {
		return writeJSON(stdout, stderr, pathsReport(file))
	}
	return writeJSON(stdout, stderr, showReportOf(file))
}

// parseConfigFlag reads the optional --config from a subcommand's arguments and
// returns it with whatever arguments are left over.
func parseConfigFlag(args []string) (string, []string, error) {
	var path string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config":
			if i+1 >= len(args) {
				return "", nil, errors.New("argument --config: expected one argument")
			}
			if args[i+1] == "" {
				return "", nil, errors.New("argument --config: expected a non-empty path")
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			path = strings.TrimPrefix(args[i], "--config=")
			if path == "" {
				return "", nil, errors.New("argument --config: expected a non-empty path")
			}
		default:
			rest = append(rest, args[i])
		}
	}
	return path, rest, nil
}

// pathRow is one root in the paths report.
type pathRow struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source Source `json:"source"`
}

// pathsReport is the crw config paths report: one row per root, in the order the
// decided layout lists them.
func pathsReport(file *File) []pathRow {
	roots := file.Roots()
	out := make([]pathRow, 0, len(RootNames()))
	for _, name := range RootNames() {
		out = append(out, pathRow{Name: name, Path: roots[name].Path, Source: roots[name].Source})
	}
	return out
}

// showReport is the crw config show report: the file's path, where that path came
// from, and the document verbatim. Config is null when no file was read.
type showReport struct {
	Path   string          `json:"path"`
	Source Source          `json:"source"`
	Config json.RawMessage `json:"config"`
}

// showReportOf builds the show report from a loaded file.
func showReportOf(file *File) showReport {
	report := showReport{Path: file.path, Source: file.source}
	if file.raw != nil {
		report.Config = file.raw
	}
	return report
}

// writeJSON writes one report to stdout and returns the status.
func writeJSON(stdout, stderr io.Writer, v any) int {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "crw config: error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", data)
	return 0
}

// usage writes the usage line and one line per subcommand.
func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: crw config [-h] {paths,show} ...")
	fmt.Fprintln(w, "  paths\tprint every named root, its path and where the path came from")
	fmt.Fprintln(w, "  show\tprint the configuration file's path, where it came from and its document")
}
