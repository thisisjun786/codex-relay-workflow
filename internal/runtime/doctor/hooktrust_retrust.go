// Hook trust retrust, ported from CXC v0.2.40 cxc-ops/src/hook-trust.ts:345-477 (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d): insertMissingSections (:345-365), writeAtomic
// (:367-381), resolvedConfigPath (:383-385), verifyCodexConfig (:387-410) and retrustHooks
// (:412-477), with the option parser and the answer of the cxc-ops/src/cli.ts hooks case
// (:40-71 parseHookOptions and resolvePluginKey, :96-115). It writes the trusted_hash of every hook
// a plugin declares into Codex's config.toml. Nothing here is a hook, an installer step or a
// SessionStart path: only the user runs "crw doctor retrust" (decision 8, J4).
//
// CRW-844 replaced the oracle's write-then-verify-then-roll-back with a swap-then-verify
// publication (crwdir.PublishSwap): the lock is taken first, next is read from the locked file and
// verified in a temporary Codex home before anything is published, and the file the publication
// displaced is kept as the backup instead of being copied beforehand. There is no rollback path
// any more, because nothing unverified is ever visible at the real path.
//
// Two name rules apply (decision 1, contract/schema/cxc/name-substitution.json): the CLI table maps
// cxc hooks retrust to crw doctor retrust, and R29 renames the component word, so the usage line and
// the catch prefix name crw doctor retrust rather than cxc-ops hooks retrust. Every refusal text,
// the backup name and the answer lines are the oracle text.
//
// Three behaviours are deliberately not the oracle's, each one line in
// docs/port-cxc/known-defects/CRW-362.md:
//
//   - The write publishes through crwdir.PublishSwap (CRW-427's rule, kept) instead of the oracle's
//     writeAtomic, so a config.toml the process cannot open for writing is refused rather than
//     replaced.
//   - The write is serialized against every other CRW writer of config.toml and refuses to publish
//     over bytes that changed since it read them, so a settings writer that published in between is
//     reported rather than silently replaced (a settings data-loss fix; the oracle writes
//     unconditionally).
//   - The backup is the very file the publication displaced, and it is never deleted, so a failure
//     always names the file that holds the displaced content (J4, 2026-10-06).
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
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// hookTrustRetrustNullName is the TypeError V8 throws for the "name" read of a JSON null plugin
// manifest (cli.ts:64), kept verbatim as hookTrustEntriesNullDocument keeps its sibling.
const hookTrustRetrustNullName = "Cannot read properties of null (reading 'name')"

// hookTrustRetrustLockWait is how long retrust waits for another CRW writer's sidecar lock before it
// refuses. It is brief on purpose: a user-run command should not hang behind another writer, and the
// refusal names the reason.
const hookTrustRetrustLockWait = 2 * time.Second

