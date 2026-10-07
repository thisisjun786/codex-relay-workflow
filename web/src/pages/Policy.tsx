// Original CRW screen (no CXC counterpart): the execution-policy screen.
//
// It renders the first screen the issue fixes - the supervisor row (read-only), the parent and child
// pair rows with their source, the allowed list and the exceptions - and drives one pending change
// through a preview and a save. Everything it decides lives in policy-state.ts, which is pure, so
// this file is only the rendering and the effects that read the routes.
//
// One rule shapes the controls: a select or input shows the PENDING change where there is one for
// its item, and the saved reading where there is not. Binding them to the saved reading instead
// would make an edit snap back on screen, and would rebuild each edit from the saved value, so two
// edits to the same item could not be composed.
import { useEffect, useRef, useState } from "react";
import {
  POLICY_BLAST_RADIUS,
  allowedEffortsLabel,
  catalogNotice,
  checkNotice,
  decodeCheck,
  decodePolicy,
  lostWriteNotice,
  modelOptions,
  noticeForWrite,
  pairEffortLabel,
  pairModelLabel,
  policyEfforts,
  policyView,
  previewChange,
  removeExceptionLabel,
  roleControlsLabel,
  type ModelCatalog,
  type PolicyChange,
  type PolicyExceptionView,
  type PolicyNotice,
  type PolicyPair,
  type PolicyReading,
} from "../policy-state.ts";
import { checkPolicy, getModelCatalog, getPolicy, writePolicy } from "../api.ts";
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

/** effortDraft is the allowed-efforts input's text for one model, from the pending change first. */
function effortDraft(change: PolicyChange | null, model: string, saved: string[]): string {
  if (change?.kind === "setAllowed" && change.model === model) return change.efforts.join(", ");
  return saved.join(", ");
}

