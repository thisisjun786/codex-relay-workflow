// Hook trust retrust, ported from CXC v0.2.40 cxc-ops/src/hook-trust.ts:345-477 (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d): insertMissingSections (:345-365), writeAtomic
// (:367-381), resolvedConfigPath (:383-385), verifyCodexConfig (:387-410) and retrustHooks
// (:412-477), with the option parser and the answer of the cxc-ops/src/cli.ts hooks case
// (:40-71 parseHookOptions and resolvePluginKey, :96-115). It writes the trusted_hash of every hook
// a plugin declares into Codex's config.toml, after an exclusive timestamped backup, and rolls back
// from that backup when the write or its verification fails. Nothing here is a hook, an installer
// step or a SessionStart path: only the user runs "crw doctor retrust" (decision 8, J4).
//
// Two name rules apply (decision 1, contract/schema/cxc/name-substitution.json): the CLI table maps
// cxc hooks retrust to crw doctor retrust, and R29 renames the component word, so the usage line and
// the catch prefix name crw doctor retrust rather than cxc-ops hooks retrust. Every refusal text,
// the backup name and the answer lines are the oracle text.
//
// Three behaviours are deliberately not the oracle's, each one line in
// docs/port-cxc/known-defects/CRW-362.md:
//
//   - The write publishes through crwdir.Publish (CRW-427) instead of the oracle's writeAtomic, so a
//     config.toml the process cannot open for writing is refused rather than replaced.
//   - The write and the rollback both apply a compare-and-swap against the bytes they read, so a
//     settings writer that published in between is reported rather than silently replaced (a
//     settings data-loss fix; the oracle writes unconditionally).
//   - A failure after the backup names the backup it left behind (J4, 2026-10-06).
//
// Windows and WSL are out of the port scope (inventory.md: win-exec.ts and wsl.ts are OUT), so the
// codex invocation is the POSIX one: CODEX_BIN when it holds a non-blank value, else the codex on
// PATH (codex-bin.ts:112 resolveCodexInvocation on a non-win32 platform). The plugin root the oracle
// derives from its own module path (cli.ts:28-32) is PLUGIN_ROOT, the port's convention, or the
// package the Codex home's plugin cache holds (the user-run command has no host-supplied root).
package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// hookTrustRetrustNullName is the TypeError V8 throws for the "name" read of a JSON null plugin
// manifest (cli.ts:64), kept verbatim as hookTrustEntriesNullDocument keeps its sibling.
const hookTrustRetrustNullName = "Cannot read properties of null (reading 'name')"

// HookTrustRetrustResult is RetrustResult (hook-trust.ts:64-68): what one retrust changed, and the
// backup it left behind. Updated counts the entries whose trusted_hash was rewritten, Appended the
// sections that were inserted for hooks the config did not mention.
type HookTrustRetrustResult struct {
	Updated    int
	Appended   int
	BackupPath string
}

// HookTrustRetrustRun is one runner answer (hook-trust.ts:96-100): the exit status, the two streams,
// and the spawn error's message. Status nil is the oracle's null status, a process that did not
// start or one a signal ended.
type HookTrustRetrustRun struct {
	Status *int
	Stdout string
	Stderr string
	Error  string
}

// HookTrustRetrustRunner is HookTrustRunner (hook-trust.ts:96-100): run file with argv under the
// environment the verification passes, and answer the run. It is an argument so no caller or test
// needs a real codex.
type HookTrustRetrustRunner func(file string, args []string, env []string) HookTrustRetrustRun

// hookTrustRetrustReplacement is one trusted_hash value the write replaces, by byte offsets into the
// document (the oracle's offsets are UTF-16 code units; each side slices only its own string).
type hookTrustRetrustReplacement struct {
	start int
	end   int
	value string
}

