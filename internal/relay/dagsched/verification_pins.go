package dagsched

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The tool pins a verified commit declares (CRW-1026, d1). CRW-964's writer (internal/dev/ci/local_tools.go) reads the same
// files when it seals a record; this reader is the judge's own, because the relay cannot import a dev-tag package. The
// required pin set of a record is what the verified commit itself declares, so it never depends on the PATH of the host that
// judges. A declaration this reader finds but cannot read is an error, never "the tool is not declared": the pin set of a
// record must not shrink because the commit wrote a declaration in a form the reader does not follow.
var (
	// pinSecretsAssignment is a line that assigns scan_version, bare or after a declaring builtin; its group is the text after
	// the equals sign. pinSecretsValue is the one readable form of that text: a version, plain or in matching quotes, and at
	// most a trailing comment after it.
	pinSecretsAssignment = regexp.MustCompile(`^[ \t]*(?:(?:export|readonly|declare|typeset|local)[ \t]+(?:-[A-Za-z]+[ \t]+)*)?scan_version(\+?)=(.*)$`)
	pinSecretsValue      = regexp.MustCompile(`^(?:([0-9][A-Za-z0-9._+-]*)|'([0-9][A-Za-z0-9._+-]*)'|"([0-9][A-Za-z0-9._+-]*)")(?:[ \t]+#.*)?$`)
)

// pinStaticcheckModule is the module whose require is the staticcheck pin.
const pinStaticcheckModule = "honnef.co/go/tools"

// verificationPinFiles are the files DeclaredToolPins reads from the commit.
const (
	pinGoModFile    = "go.mod"
	pinSecretsFile  = "scripts/ci/secrets.sh"
	pinWorkflowFile = verificationCIFile
)

// DeclaredToolPins is what a commit declares about its tools, read through read, which answers a file of the commit (found
// is false for a file the commit does not have). go is the go.mod toolchain, staticcheck the honnef.co/go/tools require
// (single-line or in a block, any comment, LF or CRLF),
// gitleaks the scan_version of scripts/ci/secrets.sh, and node the distinct node-version values of the setup-node steps of
// ci.yml (sorted, joined by a comma), present only when the workflow has such a step with a version. A tool the commit does
// not declare has no entry. The result is never nil.
func DeclaredToolPins(read func(path string) (body []byte, found bool, err error)) (map[string]string, error) {
	pins, err := DeclaredToolchainPins(read)
	if err != nil {
		return nil, err
	}
	body, found, err := read(pinWorkflowFile)
	if err != nil {
		return nil, err
	} else if found {
		nodes, err := workflowNodeVersions(string(body))
		if err != nil {
			return nil, err
		}
		if len(nodes) > 0 {
			pins["node"] = strings.Join(nodes, ",")
		}
	}
	return pins, nil
}

// DeclaredToolchainPins is DeclaredToolPins without the node pin: the go.mod toolchain, the staticcheck require and the
// scan_version of scripts/ci/secrets.sh. The record writer (crw-dev ci local) reads its pins through it, so the writer and the
// judge read one declaration.
func DeclaredToolchainPins(read func(path string) (body []byte, found bool, err error)) (map[string]string, error) {
	pins := map[string]string{}
	body, found, err := read(pinGoModFile)
	if err != nil {
		return nil, err
	}
	if found {
		if err := goModPins(string(body), pins); err != nil {
			return nil, err
		}
	}
	if body, found, err = read(pinSecretsFile); err != nil {
		return nil, err
	} else if found {
		version, err := secretsPin(string(body))
		if err != nil {
			return nil, err
		}
		if version != "" {
			pins["gitleaks"] = version
		}
	}
	return pins, nil
}