/** unreachableNotice is the sentence a read or a check that could not be answered becomes. */
function unreachableNotice(): PolicyNotice {
  return {
    tone: "err",
    text: "The backend could not be reached, so the policy was not changed.",
    stored: null,
    registered: null,
    applied: null,
    actions: [],
    errors: [],
    warnings: [],
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

  /** reread drops the pending change and reads the policy again. */
  function reread() {
    setChange(null);
    setNotice(null);
    setReload((n) => n + 1);
  }

  async function save() {
    if (!reading || !change || savingRef.current) return;
    savingRef.current = true;
    setSaving(true);
    setNotice(null);
    // Check first: the server judges the change with the bridge's own parser, so a refusal here is a
    // refusal the write would meet. Sending only a change the check accepted keeps the two answers
    // from disagreeing, and a refusal is shown without touching the file.
    let checked;
    try {
      checked = decodeCheck((await checkPolicy({ expectedDigest: reading.digest ?? "", change })).body);
    } catch {
      // The check itself could not be answered. Nothing was written, so this is a plain failure.
      savingRef.current = false;
      setSaving(false);
      setNotice(unreachableNotice());
      return;
    }
    if (!checked.valid) {
      const refused = checkNotice(checked);
      savingRef.current = false;
      setSaving(false);
      setNotice(refused);
      if (refused.reread) setReload((n) => n + 1);
      return;
    }
    let result: PolicyNotice;
    try {
      const answer = await writePolicy({ expectedDigest: reading.digest ?? "", change });
      result = noticeForWrite(answer.status, answer.body);
    } catch {
      // A lost response is not a lost write: the server finishes registration after it has replaced
      // the file, so the screen re-reads rather than claiming nothing changed.
      result = lostWriteNotice();
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
      // A stale digest or a lost response: the caller's inputs are kept and the file is read again.
      setReload((n) => n + 1);
    }
    if (result.blockEditing) setChange(null);
  }

  const editable = (view?.editable ?? false) && !(notice?.blockEditing ?? false);
  const preview = reading && change ? previewChange(reading, change) : null;

  /** pairsFor is the pair list a role's controls show: the pending change first, then the reading. */
  function pairsFor(name: string, saved: PolicyPair[]): PolicyPair[] {
    if (change?.kind === "setRolePairs" && change.role === name) return change.pairs;
    return saved;
  }

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
              {view.source.actions.map((action) => (
                <p className="sub" role="status" key={action}>To apply it: {action}</p>
              ))}
              {!view.editable ? (
                <p role="status" className="sub">
                  This host has no registered execution policy this screen can read{view.reason ? `: ${view.reason}` : "."} Editing is blocked until one is registered.
                </p>
              ) : null}
            </div>

            <div className="row-list" style={{ marginTop: 12 }}>
              {view.roles.map((role) => {
                const pairs = pairsFor(role.name, role.pairs);
                return (
                  <section key={role.name} className="list-row role-row" aria-label={`${role.name} policy`}>
                    <div className="row-id">
                      <span className="row-name">{role.name}</span>
                      <span className="row-sub">
                        {role.name === "supervisor"
                          ? role.label
                          : pairs.length === 0
                            ? "no pair declared"
                            : pairs.map(pairText).join(" · ")}
                      </span>
                      {role.expectation ? <span className="row-sub">expectation: {role.expectation}</span> : null}
                    </div>
                    {role.editable && editable ? (
                      <fieldset className="role-controls" disabled={saving} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }} aria-label={roleControlsLabel(role.name)}>
                        {pairs.map((pair, index) => (
                          <div className="role-selects" key={index}>
                            <select
                              className="select"
                              style={{ maxWidth: "220px" }}
                              aria-label={pairModelLabel(role.name, index)}
                              value={pair.model}
                              onChange={(e) => {
                                const next = pairs.map((current, at) => (at === index ? { ...current, model: e.target.value } : current));
                                propose({ kind: "setRolePairs", role: role.name, pairs: next });
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
                                const next = pairs.map((current, at) => (at === index ? { ...current, reasoningEffort: e.target.value } : current));
                                propose({ kind: "setRolePairs", role: role.name, pairs: next });
                              }}
                            >
                              {efforts.includes(pair.reasoningEffort) ? null : (
                                <option value={pair.reasoningEffort}>{pair.reasoningEffort} (saved, unavailable)</option>
                              )}
                              {efforts.map((effort) => (
                                <option key={effort} value={effort}>{effort}</option>
                              ))}
                            </select>
                            {pairs.length > 1 ? (
                              <button className="btn" onClick={() => propose({ kind: "setRolePairs", role: role.name, pairs: pairs.filter((_, at) => at !== index) })}>
                                Remove pair
                              </button>
                            ) : null}
                          </div>
                        ))}
                        <div className="role-selects">
                          <button
                            className="btn"
                            onClick={() => propose({ kind: "setRolePairs", role: role.name, pairs: [...pairs, { model: models[0]?.id ?? "", reasoningEffort: efforts[0] ?? "" }] })}
                          >
                            Add pair
                          </button>
                        </div>
                        <p className="sub">A role runs on one of the pairs listed here; the file is the only source of them.</p>
                      </fieldset>
                    ) : (
                      <span className="badge" role="status">{role.name === "supervisor" ? "read-only" : "not editable"}</span>
                    )}
                  </section>
                );
              })}
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
                        value={effortDraft(change, entry.model, entry.efforts)}
                        onChange={(e) => {
                          const next = e.target.value.split(",").map((name) => name.trim()).filter((name) => name !== "");
                          // An empty list is not a change: the backend refuses it, and clearing the
                          // field mid-edit must not raise a preview for a change nobody asked for.
                          propose(next.length === 0 ? null : { kind: "setAllowed", model: entry.model, efforts: next });
                        }}
                      />
                      <button className="btn" onClick={() => propose({ kind: "removeAllowed", model: entry.model })}>Remove</button>
                    </div>
                  ) : null}
                </section>
              ))}
              {editable ? (
                <AllowedAdder
                  models={models}
                  efforts={efforts}
                  existing={view.allowed.map((entry) => entry.model)}
                  onAdd={(model, names) => propose({ kind: "setAllowed", model, efforts: names })}
                />
              ) : null}
            </div>

            <div className="row-list" style={{ marginTop: 12 }}>
              {view.exceptions.length === 0 ? (
                <div className="list-row"><span className="row-sub">No exception is declared, so every scope runs on its role's pair.</span></div>
              ) : null}
              {view.exceptions.map((exception) => (
                <ExceptionRow
                  key={exception.id}
                  exception={exception}
                  roles={view.roles.filter((role) => role.editable).map((role) => role.name)}
                  models={models}
                  efforts={efforts}
                  editable={editable}
                  onChange={propose}
                />
              ))}
              {editable ? <ExceptionAdder roles={view.roles.filter((role) => role.editable).map((role) => role.name)} models={models} efforts={efforts} onAdd={propose} /> : null}
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
                {notice.warnings.map((warning) => (
                  <p className="sub" key={warning}>Warning: {warning}</p>
                ))}
                {notice.errors.map((message) => (
                  <p className="sub" key={message}>{message}</p>
                ))}
                {notice.blockEditing ? <p className="sub">Editing is blocked until the recovery above is done.</p> : null}
              </div>
            ) : null}

            <p className="sub" style={{ marginTop: 12 }}>{catalogNotice(catalog)}</p>
            <div className="role-selects">
              <button className="btn" disabled={saving} onClick={reread}>Read the policy again</button>
            </div>
          </>
        ) : null}
      </div>
      <HelpDrawer open={helpOpen} topic={helpTopic} onClose={closeHelp} />
    </>
  );
}

