// Harness install checks, ported from CXC v0.2.40 cxc-ops/src/doctor.ts (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d): checkPabcdHealth (:166-181), manifestTargetChecks
// (:228-254) and runInstalledRootCheck (:401-445), the F4b slice of the harness report. They
// return CRW-346's HarnessCheck and add no command: assembling runDoctor and connecting
// `crw doctor harness` is the sibling issue CRW-618.
//
// Two name rules apply (decision 1, contract/schema/cxc/name-substitution.json): R32 makes
// `.codexclaw` `.crw` and the cli table maps `cxc reset --state` to `crw pabcd reset --state` in the
// repair hint. Windows and WSL are out of scope (inventory.md: win-exec.ts and wsl.ts are OUT).
//
// The oracle's error text is an observable part of the report, so the filesystem errors are
// printed in Node's shape (CODE: description, syscall ['path']) like the cli port's planFailure.
// Where the oracle throws outside every catch -- an unreadable sessions directory, a homedir that
// cannot be established -- the port panics with the same message, the throw the CLI boundary
// (CRW-618) catches as cli.ts catches it.
package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// harnessInstallSessionsDir is the state directory checkPabcdHealth inspects, in CRW names.
const harnessInstallSessionsDir = ".crw/sessions"

// harnessInstallCheck is one install-root check, the only check shape the oracle builds there.
func harnessInstallCheck(severity HarnessSeverity, evidence, repair string) HarnessCheck {
	return HarnessCheck{Name: "install-root", Severity: severity, Evidence: evidence, Repair: harnessReportRepair(repair)}
}

// HarnessPabcdCheck is checkPabcdHealth (doctor.ts:166-181): the sessions directory of the
// project, a PASS while every `*.json` session parses, and a WARN naming the corrupt ones. A file
// that cannot be read counts as corrupt, as the oracle's catch does; the oracle's own
// `readdirSync` failure is outside that catch and aborts the report, so this port panics with the
// same message.
func HarnessPabcdCheck(projectRoot string) HarnessCheck {
	stateDir := filepath.Join(projectRoot, harnessInstallSessionsDir)
	if !harnessReportIsDir(stateDir) {
		return HarnessCheck{Name: "pabcd-state", Severity: HarnessPass, Evidence: "no .crw/sessions/ directory (clean state)"}
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		panic(harnessInstallScandirError(err))
	}
	total := 0
	corrupt := []string{}
	for _, entry := range entries {
		name := harnessInstallNodeName(entry.Name())
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		total++
		raw, err := os.ReadFile(filepath.Join(stateDir, name))
		if err != nil {
			corrupt = append(corrupt, name)
			continue
		}
		if _, err := harnessInstallParseJSON(raw); err != nil {
			corrupt = append(corrupt, name)
		}
	}
	if len(corrupt) > 0 {
		return HarnessCheck{
			Name:     "pabcd-state",
			Severity: HarnessWarn,
			Evidence: fmt.Sprintf("%d corrupt session file(s): %s", len(corrupt), strings.Join(corrupt, ", ")),
			Repair:   harnessReportRepair("crw pabcd reset --state"),
		}
	}
	return HarnessCheck{Name: "pabcd-state", Severity: HarnessPass, Evidence: fmt.Sprintf("%d session file(s), all parseable", total)}
}

// harnessInstallTargetKind is one KINDS row of manifestTargetChecks (doctor.ts:229-232): the
// validator's kind and the check name it is reported under.
type harnessInstallTargetKind struct {
	kind TargetKind
	name string
}

func harnessInstallTargetKinds() []harnessInstallTargetKind {
	return []harnessInstallTargetKind{{TargetHook, "hooks"}, {TargetMCP, "mcp-targets"}}
}

// HarnessManifestTargetChecks is manifestTargetChecks (doctor.ts:228-254) over the shared
// validator of the build (ValidateManifestTargets, CRW-332/555). A malformed document fails its
// own kind and leaves the other unevaluated rather than passing it; any other error lands in one
// generic check instead of guessing a kind.
func HarnessManifestTargetChecks(pluginRoot string) []HarnessCheck {
	kinds := harnessInstallTargetKinds()
	issues, err := ValidateManifestTargets(pluginRoot)
	if err != nil {
		var parse *TargetParseError
		if errors.As(err, &parse) {
			checks := make([]HarnessCheck, 0, len(kinds))
			for _, row := range kinds {
				if row.kind == parse.Kind {
					checks = append(checks, HarnessCheck{Name: row.name, Severity: HarnessFail, Evidence: "unparseable " + string(parse.Kind) + " json: " + parse.Path})
				} else {
					checks = append(checks, HarnessCheck{Name: row.name, Severity: HarnessWarn, Evidence: "not evaluated after " + string(parse.Kind) + " parse failure"})
				}
			}
			return checks
		}
		return []HarnessCheck{{Name: "manifest-targets", Severity: HarnessFail, Evidence: "target validation failed: " + harnessInstallString(err)}}
	}
	checks := make([]HarnessCheck, 0, len(kinds))
	for _, row := range kinds {
		messages := []string{}
		for _, issue := range issues {
			if issue.Kind == row.kind {
				messages = append(messages, issue.Message)
			}
		}
		if len(messages) == 0 {
			checks = append(checks, HarnessCheck{Name: row.name, Severity: HarnessPass, Evidence: "all " + string(row.kind) + " target(s) present"})
			continue
		}
		checks = append(checks, HarnessCheck{Name: row.name, Severity: HarnessFail, Evidence: strings.Join(messages, ", ")})
	}
	return checks
}

