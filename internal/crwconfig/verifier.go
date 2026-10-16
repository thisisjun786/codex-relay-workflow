package crwconfig

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CrwDevTree is the verifier.crw_dev value that runs the verified tree's own crw-dev (`go run -tags dev ./cmd/crw-dev`).
const CrwDevTree = "tree"

// VerifierTools is the verifier section of the configuration (CRW-1319): the tools a verification of a head or of an integration
// batch's merged tree runs. GoRoot is the Go toolchain root (its bin/go is the go the verifier and every step of `crw-dev ci
// local` run); CrwDev is how crw-dev runs: "tree" for the verified tree's own, an absolute path for an installed binary, empty
// for "tree" when a Go root is set. Config is the file the section was read from, for a message; Ignored is why a section the
// verifier cannot use was ignored (LoadVerifierTools), so the tools are the PATH fallback. Neither is part of the section.
type VerifierTools struct {
	GoRoot  string `json:"go_root,omitempty"`
	CrwDev  string `json:"crw_dev,omitempty"`
	Config  string `json:"-"`
	Ignored string `json:"-"`
}

// CheckVerifierSection is the warning of a verifier section the verifier cannot use: the reading of the file ignores it, so it
// stops no other reader, and the verifier takes its tools from PATH (fail-open).
const CheckVerifierSection = "verifier_section"

// SectionVerifier is the Root a finding about the verifier section names.
const SectionVerifier = "verifier"

// verifierWarning is the warning a file's reading records for a verifier section the verifier cannot use, nil for a usable one.
func (f *File) verifierWarning() *Finding {
	if _, err := f.VerifierTools(); err != nil {
		return &Finding{Level: LevelWarning, Root: SectionVerifier, Check: CheckVerifierSection,
			Message: err.Error() + "; the section is ignored and the verifier takes crw-dev and go from PATH (fail-open); record it with crw config verifier --go-root <dir>"}
	}
	return nil
}

// Warnings are what the reading of the file found and did not refuse (a verifier section the verifier cannot use). The slice is
// a copy.
func (f *File) Warnings() []Finding {
	return append([]Finding(nil), f.warnings...)
}

// VerifierTools is the file's verifier section. A file without it names no tool; a section whose go_root is not an absolute path,
// or whose crw_dev is neither "tree" nor an absolute path, is refused.
func (f *File) VerifierTools() (VerifierTools, error) {
	tools := VerifierTools{}
	if err := f.Section("verifier", &tools); err != nil {
		return VerifierTools{}, fmt.Errorf("%s: verifier is not an object with go_root and crw_dev paths: %v", f.path, err)
	}
	if err := tools.validate(); err != nil {
		return VerifierTools{}, fmt.Errorf("%s: %v", f.path, err)
	}
	tools.Config = f.path
	return tools, nil
}

func (t VerifierTools) validate() error {
	if t.GoRoot != "" && !filepath.IsAbs(t.GoRoot) {
		return fmt.Errorf("verifier.go_root %q: not an absolute path", t.GoRoot)
	}
	if t.CrwDev != "" && t.CrwDev != CrwDevTree && !filepath.IsAbs(t.CrwDev) {
		return fmt.Errorf("verifier.crw_dev %q: neither %q nor an absolute path", t.CrwDev, CrwDevTree)
	}
	return nil
}

// LoadVerifierTools reads the configuration the way every command does and returns its verifier section. A host whose default
// configuration location cannot be named gets the zero value, as LoadToolRoots does. A section the verifier cannot use is no
// error: the answer names no tool (the PATH fallback) and Ignored says why, so the verifier's note and the probe report it.
func LoadVerifierTools(getenv func(string) string, flagPath string) (VerifierTools, error) {
	file, err := loadOptional(getenv, flagPath)
	if err != nil || file == nil {
		return VerifierTools{}, err
	}
	tools, err := file.VerifierTools()
	if err != nil {
		return VerifierTools{Config: file.path, Ignored: err.Error()}, nil
	}
	return tools, nil
}

// Mode is how crw-dev runs: "tree" (the verified tree's own, by go run), "binary" (the configured crw-dev) or "path" (crw-dev on
// PATH, because nothing is configured).
func (t VerifierTools) Mode() string {
	switch {
	case t.CrwDev == CrwDevTree || (t.CrwDev == "" && t.GoRoot != ""):
		return "tree"
	case t.CrwDev != "":
		return "binary"
	}
	return "path"
}