// HookTrustRetrustResult is RetrustResult (hook-trust.ts:64-68): what one retrust changed, and the
// backup it left behind. Updated counts the entries whose trusted_hash was rewritten, Appended the
// sections that were inserted for hooks the config did not mention.
//
// CRW-844 adds the publication state the report needs: Planned is set once the plan exists, so a
// refusal that comes after it still reports the planned counts; Published records that the exchange
// ran; Conflict records that the displaced content was not what retrust read; LateWrite records that
// config.toml held something else again when it was read back; Warning carries a post-publication
// failure that is counted as written.
//
// CRW-936 adds the two states the overlap report needs, and the reason a refusal prints: RecheckFailed
// and RecheckError record that the target could not be read again after a conflict, so the report
// asserts nothing about its content; Reason carries the refusal's message for the report of a refusal
// that came before the plan. ConfigPath is set as soon as the config path is known, so every refusal
// can name the file it left alone.
type HookTrustRetrustResult struct {
	Updated    int
	Appended   int
	BackupPath string
	ConfigPath string
	Planned    bool
	Published  bool
	Conflict   bool
	LateWrite  bool
	// RecheckFailed records that the target could not be read again after the exchange displaced
	// content that was not what retrust read (another save landed in between). The report then says
	// the read failed and asserts nothing about what the target holds; RecheckError carries why.
	RecheckFailed bool
	RecheckError  string
	// Reason is the refusal's message, set by the command line for a refusal that came before the
	// plan existed. The report prints it as the "no plan" reason.
	Reason  string
	Warning string
	// DisplacedAt names the file that holds the content the publication displaced when the backup
	// path could not be filled (a failure after the exchange). It is empty when the backup path
	// holds it, which is every other case.
	DisplacedAt string
	// UpdatedKeys and AppendedKeys name the items the plan rewrote and inserted, in listing order,
	// so the report can print them for a success, a refusal that had a plan, and a conflict alike.
	UpdatedKeys  []string
	AppendedKeys []string
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

// hookTrustRetrustVerifyNext proves that next is a config Codex still accepts, before it is
// published. It is the oracle's verifyCodexConfig (hook-trust.ts:387-410) and its post-write
// diagnoseHookTrust check, moved ahead of the publication (CRW-844): a temporary CODEX_HOME holding
// only config.toml is built under TMPDIR, codex features list runs against it, and the hook trust is
// diagnosed there with the real plugin root. A failure refuses without writing, so no unverified
// config is ever visible at the real path and no rollback path is needed. The refusal text is the
// issue's: "pre-write verification failed ...; config.toml unchanged".
func hookTrustRetrustVerifyNext(pluginRoot, pluginKey, next string, runner HookTrustRetrustRunner) error {
	refuse := func(detail string) error {
		return errors.New("pre-write verification failed: " + detail + "; config.toml unchanged")
	}
	temp, err := os.MkdirTemp("", "crw-retrust-verify-")
	if err != nil {
		return refuse(err.Error())
	}
	defer func() { _ = os.RemoveAll(temp) }()
	if err := os.WriteFile(filepath.Join(temp, "config.toml"), []byte(next), 0o600); err != nil {
		return refuse(err.Error())
	}
	if err := hookTrustRetrustVerify(temp, runner); err != nil {
		return refuse(err.Error())
	}
	results, err := DiagnoseHookTrust(temp, pluginRoot, pluginKey)
	if err != nil {
		return refuse(err.Error())
	}
	var failed []string
	for _, item := range results {
		if item.Status != "trusted" {
			failed = append(failed, item.Key)
		}
	}
	if len(failed) > 0 {
		return refuse("post-write verification failed for " + strings.Join(failed, ", "))
	}
	return nil
}

// HookTrustRetrust is retrustHooks (hook-trust.ts:412-477) with CRW-844's swap-then-verify
// publication: rewrite the trusted_hash of every hook the plugin at pluginRoot declares, in the
// config.toml of codexHome. It answers the result, the diagnosis the report prints, and an error.
//
// The order is the issue's answer, and it is what makes the data-loss fix real:
//
//   - the sidecar lock is taken first, so every CRW writer of config.toml is serialized and a
//     concurrent one is refused rather than raced;
//   - config.toml (B) is read inside the lock and next is computed from B only;
//   - next is verified in a temporary Codex home before anything is published, so an unverified
//     config is never visible at the real path and there is no rollback path;
//   - crwdir.PublishSwap exchanges the temp file with the target atomically and keeps the displaced
//     file as the backup. A displaced file whose bytes differ from B is a non-cooperative writer
//     that saved in between: its content stays in the backup, nothing is exchanged back, and the
//     conflict is reported with both paths. What the target holds then is read again before the
//     answer (CRW-936): retrust's content when nothing saved after the exchange, else the newer save
//     (Conflict and LateWrite), or nothing is claimed about it when that read fails (RecheckFailed);
//   - a sync-only failure is a *crwdir.PublishedError: the publication counts as done and the
//     failure is reported as a warning;
//   - config.toml is read back, and a value that is not next is reported and left in place.
//
// The refusals, in the oracle's order and words: a missing config.toml; a plugin that declares no
// synchronous command hooks; a duplicate exact section header; more than one, or no, trusted_hash in
// one such section; no existing entry at all unless bootstrapOK; and an existing entry none of whose
// recorded hashes matches its recomputed hash (the safety pin). Planned is set once the plan exists,
// so a refusal after that point still reports the planned counts.
func HookTrustRetrust(codexHome, pluginRoot, pluginKey string, bootstrapOK bool, runner HookTrustRetrustRunner, env host.LookupEnv, now time.Time) (HookTrustRetrustResult, []HookTrustResult, error) {
	return hookTrustRetrustWith(codexHome, pluginRoot, pluginKey, bootstrapOK, runner, env, now, nil)
}

// hookTrustRetrustSeams is the publication a test stages, a field rather than a package-level
// variable (the idiom orchestrateCommitSeams uses): nil means the real crwdir.PublishSwap. It exists
// because the last check and the exchange are two steps inside one call, so a test cannot otherwise
// put a save between them; crwdir's own tests prove the real behaviour at every step.
type hookTrustRetrustSeams struct {
	publish func(target string, expected, next []byte, backupPath string) ([]byte, error)
}

func hookTrustRetrustWith(codexHome, pluginRoot, pluginKey string, bootstrapOK bool, runner HookTrustRetrustRunner, env host.LookupEnv, now time.Time, seams *hookTrustRetrustSeams) (HookTrustRetrustResult, []HookTrustResult, error) {
	publish := crwdir.PublishSwap
	if seams != nil && seams.publish != nil {
		publish = seams.publish
	}
	configPath := filepath.Join(codexHome, "config.toml")
	// The result is declared before anything can refuse, so every refusal names the config path it
	// left alone and the report can print it (CRW-936). The joined path is enough before the file is
	// resolved; it is replaced by the resolved target once the lock names it.
	result := HookTrustRetrustResult{ConfigPath: configPath}
	if _, err := os.Stat(configPath); err != nil {
		return result, nil, errors.New("missing " + configPath)
	}
	targetPath, err := hookTrustRetrustResolvedConfigPath(configPath)
	if err != nil {
		return result, nil, err
	}
	lock, err := crwdir.LockConfig(targetPath, hookTrustRetrustLockWait)
	if err != nil {
		return result, nil, err
	}
	defer lock.Release()
	targetPath = lock.Target
	result.ConfigPath = targetPath
	raw, err := os.ReadFile(targetPath)
	if err != nil {
		return result, nil, err
	}
	original := string(raw)
	expected, err := ListHookTrustEntries(pluginRoot, pluginKey)
	if err != nil {
		return result, nil, err
	}
	if len(expected) == 0 {
		return result, nil, errors.New("plugin declares no synchronous command hooks to trust")
	}

	var replacements []hookTrustRetrustReplacement
	var missing []HookTrustEntry
	var updatedKeys, appendedKeys []string
	existingCount, matchingCount := 0, 0
	for _, entry := range expected {
		sections := hookTrustTomlExactHookSections(original, entry.Key)
		if len(sections) > 1 {
			return result, nil, errors.New("duplicate section header: [hooks.state.\"" + entry.Key + "\"]")
		}
		if len(sections) == 0 {
			missing = append(missing, entry)
			appendedKeys = append(appendedKeys, entry.Key)
			continue
		}
		existingCount++
		section := sections[0]
		hashes := hookTrustTomlTrustedHashLines(original[section.BodyStart:section.End])
		if len(hashes) > 1 {
			return result, nil, errors.New("multiple trusted_hash lines in [hooks.state.\"" + entry.Key + "\"]")
		}
		if len(hashes) == 0 {
			return result, nil, errors.New("missing trusted_hash in [hooks.state.\"" + entry.Key + "\"]")
		}
		if hashes[0].Value == entry.Hash {
			matchingCount++
		}
		lineStart := section.BodyStart + hashes[0].Start
		valueStart := lineStart + strings.Index(hashes[0].Text, "\"") + 1
		replacements = append(replacements, hookTrustRetrustReplacement{valueStart, valueStart + len(hashes[0].Value), entry.Hash})
		updatedKeys = append(updatedKeys, entry.Key)
	}

	// The plan exists from here on, so a refusal below reports the items it would have changed.
	backupPath := hookTrustRetrustBackupPath(targetPath, now)
	result.Updated = len(replacements)
	result.Appended = len(missing)
	result.BackupPath = backupPath
	result.Planned = true
	result.UpdatedKeys = updatedKeys
	result.AppendedKeys = appendedKeys
	if existingCount == 0 && !bootstrapOK {
		return result, nil, errors.New("no existing hook trust entries match this plugin key; pass --bootstrap-ok to initialize trust")
	}
	if existingCount > 0 && matchingCount == 0 {
		return result, nil, errors.New("safety pin failed: no existing hook trust entry matches its recomputed hash")
	}

	next := original
	sorted := append([]hookTrustRetrustReplacement(nil), replacements...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start > sorted[j].start })
	for _, replacement := range sorted {
		next = next[:replacement.start] + replacement.value + next[replacement.end:]
	}
	next = hookTrustRetrustInsertMissingSections(next, missing)

	// Nothing is published before next is proven good in a temporary Codex home. A refusal here
	// leaves config.toml exactly as it was, so there is no rollback and no unverified publication.
	if err := hookTrustRetrustVerifyNext(pluginRoot, pluginKey, next, runner); err != nil {
		return result, nil, err
	}
	displaced, err := publish(targetPath, raw, []byte(next), backupPath)
	if err != nil && !crwdir.Published(err) {
		// The exchange never ran, so config.toml is as it was; the report still names the plan.
		return result, nil, err
	}
	result.Published = true
	if err != nil {
		result.Warning = err.Error()
		// The backup could not be filled, so the displaced content is wherever PublishSwap says it
		// is; the report must not claim the backup path holds it, and the displaced bytes must be
		// read from there so the conflict comparison is against the real content.
		var published *crwdir.PublishedError
		if errors.As(err, &published) && published.DisplacedAt != "" {
			result.DisplacedAt = published.DisplacedAt
			if displaced, err = os.ReadFile(published.DisplacedAt); err != nil {
				return result, nil, fmt.Errorf("%w (the content the publication displaced is at %s and could not be read back)", err, published.DisplacedAt)
			}
		}
	}
	if !bytes.Equal(displaced, raw) {
		result.Conflict = true
		// The displaced content is not what retrust read, so a save landed between the last check and
		// the exchange. Read the target again before answering: a second save may have landed right
		// after the publication, and the report must name what config.toml really holds rather than
		// assume retrust's content (CRW-936). A failed re-read is reported as such and makes no claim
		// about the target's content.
		after, recheckErr := os.ReadFile(targetPath)
		switch {
		case recheckErr != nil:
			result.RecheckFailed = true
			result.RecheckError = recheckErr.Error()
			return result, nil, fmt.Errorf("config.toml changed between the last check and the publication: the content that was there is preserved at %s, and %s could not be read again to say what it holds now (%s)", result.displacedPath(), targetPath, recheckErr)
		case !bytes.Equal(after, []byte(next)):
			result.LateWrite = true
			return result, nil, fmt.Errorf("config.toml changed between the last check and the publication: the content that was there is preserved at %s, and %s holds a newer save, not retrust's content", result.displacedPath(), targetPath)
		default:
			return result, nil, fmt.Errorf("config.toml changed between the last check and the publication: the content that was there is preserved at %s and %s holds retrust's content", result.displacedPath(), targetPath)
		}
	}
	after, err := os.ReadFile(targetPath)
	if err != nil {
		return result, nil, err
	}
	if !bytes.Equal(after, []byte(next)) {
		result.LateWrite = true
		return result, nil, fmt.Errorf("%s changed again after retrust published it; the newer content was left in place", targetPath)
	}
	verification, err := DiagnoseHookTrust(codexHome, pluginRoot, pluginKey)
	if err != nil {
		return result, nil, err
	}
	// The hooks can drift between the pre-write verification and this read: a declaration or a hook
	// file that changed in that window makes the published hashes drifted or untrusted, and the
	// oracle's own post-write check failed on exactly that. Reporting it as success would tell the
	// operator a config is trusted when it is not.
	var drifted []string
	for _, item := range verification {
		if item.Status != "trusted" {
			drifted = append(drifted, item.Key)
		}
	}
	if len(drifted) > 0 {
		return result, verification, errors.New("post-publication verification failed for " + strings.Join(drifted, ", ") + "; the hooks changed after the pre-write verification")
	}
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
	// The oracle reads this probe's streams through spawnSync with encoding utf8 (hook-trust.ts:398),
	// so an invalid byte sequence is one U+FFFD per maximal invalid subpart and can never be a lone
	// surrogate. Decode here, at the seam, so every consumer -- the verify detail chain and the CLI
	// failure text a person reads -- sees the text the oracle saw (the same place and rule as
	// harnessRunExec, harness_run.go).
	run := HookTrustRetrustRun{Stdout: source.DecodeUTF8(stdout.Bytes()), Stderr: source.DecodeUTF8(stderr.Bytes())}
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
	// The options are parsed first so the failure closure below can report the config path a refusal
	// raised before the file was resolved implies, even when the parse itself refused.
	options, err := hookTrustRetrustOptions(args, env)
	fail := func(err error, result HookTrustRetrustResult) int {
		// Every refusal prints a report (CRW-936). A refusal that came before the plan has no items to
		// list, so it reports the config path, the reason and the file state instead; the reason is the
		// refusal's own message, which is the only place it is written down.
		if result.ConfigPath == "" {
			result.ConfigPath = hookTrustRetrustReportPath(options.codexHome)
		}
		if result.Reason == "" {
			result.Reason = err.Error()
		}
		hookTrustRetrustReport(stdout, result)
		fmt.Fprintln(stderr, "crw doctor retrust: "+err.Error())
		hookTrustRetrustWarn(stderr, result)
		return 1
	}
	if err != nil {
		return fail(err, HookTrustRetrustResult{ConfigPath: hookTrustRetrustReportPath(options.codexHome)})
	}
	root, err := hookTrustRetrustPluginRoot(pluginRoot, options.pluginRoot, options.codexHome)
	if err != nil {
		return fail(err, HookTrustRetrustResult{ConfigPath: hookTrustRetrustReportPath(options.codexHome)})
	}
	key, err := hookTrustRetrustResolveKey(root, options.pluginKey, options.codexHome)
	if err != nil {
		return fail(err, HookTrustRetrustResult{ConfigPath: hookTrustRetrustReportPath(options.codexHome)})
	}
	result, results, err := HookTrustRetrust(options.codexHome, root, key, options.bootstrapOK, runner, env, now)
	if err != nil {
		return fail(err, result)
	}
	for _, item := range results {
		actual := "(none)"
		if item.Actual != nil {
			actual = *item.Actual
		}
		fmt.Fprintf(stdout, "[%s] %s expected=%s actual=%s\n", item.Status, item.Key, item.Hash, actual)
	}
	fmt.Fprintf(stdout, "updated=%d appended=%d\nbackup: %s\n", result.Updated, result.Appended, result.BackupPath)
	hookTrustRetrustReport(stdout, result)
	hookTrustRetrustWarn(stderr, result)
	return 0
}

