package attest

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// PlanResult is plan-gate.ts PlanArtifactResult: on success Unit is the validated plan unit, normalised relative to cwd, so the
// caller persists exactly what was validated (REVIEW-BINDING-01); on refusal Reason says why. Unit is the spelling that resolves
// from cwd to the directory the gate validated: the lexical relative one, else the physical relative one, else the absolute real path.
type PlanResult struct {
	OK     bool
	Unit   string
	Reason string
}

// ValidatePlanArtifacts is plan-gate.ts validatePlanArtifacts, the P>A gate: the attestation must name a directory that holds at
// least one numbered plan document (000_*.md), and every planPaths entry must exist. att may be nil, so the first refusal names
// planUnit. Paths are relative to cwd unless absolute (used as written). Where the oracle throws, because a relative path needs
// the process's working directory and getcwd cannot answer, the port refuses.
//
// One departure from the oracle, by decision (a review finding of kind security, CRW-425): the planUnit and every planPaths entry
// must be the working directory or lie below it, judged on real paths, with the symlinks of the entry and of the working directory
// followed. The oracle accepted any existing directory or path, so an absolute planUnit, one that climbs out with "..", and a
// planPaths entry such as /etc/hostname all satisfied the gate. The refusal names the entry as written, never a real path.
// Whether an entry exists is still the oracle's own test (os.Stat on the path as written, so the kernel's limits apply); the real
// path serves containment and is then the path that is read, so the path checked is the path used. A symlink swapped in between
// the check and the use is not guarded against: the gate is a work control that makes the plan exist, not a boundary against
// another writer of the workspace. A path that cannot be resolved refuses.
func ValidatePlanArtifacts(att *Attestation, cwd string) PlanResult {
	if att == nil || att.PlanUnit == "" {
		return PlanResult{Reason: "P -> A requires \"planUnit\": the devlog/_plan/YYMMDD_slug/ unit this plan lives in " +
			"(DIFFLEVEL-ROADMAP-01 \u2014 the plan must exist as numbered files, not chat). " +
			"Scaffold one with `crw pabcd plan init <slug>` if missing, write the docs, then re-attest. " +
			"Put the JSON in a file and pass --attest-file <path> (required on Windows, where " +
			"inline JSON cannot survive shell argument parsing): " +
			`{"from":"P","to":"A","did":"...","planUnit":"devlog/_plan/..."}`}
	}
	unit, err := resolve(cwd, att.PlanUnit)
	if err != nil {
		return unreadable(err)
	}
	missing := PlanResult{Reason: "planUnit " + att.PlanUnit + " does not exist (or is not a directory). Create it with `crw pabcd plan init <slug>` and write the plan docs before P -> A."}
	if _, err := os.Stat(unit); err != nil {
		return missing
	}
	realUnit, err := filepath.EvalSymlinks(unit)
	if err != nil {
		return unresolved("planUnit " + att.PlanUnit)
	}
	base, err := resolve("", cwd)
	if err != nil {
		return unreadable(err)
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return unreadable(err)
	}
	rel, ok := within(realBase, realUnit)
	if !ok {
		return PlanResult{Reason: "planUnit " + att.PlanUnit + " resolves outside the working directory (symlinks followed). A plan unit must live inside the workspace: create it there with `crw pabcd plan init <slug>` and write the plan docs before P -> A."}
	}
	if info, err := os.Stat(realUnit); err != nil || !info.IsDir() {
		return missing
	}
	numbered := false
	// An unreadable directory has no documents, as readdirSync's catch makes it; a partial listing is not used.
	if entries, err := os.ReadDir(realUnit); err == nil {
		for _, e := range entries {
			numbered = numbered || isNumberedDoc(e.Name())
		}
	}
	if !numbered {
		return PlanResult{Reason: "planUnit " + att.PlanUnit + " has no numbered plan docs (000_*.md ...). A chat-message plan does not satisfy P (LEXICO-SPLIT-01) \u2014 write the diff-level docs first."}
	}
	for _, p := range att.PlanPaths {
		path, err := resolve(cwd, p)
		if err != nil {
			return unreadable(err)
		}
		gone := PlanResult{Reason: "planPaths entry " + p + " does not exist on disk."}
		if _, err := os.Stat(path); err != nil {
			return gone
		}
		phys, err := filepath.EvalSymlinks(path)
		if err != nil {
			return unresolved("planPaths entry " + p)
		}
		if _, ok := within(realBase, phys); !ok {
			return PlanResult{Reason: "planPaths entry " + p + " resolves outside the working directory (symlinks followed)."}
		}
	}
	// Relative to cwd: the round binding compares paths, and an absolute and a relative attestation naming one unit must agree. The
	// caller persists this and reads it again as resolve(cwd, unit), so it must name the directory that was validated: the lexical
	// spelling does so unless an absolute entry followed "symlink/..", the physical one unless cwd's own spelling does, and the real
	// path (absolute, so used as written) names it unless the filesystem changed meanwhile, which refuses.
	spellings := []string{rel, realUnit}
	if lexical, err := filepath.Rel(base, unit); err == nil {
		spellings = append([]string{lexical}, spellings...)
	}
	for _, spelling := range spellings {
		if names(cwd, spelling, realUnit) {
			return PlanResult{OK: true, Unit: spelling}
		}
	}
	return PlanResult{Reason: "planUnit " + att.PlanUnit + " could not be pinned to one directory: the path changed while it was checked."}
}

// within reports whether target, a real path, is dir or lies below it (dir a real path too), and returns target relative to dir. A
// name that merely begins with ".." (a directory called "..x") is below dir; a sibling that merely begins with dir's name (/ws2
// beside /ws) is not.
func within(dir, target string) (string, bool) {
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// names reports whether spelling, resolved against cwd as the caller resolves it, is the real path phys.
func names(cwd, spelling, phys string) bool {
	path, err := resolve(cwd, spelling)
	if err != nil {
		return false
	}
	got, err := filepath.EvalSymlinks(path)
	return err == nil && got == phys
}

// unreadable is the refusal for the oracle's exception: the working directory cannot be read.
func unreadable(err error) PlanResult {
	return PlanResult{Reason: "the working directory cannot be read: " + err.Error()}
}

// unresolved is the refusal for an entry that exists but whose real path cannot be named: a real path past the system's length limit
// (only a shorter spelling through a symlink reaches it, which the oracle's statSync accepts), or one changed while it was checked.
// Containment cannot be shown for it, so the gate refuses instead of guessing.
func unresolved(what string) PlanResult {
	return PlanResult{Reason: what + " could not be resolved to a real path, so it cannot be shown to lie inside the working directory (its real path is longer than the system allows, or it changed while it was checked)."}
}

// resolve is path.isAbsolute(p) ? p : path.resolve(cwd, p). A relative result is made absolute against the process's physical
// working directory, which is what process.cwd() answers (os.Getwd and filepath.Abs would follow a symlinked $PWD), and it fails
// where process.cwd() throws.
func resolve(cwd, p string) (string, error) {
	if filepath.IsAbs(p) {
		return p, nil
	}
	if p = filepath.Join(cwd, p); filepath.IsAbs(p) {
		return p, nil
	}
	wd, err := syscall.Getwd()
	return filepath.Join(wd, p), err
}
