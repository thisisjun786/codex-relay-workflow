// Original CRW screen (no CXC counterpart): the execution-policy screen.
//
// Two components, deliberately split. PolicyScreen is presentational: it takes a state and a set of
// handlers and returns the tree, so it renders under react-dom/server and its markup can be asserted
// in a test without a DOM. PolicyPage owns the state, the two effects that read the routes, and the
// save sequence, and every transition it makes goes through the screen* functions in
// policy-state.ts. That split is what makes a screen criterion observable by a test.
import { useEffect, useRef, useState } from "react";
import {
  POLICY_BLAST_RADIUS,
  allowedAddChoice,
  allowedAddBlocked,
  NEW_EXCEPTION_TOKEN,
  exceptionEditToken,
  addModelOptions,
  exceptionRoleOptions,
  allowedEffortsLabel,
  allowedEntriesOf,
  allowedNewOf,
  catalogNotice,
  changeFromExceptionDraft,
  editExceptionLabel,
  draftForExceptionEdit,
  draftForNewException,
  modelOptionLabel,
  modelOptions,
  pairEffortLabel,
  pairModelLabel,
  policyEfforts,
  policyView,
  pendingAllowedModel,
  pendingExceptionId,
  previewChange,
  removeExceptionLabel,
  roleControlsLabel,
  screenAllowedEntryAdded,
  screenAllowedEntryRemoved,
  screenAllowedEntryText,
  screenAllowedNewText,
  screenAllowedAddModel,
  screenCatalogLoaded,
  screenEditable,
  screenEffortUnavailable,
  screenExceptionDraft,
  screenDraftIsNew,
  screenLoaded,
  screenMayEdit,
  screenPropose,
  screenRetryRead,
  screenReread,
  runSave,
  runRead,
  screenRepairCleared,
  screenSaving,
  initialScreen,
  isBlankText,
  screenBusy,
  type ExceptionDraft,
  type PolicyChange,
  type PolicyExceptionView,
  type PolicyPair,
  type PolicyScreenState,
} from "../policy-state.ts";
import { checkPolicy, getModelCatalog, getPolicy, writePolicy } from "../api.ts";
import { Loading } from "../ui/kit.tsx";
import { toast } from "../ui/toast.tsx";
import { HelpDrawer, HelpTopicButton, useHelp, type HelpTopicId } from "../ui/help.tsx";

const PAGE_TITLE = "Execution policy";

/** How often the catalog is re-read, and on window focus as well. */
const CATALOG_REFRESH_MS = 30_000;

/**
 * quoted is one identifier as a row reads it. Every value a row shows is an identifier the policy
 * compares exactly and may contain a space or a comma, so it is quoted: joining a model and an
 * effort with a space would otherwise make the pair ("a b", "c") and the pair ("a", "b c") read as
 * the same text, and one cwd root containing a comma would read as two roots.
 */
function quoted(value: string): string {
  return JSON.stringify(value);
}

/** pairText is one pair as a control row reads it: both identifiers quoted, so each stays one value. */
function pairText(pair: PolicyPair): string {
  return `${quoted(pair.model)} ${quoted(pair.reasoningEffort)}`;
}

/** exceptionScope is an exception's cwd scope as a row reads it: one quoted entry per root. */
function exceptionScope(exception: PolicyExceptionView): string {
  return exception.cwd.length === 0 ? "no cwd scope" : exception.cwd.map(quoted).join(", ");
}

/** What the screen calls when the operator does something. */
export interface PolicyScreenHandlers {
  propose: (change: PolicyChange | null) => void;
  allowedEntryText: (model: string, saved: readonly string[], index: number, text: string) => void;
  allowedEntryAdded: (model: string, saved: readonly string[], text: string) => void;
  allowedEntryRemoved: (model: string, saved: readonly string[], index: number) => void;
  allowedNewText: (model: string, text: string) => void;
  allowedAddModel: (model: string) => void;
  exceptionDraft: (draft: ExceptionDraft | null) => void;
  removeException: (id: string) => void;
  save: () => void;
  retry: () => void;
  reread: () => void;
}

/**
 * The screen's markup for one state. Every value it shows comes from the state; every control it
 * renders names itself through the label builders in policy-state.ts, so no control can appear
 * unlabelled and a test can find each one by the label the operator's assistive technology reads.
 */