// GoBinary is the go the verifier runs: the configured root's bin/go, else the go on pathEnv, else "".
func (t VerifierTools) GoBinary(pathEnv string) string {
	if t.GoRoot != "" {
		return filepath.Join(t.GoRoot, "bin", "go")
	}
	return lookPathIn("go", pathEnv)
}

// lookPathIn is the executable named name in a directory of pathEnv, or "".
func lookPathIn(name, pathEnv string) string {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// GoEnv are the environment entries that make the configured Go root the go of a command whose inherited PATH is pathEnv: the
// root's bin first on PATH, GOROOT the root, and GOTOOLCHAIN local, so no other toolchain is selected or fetched. They are the
// entries the verifier's command gets (Command), and the ones every agent call of a flow run is given (CRW-1488). Nil when no Go
// root is set.
func (t VerifierTools) GoEnv(pathEnv string) []string {
	if t.GoRoot == "" {
		return nil
	}
	path := filepath.Join(t.GoRoot, "bin")
	if pathEnv != "" {
		path += string(os.PathListSeparator) + pathEnv
	}
	return []string{"PATH=" + path, "GOROOT=" + t.GoRoot, "GOTOOLCHAIN=local"}
}

// VerifierCommand is how a verifier starts crw-dev: the program and the arguments before `ci local ...`, the environment entries
// set over the inherited environment (nil: inherited as it is), a note when a tool comes from PATH because nothing configured it,
// and a hint a verifier adds to the reason a start that fails gives.
type VerifierCommand struct {
	Argv []string
	Env  []string
	Note string
	Hint string
}

// Command is the command a verifier runs crw-dev with in tree, for a caller whose PATH is pathEnv. A configured Go root is first
// on the command's PATH (so every step of `crw-dev ci local` runs that go too), is its GOROOT, and GOTOOLCHAIN is local, so no
// other toolchain is selected or fetched. In tree mode the tree must carry cmd/crw-dev.
func (t VerifierTools) Command(tree, pathEnv string) (VerifierCommand, error) {
	where := "the configuration file"
	if t.Config != "" {
		where = t.Config
	}
	var cmd VerifierCommand
	if t.GoRoot != "" {
		cmd.Env = t.GoEnv(pathEnv)
		cmd.Hint = fmt.Sprintf("verifier.go_root in %s is %s, whose bin/go is the go the verifier runs", where, t.GoRoot)
	}
	switch t.Mode() {
	case "tree":
		if info, err := os.Stat(filepath.Join(tree, "cmd", "crw-dev")); err != nil || !info.IsDir() {
			return VerifierCommand{}, fmt.Errorf("the tree %s has no cmd/crw-dev to run; set verifier.crw_dev in %s to an installed crw-dev", tree, where)
		}
		goBin := "go"
		if t.GoRoot != "" {
			goBin = filepath.Join(t.GoRoot, "bin", "go")
		} else {
			cmd.Note = fmt.Sprintf("verifier.go_root is not set in %s: go comes from PATH", where)
			cmd.Hint = fmt.Sprintf("go is not on PATH and %s names no verifier.go_root; set it to the Go toolchain root (crw config verifier --go-root <dir>)", where)
		}
		cmd.Argv = []string{goBin, "run", "-tags", "dev", "./cmd/crw-dev"}
	case "binary":
		cmd.Argv = []string{t.CrwDev}
		if t.GoRoot == "" {
			cmd.Note = fmt.Sprintf("verifier.go_root is not set in %s: the steps of crw-dev ci local take go from PATH", where)
		}
		cmd.Hint = fmt.Sprintf("verifier.crw_dev in %s is %s", where, t.CrwDev)
	default:
		cmd.Argv = []string{"crw-dev"}
		cmd.Note = fmt.Sprintf("%s names no verifier.go_root or verifier.crw_dev: crw-dev and go come from PATH (fail-open)", where)
		cmd.Hint = fmt.Sprintf("crw-dev and go come from PATH because %s names no verifier.go_root or verifier.crw_dev; set verifier.go_root to the Go toolchain root (crw config verifier --go-root <dir>)", where)
		if t.Ignored != "" {
			cmd.Note = fmt.Sprintf("the verifier section is ignored (%s): crw-dev and go come from PATH (fail-open)", t.Ignored)
			cmd.Hint = fmt.Sprintf("crw-dev and go come from PATH because the verifier section is ignored (%s); record a Go toolchain root with crw config verifier --go-root <dir>", t.Ignored)
		}
	}
	return cmd, nil
}

// verifierReport is the crw config verifier report.
type verifierReport struct {
	Config string `json:"config"`
	GoRoot string `json:"go_root"`
	CrwDev string `json:"crw_dev"`
	Mode   string `json:"mode"`
}

// runVerifier is crw config verifier [--go-root <dir>] [--crw-dev tree|<path>]: it prints the verifier section, and with a flag
// records the value in the configuration file first (the installation step, CRW-1319), every other key of the file kept. A Go
// root whose bin/go is not an executable file and a crw-dev that is neither "tree" nor an absolute path are refused before
// anything is written.
func runVerifier(getenv func(string) string, flagPath string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crw config verifier", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	goRoot := flags.String("go-root", "", "the Go toolchain root")
	crwDev := flags.String("crw-dev", "", `how crw-dev runs: "tree" or an absolute path`)
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 {
		if err == nil {
			err = fmt.Errorf("unexpected argument %q", flags.Arg(0))
		}
		fmt.Fprintf(stderr, "crw config verifier: error: %v\n", err)
		return usageExit
	}
	file, err := Load(getenv, flagPath)
	if err != nil {
		fmt.Fprintf(stderr, "crw config: error: %v\n", err)
		return usageExit
	}
	// a section the verifier cannot use is shown as the PATH fallback it is, with the warning; a record repairs the keys it sets
	tools, terr := file.VerifierTools()
	if terr != nil {
		tools = VerifierTools{}
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if len(set) > 0 {
		if set["go-root"] {
			tools.GoRoot = *goRoot
		}
		if set["crw-dev"] {
			tools.CrwDev = *crwDev
		}
		if err := tools.validate(); err != nil {
			fmt.Fprintf(stderr, "crw config verifier: error: %v\n", err)
			return usageExit
		}
		if tools.GoRoot != "" {
			if info, err := os.Stat(filepath.Join(tools.GoRoot, "bin", "go")); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
				fmt.Fprintf(stderr, "crw config verifier: error: verifier.go_root %q: bin/go is not an executable file there\n", tools.GoRoot)
				return usageExit
			}
		}
		// the section is rewritten key by key: the keys this command does not set stay as they are, and an emptied key is removed;
		// a section that is no object is replaced by one
		section := map[string]json.RawMessage{}
		if raw, ok := file.doc["verifier"]; ok {
			if json.Unmarshal(raw, &section) != nil || section == nil {
				section = map[string]json.RawMessage{}
			}
		}
		for flagName, key := range map[string]string{"go-root": "go_root", "crw-dev": "crw_dev"} {
			if !set[flagName] {
				continue
			}
			value := tools.GoRoot
			if key == "crw_dev" {
				value = tools.CrwDev
			}
			if value == "" {
				delete(section, key)
				continue
			}
			section[key], _ = json.Marshal(value)
		}
		if err := file.writeSection("verifier", section); err != nil {
			fmt.Fprintf(stderr, "crw config verifier: error: %v\n", err)
			return 1
		}
		file.warnings = nil
		if w := file.verifierWarning(); w != nil {
			file.warnings = append(file.warnings, *w)
		}
		if tools, terr = file.VerifierTools(); terr != nil {
			tools = VerifierTools{}
		}
	}
	for _, w := range file.Warnings() {
		fmt.Fprintf(stderr, "crw config: warning: %s\n", w.Message)
	}
	return writeJSON(stdout, stderr, verifierReport{Config: file.path, GoRoot: tools.GoRoot, CrwDev: tools.CrwDev, Mode: tools.Mode()})
}

// writeSection replaces the top-level key name of the configuration document with v and writes the file in place of the old one
// (a temporary file in its directory, renamed over it), owner-only, its keys sorted; a file that is not there yet is created. It is
// the installation's one writer of the file, so it also writes the default of manage.child_check.disabled_servers where the
// document does not name it (withChildCheckDefault, CRW-1398). A
// file that is a symbolic link is written through: the file the link names is replaced and the link stays.
func (f *File) writeSection(name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	doc := map[string]json.RawMessage{}
	for k, value := range f.doc {
		doc[k] = value
	}
	doc[name] = raw
	withChildCheckDefault(doc)
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	target, err := linkTarget(f.path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	f.doc = doc
	return nil
}

// linkTarget is the file a write of path replaces: path itself, or, when path is a symbolic link, the file at the end of its
// links (which need not exist yet).
func linkTarget(path string) (string, error) {
	current := path
	for range 40 {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return current, nil
		}
		dest, err := os.Readlink(current)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(current), dest)
		}
		current = dest
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}
