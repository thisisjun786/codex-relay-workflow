// Harness drift and ast-grep checks, ported from CXC v0.2.40 cxc-ops/src/doctor.ts (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d): runDriftCheck (:543-596) and the POSIX branch of
// runAstGrepCheck (:597-645). HarnessDriftChecks answers the manifest's declared version, its
// mcpServers reference and a known-issues hint; HarnessAstGrepCheck probes the optional ast-grep
// helper through the caller's runner. Neither writes anything, and the command that assembles a
// report from the checks -- `crw doctor harness` with its text/JSON forms and its exit code --
// belongs to its own port issue.
//
// Two name rules apply (decision 1, contract/schema/cxc/name-substitution.json): the check names,
// severities and evidence strings are the oracle text, kept verbatim (the `npm run build` hint
// included, because no substitution rule rewrites it), and the ast-grep helper lives under the
// installed crw skill name, skills/crw-ast-grep, where the oracle spells skills/ast-grep. The
// windows branch (`py`, 9009's Store alias) and WSL are out of the port scope; 9009 stays in the
// interpreter test because the oracle tests it unconditionally.
//
// The runner seam is the package's HarnessRunner, and the oracle distinguishes two spawn results:
// a process that never started (spawnSync error.code ENOENT) and one the probe timeout killed
// (status null, signal SIGTERM, error.code ETIMEDOUT). The port keeps them apart (CRW-1015): a run
// with no status and no kill marker is the missing interpreter the check exists to report, while a
// killed run (HarnessRun.Killed, status harnessDriftKilled, or the "signal: ..." text) falls
// through to the install hint exactly as the oracle's timeout does. Both readings are pinned by the
// replay, which passes the recorded timeout through as the runner contract gives it.
package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const (
	// harnessDriftManifestRelative is the manifest drift:version reads (doctor.ts:546).
	harnessDriftManifestRelative = ".codex-plugin/plugin.json"
	// harnessDriftHelperRelative is the ast-grep helper the probe needs, under the installed crw
	// skill name (the oracle spells skills/ast-grep at :598; the names decision renames the folder
	// cxc-ast-grep to crw-ast-grep, which the staged skill under port/cxc/skills follows).
	harnessDriftHelperRelative = "skills/crw-ast-grep/scripts/ast_grep_helper.py"
	// harnessDriftPython and harnessDriftAstGrepTimeout are the interpreter and the probe timeout
	// of the POSIX branch (doctor.ts:612-613).
	harnessDriftPython         = "python3"
	harnessDriftAstGrepTimeout = 8 * time.Second
	// harnessDriftKilled is the status of a probe the runner ran and then killed when the timeout
	// expired: Go answers -1 for a process that died on a signal where the oracle's spawnSync
	// answers status null beside signal SIGTERM. It is not the missing interpreter (the ENOENT
	// spawn failure); it falls through to the install hint.
	harnessDriftKilled = -1
	// harnessDriftJSSpace is JavaScript's \s set as a Go regexp class (doctor.ts:632-633), the one
	// the oracle's version and path matches use: the ASCII whitespace plus the Unicode spaces V8
	// adds (NBSP, the en and em spaces, the line and paragraph separators, the byte order mark;
	// U+0085 is not one of them). harnessDriftJSNonSpace is \S.
	harnessDriftJSSpace    = "[\\t\\n\\v\\f\\r \\x{A0}\\x{1680}\\x{2000}-\\x{200A}\\x{2028}\\x{2029}\\x{202F}\\x{205F}\\x{3000}\\x{FEFF}]"
	harnessDriftJSNonSpace = "[^\\t\\n\\v\\f\\r \\x{A0}\\x{1680}\\x{2000}-\\x{200A}\\x{2028}\\x{2029}\\x{202F}\\x{205F}\\x{3000}\\x{FEFF}]"
)

