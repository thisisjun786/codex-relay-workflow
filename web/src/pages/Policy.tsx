// Original CRW screen (no CXC counterpart): the execution-policy screen.
//
// It renders the first screen the issue fixes - the supervisor row (read-only), the parent and child
// pair rows with their source, the allowed list and the exceptions - and drives one pending change
// through a preview and a save. Everything it decides lives in policy-state.ts, which is pure, so
// this file is only the rendering and the two effects that read the routes.
import { useEffect, useRef, useState } from "react";
import {
  POLICY_BLAST_RADIUS,
  catalogNotice,
  decodePolicy,
  modelOptions,
  noticeForWrite,
  policyEfforts,
  policyView,
  previewChange,
  allowedEffortsLabel,
  pairEffortLabel,
  pairModelLabel,
  removeExceptionLabel,
  roleControlsLabel,
  type ModelCatalog,
  type PolicyChange,
  type PolicyExceptionView,
  type PolicyNotice,
  type PolicyPair,
  type PolicyReading,
} from "../policy-state.ts";
import { getModelCatalog, getPolicy, writePolicy } from "../api.ts";
import { Loading } from "../ui/kit.tsx";
import { toast } from "../ui/toast.tsx";
import { HelpDrawer, HelpTopicButton, useHelp } from "../ui/help.tsx";

const PAGE_TITLE = "Execution policy";

/** How often the catalog is re-read, and on window focus as well. */
const CATALOG_REFRESH_MS = 30_000;

/** pairText is one pair as a control row reads it. */
function pairText(pair: PolicyPair): string {
  return `${pair.model} ${pair.reasoningEffort}`.trim();
}

/** exceptionScope is an exception's cwd scope as a row reads it. */
function exceptionScope(exception: PolicyExceptionView): string {
  return exception.cwd.length === 0 ? "no cwd scope" : exception.cwd.join(", ");
}

/** unreachableNotice is the sentence a transport failure becomes. */
function unreachableNotice(): PolicyNotice {
  return {
    tone: "err",
    text: "The backend could not be reached, so the policy was not changed.",
    stored: null,
    registered: null,
    applied: null,
    actions: [],
    errors: [],
    restored: null,
    keepInputs: true,
    reread: false,
    blockEditing: false,
  };
}