// hookTrustRetrustInsertMissingSections is insertMissingSections (hook-trust.ts:345-365): the
// document with one [hooks.state."<key>"] trusted_hash section per missing entry, inserted before
// the first [hooks.state. section, else appended. The newline the document already uses is the one
// the block uses, so a CRLF config keeps CRLF.
func hookTrustRetrustInsertMissingSections(content string, entries []HookTrustEntry) string {
	if len(entries) == 0 {
		return content
	}
	newline := "\n"
	if strings.Contains(content, "\r\n") {
		newline = "\r\n"
	}
	blocks := make([]string, 0, len(entries))
	for _, entry := range entries {
		blocks = append(blocks, "[hooks.state.\""+entry.Key+"\"]"+newline+"trusted_hash = \""+entry.Hash+"\""+newline)
	}
	block := strings.Join(blocks, newline)
	for _, section := range hookTrustTomlSections(content) {
		if !strings.HasPrefix(section.Header, "[hooks.state.") {
			continue
		}
		before := content[:section.Start]
		separator := ""
		if len(before) > 0 && !strings.HasSuffix(before, newline) {
			separator = newline
		}
		return before + separator + block + newline + content[section.Start:]
	}
	separator := ""
	if len(content) > 0 && !strings.HasSuffix(content, newline) {
		separator = newline
	}
	leading := ""
	if len(content) > 0 {
		leading = newline
	}
	return content + separator + leading + block
}

// hookTrustRetrustResolvedConfigPath is resolvedConfigPath (hook-trust.ts:383-385): a config.toml
// that is a symlink is followed to the file it names, so the write lands on the target the reader
// reads and the link itself stays; any other path is answered as it is.
func hookTrustRetrustResolvedConfigPath(configPath string) (string, error) {
	info, err := os.Lstat(configPath)
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return configPath, nil
	}
	return filepath.EvalSymlinks(configPath)
}

// hookTrustRetrustBackupPath is the oracle's backup name (hook-trust.ts:463):
// <config.toml target>.bak-<ISO time with : replaced by ->.
func hookTrustRetrustBackupPath(target string, now time.Time) string {
	stamp := now.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	return target + ".bak-" + strings.ReplaceAll(stamp, ":", "-")
}

// hookTrustRetrustCopyExclusive is copyFileSync(target, backup, COPYFILE_EXCL) (hook-trust.ts:464):
// the backup is created exclusively, so an existing file is never overwritten, and it keeps the
// target's permission bits. A backup that could not be written whole is removed rather than left as
// a partial file an operator could mistake for a usable recovery copy.
func hookTrustRetrustCopyExclusive(target, backup string) error {
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = file.Close()
		if rmErr := os.Remove(backup); rmErr != nil {
			return errors.Join(err, rmErr)
		}
		return err
	}
	if _, err := file.Write(raw); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	return file.Close()
}

// hookTrustRetrustCodexBinary is resolveCodexInvocation on POSIX (codex-bin.ts:112), through the
// package's own resolver so the case-insensitive lookup and the JavaScript-style trimming stay the
// same as everywhere else: CODEX_BIN when it holds a non-blank value, else the bare codex on PATH.
func hookTrustRetrustCodexBinary(environ []string) string {
	return ResolveCodexInvocation("codex", nil, environ).File
}

// hookTrustRetrustEnv is the environment verifyCodexConfig passes (hook-trust.ts:396):
// {...process.env, CODEX_HOME: codexHome}. The process's own variables are kept, so a PATH, a proxy
// or a Codex setting the operator exported still reaches the probe.
func hookTrustRetrustEnv(codexHome string) []string {
	environ := os.Environ()
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if name, _, ok := strings.Cut(entry, "="); ok && name == "CODEX_HOME" {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "CODEX_HOME="+codexHome)
}

// hookTrustRetrustVerify is verifyCodexConfig (hook-trust.ts:387-410): prove the rewritten config
// still parses, by running codex features list against it with CODEX_HOME set. A non-zero status is
// the failure; the detail is the spawn error's message, else the trimmed stderr (which the oracle's
// nullish chain keeps even when it is empty).
func hookTrustRetrustVerify(codexHome string, runner HookTrustRetrustRunner) error {
	environ := hookTrustRetrustEnv(codexHome)
	result := runner(hookTrustRetrustCodexBinary(environ), []string{"features", "list"}, environ)
	if result.Status != nil && *result.Status == 0 {
		return nil
	}
	detail := result.Error
	if detail == "" {
		detail = strings.TrimSpace(result.Stderr)
	}
	return errors.New("codex features list verification failed: " + detail)
}

