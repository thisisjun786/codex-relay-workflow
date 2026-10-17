package crwconfig

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CrwDevTree is the verifier.crw_dev value that runs the verified tree's own crw-dev (`go run -tags dev ./cmd/crw-dev`).
const CrwDevTree = "tree"

// The verifier kinds (CRW-1594): local runs `crw-dev ci local` on the host, github-actions runs the configured
// repository's workflow on GitHub-hosted runners.
const (
	VerifierLocal         = "local"
	VerifierGitHubActions = "github-actions"
)

// VerifierTools is the verifier section of the configuration (CRW-1319): the tools a verification of a head or of an integration
// batch's merged tree runs. Kind selects the verifier (CRW-1594): empty and "local" are the host verifier; "github-actions" is
// the remote verifier, whose settings are Repository (OWNER/REPO), Workflow (default "CRW CI") and BranchPrefix (default "ci/").
// GoRoot is the Go toolchain root (its bin/go is the go the verifier and every step of `crw-dev ci
// local` run); CrwDev is how crw-dev runs: "tree" for the verified tree's own, an absolute path for an installed binary, empty
// for "tree" when a Go root is set. Config is the file the section was read from, for a message; Ignored is why a section the
// verifier cannot use was ignored (LoadVerifierTools), so the tools are the PATH fallback. Neither is part of the section.
type VerifierTools struct {
	Kind         string `json:"kind,omitempty"`
	Repository   string `json:"repository,omitempty"`
	Workflow     string `json:"workflow,omitempty"`
	BranchPrefix string `json:"branch_prefix,omitempty"`
	GoRoot       string `json:"go_root,omitempty"`
	CrwDev       string `json:"crw_dev,omitempty"`
	Config       string `json:"-"`
	Ignored      string `json:"-"`
}

// VerifierKind is the section's kind, "local" when it names none.
func (t VerifierTools) VerifierKind() string {
	if t.Kind == "" {
		return VerifierLocal
	}
	return t.Kind
}

// GitHubWorkflow is the workflow the remote verifier dispatches, "CRW CI" when the section names none.
func (t VerifierTools) GitHubWorkflow() string {
	if t.Workflow == "" {
		return "CRW CI"
	}
	return t.Workflow
}

// GitHubBranchPrefix is the prefix of the branches the remote verifier pushes, "ci/" when the section names none.
func (t VerifierTools) GitHubBranchPrefix() string {
	if t.BranchPrefix == "" {
		return "ci/"
	}
	return t.BranchPrefix
}

// CheckVerifierSection is the warning of a verifier section the verifier cannot use: the reading of the file ignores it, so it
// stops no other reader, and the verifier takes its tools from PATH (fail-open).
const CheckVerifierSection = "verifier_section"

// SectionVerifier is the Root a finding about the verifier section names.
const SectionVerifier = "verifier"

// verifierWarning is the warning a file's reading records for a verifier section the verifier cannot use, nil for a usable one.
// A local section the verifier cannot use is ignored (fail-open); a remote section the verifier cannot use is refused
// outright — a malformed remote selection never silently becomes the local verifier (CRW-1594).
func (f *File) verifierWarning() *Finding {
	if _, err := f.VerifierTools(); err != nil {
		if f.verifierSectionStrict() {
			return &Finding{Level: LevelWarning, Root: SectionVerifier, Check: CheckVerifierSection,
				Message: err.Error() + "; the verifier refuses this section (it never silently becomes the local verifier); repair it with crw config verifier"}
		}
		return &Finding{Level: LevelWarning, Root: SectionVerifier, Check: CheckVerifierSection,
			Message: err.Error() + "; the section is ignored and the verifier takes crw-dev and go from PATH (fail-open); record it with crw config verifier --go-root <dir>"}
	}
	return nil
}