export function PolicyPage() {
  const [reading, setReading] = useState<PolicyReading | null>(null);
  const [catalog, setCatalog] = useState<ModelCatalog | null>(null);
  const [change, setChange] = useState<PolicyChange | null>(null);
  const [notice, setNotice] = useState<PolicyNotice | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [reload, setReload] = useState(0);
  const generation = useRef(0);
  const savingRef = useRef(false);
  const catalogGeneration = useRef(0);
  const catalogLoading = useRef(false);
  const { helpOpen, helpTopic, openHelp, closeHelp } = useHelp("policy");

  const view = reading ? policyView(reading) : null;
  const efforts = reading ? policyEfforts(reading, catalog) : [];
  const models = reading ? modelOptions(reading, catalog) : [];

  async function refreshCatalog(force = false) {
    if (catalogLoading.current) return;
    catalogLoading.current = true;
    const current = ++catalogGeneration.current;
    const next = await getModelCatalog(force);
    if (current !== catalogGeneration.current) return;
    setCatalog(next);
    catalogLoading.current = false;
  }

  useEffect(() => {
    void refreshCatalog();
    const onFocus = () => { void refreshCatalog(); };
    window.addEventListener("focus", onFocus);
    const interval = window.setInterval(() => void refreshCatalog(), CATALOG_REFRESH_MS);
    return () => {
      catalogGeneration.current++;
      catalogLoading.current = false;
      window.removeEventListener("focus", onFocus);
      window.clearInterval(interval);
    };
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    const current = ++generation.current;
    setReading(null);
    setError(null);
    void getPolicy(controller.signal)
      .then((body) => {
        if (controller.signal.aborted || current !== generation.current) return;
        // A malformed answer is refused here rather than rendered as a half-populated screen.
        setReading(decodePolicy(body));
      })
      .catch((err) => {
        if (!controller.signal.aborted) setError(err instanceof Error ? err.message : "The execution policy could not be read.");
      });
    return () => {
      controller.abort();
      generation.current++;
    };
  }, [reload]);

  /** propose sets the one pending change and clears the notice of the previous attempt. */
  function propose(next: PolicyChange | null) {
    setChange(next);
    setNotice(null);
  }

  async function save() {
    if (!reading || !change || savingRef.current) return;
    savingRef.current = true;
    setSaving(true);
    setNotice(null);
    let result: PolicyNotice;
    try {
      const answer = await writePolicy({ expectedDigest: reading.digest ?? "", change });
      result = noticeForWrite(answer.status, answer.body);
    } catch {
      result = unreachableNotice();
    }
    savingRef.current = false;
    setSaving(false);
    setNotice(result);
    if (result.tone === "ok") {
      // The file moved, so the reading is stale by definition. Re-read before showing the new state.
      toast("Execution policy saved", "ok");
      setChange(null);
      setReload((n) => n + 1);
      return;
    }
    if (result.reread) {
      // A stale digest: the caller's inputs are kept and the file is read again, exactly as the
      // decided answer says. The pending change survives so the operator can compare and retry.
      setReload((n) => n + 1);
    }
    if (result.blockEditing) setChange(null);
  }

  const editable = (view?.editable ?? false) && !(notice?.blockEditing ?? false);
  const preview = reading && change ? previewChange(reading, change) : null;

  return (
    <>
      <div className="page-header">
        <span className="page-header-title">Execution policy</span>
        <HelpTopicButton topic="policy" onOpen={openHelp} />
      </div>
      <div className="page-head">
        <div>
          <h1>{PAGE_TITLE}</h1>
          <div className="sub">The supervisor, parent and child model and effort pairs, the allowed list and the exceptions, from the policy file the wiring record names.</div>
        </div>
        {view ? <span className={`badge ${view.editable ? "ok" : ""}`}>{view.state}</span> : null}
      </div>
      <div className="page-body">
        {error ? (
          <div role="alert">
            <p>{error}</p>
            <button className="btn" onClick={() => setReload((n) => n + 1)}>Retry</button>
          </div>
        ) : null}
        {!reading && !error ? <Loading label="Loading the execution policy..." /> : null}
        {view ? (
          <>
            <div className="card">
              <h2 className="card-title">Source</h2>
              <p className="card-desc">Every value below is read from this file. Nothing on this screen supplies a default.</p>
              <p className="sub mono" style={{ wordBreak: "break-all" }}>{view.source.path || "(no path)"}</p>
              <p className="sub mono">file {view.source.digest || "-"}</p>
              <p className="sub mono">registered {view.source.registeredDigest || "-"}</p>
              <p className="sub mono">running {view.source.runningDigest ?? `unknown (${view.source.runningReason || "no reason"})`}</p>
              <p className="sub">applied: {view.source.applied}{view.source.mode ? ` · mode ${view.source.mode}` : ""}</p>
              {!view.editable ? (
                <p role="status" className="sub">
                  This host has no registered execution policy this screen can read{view.reason ? `: ${view.reason}` : "."} Editing is blocked until one is registered.
                </p>
              ) : null}
            </div>

            <div className="row-list" style={{ marginTop: 12 }}>
              {view.roles.map((role) => (
                <section key={role.name} className="list-row role-row" aria-label={`${role.name} policy`}>
                  <div className="row-id">
                    <span className="row-name">{role.name}</span>
                    <span className="row-sub">
                      {role.name === "supervisor"
                        ? role.label
                        : role.pairs.length === 0
                          ? "no pair declared"
                          : role.pairs.map(pairText).join(" · ")}
                    </span>
                    {role.expectation ? <span className="row-sub">expectation: {role.expectation}</span> : null}
                  </div>
                  {role.editable && editable ? (
                    <fieldset className="role-controls" disabled={saving} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }} aria-label={roleControlsLabel(role.name)}>
                      {role.pairs.map((pair, index) => (
                        <div className="role-selects" key={index}>
                          <select
                            className="select"
                            style={{ maxWidth: "220px" }}
                            aria-label={pairModelLabel(role.name, index)}
                            value={pair.model}
                            onChange={(e) => {
                              const pairs = role.pairs.map((current, at) => (at === index ? { ...current, model: e.target.value } : current));
                              propose({ kind: "setRolePairs", role: role.name, pairs });
                            }}
                          >
                            {models.some((option) => option.id === pair.model) ? null : (
                              <option value={pair.model}>{pair.model} (saved, unavailable)</option>
                            )}
                            {models.map((option) => (
                              <option key={option.id} value={option.id}>{option.label}</option>
                            ))}
                          </select>
                          <select
                            className="select"
                            style={{ maxWidth: "160px" }}
                            aria-label={pairEffortLabel(role.name, index)}
                            value={pair.reasoningEffort}
                            onChange={(e) => {
                              const pairs = role.pairs.map((current, at) => (at === index ? { ...current, reasoningEffort: e.target.value } : current));
                              propose({ kind: "setRolePairs", role: role.name, pairs });
                            }}
                          >
                            {efforts.includes(pair.reasoningEffort) ? null : (
                              <option value={pair.reasoningEffort}>{pair.reasoningEffort} (saved, unavailable)</option>
                            )}
                            {efforts.map((effort) => (
                              <option key={effort} value={effort}>{effort}</option>
                            ))}
                          </select>
                          {role.pairs.length > 1 ? (
                            <button
                              className="btn"
                              onClick={() => propose({ kind: "setRolePairs", role: role.name, pairs: role.pairs.filter((_, at) => at !== index) })}
                            >
                              Remove pair
                            </button>
                          ) : null}
                        </div>
                      ))}
                      <p className="sub">A role runs on one of the pairs listed here; the file is the only source of them.</p>
                    </fieldset>
                  ) : (
                    <span className="badge" role="status">{role.name === "supervisor" ? "read-only" : "not editable"}</span>
                  )}
                </section>
              ))}
            </div>

            <div className="row-list" style={{ marginTop: 12 }}>
              {view.allowed.length === 0 ? (
                <div className="list-row"><span className="row-sub">This policy declares no allowed list (mode {view.source.mode || "presence_only"}).</span></div>
              ) : null}
              {view.allowed.map((entry) => (
                <section key={entry.model} className="list-row" aria-label={`allowed ${entry.model}`}>
                  <div className="row-id">
                    <span className="row-name">{entry.model}</span>
                    <span className="row-sub">{entry.efforts.join(" · ") || "no effort listed"}</span>
                  </div>
                  {editable ? (
                    <div className="row-actions">
                      <label className="sub" htmlFor={`allowed-${entry.model}`}>efforts</label>
                      <input
                        id={`allowed-${entry.model}`}
                        className="input"
                        style={{ maxWidth: "220px" }}
                        aria-label={allowedEffortsLabel(entry.model)}
                        defaultValue={entry.efforts.join(", ")}
                        onBlur={(e) => {
                          const next = e.target.value.split(",").map((name) => name.trim()).filter((name) => name !== "");
                          if (next.length === 0) return;
                          propose({ kind: "setAllowed", model: entry.model, efforts: next });
                        }}
                      />
                      <button className="btn" onClick={() => propose({ kind: "removeAllowed", model: entry.model })}>Remove</button>
                    </div>
                  ) : null}
                </section>
              ))}
            </div>

            <div className="row-list" style={{ marginTop: 12 }}>
              {view.exceptions.length === 0 ? (
                <div className="list-row"><span className="row-sub">No exception is declared, so every scope runs on its role's pair.</span></div>
              ) : null}
              {view.exceptions.map((exception) => (
                <section key={exception.id} className="list-row" aria-label={`exception ${exception.id}`}>
                  <div className="row-id">
                    <span className="row-name">{exception.id}</span>
                    <span className="row-sub">{exception.role || "any role"} · {exception.model} · {exception.reasoningEffort} · {exceptionScope(exception)}</span>
                  </div>
                  {editable ? (
                    <div className="row-actions">
                      <button
                        className="btn danger"
                        onClick={() => propose({ kind: "removeException", id: exception.id })}
                        aria-label={removeExceptionLabel(exception.id)}
                      >
                        Remove
                      </button>
                    </div>
                  ) : null}
                </section>
              ))}
            </div>

            {preview ? (
              <div className="card" style={{ marginTop: 12 }}>
                <h2 className="card-title">Preview before saving</h2>
                <table className="table compact">
                  <thead>
                    <tr><th>Item</th><th>Before</th><th>After</th></tr>
                  </thead>
                  <tbody>
                    {preview.items.map((item) => (
                      <tr key={item.label}>
                        <td>{item.label}</td>
                        <td className="mono">{item.before}</td>
                        <td className="mono">{item.after}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {preview.fallback ? <p className="sub" role="status">{preview.fallback}</p> : null}
                <p className="sub" role="status">{POLICY_BLAST_RADIUS}</p>
                <div className="modal-foot">
                  <button className="btn" disabled={saving} onClick={() => propose(null)}>Cancel</button>
                  <button className="btn primary" disabled={saving} onClick={() => void save()}>
                    {saving ? "Saving..." : "Save"}
                  </button>
                </div>
              </div>
            ) : null}

            {notice ? (
              <div className="card" role="status" style={{ marginTop: 12 }}>
                <h2 className="card-title">{notice.tone === "ok" ? "Saved" : "Not saved"}</h2>
                <p className="sub">{notice.text}</p>
                {notice.stored ? <p className="sub mono">stored {notice.stored}</p> : null}
                {notice.registered ? <p className="sub mono">registered {notice.registered}</p> : null}
                {notice.applied ? <p className="sub mono">applied {notice.applied}</p> : null}
                {notice.actions.map((action) => (
                  <p className="sub" key={action}>To do: {action}</p>
                ))}
                {notice.errors.map((message) => (
                  <p className="sub" key={message}>{message}</p>
                ))}
                {notice.blockEditing ? <p className="sub">Editing is blocked until the recovery above is done.</p> : null}
              </div>
            ) : null}

            <p className="sub" style={{ marginTop: 12 }}>{catalogNotice(catalog)}</p>
            <div className="role-selects">
              <button className="btn" disabled={saving} onClick={() => { setChange(null); setReload((n) => n + 1); }}>Read the policy again</button>
            </div>
          </>
        ) : null}
      </div>
      <HelpDrawer open={helpOpen} topic={helpTopic} onClose={closeHelp} />
    </>
  );
}