// hookTrustRetrustUnchanged is the compare-and-swap the repository's other settings writer applies
// (internal/role/registration.go registrationUnchanged): it answers an error unless target still
// holds the exact bytes expected, so a settings writer that published in between is reported rather
// than silently replaced with stale content (a settings data-loss fix over the oracle, which writes
// unconditionally; docs/port-cxc/known-defects/CRW-362.md).
func hookTrustRetrustUnchanged(target, expected string) error {
	current, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if string(current) != expected {
		return errors.New("config.toml changed while retrust was running")
	}
	return nil
}

// hookTrustRetrustReplace publishes next only while target is unchanged.
func hookTrustRetrustReplace(target, original, next string) error {
	if err := hookTrustRetrustUnchanged(target, original); err != nil {
		return err
	}
	return crwdir.Publish(target, []byte(next))
}

// hookTrustRetrustRollback restores target from backup after a failed write or verification, and
// answers the failure the caller reports. It restores only while target still holds the bytes the
// write published: a settings writer that published in between is never silently replaced, and the
// backup is kept instead (a settings data-loss fix over the oracle, which restores unconditionally;
// docs/port-cxc/known-defects/CRW-362.md). A rollback that does not happen names the backup it kept
// (J4, 2026-10-06): the message ends with " (restored from backup <path>)", or with
// " (rollback failed: <error>; backup kept at <path>)".
func hookTrustRetrustRollback(target, backup, published string, cause error) error {
	raw, err := os.ReadFile(backup)
	if err == nil {
		if err = hookTrustRetrustUnchanged(target, published); err == nil {
			err = crwdir.Publish(target, raw)
		}
	}
	if err != nil {
		return fmt.Errorf("%w (rollback failed: %v; backup kept at %s)", cause, err, backup)
	}
	return fmt.Errorf("%w (restored from backup %s)", cause, backup)
}