export function PolicyScreen({ state, handlers, help }: { state: PolicyScreenState; handlers: PolicyScreenHandlers; help: { open: boolean; topic: HelpTopicId; openHelp: (topic: HelpTopicId) => void; closeHelp: () => void } }) {
  const view = state.reading ? policyView(state.reading) : null;
  const efforts = state.reading ? policyEfforts(state.reading, state.catalog) : [];
  const models = state.reading ? modelOptions(state.reading, state.catalog) : [];
  const editable = screenEditable(state);
  // One pending change at a time: an answer in flight or another row's live edit disables this
  // control, so the single change the API applies is never silently replaced.
  const busy = screenBusy(state);
  const saving = screenSaving(state);
  const preview = state.reading && state.change ? previewChange(state.reading, state.change) : null;
  const roles = view ? view.roles.filter((role) => role.editable).map((role) => role.name) : [];
  const draftIsNew = screenDraftIsNew(state);
  const pendingModel = state.reading ? pendingAllowedModel(state, state.reading) : null;
  // The pending exception change the file no longer declares, rendered as its own row from the
  // change itself (the file has no entry to build it from).
  const pendingException = state.reading && state.change?.kind === "setException" && pendingExceptionId(state, state.reading) !== null
    ? { id: state.change.id, role: state.change.role, model: state.change.model, reasoningEffort: state.change.effort, cwd: [...state.change.cwd] }
    : null;

  /** pairsFor is the pair list a role's controls show: the pending change first, then the reading. */
  function pairsFor(name: string, saved: PolicyPair[]): PolicyPair[] {
    if (state.change?.kind === "setRolePairs" && state.change.role === name) return state.change.pairs;
    return saved;
  }

  return (
    <>
      <div className="page-header">
        <span className="page-header-title">Execution policy</span>
        <HelpTopicButton topic="policy" onOpen={help.openHelp} />
      </div>
      <div className="page-head">
        <div>
          <h1>{PAGE_TITLE}</h1>
          <div className="sub">The supervisor, parent and child model and effort pairs, the allowed list and the exceptions, from the policy file the wiring record names.</div>
        </div>
        {view ? <span className={`badge ${view.editable ? "ok" : ""}`}>{view.state}</span> : null}
      </div>
      <div className="page-body">
        {state.error ? (
          <div role="alert">
            <p>{state.error}</p>
            {/* Retry re-reads the policy the same way the automatic conflict re-read does, so it keeps
                the operator's inputs rather than dropping them. */}
            <button className="btn" onClick={handlers.retry}>Retry</button>
          </div>
        ) : null}
        {!state.reading && !state.error ? <Loading label="Loading the execution policy..." /> : null}
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
                            ? role.declared ? "no pair declared" : "not declared in this file"
                            : pairs.map(pairText).join(" · ")}
                      </span>
                      {role.expectation ? <span className="row-sub">expectation: {role.expectation}</span> : null}
                    </div>
                    {role.editable && editable ? (
                      <fieldset className="role-controls" disabled={busy || !screenMayEdit(state, `role:${role.name}`)} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }} aria-label={roleControlsLabel(role.name)}>
                        {pairs.map((pair, index) => {
                          const savedEffort = !efforts.includes(pair.reasoningEffort) || screenEffortUnavailable(state, pair.model, pair.reasoningEffort);
                          return (
                            <div className="role-selects" key={index}>
                              <select
                                className="select"
                                style={{ maxWidth: "220px" }}
                                aria-label={pairModelLabel(role.name, index)}
                                value={pair.model}
                                onChange={(e) => {
                                  const next = pairs.map((current, at) => (at === index ? { ...current, model: e.target.value } : current));
                                  handlers.propose({ kind: "setRolePairs", role: role.name, pairs: next });
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
                                style={{ maxWidth: "180px" }}
                                aria-label={pairEffortLabel(role.name, index)}
                                value={pair.reasoningEffort}
                                onChange={(e) => {
                                  const next = pairs.map((current, at) => (at === index ? { ...current, reasoningEffort: e.target.value } : current));
                                  handlers.propose({ kind: "setRolePairs", role: role.name, pairs: next });
                                }}
                              >
                                {savedEffort ? (
                                  <option value={pair.reasoningEffort}>{pair.reasoningEffort} (saved, unavailable)</option>
                                ) : null}
                                {efforts.map((effort) => {
                                  const refused = screenEffortUnavailable(state, pair.model, effort);
                                  return (
                                    <option key={effort} value={effort} disabled={refused}>
                                      {refused ? `${effort} (not advertised by this model)` : effort}
                                    </option>
                                  );
                                })}
                              </select>
                              {pairs.length > 1 ? (
                                <button className="btn" onClick={() => handlers.propose({ kind: "setRolePairs", role: role.name, pairs: pairs.filter((_, at) => at !== index) })}>
                                  Remove pair
                                </button>
                              ) : null}
                            </div>
                          );
                        })}
                        <div className="role-selects">
                          <button
                            className="btn"
                            disabled={models.length === 0 || efforts.length === 0}
                            onClick={() => handlers.propose({ kind: "setRolePairs", role: role.name, pairs: [...pairs, { model: models[0]?.id ?? "", reasoningEffort: efforts[0] ?? "" }] })}
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
                    <span className="row-sub">{entry.efforts.map(quoted).join(" · ") || "no effort listed"}</span>
                  </div>
                  {editable ? (
                    <AllowedRow
                      model={entry.model}
                      entries={allowedEntriesOf(state, entry.model, entry.efforts)}
                      newText={allowedNewOf(state, entry.model)}
                      onNewText={(text) => handlers.allowedNewText(entry.model, text)}
                      onEntryText={(index, text) => handlers.allowedEntryText(entry.model, entry.efforts, index, text)}
                      onEntryAdded={(text) => handlers.allowedEntryAdded(entry.model, entry.efforts, text)}
                      onEntryRemoved={(index) => handlers.allowedEntryRemoved(entry.model, entry.efforts, index)}
                      onRemoveModel={() => handlers.propose({ kind: "removeAllowed", model: entry.model })}
                      disabled={busy || !screenMayEdit(state, `allowed:${entry.model}`)}
                    />
                  ) : null}
                </section>
              ))}
              {/* A model the Add control has just proposed is not in the file yet, so it has no row
                  above. It gets one here, with the same editor, so its effort set can be set before
                  Save instead of saving an unwanted approval first and editing it after. */}
              {editable && pendingModel !== null ? (
                <section className="list-row" aria-label={`allowed ${pendingModel} (pending)`}>
                  <div className="row-id">
                    <span className="row-name">{pendingModel}</span>
                    <span className="row-sub">new - not in the file yet</span>
                  </div>
                  <AllowedRow
                    model={pendingModel}
                    entries={allowedEntriesOf(state, pendingModel, [])}
                    newText={allowedNewOf(state, pendingModel)}
                    onNewText={(text) => handlers.allowedNewText(pendingModel, text)}
                    onEntryText={(index, text) => handlers.allowedEntryText(pendingModel, [], index, text)}
                    onEntryAdded={(text) => handlers.allowedEntryAdded(pendingModel, [], text)}
                    onEntryRemoved={(index) => handlers.allowedEntryRemoved(pendingModel, [], index)}
                    onRemoveModel={() => handlers.propose(null)}
                    disabled={busy || !screenMayEdit(state, `allowed:${pendingModel}`)}
                  />
                </section>
              ) : null}
              {editable ? (
                (() => {
                  const free = models.filter((option) => !view.allowed.some((entry) => entry.model === option.id));
                  const listed = view.allowed.map((entry) => entry.model);
                  const freeIds = free.map((option) => option.id);
                  const choice = allowedAddChoice(state, freeIds, listed);
                  // The add control is one more edit: it proposes a setAllowed for `choice`, so it is
                  // disabled in the same condition as that row's own controls, and also when the
                  // choice is a model the file already lists (adding it again would replace its
                  // approved efforts with the single first one).
                  const addDisabled = busy || allowedAddBlocked(state, freeIds, listed) || !screenMayEdit(state, `allowed:${choice}`);
                  if (choice === "" && free.length === 0) return null;
                  return (
                    <div className="list-row">
                      <div className="row-id"><span className="row-sub">Add a model to the allowed list</span></div>
                      <div className="row-actions">
                        <select className="select" style={{ maxWidth: "220px" }} aria-label="model to allow" value={choice} disabled={addDisabled} onChange={(e) => handlers.allowedAddModel(e.target.value)}>
                          {/* The chosen model keeps a matching option even when the refreshed catalog
                              no longer lists it, so the control can never display one model while Add
                              proposes another. */}
                          {addModelOptions(state, freeIds, listed).map((id) => (
                            <option key={id} value={id}>{id}</option>
                          ))}
                        </select>
                        <button className="btn" disabled={addDisabled || efforts.length === 0} aria-label="Add allowed model" onClick={() => handlers.propose({ kind: "setAllowed", model: choice, efforts: [efforts[0] ?? ""] })}>Add</button>
                      </div>
                    </div>
                  );
                })()
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
                  roles={roles}
                  models={models}
                  efforts={efforts}
                  editable={editable}
                  draft={!draftIsNew && state.exceptionDraft?.id === exception.id ? state.exceptionDraft : null}
                  editDraft={draftForExceptionEdit(state, exception)}
                  disabled={busy || !screenMayEdit(state, exceptionEditToken(exception.id))}
                  effortRefused={(model, effort) => screenEffortUnavailable(state, model, effort)}
                  onDraft={handlers.exceptionDraft}
                  onRemove={() => handlers.removeException(exception.id)}
                  onApply={(draft) => {
                    const change = changeFromExceptionDraft(draft);
                    if (change) handlers.propose(change);
                  }}
                />
              ))}
              {/* A pending exception change whose id the file no longer declares has no row above: the
                  conflict re-read kept the operator's change but the file lost the entry. It gets a row
                  here, built from the change, so the pending work is visible and can still be edited or
                  cancelled instead of being pending with no control. */}
              {editable && pendingException !== null ? (
                <ExceptionRow
                  key={`${pendingException.id} (pending)`}
                  exception={pendingException}
                  roles={roles}
                  models={models}
                  efforts={efforts}
                  editable={editable}
                  draft={!draftIsNew && state.exceptionDraft?.id === pendingException.id ? state.exceptionDraft : null}
                  editDraft={draftForExceptionEdit(state, pendingException)}
                  disabled={busy || !screenMayEdit(state, exceptionEditToken(pendingException.id))}
                  effortRefused={(model, effort) => screenEffortUnavailable(state, model, effort)}
                  onDraft={handlers.exceptionDraft}
                  onRemove={() => handlers.propose(null)}
                  onApply={(draft) => {
                    const change = changeFromExceptionDraft(draft);
                    if (change) handlers.propose(change);
                  }}
                />
              ) : null}
              {editable ? (
                <ExceptionAdder
                  roles={roles}
                  models={models}
                  efforts={efforts}
                  draft={draftIsNew ? state.exceptionDraft : null}
                  disabled={busy || !screenMayEdit(state, NEW_EXCEPTION_TOKEN)}
                  effortRefused={(model, effort) => screenEffortUnavailable(state, model, effort)}
                  onDraft={handlers.exceptionDraft}
                  onAdd={(draft) => {
                    const change = changeFromExceptionDraft(draft);
                    if (change) handlers.propose(change);
                  }}
                />
              ) : null}
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
                  <button className="btn" disabled={busy} onClick={() => handlers.propose(null)}>Cancel</button>
                  <button className="btn primary" disabled={busy} onClick={handlers.save}>
                    {saving ? "Saving..." : "Save"}
                  </button>
                </div>
              </div>
            ) : null}

            {state.notice ? (
              <div className="card" role="status" style={{ marginTop: 12 }}>
                <h2 className="card-title">{state.notice.tone === "ok" ? "Saved" : "Not saved"}</h2>
                <p className="sub">{state.notice.text}</p>
                {state.notice.stored ? <p className="sub mono">stored {state.notice.stored}</p> : null}
                {state.notice.registered ? <p className="sub mono">registered {state.notice.registered}</p> : null}
                {state.notice.applied ? <p className="sub mono">applied {state.notice.applied}</p> : null}
                {state.notice.actions.map((action) => (
                  <p className="sub" key={action}>To do: {action}</p>
                ))}
                {state.notice.warnings.map((warning) => (
                  <p className="sub" key={warning}>Warning: {warning}</p>
                ))}
                {state.notice.errors.map((message) => (
                  <p className="sub" key={message}>{message}</p>
                ))}
              {state.notice.blockEditing ? <p className="sub">Editing is blocked until the recovery above is done.</p> : null}
              </div>
            ) : null}

            {/* The repair sentence outlives the notice it arrived with: it is the server's own
                instruction, and the block it explains is still in force until the host is repaired. */}
            {state.repair !== null && state.notice === null ? (
              <div className="card" role="status" style={{ marginTop: 12 }}>
                <h2 className="card-title">Editing is blocked until this is repaired</h2>
                <p className="sub">{state.repair}</p>
              </div>
            ) : null}

            <p className="sub" style={{ marginTop: 12 }}>{catalogNotice(state.catalog)}</p>
            <div className="role-selects">
              <button className="btn" disabled={busy} onClick={handlers.reread}>Read the policy again</button>
            </div>
          </>
        ) : null}
      </div>
      <HelpDrawer open={help.open} topic={help.topic} onClose={help.closeHelp} />
    </>
  );
}

