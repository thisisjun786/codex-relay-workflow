package attest

import (
	"os"
	"path/filepath"
	"syscall"
)

// PlanResult is plan-gate.ts PlanArtifactResult: on success Unit is the validated plan unit, normalised relative to cwd, so the
// caller persists exactly what was validated (REVIEW-BINDING-01); on refusal Reason says why.
type PlanResult struct {
	OK     bool
	Unit   string
	Reason string
}

// ValidatePlanArtifacts is plan-gate.ts validatePlanArtifacts, the P>A gate: the attestation must name a directory that holds at
// least one numbered plan document (000_*.md), and every planPaths entry must exist. att may be nil, so the first refusal names
// planUnit. Paths are relative to cwd unless absolute (used as written). Where the oracle throws, because a relative path needs
// the process's working directory and getcwd cannot answer, the port refuses.
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
	if info, err := os.Stat(unit); err != nil || !info.IsDir() {
		return PlanResult{Reason: "planUnit " + att.PlanUnit + " does not exist (or is not a directory). Create it with `crw pabcd plan init <slug>` and write the plan docs before P -> A."}
	}
	numbered := false
	// An unreadable directory has no documents, as readdirSync's catch makes it; a partial listing is not used.
	if entries, err := os.ReadDir(unit); err == nil {
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
		if _, err := os.Stat(path); err != nil {
			return PlanResult{Reason: "planPaths entry " + p + " does not exist on disk."}
		}
	}
	// Relative to cwd: the round binding compares paths, and an absolute and a relative attestation naming one unit must agree.
	base, err := resolve("", cwd)
	if err != nil {
		return unreadable(err)
	}
	rel, err := filepath.Rel(base, unit)
	if err != nil {
		rel = unit
	}
	return PlanResult{OK: true, Unit: rel}
}

// unreadable is the refusal for the oracle's exception: the working directory cannot be read.
func unreadable(err error) PlanResult {
	return PlanResult{Reason: "the working directory cannot be read: " + err.Error()}
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