/** The control that adds one model to the allowed list. */
function AllowedAdder({ models, efforts, existing, onAdd }: { models: Array<{ id: string }>; efforts: string[]; existing: string[]; onAdd: (model: string, names: string[]) => void }) {
  const free = models.filter((option) => !existing.includes(option.id));
  const [model, setModel] = useState(free[0]?.id ?? "");
  if (free.length === 0) return null;
  return (
    <div className="list-row">
      <div className="row-id"><span className="row-sub">Add a model to the allowed list</span></div>
      <div className="row-actions">
        <select className="select" style={{ maxWidth: "220px" }} aria-label="model to allow" value={model} onChange={(e) => setModel(e.target.value)}>
          {free.map((option) => (
            <option key={option.id} value={option.id}>{option.id}</option>
          ))}
        </select>
        <button className="btn" aria-label="Add allowed model" onClick={() => onAdd(model, [efforts[0] ?? ""])}>Add</button>
      </div>
    </div>
  );
}

/** One declared exception's row: it edits the exception in place or removes it. */
function ExceptionRow({
  exception,
  roles,
  models,
  efforts,
  editable,
  onChange,
}: {
  exception: PolicyExceptionView;
  roles: string[];
  models: Array<{ id: string }>;
  efforts: string[];
  editable: boolean;
  onChange: (change: PolicyChange | null) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<PolicyExceptionView>(exception);
  if (!editable) {
    return (
      <section className="list-row" aria-label={`exception ${exception.id}`}>
        <div className="row-id">
          <span className="row-name">{exception.id}</span>
          <span className="row-sub">{exception.role || "any role"} · {exception.model} · {exception.reasoningEffort} · {exceptionScope(exception)}</span>
        </div>
      </section>
    );
  }
  if (!editing) {
    return (
      <section className="list-row" aria-label={`exception ${exception.id}`}>
        <div className="row-id">
          <span className="row-name">{exception.id}</span>
          <span className="row-sub">{exception.role || "any role"} · {exception.model} · {exception.reasoningEffort} · {exceptionScope(exception)}</span>
        </div>
        <div className="row-actions">
          <button className="btn" aria-label={`Edit exception ${exception.id}`} onClick={() => { setDraft(exception); setEditing(true); }}>Edit</button>
          <button className="btn danger" aria-label={removeExceptionLabel(exception.id)} onClick={() => onChange({ kind: "removeException", id: exception.id })}>Remove</button>
        </div>
      </section>
    );
  }
  return (
    <section className="list-row role-row" aria-label={`edit exception ${exception.id}`}>
      <div className="row-id"><span className="row-name">{exception.id}</span></div>
      <fieldset className="role-controls" style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }} aria-label={`${exception.id} exception controls`}>
        <div className="role-selects">
          <select className="select" style={{ maxWidth: "160px" }} aria-label={`${exception.id} exception role`} value={draft.role ?? ""} onChange={(e) => setDraft({ ...draft, role: e.target.value === "" ? undefined : e.target.value })}>
            <option value="">any role</option>
            {roles.map((role) => (
              <option key={role} value={role}>{role}</option>
            ))}
          </select>
          <select className="select" style={{ maxWidth: "220px" }} aria-label={`${exception.id} exception model`} value={draft.model} onChange={(e) => setDraft({ ...draft, model: e.target.value })}>
            {models.some((option) => option.id === draft.model) ? null : <option value={draft.model}>{draft.model} (saved, unavailable)</option>}
            {models.map((option) => (
              <option key={option.id} value={option.id}>{option.id}</option>
            ))}
          </select>
          <select className="select" style={{ maxWidth: "160px" }} aria-label={`${exception.id} exception effort`} value={draft.reasoningEffort} onChange={(e) => setDraft({ ...draft, reasoningEffort: e.target.value })}>
            {efforts.includes(draft.reasoningEffort) ? null : <option value={draft.reasoningEffort}>{draft.reasoningEffort} (saved, unavailable)</option>}
            {efforts.map((effort) => (
              <option key={effort} value={effort}>{effort}</option>
            ))}
          </select>
          <input
            className="input"
            style={{ maxWidth: "220px" }}
            aria-label={`${exception.id} exception cwd`}
            value={draft.cwd.join(", ")}
            onChange={(e) => setDraft({ ...draft, cwd: e.target.value.split(",").map((root) => root.trim()).filter((root) => root !== "") })}
          />
        </div>
        <div className="role-selects">
          <button className="btn primary" onClick={() => { onChange({ kind: "setException", id: draft.id, role: draft.role, model: draft.model, effort: draft.reasoningEffort, cwd: draft.cwd }); setEditing(false); }}>Apply</button>
          <button className="btn" onClick={() => setEditing(false)}>Cancel</button>
        </div>
      </fieldset>
    </section>
  );
}