/** One declared exception's row: it edits the exception in place or removes it. */
/**
 * One allowlist row's editor: one field per approved effort, plus a field to add another. Each entry
 * gets its own field because an effort name is an identifier the policy compares exactly and the
 * parser does not forbid a comma inside one, so a single comma-separated field would turn one stored
 * approval into two (or change the name) the moment it was split or joined.
 */
function AllowedRow({
  model,
  entries,
  newText,
  onNewText,
  onEntryText,
  onEntryAdded,
  onEntryRemoved,
  onRemoveModel,
  disabled,
}: {
  model: string;
  entries: readonly string[];
  newText: string;
  onNewText: (text: string) => void;
  onEntryText: (index: number, text: string) => void;
  onEntryAdded: (text: string) => void;
  onEntryRemoved: (index: number) => void;
  onRemoveModel: () => void;
  disabled: boolean;
}) {
  return (
    <fieldset className="role-controls" disabled={disabled} style={{ border: 0, padding: 0, margin: 0, minWidth: 0, flex: 2 }} aria-label={`allowed ${model} controls`}>
      {entries.map((effort, index) => (
        <div className="role-selects" key={index}>
          <input className="input" style={{ maxWidth: "220px" }} aria-label={allowedEffortsLabel(model, index)} value={effort} onChange={(e) => onEntryText(index, e.target.value)} />
          <button className="btn" aria-label={`Remove ${model} effort ${index + 1}`} onClick={() => onEntryRemoved(index)}>Remove effort</button>
        </div>
      ))}
      <div className="role-selects">
        <input className="input" style={{ maxWidth: "220px" }} aria-label={`${model} effort to add`} placeholder="one effort name" value={newText} onChange={(e) => onNewText(e.target.value)} />
        <button className="btn" disabled={newText === ""} onClick={() => onEntryAdded(newText)}>Add effort</button>
        <button className="btn danger" aria-label={`Remove ${model} from the allowed list`} onClick={onRemoveModel}>Remove model</button>
      </div>
    </fieldset>
  );
}