// HarnessDriftChecks is runDriftCheck (doctor.ts:543-596): drift:version, drift:mcp and the
// known-issues hint, in that order. A manifest that cannot be read, parsed or member-read -- a
// missing file, a directory, invalid JSON, or a JSON null whose property read throws -- leaves
// exactly two checks, drift:version FAIL and known-issues: the oracle's drift:mcp pushes sit
// inside the same try block (:548-581) and are skipped. A document that parses but is not an
// object reads its members as undefined, exactly as JavaScript does.
func HarnessDriftChecks(pluginRoot string) []HarnessCheck {
	checks := make([]HarnessCheck, 0, 3)
	// The manifest is read through the same resolution the drift:mcp reference below is judged with,
	// so one root governs the whole check (CRW-937).
	manifest, err := harnessDriftReadJSON(targetResolve(pluginRoot, harnessDriftManifestRelative))
	if err != nil {
		checks = append(checks, harnessDriftManifestFailure(err))
	} else {
		version, memberErr := harnessDriftMember(manifest, "version")
		if memberErr != nil {
			checks = append(checks, harnessDriftManifestFailure(memberErr))
		} else {
			if text, ok := version.(string); ok && text != "" {
				checks = append(checks, HarnessCheck{Name: "drift:version", Severity: HarnessPass, Evidence: "declared plugin version " + text})
			} else {
				checks = append(checks, HarnessCheck{Name: "drift:version", Severity: HarnessWarn, Evidence: "manifest has no version field (cannot baseline drift)"})
			}
			checks = append(checks, harnessDriftMCPCheck(pluginRoot, manifest))
		}
	}
	failing := make([]string, 0, len(checks))
	for _, check := range checks {
		if check.Severity == HarnessFail {
			failing = append(failing, check.Name)
		}
	}
	if len(failing) == 0 {
		checks = append(checks, HarnessCheck{Name: "known-issues", Severity: HarnessPass, Evidence: "no known-issue signature matched"})
	} else {
		checks = append(checks, HarnessCheck{Name: "known-issues", Severity: HarnessWarn, Evidence: "drift FAIL in [" + strings.Join(failing, ", ") + "] — re-run `npm run build`, then inspect the named file before reinstalling"})
	}
	return checks
}

// harnessDriftManifestFailure is the outer catch of runDriftCheck (doctor.ts:579-581).
func harnessDriftManifestFailure(err error) HarnessCheck {
	return HarnessCheck{Name: "drift:version", Severity: HarnessFail, Evidence: "cannot read manifest for drift baseline: " + err.Error()}
}

// harnessDriftMCPCheck is the drift:mcp branch (doctor.ts:556-578): the reference the manifest
// declares must exist and parse, and the servers it declares must be countable. Everything it
// reads is evidence only; nothing is run or written. One deviation is deliberate (a Devin review
// finding of kind security, port: fixed in docs/port-cxc/known-defects.md): a reference that
// resolves outside the plugin root is refused instead of read, where the oracle reads wherever
// path.join lands.
func harnessDriftMCPCheck(pluginRoot string, manifest any) HarnessCheck {
	// The member read cannot throw here: the version read proved the manifest is not null.
	reference, _ := harnessDriftMember(manifest, "mcpServers")
	text, ok := reference.(string)
	if !ok || text == "" {
		return HarnessCheck{Name: "drift:mcp", Severity: HarnessWarn, Evidence: "manifest declares no mcpServers reference"}
	}
	// Node passes the decoded string to the filesystem as hookTrustEntriesFSPath holds it (a lone
	// surrogate becomes U+FFFD), and path.join keeps a trailing separator, which existsSync then
	// rejects for a regular file (doctor.ts:565-566); the resolution below drops it, so put it back.
	//
	// The reference is resolved with the same helper the manifest-target validator uses (CRW-937):
	// filepath.Join would clean a '..' the reference spelled after a symlink before the containment
	// check below, so this check would read an inside file while the validator refused the same
	// reference. Both judgements of one manifest field have to resolve it the same way.
	path := targetResolve(pluginRoot, hookTrustEntriesFSPath(text))
	if strings.HasSuffix(text, "/") && !strings.HasSuffix(path, "/") {
		path += "/"
	}
	if targetEscapesRoot(pluginRoot, path) {
		return HarnessCheck{Name: "drift:mcp", Severity: HarnessFail, Evidence: "mcpServers -> " + text + " resolves outside the plugin root"}
	}
	if _, err := os.Stat(path); err != nil {
		return HarnessCheck{Name: "drift:mcp", Severity: HarnessFail, Evidence: "mcpServers -> " + text + " but file is missing"}
	}
	document, err := harnessDriftReadJSON(path)
	if err == nil {
		var servers any
		servers, err = harnessDriftMember(document, "mcpServers")
		if err == nil {
			return HarnessCheck{Name: "drift:mcp", Severity: HarnessPass, Evidence: fmt.Sprintf("%s parses, %d server(s) declared", text, harnessDriftServerCount(servers))}
		}
	}
	return HarnessCheck{Name: "drift:mcp", Severity: HarnessFail, Evidence: text + " is unparseable: " + err.Error()}
}

// harnessDriftServerCount is Object.keys(mcpServers).length (doctor.ts:571): the own enumerable
// property count of an object or an array, the UTF-16 code-unit length of a string (an astral
// character counts two, as JavaScript counts it), and 0 for an absent member or a primitive --
// Object.keys(5) is empty, and the falsy cases never reach the call.
func harnessDriftServerCount(servers any) int {
	switch value := servers.(type) {
	case pyjson.Object:
		return len(value)
	case []any:
		return len(value)
	case string:
		units := 0
		for i := 0; i < len(value); {
			r, size := pyjson.CodePoint(value, i)
			switch {
			case size == 3 && pyjson.IsSurrogate(r):
				// A lone surrogate the reader kept as its three WTF-8 bytes is one UTF-16 unit.
				units++
			case r > 0xFFFF:
				units += 2
			default:
				units++
			}
			i += size
		}
		return units
	}
	return 0
}