// HookTrustRetrust is retrustHooks (hook-trust.ts:412-477): rewrite the trusted_hash of every hook
// the plugin at pluginRoot declares, in the config.toml of codexHome. It answers the result, the
// diagnosis it verified with (so the caller does not read the config a second time), and an error.
//
// The refusals, in the oracle's order and words: a missing config.toml; a plugin that declares no
// synchronous command hooks; a duplicate exact section header; more than one, or no, trusted_hash in
// one such section; no existing entry at all unless bootstrapOK; and an existing entry none of whose
// recorded hashes matches its recomputed hash (the safety pin). The backup is made, exclusively,
// before the write, and both the write and the rollback go through crwdir.Publish.
func HookTrustRetrust(codexHome, pluginRoot, pluginKey string, bootstrapOK bool, runner HookTrustRetrustRunner, env host.LookupEnv, now time.Time) (HookTrustRetrustResult, []HookTrustResult, error) {
	configPath := filepath.Join(codexHome, "config.toml")
	if _, err := os.Stat(configPath); err != nil {
		return HookTrustRetrustResult{}, nil, errors.New("missing " + configPath)
	}
	targetPath, err := hookTrustRetrustResolvedConfigPath(configPath)
	if err != nil {
		return HookTrustRetrustResult{}, nil, err
	}
	raw, err := os.ReadFile(targetPath)
	if err != nil {
		return HookTrustRetrustResult{}, nil, err
	}
	original := string(raw)
	expected, err := ListHookTrustEntries(pluginRoot, pluginKey)
	if err != nil {
		return HookTrustRetrustResult{}, nil, err
	}
	if len(expected) == 0 {
		return HookTrustRetrustResult{}, nil, errors.New("plugin declares no synchronous command hooks to trust")
	}

	var replacements []hookTrustRetrustReplacement
	var missing []HookTrustEntry
	existingCount, matchingCount := 0, 0
	for _, entry := range expected {
		sections := hookTrustTomlExactHookSections(original, entry.Key)
		if len(sections) > 1 {
			return HookTrustRetrustResult{}, nil, errors.New("duplicate section header: [hooks.state.\"" + entry.Key + "\"]")
		}
		if len(sections) == 0 {
			missing = append(missing, entry)
			continue
		}
		existingCount++
		section := sections[0]
		hashes := hookTrustTomlTrustedHashLines(original[section.BodyStart:section.End])
		if len(hashes) > 1 {
			return HookTrustRetrustResult{}, nil, errors.New("multiple trusted_hash lines in [hooks.state.\"" + entry.Key + "\"]")
		}
		if len(hashes) == 0 {
			return HookTrustRetrustResult{}, nil, errors.New("missing trusted_hash in [hooks.state.\"" + entry.Key + "\"]")
		}
		if hashes[0].Value == entry.Hash {
			matchingCount++
		}
		lineStart := section.BodyStart + hashes[0].Start
		valueStart := lineStart + strings.Index(hashes[0].Text, "\"") + 1
		replacements = append(replacements, hookTrustRetrustReplacement{valueStart, valueStart + len(hashes[0].Value), entry.Hash})
	}

	if existingCount == 0 && !bootstrapOK {
		return HookTrustRetrustResult{}, nil, errors.New("no existing hook trust entries match this plugin key; pass --bootstrap-ok to initialize trust")
	}
	if existingCount > 0 && matchingCount == 0 {
		return HookTrustRetrustResult{}, nil, errors.New("safety pin failed: no existing hook trust entry matches its recomputed hash")
	}

	next := original
	sorted := append([]hookTrustRetrustReplacement(nil), replacements...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start > sorted[j].start })
	for _, replacement := range sorted {
		next = next[:replacement.start] + replacement.value + next[replacement.end:]
	}
	next = hookTrustRetrustInsertMissingSections(next, missing)

	// The oracle backs the file up before it writes, and so does this. The compare-and-swap runs
	// first, so a settings writer that published since the read is reported before a backup is made
	// for bytes that are already stale.
	if err := hookTrustRetrustUnchanged(targetPath, original); err != nil {
		return HookTrustRetrustResult{}, nil, err
	}
	backupPath := hookTrustRetrustBackupPath(targetPath, now)
	if err := hookTrustRetrustCopyExclusive(targetPath, backupPath); err != nil {
		return HookTrustRetrustResult{}, nil, err
	}
	result := HookTrustRetrustResult{BackupPath: backupPath}
	if err := hookTrustRetrustReplace(targetPath, original, next); err != nil {
		return result, nil, hookTrustRetrustRollback(targetPath, backupPath, next, err)
	}
	if err := hookTrustRetrustVerify(codexHome, runner); err != nil {
		return result, nil, hookTrustRetrustRollback(targetPath, backupPath, next, err)
	}
	verification, err := DiagnoseHookTrust(codexHome, pluginRoot, pluginKey)
	if err != nil {
		return result, nil, hookTrustRetrustRollback(targetPath, backupPath, next, err)
	}
	var failed []string
	for _, item := range verification {
		if item.Status != "trusted" {
			failed = append(failed, item.Key)
		}
	}
	if len(failed) > 0 {
		cause := errors.New("post-write verification failed for " + strings.Join(failed, ", "))
		return result, nil, hookTrustRetrustRollback(targetPath, backupPath, next, cause)
	}
	result.Updated = len(replacements)
	result.Appended = len(missing)
	return result, verification, nil
}

// hookTrustRetrustExec is the real HookTrustRetrustRunner: run file with argv under env and answer
// the exit status and the two streams. The probe gets the shared 8 s timeout the harness features
// check uses (doctor.ts:349); a process that cannot start answers the spawn error's message, which
// is what the oracle's result.error carries.
func hookTrustRetrustExec(file string, args []string, env []string) HookTrustRetrustRun {
	ctx, cancel := context.WithTimeout(context.Background(), harnessRunFeaturesTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, file, args...)
	command.WaitDelay = commandWaitDelay
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	run := HookTrustRetrustRun{Stdout: stdout.String(), Stderr: stderr.String()}
	switch {
	case ctx.Err() == context.DeadlineExceeded || errors.Is(err, exec.ErrWaitDelay):
		run.Status = harnessRunInt(harnessDriftKilled)
	case err == nil:
		run.Status = harnessRunInt(0)
	default:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code := exit.ExitCode()
			run.Status = &code
		} else {
			run.Error = err.Error()
		}
	}
	return run
}