function ExceptionRow({
  exception,
  roles,
  models,
  efforts,
  editable,
  draft,
  editDraft,
  disabled,
  effortRefused,
  onDraft,
  onRemove,
  onApply,
}: {
  exception: PolicyExceptionView;
  roles: string[];
  models: Array<{ id: string; label: string; unavailable: boolean }>;
  efforts: string[];
  editable: boolean;
  draft: ExceptionDraft | null;
  /** The draft Edit opens: the pending change for this exception when there is one, else the file. */
  editDraft: ExceptionDraft;
  disabled: boolean;
  effortRefused: (model: string, effort: string) => boolean;
  onDraft: (draft: ExceptionDraft | null) => void;
  onRemove: () => void;
  onApply: (draft: ExceptionDraft) => void;
}) {
  if (!editable) {
    return (
      <section className="list-row" aria-label={`exception ${exception.id}`}>
        <div className="row-id">
          <span className="row-name">{exception.id}</span>
              <span className="row-sub">{exception.role || "no role - covers requests that cite no role"} · {exception.model} · {exception.reasoningEffort} · {exceptionScope(exception)}</span>
        </div>
      </section>
    );
  }
  if (draft === null) {
    return (
      <section className="list-row" aria-label={`exception ${exception.id}`}>
        <div className="row-id">
          <span className="row-name">{exception.id}</span>
          <span className="row-sub">{exception.role || "no role - covers requests that cite no role"} · {exception.model} · {exception.reasoningEffort} · {exceptionScope(exception)}</span>
        </div>
        <div className="row-actions">
          <button className="btn" disabled={disabled} aria-label={editExceptionLabel(exception.id)} onClick={() => onDraft(editDraft)}>Edit</button>
          <button className="btn danger" disabled={disabled} aria-label={removeExceptionLabel(exception.id)} onClick={onRemove}>Remove</button>
        </div>
      </section>
    );
  }
  return (
    <section className="list-row role-row" aria-label={`edit exception ${exception.id}`}>
      <div className="row-id"><span className="row-name">{exception.id}</span></div>
      <fieldset className="role-controls" disabled={disabled} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }} aria-label={`${exception.id} exception controls`}>
        <div className="role-selects">
          <select className="select" style={{ maxWidth: "160px" }} aria-label={`${exception.id} exception role`} value={draft.role} onChange={(e) => onDraft({ ...draft, role: e.target.value })}>
            {draft.role === "" ? <option value="">no role - covers requests that cite no role</option> : null}
            {exceptionRoleOptions(draft.role).map((role) => (
              <option key={role} value={role}>{role}</option>
            ))}
          </select>
          <select className="select" style={{ maxWidth: "220px" }} aria-label={`${exception.id} exception model`} value={draft.model} onChange={(e) => onDraft({ ...draft, model: e.target.value })}>
            {models.some((option) => option.id === draft.model) ? null : <option value={draft.model}>{draft.model} (saved, unavailable)</option>}
            {models.map((option) => (
              <option key={option.id} value={option.id}>{modelOptionLabel(models, option.id)}</option>
            ))}
          </select>
          <select className="select" style={{ maxWidth: "180px" }} aria-label={`${exception.id} exception effort`} value={draft.effort} onChange={(e) => onDraft({ ...draft, effort: e.target.value })}>
            {!efforts.includes(draft.effort) || effortRefused(draft.model, draft.effort) ? (
              <option value={draft.effort}>{draft.effort} (saved, unavailable)</option>
            ) : null}
            {efforts.map((effort) => {
              // The same per-model judgement the role pair select makes: an effort only another model
              // advertises is not offered for this one.
              const refused = effortRefused(draft.model, effort);
              return (
                <option key={effort} value={effort} disabled={refused}>
                  {refused ? `${effort} (not advertised by this model)` : effort}
                </option>
              );
            })}
          </select>
        </div>
        {/* One input per root: a cwd is a path, and a path may contain a comma, so joining the roots
            into one comma-separated field and splitting them again would change the scope. */}
        {draft.cwd.map((root, index) => (
          <div className="role-selects" key={index}>
            <input className="input" style={{ maxWidth: "320px" }} aria-label={`${exception.id} exception cwd ${index + 1}`} value={root} onChange={(e) => onDraft({ ...draft, cwd: draft.cwd.map((current, at) => (at === index ? e.target.value : current)) })} />
            <button className="btn" aria-label={`Remove ${exception.id} cwd ${index + 1}`} onClick={() => onDraft({ ...draft, cwd: draft.cwd.filter((_, at) => at !== index) })}>Remove root</button>
          </div>
        ))}
        <div className="role-selects">
          <input className="input" style={{ maxWidth: "320px" }} aria-label={`${exception.id} exception cwd to add`} placeholder="one cwd root" value={draft.cwdNew} onChange={(e) => onDraft({ ...draft, cwdNew: e.target.value })} />
          <button
            className="btn"
            disabled={isBlankText(draft.cwdNew)}
            onClick={() => onDraft({ ...draft, cwd: [...draft.cwd, draft.cwdNew], cwdNew: "" })}
          >
            Add root
          </button>
        </div>
        <div className="role-selects">
          <button className="btn primary" disabled={isBlankText(draft.id) || isBlankText(draft.model)} onClick={() => onApply(draft)}>Apply</button>
          <button className="btn" onClick={() => onDraft(null)}>Cancel</button>
        </div>
      </fieldset>
    </section>
  );
}