// verifierSectionStrict reports whether the section's kind selects the strict reader: any explicit kind that
// is not null, empty or "local" — a non-string kind included — is refused rather than ignored when the
// section cannot be used.
func (f *File) verifierSectionStrict() bool {
	var section map[string]json.RawMessage
	if json.Unmarshal(f.doc["verifier"], &section) != nil {
		return false
	}
	raw, present := section["kind"]
	if !present {
		return false
	}
	var kind string
	if json.Unmarshal(raw, &kind) != nil {
		return true
	}
	return kind != "" && kind != VerifierLocal
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

// Validate is the section's own check: what a direct construction of a verifier must still satisfy.
func (t VerifierTools) Validate() error { return t.validate() }

func (t VerifierTools) validate() error {
	switch t.VerifierKind() {
	case VerifierGitHubActions:
		return t.validateGitHub()
	case VerifierLocal:
	default:
		return fmt.Errorf("verifier.kind %q: %q or %q only", t.Kind, VerifierLocal, VerifierGitHubActions)
	}
	if t.GoRoot != "" && !filepath.IsAbs(t.GoRoot) {
		return fmt.Errorf("verifier.go_root %q: not an absolute path", t.GoRoot)
	}
	if t.CrwDev != "" && t.CrwDev != CrwDevTree && !filepath.IsAbs(t.CrwDev) {
		return fmt.Errorf("verifier.crw_dev %q: neither %q nor an absolute path", t.CrwDev, CrwDevTree)
	}
	return nil
}

// githubRepositoryPattern is OWNER/REPO of GitHub identifiers: no URL, no whitespace or control characters,
// no options and no path traversal.
var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// githubRefBad is any byte a Git ref component may not carry.
var githubRefBad = regexp.MustCompile(`[\x00-\x20\\~^:?*\[\]]`)

func (t VerifierTools) validateGitHub() error {
	if !githubRepositoryPattern.MatchString(t.Repository) || strings.Contains(t.Repository, "..") {
		return fmt.Errorf("verifier.repository %q: not OWNER/REPO of GitHub identifiers", t.Repository)
	}
	if strings.TrimSpace(t.GitHubWorkflow()) == "" || strings.ContainsAny(t.GitHubWorkflow(), "\x00\n\r\t") {
		return fmt.Errorf("verifier.workflow %q: not a workflow name", t.Workflow)
	}
	prefix := t.GitHubBranchPrefix()
	if !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("verifier.branch_prefix %q: not a branch prefix ending in /", t.BranchPrefix)
	}
	if strings.HasPrefix(prefix, "refs/") {
		return fmt.Errorf("verifier.branch_prefix %q: a prefix below refs/ is not a branch prefix", t.BranchPrefix)
	}
	for _, component := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		switch {
		case component == "":
			return fmt.Errorf("verifier.branch_prefix %q: an empty component", t.BranchPrefix)
		case strings.Contains(component, ".."), strings.Contains(component, "@{"), githubRefBad.MatchString(component),
			strings.HasPrefix(component, "."), strings.HasSuffix(component, "."), strings.HasSuffix(component, ".lock"), component == "@":
			return fmt.Errorf("verifier.branch_prefix %q: %q is not a Git ref component", t.BranchPrefix, component)
		}
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
		if file.verifierSectionStrict() {
			return VerifierTools{}, err
		}
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

// verifierReport is the crw config verifier report: the tools as the verifier reads them, the normalized
// kind and, for a remote verifier, its repository, workflow and branch prefix.
type verifierReport struct {
	Config       string `json:"config"`
	Kind         string `json:"kind"`
	Repository   string `json:"repository,omitempty"`
	Workflow     string `json:"workflow,omitempty"`
	BranchPrefix string `json:"branch_prefix,omitempty"`
	GoRoot       string `json:"go_root"`
	CrwDev       string `json:"crw_dev"`
	Mode         string `json:"mode"`
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
	kind := flags.String("kind", "", `the verifier kind: "local" or "github-actions"`)
	repository := flags.String("repository", "", "the GitHub repository of the remote verifier, OWNER/REPO")
	workflow := flags.String("workflow", "", `the workflow the remote verifier dispatches (default "CRW CI")`)
	branchPrefix := flags.String("branch-prefix", "", `the remote branch prefix of the remote verifier (default "ci/")`)
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
	// a section the verifier cannot use is shown as the PATH fallback it is, with the warning; a record repairs the keys it sets.
	// With flags, a section that fails validation still decodes without it, so an explicit kind is preserved while its other keys
	// are repaired; a kind the raw section cannot decode is repaired only through --kind.
	tools, terr := file.VerifierTools()
	if terr != nil {
		tools = VerifierTools{}
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	// the raw section is decoded without validating only for a repair of a strict section — an explicit
	// remote or unknown kind, or --kind selecting one — so its complete resulting section validates; an
	// ordinary malformed local section keeps the zero-tools-plus-flag repair of the fail-open contract.
	strictRepair := file.verifierSectionStrict() || (set["kind"] && *kind != "" && *kind != VerifierLocal)
	kindUnreadable := false
	if terr != nil && len(set) > 0 && strictRepair {
		var section map[string]json.RawMessage
		if raw, ok := file.doc["verifier"]; ok && json.Unmarshal(raw, &section) == nil {
			_ = json.Unmarshal(raw, &tools)
			if kraw, present := section["kind"]; present {
				var k string
				if json.Unmarshal(kraw, &k) != nil {
					kindUnreadable = true
				}
			}
		}
	}
	if len(set) > 0 {
		if set["go-root"] {
			tools.GoRoot = *goRoot
		}
		if set["crw-dev"] {
			tools.CrwDev = *crwDev
		}
		if set["kind"] {
			tools.Kind = *kind
		}
		if set["repository"] {
			tools.Repository = *repository
		}
		if set["workflow"] {
			tools.Workflow = *workflow
		}
		if set["branch-prefix"] {
			tools.BranchPrefix = *branchPrefix
		}
		if kindUnreadable && !set["kind"] {
			fmt.Fprintf(stderr, "crw config verifier: error: the verifier section's kind could not be read; repair it with --kind\n")
			return usageExit
		}
		if err := tools.validate(); err != nil {
			fmt.Fprintf(stderr, "crw config verifier: error: %v\n", err)
			return usageExit
		}
		if tools.VerifierKind() == VerifierLocal && tools.GoRoot != "" {
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
		keyValues := map[string]string{"go_root": tools.GoRoot, "crw_dev": tools.CrwDev, "kind": tools.Kind,
			"repository": tools.Repository, "workflow": tools.Workflow, "branch_prefix": tools.BranchPrefix}
		for flagName, key := range map[string]string{"go-root": "go_root", "crw-dev": "crw_dev", "kind": "kind",
			"repository": "repository", "workflow": "workflow", "branch-prefix": "branch_prefix"} {
			if !set[flagName] {
				continue
			}
			value := keyValues[key]
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
	report := verifierReport{Config: file.path, Kind: tools.VerifierKind(), GoRoot: tools.GoRoot, CrwDev: tools.CrwDev, Mode: tools.Mode()}
	if tools.VerifierKind() == VerifierGitHubActions {
		report.Mode = VerifierGitHubActions
		report.Repository, report.Workflow, report.BranchPrefix = tools.Repository, tools.GitHubWorkflow(), tools.GitHubBranchPrefix()
	}
	return writeJSON(stdout, stderr, report)
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