// hookTrustRetrustReportPath names the config.toml a refusal concerns before the file is resolved:
// the Codex home's config.toml when the command line carried one, else the empty string the report
// spells "(unknown)". A refusal that never learned a Codex home (an option that failed to parse
// before --codex-home, with no CODEX_HOME and no HOME) must not print a path it guessed.
func hookTrustRetrustReportPath(codexHome string) string {
	if codexHome == "" {
		return ""
	}
	return filepath.Join(codexHome, "config.toml")
}

// hookTrustRetrustWarn prints the post-publication failure the command counted as a warning. It is
// printed on a success and on a failure alike: when a conflict or a late write follows a
// post-exchange failure, the failure detail is the only place the operator learns where the
// displaced content really is, so dropping it would leave stderr pointing at the wrong file.
func hookTrustRetrustWarn(stderr io.Writer, result HookTrustRetrustResult) {
	if result.Warning != "" {
		fmt.Fprintln(stderr, "crw doctor retrust: warning: "+result.Warning)
	}
}

// hookTrustRetrustReport prints, for a success, a refusal and a conflict alike, the items the plan
// changed (or would have changed), the backup path and which file holds what (CRW-844 requirement 8).
// The oracle's own success lines are printed by the caller before this, so this only appends.
//
// CRW-936 B3: every refusal prints. A refusal that came before the plan (the lock is busy, the config
// could not be read, the plugin declares no hooks) has no items to list, so it prints the config path,
// the reason and the file state; a refusal that had a plan but never published prints the planned
// items, the unchanged line and the backup it did not create. The success and conflict lines keep
// their wording and order and the new lines are appended.
func hookTrustRetrustReport(stdout io.Writer, result HookTrustRetrustResult) {
	if !result.Planned {
		path := result.ConfigPath
		if path == "" {
			path = "(unknown)"
		}
		fmt.Fprintf(stdout, "config.toml: %s\n", path)
		fmt.Fprintf(stdout, "no plan: %s\n", result.Reason)
		fmt.Fprintf(stdout, "config.toml unchanged; no backup\n")
		return
	}
	fmt.Fprintf(stdout, "updated keys: %s\n", hookTrustRetrustList(result.UpdatedKeys))
	fmt.Fprintf(stdout, "appended keys: %s\n", hookTrustRetrustList(result.AppendedKeys))
	displaced := result.displacedPath()
	switch {
	case result.Conflict && result.RecheckFailed:
		fmt.Fprintf(stdout, "%s could not be read again to say what it holds now (%s); %s holds the content that was saved in between\n", result.ConfigPath, result.RecheckError, displaced)
	case result.Conflict && result.LateWrite:
		fmt.Fprintf(stdout, "%s holds a newer save, not retrust's config; %s holds the content that was saved in between\n", result.ConfigPath, displaced)
	case result.Conflict:
		fmt.Fprintf(stdout, "%s holds retrust's config; %s holds the content that was saved in between\n", result.ConfigPath, displaced)
	case result.Published && result.LateWrite:
		fmt.Fprintf(stdout, "%s holds the newer save, not retrust's config; %s holds the content retrust displaced\n", result.ConfigPath, displaced)
	case result.Published:
		fmt.Fprintf(stdout, "%s holds the rewritten config; %s holds the content it displaced\n", result.ConfigPath, displaced)
	default:
		fmt.Fprintf(stdout, "%s unchanged; nothing was published\n", result.ConfigPath)
		fmt.Fprintf(stdout, "backup: %s (not created)\n", result.BackupPath)
	}
}

// displacedPath names the file that actually holds the content the publication displaced: the backup
// path, or, when a failure after the exchange stopped the backup being filled, the file PublishSwap
// reports. The report and the conflict error both use it, so neither claims a file holds content it
// does not.
func (r HookTrustRetrustResult) displacedPath() string {
	if r.DisplacedAt != "" {
		return r.DisplacedAt
	}
	return r.BackupPath
}

// hookTrustRetrustList spells the keys of one plan item list, or "(none)".
func hookTrustRetrustList(keys []string) string {
	if len(keys) == 0 {
		return "(none)"
	}
	return strings.Join(keys, ", ")
}