/** The control that adds a new exception. */
function ExceptionAdder({ roles, models, efforts, onAdd }: { roles: string[]; models: Array<{ id: string }>; efforts: string[]; onAdd: (change: PolicyChange) => void }) {
  const [open, setOpen] = useState(false);
  const [id, setId] = useState("");
  const [role, setRole] = useState("");
  const [model, setModel] = useState(models[0]?.id ?? "");
  const [effort, setEffort] = useState(efforts[0] ?? "");
  const [cwd, setCwd] = useState("");
  if (!open) {
    return (
      <div className="list-row">
        <div className="row-id"><span className="row-sub">Add an exception for one scope</span></div>
        <div className="row-actions"><button className="btn" onClick={() => setOpen(true)}>Add exception</button></div>
      </div>
    );
  }
  const roots = cwd.split(",").map((root) => root.trim()).filter((root) => root !== "");
  return (
    <section className="list-row role-row" aria-label="add exception">
      <div className="row-id"><span className="row-name">New exception</span></div>
      <fieldset className="role-controls" style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }} aria-label="new exception controls">
        <div className="role-selects">
          <input className="input" style={{ maxWidth: "160px" }} aria-label="new exception id" placeholder="id" value={id} onChange={(e) => setId(e.target.value)} />
          <select className="select" style={{ maxWidth: "160px" }} aria-label="new exception role" value={role} onChange={(e) => setRole(e.target.value)}>
            <option value="">any role</option>
            {roles.map((name) => (
              <option key={name} value={name}>{name}</option>
            ))}
          </select>
          <select className="select" style={{ maxWidth: "220px" }} aria-label="new exception model" value={model} onChange={(e) => setModel(e.target.value)}>
            {models.map((option) => (
              <option key={option.id} value={option.id}>{option.id}</option>
            ))}
          </select>
          <select className="select" style={{ maxWidth: "160px" }} aria-label="new exception effort" value={effort} onChange={(e) => setEffort(e.target.value)}>
            {efforts.map((name) => (
              <option key={name} value={name}>{name}</option>
            ))}
          </select>
          <input className="input" style={{ maxWidth: "220px" }} aria-label="new exception cwd" placeholder="cwd roots, comma separated" value={cwd} onChange={(e) => setCwd(e.target.value)} />
        </div>
        <div className="role-selects">
          <button
            className="btn primary"
            disabled={id === "" || model === ""}
            onClick={() => { onAdd({ kind: "setException", id, role: role === "" ? undefined : role, model, effort, cwd: roots }); setOpen(false); setId(""); setCwd(""); }}
          >
            Add
          </button>
          <button className="btn" onClick={() => setOpen(false)}>Cancel</button>
        </div>
      </fieldset>
    </section>
  );
}
