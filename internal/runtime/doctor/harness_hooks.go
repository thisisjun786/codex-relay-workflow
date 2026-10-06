// Harness hook checks, ported from CXC v0.2.40 cxc-ops/src/doctor.ts (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d): runHookExecutionCheck (:448-467) and runHookTrustCheck
// (:468-542). The execution check reports whether this plugin's hooks left invocation records for a session
// (harness.ReadHookObservations); the trust check reports whether Codex's config.toml trusts the hash of
// each hook the plugin declares (ListHookTrustEntries, DiagnoseHookTrust, ReadInstalledPluginKeys). Neither
// writes: the trust check reads config.toml only, and the retrust write is a later port. The command that
// assembles them into a report belongs to its own port issue.
//
// The evidence is the oracle text. Two names change with the CLI table (decision 1,
// contract/schema/cxc/name-substitution.json): the repair command `cxc hooks retrust` is
// `crw doctor retrust`, and the entrypoint a record names is the manifest the Go writer names. The environment
// (CODEX_THREAD_ID, CODEX_HOME, HOME) and the clock are arguments, so a test supplies them. What differs from
// the oracle, and why, is in docs/port-cxc/known-defects.md (section: the hook observation reader and
// harness hook checks port).
package doctor

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// harnessHooksCodexHome is options.codexHome ?? process.env.CODEX_HOME ?? join(homedir(), ".codex"). An
// empty CodexHome counts as unset: HarnessOptions has no way to tell it from an empty string the oracle keeps.
func harnessHooksCodexHome(options HarnessOptions, env host.LookupEnv) (string, error) {
	if options.CodexHome != "" {
		return options.CodexHome, nil
	}
	if home, set := env("CODEX_HOME"); set {
		return home, nil
	}
	home, err := host.Home(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// HarnessHookExecutionCheck is runHookExecutionCheck (doctor.ts:448-467): an invocation is a diagnostic
// fact; neither trust nor enforcement follows from it. The session is options.SessionID, else
// CODEX_THREAD_ID; the actor is options.AgentID, nil for the root; the freshness window is the options', else
// now and a day.
func HarnessHookExecutionCheck(pluginRoot string, options HarnessOptions, env host.LookupEnv, now time.Time) HarnessCheck {
	session := ""
	if options.SessionID != nil {
		session = *options.SessionID
	} else {
		session, _ = env("CODEX_THREAD_ID")
	}
	query := harness.ObservationQuery{PluginRoot: pluginRoot, SessionID: session, AgentID: options.AgentID, NowMS: now.UnixMilli(), MaxAgeMS: harness.HookObservationMaxAgeMS}
	if options.ObservationNow != nil {
		query.NowMS = *options.ObservationNow
	}
	if options.ObservationMaxAgeMS != nil {
		query.MaxAgeMS = *options.ObservationMaxAgeMS
	}
	result := harness.HookObservations{Reason: "invocation store unreadable"}
	if home, err := harnessHooksCodexHome(options, env); err == nil {
		query.CodexHome = home
		result = harness.ReadHookObservations(query)
	}
	observed := make([]string, 0, len(result.Observations))
	for _, record := range result.Observations {
		observed = append(observed, fmt.Sprintf("%s/%s (%s, %s)", record.Component, record.Event, record.Entrypoint, record.ObservedAt))
	}
	identity := "native session unavailable"
	if session != "" {
		actor := "root"
		if options.AgentID != nil {
			actor = *options.AgentID
		}
		identity = "session=" + session + " actor=" + actor
	}
	evidence := fmt.Sprintf("%s: %d current invocation(s)", identity, len(observed))
	if len(observed) > 0 {
		evidence += ": " + strings.Join(observed, "; ")
	} else if result.Reason != "" {
		evidence += "; unverified (" + result.Reason + ")"
	} else {
		evidence += "; unverified (no matching fresh evidence)"
	}
	evidence += fmt.Sprintf("; %d ignored record(s). Declaration coverage and handler results remain unknown. ", result.Ignored) +
		"Same-user writable/replayable diagnostics, not host attestations or proof of enforcement."
	check := HarnessCheck{Name: "hook-execution", Severity: HarnessWarn, Evidence: evidence}
	if len(observed) > 0 {
		check.Severity = HarnessPass
	}
	return check
}

// HarnessHookTrustCheck is runHookTrustCheck (doctor.ts:468-542). Drift and absence are different facts with
// the same consequence: the host classifies an absent hash as Untrusted and a mismatched one as Modified and
// excludes both from execution unless hook trust is bypassed, so both fail and only the repair differs. What
// it cannot see is whether the host executed a hook or bypasses trust: it reports the stored record. The
// manifest is read through the same containment as the hook listing, and an error of any step is the
// evidence of a FAIL in Go's words where the oracle's are its engine's.
func HarnessHookTrustCheck(pluginRoot string, options HarnessOptions, env host.LookupEnv) HarnessCheck {
	failed := func(evidence string) HarnessCheck {
		return HarnessCheck{Name: "hook-trust", Severity: HarnessFail, Evidence: evidence}
	}
	warned := func(evidence string) HarnessCheck {
		return HarnessCheck{Name: "hook-trust", Severity: HarnessWarn, Evidence: evidence}
	}
	codexHome, err := harnessHooksCodexHome(options, env)
	if err != nil {
		return failed(err.Error())
	}
	manifest, err := hookTrustEntriesReadContained(pluginRoot, ".codex-plugin/plugin.json", nil)
	if err != nil {
		return failed(err.Error())
	}
	document, err := hookTrustEntriesParse(manifest)
	if err != nil {
		return failed(err.Error())
	}
	if document == nil {
		return failed("Cannot read properties of null (reading 'name')")
	}
	object, _ := document.(pyjson.Object)
	name, _ := object.Get("name").(string)
	if name == "" {
		return warned("manifest has no plugin name; cannot resolve install key")
	}
	candidates, err := ReadInstalledPluginKeys(codexHome, name)
	if err != nil {
		return failed(err.Error())
	}
	key := options.PluginKey
	if key == "" && len(candidates) == 1 {
		key = candidates[0]
	}
	if key == "" {
		listed := "(none)"
		if len(candidates) > 0 {
			listed = strings.Join(candidates, ", ")
		}
		return warned(fmt.Sprintf("enabled install key is ambiguous (%d): %s", len(candidates), listed))
	}
	results, err := DiagnoseHookTrust(codexHome, pluginRoot, key)
	if err != nil {
		return failed(err.Error())
	}
	var untrusted []HookTrustResult
	neverTrusted := 0
	for _, result := range results {
		if result.Status != "trusted" {
			untrusted = append(untrusted, result)
			if result.Actual == nil {
				neverTrusted++
			}
		}
	}
	check := HarnessCheck{Name: "hook-trust", Severity: HarnessPass, Evidence: fmt.Sprintf("%d hook hash(es) trusted for %s", len(results), key)}
	switch {
	case len(results) == 0:
		// An empty set is not a pass: a handler that cannot be hashed is skipped, so nothing was verified.
		check.Severity, check.Evidence = HarnessWarn, "no hook handler could be hashed for "+key+"; nothing was verified"
	case len(untrusted) > 0:
		detail := make([]string, 0, len(untrusted))
		for _, result := range untrusted {
			actual := "(none)"
			if result.Actual != nil {
				actual = *result.Actual
			}
			detail = append(detail, fmt.Sprintf("%s %s expected=%s actual=%s file_sha256=%s", result.Status, result.Key, result.Hash, actual, result.FileSha256[:16]))
		}
		check.Severity = HarnessFail
		check.Evidence = fmt.Sprintf("%d of %d hook(s) not trusted for %s: %d with no trust record, %d whose recorded hash no longer matches. "+
			"The host excludes both from execution unless hook trust is bypassed, so these need approval. Execution itself was not verified here. %s",
			len(untrusted), len(results), key, neverTrusted, len(untrusted)-neverTrusted, strings.Join(detail, "; "))
		// A fresh install has no [hooks.state.*] section: only the host writes them, and nothing here forges them.
		repair := fmt.Sprintf("crw doctor retrust --key %s --codex-home %s", key, codexHome)
		if neverTrusted == len(untrusted) {
			repair = fmt.Sprintf("%d hook(s) have no trust entry in %s; only Codex itself writes those on hook approval. Approve this plugin's hooks in Codex, or record them explicitly with: %s --bootstrap-ok",
				len(untrusted), filepath.Join(codexHome, "config.toml"), repair)
		}
		check.Repair = harnessReportRepair(repair)
	}
	return check
}