// HarnessInstalledRootCheck is runInstalledRootCheck (doctor.ts:401-445): the STALE-ROOT-01 check
// that compares the payload's declared version with the version directories the plugin cache
// holds. A throw inside its try is a WARN whose evidence is the oracle's message; a homedir that
// cannot be established is outside that try and aborts, as the port panics.
func HarnessInstalledRootCheck(pluginRoot string, options HarnessOptions) HarnessCheck {
	return harnessInstallInstalledRootCheck(pluginRoot,
		func() (string, error) {
			return harnessInstallCodexHome(options.CodexHome, os.LookupEnv, harnessInstallPasswdHome)
		},
		os.ReadFile)
}

// harnessInstallInstalledRootCheck is HarnessInstalledRootCheck with the resolution and the
// manifest read injected, so the tests can reach the host-dependent branches.
func harnessInstallInstalledRootCheck(pluginRoot string, resolve func() (string, error), readFile func(string) ([]byte, error)) HarnessCheck {
	codexHome, err := resolve()
	if err != nil {
		// doctor.ts:402 reads the home outside the try: nothing is read before it fails.
		panic(err)
	}
	check, err := harnessInstallRootBody(pluginRoot, codexHome, readFile)
	if err != nil {
		return harnessInstallCheck(HarnessWarn, harnessInstallErrorMessage(err, ""), "")
	}
	return check
}

