// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/pages/Subagents.tsx (1-154), modified: the
// project scope is gone (the CRW store is global only), so the scope prop, the trust warning, the
// inherited state and the "Global settings" option have no counterpart; the title is the issue's
// own; and the effort names come from the catalog and the execution policy instead of the fixed
// CXC spawn enum.
import { useEffect, useRef, useState } from "react";
import {
  HELPER_ROLES,
  effortSelectable,
  getHelperRoleSettings,
  getModelCatalog,
  getPolicyEffortNames,
  helperRoleEfforts,
  setHelperRole,
  type HelperRole,
  type HelperRolePatch,
  type HelperRoleSettings,
  type ModelCatalog,
} from "../api.ts";
import { ModelSelect } from "../components/ModelSelect.tsx";
import { PromptOverrideEditor } from "../components/PromptOverrideEditor.tsx";
import { effortExcluded } from "../effort-support.ts";
import { Loading } from "../ui/kit.tsx";
import { toast } from "../ui/toast.tsx";
import { HelpDrawer, HelpTopicButton, useHelp } from "../ui/help.tsx";

/** The exact title the issue names for this screen. */
const PAGE_TITLE = "Helper roles (inside a child's loop)";

const ROLE_DESC: Record<HelperRole, string> = {
  explorer: "Read-only search and codebase mapping.",
  reviewer: "Independent verification and audits.",
  executor: "Implementation and mutation work.",
  architect: "Design proposals and plan alignment checks.",
};

/** Where a role's value comes from. There is no project layer here, so there are two sources. */
const SOURCE_LABEL: Record<"global" | "session", string> = {
  global: "Global defaults",
  session: "Original session",
};

/** How often the catalog is re-read, and on window focus as well. */
const CATALOG_REFRESH_MS = 30_000;

