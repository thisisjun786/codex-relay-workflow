package dagsched

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The tool pins a verified commit declares (CRW-1026, d1). CRW-964's writer (internal/dev/ci/local_tools.go) reads the same
// files with the same patterns when it seals a record; this reader is the judge's own copy, because the relay cannot import
// a dev-tag package. The required pin set of a record is what the verified commit itself declares, so it never depends on
// the PATH of the host that judges.
var (
	pinGoModToolchain = regexp.MustCompile(`(?m)^toolchain go([0-9][^\s]*)$`)
	pinGoModRequire   = regexp.MustCompile(`(?m)^\s*(honnef\.co/go/tools)\s+v([0-9][^\s]*)$`)
	pinSecretsVersion = regexp.MustCompile(`(?m)^scan_version=([0-9][^\s]*)$`)
	pinStepKey        = regexp.MustCompile(`^([a-z][a-z-]*):(?: (.*))?$`)
	pinWithEntry      = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_.-]*):(?: (.*))?$`)
)

// verificationPinFiles are the files DeclaredToolPins reads from the commit.
const (
	pinGoModFile    = "go.mod"
	pinSecretsFile  = "scripts/ci/secrets.sh"
	pinWorkflowFile = verificationCIFile
)

// DeclaredToolPins is what a commit declares about its tools, read through read, which answers a file of the commit (found
// is false for a file the commit does not have). go is the go.mod toolchain, staticcheck the honnef.co/go/tools require,
// gitleaks the scan_version of scripts/ci/secrets.sh, and node the distinct node-version values of the setup-node steps of
// ci.yml (sorted, joined by a comma), present only when the workflow has such a step with a version. A tool the commit does
// not declare has no entry. The result is never nil.
func DeclaredToolPins(read func(path string) (body []byte, found bool, err error)) (map[string]string, error) {
	pins := map[string]string{}
	body, found, err := read(pinGoModFile)
	if err != nil {
		return nil, err
	}
	if found {
		if m := pinGoModToolchain.FindSubmatch(body); m != nil {
			pins["go"] = string(m[1])
		}
		if m := pinGoModRequire.FindSubmatch(body); m != nil {
			pins["staticcheck"] = string(m[2])
		}
	}
	if body, found, err = read(pinSecretsFile); err != nil {
		return nil, err
	} else if found {
		if m := pinSecretsVersion.FindSubmatch(body); m != nil {
			pins["gitleaks"] = string(m[1])
		}
	}
	if body, found, err = read(pinWorkflowFile); err != nil {
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

// workflowNodeVersions are the distinct node-version values of the steps that use actions/setup-node, sorted. It reads the
// steps' `uses:` and `with:` keys line by line, as CRW-964's writer does; it is not a YAML parser. A step is judged once it
// is read whole, so `with:` before `uses:` is the same step as the reverse order. A setup-node step that names no
// node-version it can read is an error, never "no Node declaration": the pin set of a record must not shrink because the
// workflow was written in a shape this reader does not understand.
func workflowNodeVersions(text string) ([]string, error) {
	var versions []string
	var step struct {
		open, setupNode bool
		versions        []string
	}
	flush := func() error {
		if step.open && step.setupNode {
			if len(step.versions) == 0 {
				return refuse(contract.RefusalDispositionConflict, "ci.yml has an actions/setup-node step with no node-version this reader can read, so the node pin the commit declares is unknown")
			}
			versions = append(versions, step.versions...)
		}
		step.open, step.setupNode, step.versions = false, false, nil
		return nil
	}
	inWith := false
	withIndent := 0
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if inWith {
			if indent > withIndent {
				if m := pinWithEntry.FindStringSubmatch(trimmed); m != nil && m[1] == "node-version" {
					if value := pinScalar(m[2]); value != "" {
						step.versions = append(step.versions, value)
					}
				}
				continue
			}
			inWith = false
		}
		if strings.HasPrefix(trimmed, "- ") {
			// a new step: its first key sits after the dash
			if err := flush(); err != nil {
				return nil, err
			}
			step.open = true
			trimmed, indent = strings.TrimSpace(trimmed[2:]), indent+2
		}
		m := pinStepKey.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		switch m[1] {
		case "uses":
			if fields := strings.Fields(m[2]); len(fields) > 0 && strings.HasPrefix(fields[0], "actions/setup-node@") {
				step.setupNode = true
			}
		case "with":
			if strings.TrimSpace(m[2]) == "" {
				inWith, withIndent = true, indent
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	slices.Sort(versions)
	return slices.Compact(versions), nil
}

// pinScalar reads a YAML scalar of a with: entry: quotes are dropped and a trailing comment is not part of the value.
func pinScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '\'' {
		if end := strings.Index(value[1:], "'"); end >= 0 {
			return strings.ReplaceAll(value[1:1+end], "''", "'")
		}
	}
	if len(value) >= 2 && value[0] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
		if end := strings.Index(value[1:], `"`); end >= 0 {
			return value[1 : 1+end]
		}
	}
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return value
}