// harnessDriftMember is the oracle's `document.key` read: a member of anything but an object is
// undefined, and a read of a JSON null throws the V8 TypeError the catch clauses report, spelled
// here as the oracle prints it (doctor.ts:550, :571).
func harnessDriftMember(document any, key string) (any, error) {
	if document == nil {
		return nil, harnessDriftNullMember(key)
	}
	object, _ := document.(pyjson.Object)
	return object.Get(key), nil
}

// harnessDriftNullMember is the TypeError V8 throws for a property read of null.
func harnessDriftNullMember(key string) error {
	return errors.New("TypeError: Cannot read properties of null (reading '" + key + "')")
}

// harnessDriftReadJSON is the oracle's readFileSync + JSON.parse (doctor.ts:549, :568): the file's
// bytes as Buffer.toString("utf8") holds them, parsed the way JSON.parse does, or the engine
// error the catch clauses report. The depth is the manifest-target reader's (Deep), not the
// hook-trust reader's shallower cap: a document JSON.parse accepts must not read as unparseable.
func harnessDriftReadJSON(path string) (any, error) {
	if doctorRootedReadSeam != nil {
		doctorRootedReadSeam(path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(hookTrustEntriesUTF8(raw), pyjson.LoadOptions{Surrogates: true, Deep: true})
}

// harnessDriftRunKilled reads a run the timeout or a signal ended, as against one that never
// started. The runner says so with Killed; a runner that does not is read by the status -1 an
// os.ProcessState gives a signalled process and by the "signal: ..." text os/exec's ExitError
// writes for it, which a run with no status carries on stderr (the shared contract of
// harness_report.go). A spawn failure has neither: no status, no signal text.
func harnessDriftRunKilled(run HarnessRun) bool {
	if run.Killed || (run.Status != nil && *run.Status == harnessDriftKilled) {
		return true
	}
	return run.Status == nil && strings.HasPrefix(strings.TrimSpace(run.Stderr), "signal: ")
}

// HarnessAstGrepCheck is the POSIX branch of runAstGrepCheck (doctor.ts:597-645): the ast-grep
// helper is optional on-demand tooling, so a missing helper or interpreter is a WARN with the
// install hint, never a FAIL. The probe runs the caller's runner (`python3 <helper> doctor`, 8 s).
// The oracle's catch branch ("ast-grep probe skipped") is not ported: it answers a runner that
// throws, and a Go runner returns a run instead.
func HarnessAstGrepCheck(pluginRoot string, runner HarnessRunner) HarnessCheck {
	helper := filepath.Join(pluginRoot, harnessDriftHelperRelative)
	if _, err := os.Stat(helper); err != nil {
		// existsSync: any path that stats -- a directory included -- goes on to the probe.
		return HarnessCheck{Name: "ast-grep", Severity: HarnessWarn, Evidence: "ast-grep skill helper not installed"}
	}
	result := runner(harnessDriftPython, []string{helper, "doctor"}, harnessDriftAstGrepTimeout)
	switch {
	case harnessDriftRunKilled(result):
		// A probe the timeout or a signal ended ran, so the interpreter exists: it falls through to
		// the install hint exactly as the oracle's ETIMEDOUT run does (CRW-1015).
		return HarnessCheck{Name: "ast-grep", Severity: HarnessWarn, Evidence: "sg not resolved — run `ast_grep_helper.py install` to provision"}
	case result.Status == nil || *result.Status == 127 || *result.Status == 9009:
		return HarnessCheck{Name: "ast-grep", Severity: HarnessWarn, Evidence: "python3 not found - install Python 3.9+ to run the ast-grep helper"}
	}
	out := result.Stdout + result.Stderr
	version := regexp.MustCompile("ast-grep" + harnessDriftJSSpace + "+([0-9]+\\.[0-9]+\\.[0-9]+)").FindStringSubmatch(out)
	path := regexp.MustCompile("ast-grep binary:" + harnessDriftJSSpace + "*(" + harnessDriftJSNonSpace + "+)").FindStringSubmatch(out)
	if *result.Status == 0 && version != nil {
		resolved := "(path n/a)"
		if path != nil {
			resolved = path[1]
		}
		return HarnessCheck{Name: "ast-grep", Severity: HarnessPass, Evidence: "sg resolved at " + resolved + " (version " + version[1] + ")"}
	}
	return HarnessCheck{Name: "ast-grep", Severity: HarnessWarn, Evidence: "sg not resolved — run `ast_grep_helper.py install` to provision"}
}