// hookTrustRetrustOptionSet is the parsed command line.
type hookTrustRetrustOptionSet struct {
	codexHome   string
	pluginKey   string
	pluginRoot  string
	bootstrapOK bool
}

// hookTrustRetrustOptions is parseHookOptions (cli.ts:40-60): --bootstrap-ok, --key <value> and
// --codex-home <value> (resolved), and anything else is the "unknown hooks option" the catch prints.
// The default Codex home is CODEX_HOME when set, else the account's ~/.codex. --plugin-root is the
// port's own addition: the oracle read its plugin package from its module path, and a user-run Go
// command has no such relation (docs/port-cxc/known-defects/CRW-362.md).
func hookTrustRetrustOptions(args []string, env host.LookupEnv) (options hookTrustRetrustOptionSet, err error) {
	if value, set := env("CODEX_HOME"); set {
		options.codexHome = value
	} else {
		home, err := host.Home(env)
		if err != nil {
			return options, err
		}
		options.codexHome = filepath.Join(home, ".codex")
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--bootstrap-ok":
			options.bootstrapOK = true
		case "--key", "--codex-home", "--plugin-root":
			// The oracle checks only that a value is present and truthy (cli.ts:51: if (!value)),
			// so an empty value is refused exactly as the oracle refuses it.
			if index+1 >= len(args) || args[index+1] == "" {
				return options, errors.New(arg + " requires a value")
			}
			value := args[index+1]
			switch arg {
			case "--key":
				options.pluginKey = value
			case "--plugin-root":
				options.pluginRoot = value
			default:
				if resolved, err := filepath.Abs(value); err == nil {
					options.codexHome = resolved
				} else {
					options.codexHome = value
				}
			}
			index++
		default:
			return options, errors.New("unknown hooks option: " + arg)
		}
	}
	return options, nil
}

// hookTrustRetrustPluginRoot answers the plugin package the hooks are read from: the host's
// PLUGIN_ROOT (what the dispatcher passes), else --plugin-root, else the one plugin package under
// the Codex home's plugin cache whose manifest declares hooks. The oracle derived its root from its
// own module path (cli.ts:28-32), which a Go binary installed through the runtime pointer has no
// equivalent of, and PLUGIN_ROOT reaches plugin-declared hook commands rather than a user's shell
// (docs/plugin-packaging.md), so the cache is what lets the documented user-run command work from a
// terminal.
func hookTrustRetrustPluginRoot(pluginRoot, explicit, codexHome string) (string, error) {
	if pluginRoot != "" {
		return pluginRoot, nil
	}
	if explicit != "" {
		return explicit, nil
	}
	cacheRoot := filepath.Join(codexHome, "plugins", "cache")
	roots, err := hookTrustRetrustCachedPluginRoots(cacheRoot)
	if err != nil {
		return "", err
	}
	if len(roots) != 1 {
		return "", errors.New("no single plugin package under " + cacheRoot + " declares hooks; pass --plugin-root <dir>")
	}
	return roots[0], nil
}

// hookTrustRetrustCachedPluginRoots lists every <cacheRoot>/<marketplace>/<plugin>/<version> whose
// manifest declares at least one hook (the layout harnessInstallRootBody reads). A cache that does
// not exist holds none.
func hookTrustRetrustCachedPluginRoots(cacheRoot string) ([]string, error) {
	markets, err := os.ReadDir(cacheRoot)
	if err != nil {
		return nil, nil
	}
	var found []string
	for _, market := range markets {
		if !market.IsDir() {
			continue
		}
		plugins, err := os.ReadDir(filepath.Join(cacheRoot, market.Name()))
		if err != nil {
			continue
		}
		for _, plugin := range plugins {
			if !plugin.IsDir() {
				continue
			}
			versions, err := os.ReadDir(filepath.Join(cacheRoot, market.Name(), plugin.Name()))
			if err != nil {
				continue
			}
			for _, version := range versions {
				if !version.IsDir() {
					continue
				}
				root := filepath.Join(cacheRoot, market.Name(), plugin.Name(), version.Name())
				if hookTrustRetrustDeclaresHooks(root) {
					found = append(found, root)
				}
			}
		}
	}
	return found, nil
}