/** The control that adds a new exception. */
function ExceptionAdder({
  roles,
  models,
  efforts,
  draft,
  disabled,
  effortRefused,
  onDraft,
  onAdd,
}: {
  roles: string[];
  models: Array<{ id: string; label: string; unavailable: boolean }>;
  efforts: string[];
  draft: ExceptionDraft | null;
  disabled: boolean;
  effortRefused: (model: string, effort: string) => boolean;
  onDraft: (draft: ExceptionDraft | null) => void;
  onAdd: (draft: ExceptionDraft) => void;
}) {
  if (draft === null) {
    return (
      <div className="list-row">
        <div className="row-id"><span className="row-sub">Add an exception for one scope</span></div>
        <div className="row-actions">
          <button
            className="btn"
            disabled={disabled || models.length === 0 || efforts.length === 0}
            onClick={() => onDraft(draftForNewException(roles[0] ?? "", models[0]?.id ?? "", efforts[0] ?? ""))}
          >
            Add exception
          </button>
        </div>
      </div>
    );
  }
  return (
    <section className="list-row role-row" aria-label="add exception">
      <div className="row-id"><span className="row-name">New exception</span></div>
      <fieldset className="role-controls" disabled={disabled} style={{ border: 0, padding: 0, margin: 0, minWidth: 0 }} aria-label="new exception controls">
        <div className="role-selects">
          <input className="input" style={{ maxWidth: "160px" }} aria-label="new exception id" placeholder="id" value={draft.id} onChange={(e) => onDraft({ ...draft, id: e.target.value })} />
          <select className="select" style={{ maxWidth: "160px" }} aria-label="new exception role" value={draft.role} onChange={(e) => onDraft({ ...draft, role: e.target.value })}>
            {exceptionRoleOptions(draft.role).map((role) => (
              <option key={role} value={role}>{role}</option>
            ))}
          </select>
          <select className="select" style={{ maxWidth: "220px" }} aria-label="new exception model" value={draft.model} onChange={(e) => onDraft({ ...draft, model: e.target.value })}>
            {/* A draft value the catalog no longer lists keeps a matching option, so the control can
                never display a different model than the draft it will submit. */}
            {models.some((option) => option.id === draft.model) ? null : <option value={draft.model}>{draft.model} (not in the current list)</option>}
            {models.map((option) => (
              <option key={option.id} value={option.id}>{modelOptionLabel(models, option.id)}</option>
            ))}
          </select>
          <select className="select" style={{ maxWidth: "180px" }} aria-label="new exception effort" value={draft.effort} onChange={(e) => onDraft({ ...draft, effort: e.target.value })}>
            {/* The same per-model judgement the role rows and the existing-exception editor make. */}
            {!efforts.includes(draft.effort) || effortRefused(draft.model, draft.effort) ? (
              <option value={draft.effort}>{draft.effort} (not advertised by this model)</option>
            ) : null}
            {efforts.map((effort) => {
              const refused = effortRefused(draft.model, effort);
              return (
                <option key={effort} value={effort} disabled={refused}>
                  {refused ? `${effort} (not advertised by this model)` : effort}
                </option>
              );
            })}
          </select>
        </div>
        {/* One input per root: a cwd is a path, and a path may contain a comma. */}
        {draft.cwd.map((root, index) => (
          <div className="role-selects" key={index}>
            <input className="input" style={{ maxWidth: "320px" }} aria-label={`new exception cwd ${index + 1}`} value={root} onChange={(e) => onDraft({ ...draft, cwd: draft.cwd.map((current, at) => (at === index ? e.target.value : current)) })} />
            <button className="btn" aria-label={`Remove cwd ${index + 1}`} onClick={() => onDraft({ ...draft, cwd: draft.cwd.filter((_, at) => at !== index) })}>Remove root</button>
          </div>
        ))}
        <div className="role-selects">
          <input className="input" style={{ maxWidth: "320px" }} aria-label="new exception cwd to add" placeholder="one cwd root" value={draft.cwdNew} onChange={(e) => onDraft({ ...draft, cwdNew: e.target.value })} />
          <button
            className="btn"
            disabled={isBlankText(draft.cwdNew)}
            onClick={() => onDraft({ ...draft, cwd: [...draft.cwd, draft.cwdNew], cwdNew: "" })}
          >
            Add root
          </button>
        </div>
        <div className="role-selects">
          <button
            className="btn primary"
            disabled={isBlankText(draft.id) || isBlankText(draft.model) || draft.cwd.length === 0}
            onClick={() => onAdd(draft)}
          >
            Add
          </button>
          <button className="btn" onClick={() => onDraft(null)}>Cancel</button>
        </div>
      </fieldset>
    </section>
  );
}