// harnessInstallRootBody is runInstalledRootCheck's try block.
func harnessInstallRootBody(pluginRoot, codexHome string, readFile func(string) ([]byte, error)) (HarnessCheck, error) {
	raw, err := readFile(filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"))
	if err != nil {
		return HarnessCheck{}, err
	}
	manifest, err := harnessInstallParseJSON(raw)
	if err != nil {
		return HarnessCheck{}, err
	}
	name, err := harnessInstallManifestString(manifest, "name")
	if err != nil {
		return HarnessCheck{}, err
	}
	// A lone surrogate escape in the name becomes U+FFFD when Node hands the string to the
	// filesystem, so the cache lookup and the evidence both use the converted text.
	name = targetNodeText(name)
	version, err := harnessInstallManifestString(manifest, "version")
	if err != nil {
		return HarnessCheck{}, err
	}
	if name == "" || version == "" {
		return harnessInstallCheck(HarnessWarn, "manifest has no name/version; cannot locate the install root", ""), nil
	}
	// cache/<marketplace>/<plugin>/<version>: the marketplace segment is not in the manifest, so
	// the cache is scanned for the plugin folder rather than guessing it.
	cacheRoot := filepath.Join(codexHome, "plugins", "cache")
	if _, err := os.Stat(cacheRoot); err != nil {
		return harnessInstallCheck(HarnessWarn, "no plugin cache at "+cacheRoot+" (running uninstalled?)", ""), nil
	}
	markets, err := os.ReadDir(cacheRoot)
	if err != nil {
		return HarnessCheck{}, harnessInstallScandirError(err)
	}
	found := []string{}
	for _, market := range markets {
		dir := filepath.Join(cacheRoot, harnessInstallNodeName(market.Name()), name)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		versions, err := os.ReadDir(dir)
		if err != nil {
			return HarnessCheck{}, harnessInstallScandirError(err)
		}
		for _, entry := range versions {
			found = append(found, filepath.Join(dir, harnessInstallNodeName(entry.Name())))
		}
	}
	if len(found) == 0 {
		return harnessInstallCheck(HarnessWarn, name+" is not installed under "+cacheRoot, ""), nil
	}
	live := false
	for _, root := range found {
		if strings.HasSuffix(root, string(filepath.Separator)+version) {
			live = true
		}
	}
	if !live {
		return harnessInstallCheck(HarnessFail,
			fmt.Sprintf("this payload declares %s, but the installed root(s) are: %s. Any session started before the last reinstall is running hooks from a path that no longer exists (STALE-ROOT-01).", version, strings.Join(found, ", ")),
			"codex plugin add <plugin>@<marketplace>, then RESTART Codex — a running session keeps the old PLUGIN_ROOT"), nil
	}
	return harnessInstallCheck(HarnessPass, fmt.Sprintf("installed root matches this payload (%s); %d root(s) present", version, len(found)), ""), nil
}

// harnessInstallManifestString is the oracle's `typeof manifest[key] === "string"` read: a JSON
// null manifest reproduces the V8 TypeError, any other non-object has no such member.
func harnessInstallManifestString(manifest any, key string) (string, error) {
	if manifest == nil {
		return "", errors.New("Cannot read properties of null (reading '" + key + "')")
	}
	object, _ := manifest.(pyjson.Object)
	value, _ := object.Get(key).(string)
	return value, nil
}

// harnessInstallCodexHome is doctor.ts:402's option ?? CODEX_HOME ?? join(homedir(), ".codex"): a
// nil option is absent and falls through, a non-nil option -- the empty string included -- is used
// verbatim, and CODEX_HOME is used verbatim as well. The home results always get the .codex
// suffix, and a variable that is present but empty is present (so an empty HOME gives the relative
// ".codex").
func harnessInstallCodexHome(codexHome *string, lookup record.Environ, passwdHome func() (string, error)) (string, error) {
	if codexHome != nil {
		return *codexHome, nil
	}
	if value, ok := lookup("CODEX_HOME"); ok {
		return value, nil
	}
	if home, ok := lookup("HOME"); ok {
		return filepath.Join(home, ".codex"), nil
	}
	home, err := passwdHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// harnessInstallPasswdHome is os.homedir()'s getpwuid fallback: record.Home over a lookup that
// reports HOME unset.
func harnessInstallPasswdHome() (string, error) {
	return record.Home(func(string) (string, bool) { return "", false })
}

// harnessInstallNodeName is a filesystem entry name as Node's readdirSync reads it: the raw bytes
// decoded to UTF-8, each ill-formed sequence becoming U+FFFD. A valid name is unchanged, so the
// sorted os.ReadDir order and the evidence text match what the oracle would print for the same
// directory.
func harnessInstallNodeName(name string) string {
	return source.DecodeUTF8([]byte(name))
}

// harnessInstallParseJSON is JSON.parse over Node's UTF-8 decode. Unlike the hook-trust reader it
// has no MaxDepth cap (JSON.parse accepts nesting past 10,000, probed at 10001), keeps an
// object's key order with dict semantics for a repeated key and refuses trailing content, as
// encoding/json's decoder does.
func harnessInstallParseJSON(data []byte) (any, error) {
	return pyjson.Loads(source.DecodeUTF8(data), pyjson.LoadOptions{Surrogates: true, Deep: true})
}

// harnessInstallString is String(err) for the generic manifest-target check: the error's name and
// message.
func harnessInstallString(err error) string {
	name, message := harnessInstallError(err)
	return name + ": " + message
}

// harnessInstallError is one thrown value's name and message. A TypeError the validator models
// (the null-manifest member read) keeps its name; a filesystem error becomes Node's message.
func harnessInstallError(err error) (string, string) {
	var shape targetShapeError
	if errors.As(err, &shape) {
		return "TypeError", shape.Error()
	}
	return "Error", harnessInstallErrorMessage(err, "")
}

// harnessInstallScandirError is the error the oracle's readdirSync throws; Go reports readdirent
// (or open) for the same failure, so the syscall name is passed in.
func harnessInstallScandirError(err error) error {
	return errors.New(harnessInstallErrorMessage(err, "scandir"))
}

// harnessInstallErrorMessage is one Node error message, "CODE: description, syscall" with the
// path appended for every syscall but read, write and close (probed against Node v24.20.0: open
// and scandir append it, read omits it). syscallName overrides the Go operation name when the
// oracle calls another one; an empty name uses the error's own.
func harnessInstallErrorMessage(err error, syscallName string) string {
	var path *os.PathError
	var errno syscall.Errno
	if errors.As(err, &path) && errors.As(err, &errno) {
		if code, description := harnessInstallErrno(errno); code != "" {
			operation := syscallName
			if operation == "" {
				operation = path.Op
			}
			message := code + ": " + description + ", " + operation
			if operation != "read" && operation != "write" && operation != "close" {
				message += " '" + path.Path + "'"
			}
			return message
		}
	}
	return err.Error()
}

// harnessInstallErrno is libuv's filesystem errno name and description, the pair Node prints in
// an fs error message (the cli port's planErrno table).
func harnessInstallErrno(e syscall.Errno) (string, string) {
	switch e {
	case syscall.ENOENT:
		return "ENOENT", "no such file or directory"
	case syscall.EACCES:
		return "EACCES", "permission denied"
	case syscall.EPERM:
		return "EPERM", "operation not permitted"
	case syscall.ENOTDIR:
		return "ENOTDIR", "not a directory"
	case syscall.EEXIST:
		return "EEXIST", "file already exists"
	case syscall.EISDIR:
		return "EISDIR", "illegal operation on a directory"
	case syscall.ENOSPC:
		return "ENOSPC", "no space left on device"
	case syscall.EROFS:
		return "EROFS", "read-only file system"
	case syscall.EIO:
		return "EIO", "i/o error"
	case syscall.EMFILE:
		return "EMFILE", "too many open files"
	case syscall.ENFILE:
		return "ENFILE", "file table overflow"
	case syscall.ENAMETOOLONG:
		return "ENAMETOOLONG", "name too long"
	case syscall.ELOOP:
		return "ELOOP", "too many symbolic links encountered"
	}
	return "", ""
}