export function HelperRolesPage() {
  const [config, setConfig] = useState<HelperRoleSettings | null>(null);
  const [catalog, setCatalog] = useState<ModelCatalog | null>(null);
  const [policyEfforts, setPolicyEfforts] = useState<string[]>([]);
  const [refreshing, setRefreshing] = useState(false);
  const [savingRole, setSavingRole] = useState<HelperRole | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [reload, setReload] = useState(0);
  // The prompt draft is string-or-null, not a string: null is the store's "inherit" and the empty
  // string is a value the user stored, and collapsing the two here would lose that distinction
  // before the write even leaves the screen.
  const [prompts, setPrompts] = useState<Record<HelperRole, string | null>>({ explorer: null, reviewer: null, executor: null, architect: null });
  const saving = useRef(false);
  const generation = useRef(0);
  const catalogLoading = useRef(false);
  const catalogGeneration = useRef(0);
  const { helpOpen, helpTopic, openHelp, closeHelp } = useHelp("helper-roles");

  const entries = catalog?.entries ?? [];
  const efforts = helperRoleEfforts(entries, policyEfforts);
  const dirty = (role: HelperRole) => prompts[role] !== (config?.roles[role].promptOverride ?? null);

  async function refreshCatalog(force = false) {
    if (catalogLoading.current) return;
    catalogLoading.current = true;
    setRefreshing(true);
    // A refresh can outlive the visit that started it. The generation marks the run this screen is
    // waiting for, so a response from an earlier visit cannot repaint the new one.
    const current = ++catalogGeneration.current;
    const [next, names] = await Promise.all([getModelCatalog(force), getPolicyEffortNames()]);
    if (current !== catalogGeneration.current) return;
    setCatalog(next);
    setPolicyEfforts(names);
    catalogLoading.current = false;
    setRefreshing(false);
  }

  useEffect(() => {
    void refreshCatalog();
    const onFocus = () => { void refreshCatalog(); };
    window.addEventListener("focus", onFocus);
    const interval = window.setInterval(() => void refreshCatalog(), CATALOG_REFRESH_MS);
    return () => {
      // Abandon any in-flight refresh with the visit, so a late response is dropped rather than
      // applied to the next one.
      catalogGeneration.current++;
      catalogLoading.current = false;
      window.removeEventListener("focus", onFocus);
      window.clearInterval(interval);
    };
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    const current = ++generation.current;
    setConfig(null);
    setError(null);
    void getHelperRoleSettings(controller.signal)
      .then((next) => {
        if (controller.signal.aborted || current !== generation.current) return;
        setConfig(next);
        setPrompts({
          explorer: next.roles.explorer.promptOverride,
          reviewer: next.roles.reviewer.promptOverride,
          executor: next.roles.executor.promptOverride,
          architect: next.roles.architect.promptOverride,
        });
      })
      .catch((err) => {
        if (!controller.signal.aborted) setError(err instanceof Error ? err.message : "Settings could not be loaded.");
      });
    return () => {
      controller.abort();
      generation.current++;
    };
  }, [reload]);

  /** The catalog's advertised ladder for a model: an array, or null when it reports none. */
  function ladderFor(model: string | null | undefined): readonly string[] | null {
    if (!model) return null;
    return entries.find((entry) => entry.id === model)?.reasoningEfforts ?? null;
  }

  async function save(role: HelperRole, patch: HelperRolePatch) {
    if (!config || saving.current) return;
    if (patch.mode === "model" && patch.model && config.roles[role].effort !== null) {
      // Only an advertised ladder can refuse a model. A missing catalog entry or an unreported
      // ladder both arrive as null here, and neither is evidence that the model rejects the effort.
      if (effortExcluded(ladderFor(patch.model), config.roles[role].effort)) {
        setError("This model does not advertise the saved effort. Choose the session effort first, then pick the model.");
        return;
      }
    }
    if (patch.fallback?.effort && effortExcluded(ladderFor(patch.fallback.model), patch.fallback.effort)) {
      setError("The fallback model does not advertise this effort. Choose the session effort first.");
      return;
    }
    saving.current = true;
    setSavingRole(role);
    setError(null);
    const current = generation.current;
    const result = await setHelperRole(role, patch, config);
    saving.current = false;
    if (current !== generation.current) return;
    setSavingRole(null);
    if (!result.ok) {
      setError(result.error ?? `${role} save failed`);
      return;
    }
    setConfig(result.config);
    if (patch.inherit || patch.promptOverride !== undefined) {
      setPrompts((previous) => ({ ...previous, [role]: result.config.roles[role].promptOverride ?? "" }));
    }
    toast(`${role} ${patch.inherit ? "now inherits defaults" : "updated"}`, "ok");
  }

  const freshness = catalog
    ? catalog.status === "fresh"
      ? `Model list from ${catalog.source === "ocx" ? "OCX" : "the Codex catalog"}`
      : catalog.status === "stale"
        ? "Last known model list - refresh failed"
        : "Model list unavailable"
    : "Model list unavailable";

  return (
    <>
      <div className="page-header">
        <span className="page-header-title">Helper roles</span>
        <HelpTopicButton topic="helper-roles" onOpen={openHelp} />
      </div>
      <div className="page-head">
        <div>
          <h1>{PAGE_TITLE}</h1>
          <div className="sub">The global explorer, reviewer, executor and architect settings every child's loop uses.</div>
        </div>
        <span className="badge accent">{entries.length} models</span>
      </div>
      <div className="page-body">
        <div className="role-selects" style={{ alignItems: "center", marginBottom: 12 }}>
          <span className="sub">{freshness}</span>
          <button className="btn" disabled={refreshing} onClick={() => void refreshCatalog(true)}>
            {refreshing ? "Refreshing..." : "Refresh models"}
          </button>
        </div>
        {catalog?.message ? <p className="sub" role="status">{catalog.message}</p> : null}
        {catalog?.status === "fresh" && entries.length === 0 ? (
          <p className="sub">No models are enabled. Enable models in the catalog source, then refresh.</p>
        ) : null}
        <p className="sub">
          Main model keeps the original session's model. Session effort follows the session's effort; pick a level to
          override it for this role only.
        </p>
        {error ? (
          <div role="alert">
            <p>{error}</p>
            {!config ? <button className="btn" onClick={() => setReload((n) => n + 1)}>Retry</button> : null}
          </div>
        ) : null}
        {!config ? (
          !error ? <Loading label="Loading helper role settings..." /> : null
        ) : (
          <div className="row-list">
            {HELPER_ROLES.map((role) => {
              const r = config.roles[role];
              const source = config.sources[role];
              const effectiveModel = r.mode === "model" ? r.model : null;
              const supported = r.mode === "model" ? ladderFor(r.model) : undefined;
              const unsupported = r.effort !== null && effortExcluded(supported, r.effort);
              const fallbackSupported = r.fallback ? ladderFor(r.fallback.model) : undefined;
              const savedEffortMissing = r.effort !== null && !efforts.includes(r.effort);
              return (
                <section key={role} className="list-row role-row" aria-label={`${role} settings`}>
                  <div className="row-id">
                    <span className="row-name">{role.charAt(0).toUpperCase() + role.slice(1)}</span>
                    <span className="row-sub">{ROLE_DESC[role]}</span>
                    <span className="row-sub">
                      {SOURCE_LABEL[source]} · {r.model ?? "main model"} · {r.effort ?? "session effort"}
                    </span>
                  </div>
                  <fieldset className="role-controls" disabled={savingRole !== null} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }} aria-label={`${role} controls`}>
                    <div className="role-selects">
                      <ModelSelect
                        value={effectiveModel}
                        disabled={savingRole !== null}
                        entries={entries}
                        label={`${role} model`}
                        onChange={(model) => void save(role, { mode: model ? "model" : "default", model })}
                      />
                      <select
                        className="select"
                        style={{ maxWidth: "200px" }}
                        disabled={savingRole !== null}
                        value={r.effort ?? ""}
                        aria-label={`${role} reasoning effort`}
                        onChange={(e) => void save(role, { effort: e.target.value === "" ? null : e.target.value })}
                      >
                        <option value="">session effort</option>
                        {savedEffortMissing ? <option value={r.effort ?? ""}>{r.effort} (saved, not offered)</option> : null}
                        {efforts.map((effort) => (
                          <option key={effort} value={effort} disabled={effortExcluded(supported, effort) || !effortSelectable(effort)}>
                            {effortSelectable(effort) ? effort : `${effort} (not accepted by the helper-role store)`}
                          </option>
                        ))}
                      </select>
                    </div>
                    <div className="role-selects" style={{ marginTop: 8 }}>
                      <span className="sub">First fallback</span>
                      <ModelSelect
                        label={`${role} fallback model`}
                        emptyLabel="No fallback"
                        value={r.fallback?.model ?? null}
                        disabled={savingRole !== null}
                        entries={effectiveModel ? entries.filter((entry) => entry.id !== effectiveModel) : entries}
                        onChange={(model) => void save(role, { fallback: model ? { model, effort: r.fallback?.effort ?? null } : null })}
                      />
                      <select
                        className="select"
                        style={{ maxWidth: "200px" }}
                        disabled={savingRole !== null || !r.fallback}
                        value={r.fallback?.effort ?? ""}
                        aria-label={`${role} fallback effort`}
                        onChange={(e) =>
                          r.fallback && void save(role, { fallback: { ...r.fallback, effort: e.target.value === "" ? null : e.target.value } })
                        }
                      >
                        <option value="">session effort</option>
                        {efforts.map((effort) => (
                          <option key={effort} value={effort} disabled={effortExcluded(fallbackSupported, effort) || !effortSelectable(effort)}>
                            {effortSelectable(effort) ? effort : `${effort} (not accepted by the helper-role store)`}
                          </option>
                        ))}
                      </select>
                    </div>
                    <p className="sub">After attempts fail, the main agent takes over remaining work.</p>
                    {unsupported ? (
                      <p className="sub" role="status">
                        Saved effort {r.effort} is not advertised by this model. Choose the session effort or another level.
                      </p>
                    ) : null}
                    <PromptOverrideEditor
                      value={prompts[role]}
                      disabled={savingRole !== null}
                      label={`${role} prompt override`}
                      onChange={(next) => setPrompts((previous) => ({ ...previous, [role]: next }))}
                    />
                    <div className="role-selects">
                      {dirty(role) ? (
                        <>
                          <button className="btn" disabled={savingRole !== null} onClick={() => void save(role, { promptOverride: prompts[role] })}>
                            Save prompt
                          </button>
                          <button className="btn" onClick={() => setPrompts((previous) => ({ ...previous, [role]: r.promptOverride }))}>
                            Discard prompt changes
                          </button>
                        </>
                      ) : null}
                      <button className="btn" disabled={savingRole !== null || !config.overrides[role]} onClick={() => void save(role, { inherit: true })}>
                        Reset to inherited defaults
                      </button>
                    </div>
                  </fieldset>
                  {savingRole === role ? <span className="badge" role="status">saving...</span> : null}
                </section>
              );
            })}
          </div>
        )}
      </div>
      <HelpDrawer open={helpOpen} topic={helpTopic} onClose={closeHelp} />
    </>
  );
}