// hookTrustRetrustDeclaresHooks reports whether the package at root has a readable manifest that
// declares at least one hook. A package whose manifest cannot be read is not a candidate.
func hookTrustRetrustDeclaresHooks(root string) bool {
	raw, err := hookTrustEntriesReadContained(root, ".codex-plugin/plugin.json", nil)
	if err != nil {
		return false
	}
	document, err := hookTrustEntriesParse(raw)
	if err != nil || document == nil {
		return false
	}
	object, _ := document.(pyjson.Object)
	hooks, _ := object.Get("hooks").([]any)
	return len(hooks) > 0
}

// hookTrustRetrustResolveKey is resolvePluginKey (cli.ts:62-71): an explicit --key, else the
// manifest's name looked up among the install keys the config enables, which must be exactly one.
func hookTrustRetrustResolveKey(pluginRoot, pluginKey, codexHome string) (string, error) {
	if pluginKey != "" {
		return pluginKey, nil
	}
	raw, err := hookTrustEntriesReadContained(pluginRoot, ".codex-plugin/plugin.json", nil)
	if err != nil {
		return "", err
	}
	document, err := hookTrustEntriesParse(raw)
	if err != nil {
		return "", err
	}
	if document == nil {
		//lint:ignore ST1005 Exact V8 property access error, pinned by the oracle (cli.ts:64).
		return "", errors.New(hookTrustRetrustNullName)
	}
	object, _ := document.(pyjson.Object)
	name, _ := object.Get("name").(string)
	if name == "" {
		return "", errors.New("plugin manifest has no name")
	}
	candidates, err := ReadInstalledPluginKeys(codexHome, name)
	if err != nil {
		return "", err
	}
	if len(candidates) != 1 {
		listed := "(none)"
		if len(candidates) > 0 {
			listed = strings.Join(candidates, ", ")
		}
		return "", fmt.Errorf("enabled install key is ambiguous (%d); candidates: %s; pass --key <plugin@marketplace>", len(candidates), listed)
	}
	return candidates[0], nil
}

// HookTrustRetrustCLI is the hooks case of cxc-ops/src/cli.ts (:96-115) as the CLI table maps it:
// crw doctor retrust [--key <plugin@marketplace>] [--codex-home <path>] [--bootstrap-ok]. Every
// failure is "crw doctor retrust: <message>" on stderr and exit 1. The oracle's own usage line
// belongs to its "cxc hooks" dispatch (cli.ts:97-100), which the CLI table maps to the retrust form
// itself, so a word after the verb stays the parser's "unknown hooks option: <word>" and the form
// is advertised by the doctor dispatcher's unknown-argument list instead.
func HookTrustRetrustCLI(args []string, stdout, stderr io.Writer, env host.LookupEnv, runner HookTrustRetrustRunner, pluginRoot string, now time.Time) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "crw doctor retrust: "+err.Error())
		return 1
	}
	options, err := hookTrustRetrustOptions(args, env)
	if err != nil {
		return fail(err)
	}
	root, err := hookTrustRetrustPluginRoot(pluginRoot, options.pluginRoot, options.codexHome)
	if err != nil {
		return fail(err)
	}
	key, err := hookTrustRetrustResolveKey(root, options.pluginKey, options.codexHome)
	if err != nil {
		return fail(err)
	}
	result, results, err := HookTrustRetrust(options.codexHome, root, key, options.bootstrapOK, runner, env, now)
	if err != nil {
		return fail(err)
	}
	for _, item := range results {
		actual := "(none)"
		if item.Actual != nil {
			actual = *item.Actual
		}
		fmt.Fprintf(stdout, "[%s] %s expected=%s actual=%s\n", item.Status, item.Key, item.Hash, actual)
	}
	fmt.Fprintf(stdout, "updated=%d appended=%d\nbackup: %s\n", result.Updated, result.Appended, result.BackupPath)
	return 0
}