/** The stateful screen: it owns the state, the two reads and the save sequence. */
export function PolicyPage() {
  const [state, setState] = useState<PolicyScreenState>(initialScreen);
  const [reload, setReload] = useState(0);
  /**
   * The screen state as it is right now. React's functional setState runs its updater during the
   * render, not at call time, so a ref is what lets one event handler see the state another handler
   * set in the same tick - and what lets the save sequence ask, after several awaits, whether the
   * operator started another edit while it was in flight.
   */
  const stateRef = useRef<PolicyScreenState>(state);
  /**
   * apply is the one place the screen state changes. It derives the next state synchronously from the
   * ref, stores it back, and renders it, so the ref is never behind what the screen shows.
   */
  function apply(update: (previous: PolicyScreenState) => PolicyScreenState) {
    const next = update(stateRef.current);
    stateRef.current = next;
    setState(next);
  }
  /** propose sets the pending change in both the ref and the state. */
  function propose(change: PolicyChange | null) {
    apply((previous) => screenPropose(previous, change));
  }
  // keepInputs is read when the read resolves, so it is a ref rather than a dependency: it describes
  // the read that is in flight, not a reason to start another one.
  const keepInputs = useRef(false);
  const generation = useRef(0);
  const catalogGeneration = useRef(0);
  const catalogLoading = useRef(false);
  const { helpOpen, helpTopic, openHelp, closeHelp } = useHelp("policy");

  async function refreshCatalog(force = false) {
    if (catalogLoading.current) return;
    catalogLoading.current = true;
    const current = ++catalogGeneration.current;
    const next = await getModelCatalog(force);
    if (current !== catalogGeneration.current) return;
    apply((previous) => screenCatalogLoaded(previous, next));
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
    const keep = keepInputs.current;
    keepInputs.current = false;
    // The whole read-and-apply sequence lives in runRead (policy-state.ts), outside React, so the
    // shipped lifecycle is the one the tests drive with a fake transport rather than a parallel copy.
    void runRead(stateRef.current, () => getPolicy(controller.signal), keep).then((outcome) => {
      if (controller.signal.aborted || current !== generation.current) return;
      apply(() => outcome.state);
    });
    return () => {
      controller.abort();
      generation.current++;
    };
  }, [reload]);

  /** readAgain re-reads the policy, keeping the operator's inputs. */
  function readAgain(keep: boolean) {
    keepInputs.current = keep;
    setReload((n) => n + 1);
  }

  async function save() {
    // The whole check-then-write sequence lives in runSave (policy-state.ts), outside React, so the
    // shipped flow is the one the tests drive with a fake transport rather than a parallel copy.
    const outcome = await runSave(stateRef.current, {
      check: (payload) => checkPolicy(payload),
      write: (payload) => writePolicy(payload),
    }, (started) => apply(() => started));
    // The outcome carries the save's own fields; the reading and the catalog are merged from whatever
    // the screen holds now, so a catalog that arrived while the write was in flight is not dropped.
    apply((previous) => ({ ...outcome.state, reading: previous.reading, catalog: previous.catalog, error: previous.error }));
    if (outcome.saved) toast("Execution policy saved", "ok");
    // The re-read always starts from the file; a conflict or a lost response keeps the operator's
    // inputs across it, which is why the outcome carries that flag rather than the caller guessing.
    if (outcome.reread) readAgain(outcome.rereadKeepsInputs);
  }

  return (
    <PolicyScreen
      state={state}
      handlers={{
        propose,
        allowedEntryText: (model, saved, index, text) => apply((previous) => screenAllowedEntryText(previous, model, saved, index, text)),
        allowedEntryAdded: (model, saved, text) => apply((previous) => screenAllowedEntryAdded(previous, model, saved, text)),
        allowedEntryRemoved: (model, saved, index) => apply((previous) => screenAllowedEntryRemoved(previous, model, saved, index)),
        allowedNewText: (model, text) => apply((previous) => screenAllowedNewText(previous, model, text)),
        allowedAddModel: (model) => apply((previous) => screenAllowedAddModel(previous, model)),
        exceptionDraft: (draft) => apply((previous) => screenExceptionDraft(previous, draft)),
        removeException: (id) => propose({ kind: "removeException", id }),
        save: () => void save(),
        retry: () => { apply(screenRetryRead); readAgain(true); },
        // screenReread clears the drafts that exist now; the read keeps whatever the operator starts
        // after this point, which is why it is asked to keep the inputs rather than to drop them.
        reread: () => { apply(screenReread); readAgain(true); },
      }}
      help={{ open: helpOpen, topic: helpTopic, openHelp, closeHelp }}
    />
  );
}