// secretsPin is the Gitleaks version scripts/ci/secrets.sh assigns to scan_version, "" when it assigns none. Every line that
// assigns the variable must be one this reader can read (a version, plain or quoted, with at most a comment after it) and all
// of them must agree: a scan_version the commit sets but the reader does not follow would otherwise read as "not declared".
func secretsPin(text string) (string, error) {
	version := ""
	for _, line := range strings.Split(text, "\n") {
		m := pinSecretsAssignment.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if m == nil {
			continue
		}
		unreadable := func(format string, args ...any) error {
			return refuse(contract.RefusalDispositionConflict, "scripts/ci/secrets.sh "+format+", so the gitleaks pin the commit declares is unknown", args...)
		}
		value := pinSecretsValue.FindStringSubmatch(strings.TrimSpace(m[2]))
		if m[1] != "" || value == nil {
			return "", unreadable("assigns scan_version in a form this reader cannot read (%q)", strings.TrimSpace(line))
		}
		got := value[1] + value[2] + value[3]
		if version != "" && version != got {
			return "", unreadable("assigns scan_version at two versions (%s and %s)", version, got)
		}
		version = got
	}
	return version, nil
}

var (
	pinGoModBlockOpen = regexp.MustCompile(`^(\S+)\s*\($`)
	pinGoModToolchain = regexp.MustCompile(`^go[0-9]\S*$`)
	pinGoModVersion   = regexp.MustCompile(`^v[0-9]\S*$`)
)

// goModPins reads the toolchain and the staticcheck require of a go.mod into pins, the way the go command reads the file:
// line by line with // comments dropped, LF or CRLF, a directive on its own line or as an entry of a factored block
// (require ( ... )), a module path quoted or not. go is the toolchain without its "go" prefix, staticcheck the
// honnef.co/go/tools version without its "v". A go.mod with neither declares neither. A toolchain that is not a go version,
// two toolchains, a staticcheck require that names no version or two versions, and a block that is never closed are
// errors: the commit declared something this reader could not read.
func goModPins(text string, pins map[string]string) error {
	unreadable := func(format string, args ...any) error {
		return refuse(contract.RefusalDispositionConflict, "go.mod "+format+", so the pins the commit declares are unknown", args...)
	}
	toolchain, staticcheck := "", ""
	entry := func(verb string, args []string) error {
		switch verb {
		case "toolchain":
			if len(args) != 1 || !pinGoModToolchain.MatchString(args[0]) {
				return unreadable("has a toolchain directive this reader cannot read (%q)", strings.Join(args, " "))
			}
			if toolchain != "" {
				return unreadable("has more than one toolchain directive")
			}
			toolchain = strings.TrimPrefix(args[0], "go")
		case "require":
			if len(args) == 0 {
				return nil
			}
			path := args[0]
			if unquoted, err := strconv.Unquote(path); err == nil {
				path = unquoted
			}
			if path != pinStaticcheckModule {
				return nil
			}
			if len(args) != 2 || !pinGoModVersion.MatchString(args[1]) {
				return unreadable("requires %s in a form this reader cannot read (%q)", pinStaticcheckModule, strings.Join(args, " "))
			}
			version := strings.TrimPrefix(args[1], "v")
			if staticcheck != "" && staticcheck != version {
				return unreadable("requires %s at two versions (%s and %s)", pinStaticcheckModule, staticcheck, version)
			}
			staticcheck = version
		}
		return nil
	}
	block := ""
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if block != "" {
			if line == ")" {
				block = ""
				continue
			}
			if err := entry(block, strings.Fields(line)); err != nil {
				return err
			}
			continue
		}
		if m := pinGoModBlockOpen.FindStringSubmatch(line); m != nil {
			block = m[1]
			continue
		}
		fields := strings.Fields(line)
		if err := entry(fields[0], fields[1:]); err != nil {
			return err
		}
	}
	if block != "" {
		return unreadable("leaves its %s block open", block)
	}
	if toolchain != "" {
		pins["go"] = toolchain
	}
	if staticcheck != "" {
		pins["staticcheck"] = staticcheck
	}
	return nil
}
