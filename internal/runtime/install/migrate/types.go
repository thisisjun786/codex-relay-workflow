// Package migrate holds the foundations of the explicit CXC-to-CRW state copy (docs/port-cxc/state-migration.md): the roots a run
// reads and writes, pinned by descriptor so no link is ever followed. No hook, installer, activation or startup path calls it.
package migrate

import (
	"fmt"
	"strings"
)

// Scope selects the stores a run covers.
type Scope string

const (
	ScopeProject Scope = "project"
	ScopeUser    Scope = "user"
	ScopeCodex   Scope = "codex"
	ScopeAll     Scope = "all"
)

// ParseScope reads a scope name; the empty string is the default scope, project.
func ParseScope(s string) (Scope, error) {
	switch sc := Scope(s); sc {
	case "":
		return ScopeProject, nil
	case ScopeProject, ScopeUser, ScopeCodex, ScopeAll:
		return sc, nil
	}
	return "", fmt.Errorf("unknown scope %q: want project, user, codex or all", s)
}

// Has reports whether s covers kind; all covers every kind.
func (s Scope) Has(kind Scope) bool { return s == ScopeAll || s == kind }

// Disposition is what the inventory decides for a path, Result what a run did with it.
type (
	Disposition string
	Result      string
)

const (
	DispCopy           Disposition = "copy"
	DispTransform      Disposition = "transform"
	DispSkip           Disposition = "skip"
	ResultDryRun       Result      = "dry-run"
	ResultCopied       Result      = "copied"
	ResultAlreadyEqual Result      = "already-equal"
	ResultRefused      Result      = "refused"
	ResultFailed       Result      = "failed"
)

// Reason says why a path or a root was refused.
type Reason string

const (
	ReasonOverlap      Reason = "overlap"       // source and destination trees overlap, or one lies inside the other
	ReasonLink         Reason = "link"          // a symbolic link where a real file or directory is required
	ReasonNotDirectory Reason = "not-directory" // not a directory where one is required
	ReasonNotRegular   Reason = "not-regular"   // a directory, FIFO, socket or device where a regular file is required
	ReasonHardLinked   Reason = "hard-linked"   // a file with more than one link
	ReasonSetID        Reason = "set-id"        // a file with a set-user-ID or set-group-ID bit
)

// RefusedError is a refusal: the path is skipped or the run stops, and nothing was changed on its account.
type RefusedError struct {
	Reason Reason
	Path   string
	Detail string
}

func (e *RefusedError) Error() string {
	return strings.TrimSuffix("refused ("+string(e.Reason)+"): "+e.Path+": "+e.Detail, ": ")
}

func refuse(reason Reason, path, detail string) error { return &RefusedError{reason, path, detail} }

// ProjectSourceName is the CXC project directory; the CRW one is crwdir.DirName.
const ProjectSourceName = ".codexclaw"

// The Codex-home names the copy maps in place (R18, R19 and R20 of contract/schema/cxc/name-substitution.json).
const (
	installSource, installDest   = ".codexclaw-install.json", ".crw-install.json"
	selfHealSource, selfHealDest = "codexclaw-self-heal.json", "crw-self-heal.json"
	backupSource, backupDest     = "config.toml.codexclaw-", "config.toml.crw-"
	backupSuffix, backupStamp    = ".bak", "0123456789TZ:.-"
)

// CodexLeaf maps a Codex-home file name of CXC to its CRW name, and reports false for any other name. The result never equals
// the argument.
func CodexLeaf(name string) (string, bool) {
	switch name {
	case installSource:
		return installDest, true
	case selfHealSource:
		return selfHealDest, true
	}
	if stamp, ok := strings.CutPrefix(name, backupSource); ok {
		if stamp, ok = strings.CutSuffix(stamp, backupSuffix); ok && stamp != "" && strings.Trim(stamp, backupStamp) == "" {
			return backupDest + stamp + backupSuffix, true
		}
	}
	return "", false
}
